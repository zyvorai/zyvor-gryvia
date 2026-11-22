// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	gryviav1 "github.com/zyvorai/gryvia/operators/storage-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/storage-operator/controllers"
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
	var reportUnsupportedAPIs bool
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var enableDatasets bool
	var datasetNamespace, datasetImage, datasetS3Image, datasetDefaultSize string

	flag.BoolVar(&enableDatasets, "enable-datasets", false, "Run the GryviaDataset controller: materialize http, s3 and nfs sources into a PVC per dataset.")
	flag.StringVar(&datasetNamespace, "dataset-namespace", "gryvia-system", "Namespace for a dataset's PVC and download Jobs when spec.namespace is empty.")
	flag.StringVar(&datasetImage, "dataset-image", "busybox:1.36", "Image of the http and nfs download Jobs (sh, wget, sha256sum, find, stat).")
	flag.StringVar(&datasetS3Image, "dataset-s3-image", "amazon/aws-cli:2.17.0", "Image of the s3 download Jobs (the AWS CLI and a POSIX shell).")
	flag.StringVar(&datasetDefaultSize, "dataset-default-size", "10Gi", "PVC size of a dataset without spec.cache.size.")
	flag.BoolVar(&reportUnsupportedAPIs, "report-unsupported-apis", false, "Report unsupported legacy APIs with Ready=False instead of silently leaving them pending.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager.")

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "storage-operator.gryvia.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.GryviaStorageReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Log:    ctrl.Log.WithName("controllers").WithName("GryviaStorage"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaStorage")
		os.Exit(1)
	}

	if enableDatasets {
		if err = (&controllers.GryviaDatasetReconciler{
			Client:           mgr.GetClient(),
			Scheme:           mgr.GetScheme(),
			Log:              ctrl.Log.WithName("controllers").WithName("GryviaDataset"),
			DefaultNamespace: datasetNamespace,
			Image:            datasetImage,
			S3Image:          datasetS3Image,
			DefaultSize:      datasetDefaultSize,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaDataset")
			os.Exit(1)
		}
	}

	if reportUnsupportedAPIs && !enableDatasets {
		if err := controllers.RegisterAPIContracts(mgr); err != nil {
			setupLog.Error(err, "API capability contracts")
			os.Exit(1)
		}
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting storage operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
