// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
	"github.com/zyvorai/gryvia/operators/network-intelligence/controllers"
	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gryviav1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var cc sources.CollectorConfig

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	// Collector source (see docs/network-intelligence-sources.md). Every flag has a safe default: with no
	// collector pod the affected objects get SourceAvailable=False instead of empty data.
	defNS := os.Getenv("POD_NAMESPACE")
	if defNS == "" {
		defNS = "gryvia-network"
	}
	flag.StringVar(&cc.Namespace, "collector-namespace", defNS,
		"Namespace whose pods labelled app.kubernetes.io/component=collector are the eBPF collectors (default: the operator's namespace).")
	flag.IntVar(&cc.Port, "collector-port", 9090, "Collector HTTP port.")
	flag.StringVar(&cc.TokenFile, "collector-token-file", "",
		"File (mounted Secret) with the collector API token; requests are HMAC-signed with it, read on every request. Empty sends unsigned requests.")
	flag.BoolVar(&cc.TLS, "collector-tls", false, "Use https for collectors with the system trust store (implied by --collector-ca-file).")
	flag.StringVar(&cc.CAFile, "collector-ca-file", "", "CA bundle that must have signed the collector certificate (implies https).")
	flag.StringVar(&cc.CertFile, "collector-client-cert-file", "", "Client certificate for collectors with mTLS.")
	flag.StringVar(&cc.KeyFile, "collector-client-key-file", "", "Client key for collectors with mTLS.")
	flag.StringVar(&cc.ServerName, "collector-server-name", "",
		"Certificate name to verify (collectors are reached by pod IP, so the certificate must carry this DNS name).")
	flag.DurationVar(&cc.Timeout, "collector-timeout", 5*time.Second, "Per-request timeout for one collector.")
	flag.IntVar(&cc.MaxConcurrency, "collector-concurrency", 8, "Collectors queried in parallel.")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "network-intelligence.gryvia.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Data sources. The pod list bypasses the cache: the operator must not start a cluster-wide pod informer for it.
	collector := sources.NewCollectorClient(cc, sources.KubeDiscoverer(mgr.GetAPIReader(), cc.Namespace))
	netra := sources.NewNetraClient(sources.NetraFromEnv(os.Getenv))
	setupLog.Info("data sources", "collectorNamespace", cc.Namespace, "collectorSigned", cc.TokenFile != "",
		"collectorTLS", cc.TLS || cc.CAFile != "", "netraConfigured", netra.Configured())

	// Register GryviaFlowPolicy controller
	if err = (&controllers.GryviaFlowPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Netra:  netra,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaFlowPolicy")
		os.Exit(1)
	}

	// Register GryviaTrafficInsight controller
	if err = (&controllers.GryviaTrafficInsightReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Collector: collector,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaTrafficInsight")
		os.Exit(1)
	}

	// Register GryviaAutoPolicy controller
	if err = (&controllers.GryviaAutoPolicyReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Collector: collector,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaAutoPolicy")
		os.Exit(1)
	}

	// Register GryviaTraceSession controller
	if err = (&controllers.GryviaTraceSessionReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Netra:  netra,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaTraceSession")
		os.Exit(1)
	}

	// Register GryviaServiceGraph controller
	if err = (&controllers.GryviaServiceGraphReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Collector: collector,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaServiceGraph")
		os.Exit(1)
	}

	// Register GryviaNetworkAnomaly controller
	if err = (&controllers.GryviaNetworkAnomalyReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Collector: collector,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaNetworkAnomaly")
		os.Exit(1)
	}

	// Register GryviaSecurityPolicy controller
	if err = (&controllers.GryviaSecurityPolicyReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Collector: collector,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaSecurityPolicy")
		os.Exit(1)
	}

	// Register GryviaNetworkCost controller
	if err = (&controllers.GryviaNetworkCostReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaNetworkCost")
		os.Exit(1)
	}

	// Register GryviaTrainingInsight controller
	if err = (&controllers.GryviaTrainingInsightReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaTrainingInsight")
		os.Exit(1)
	}

	// Register GryviaInferenceInsight controller
	if err = (&controllers.GryviaInferenceInsightReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaInferenceInsight")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting NetPredator network intelligence manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
