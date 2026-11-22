// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/gpu-operator/controllers"
	"github.com/zyvorai/gryvia/operators/gpu-operator/pkg/memory"
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
	var enableGPUHealth, enableGPURemediation, enableGPUSharing bool
	var enableLeaderElection bool
	var probeAddr string
	var autoRegister bool
	var devicePluginConfigName, devicePluginConfigNamespace string

	flag.BoolVar(&enableGPUHealth, "enable-gpu-health-controller", false, "Enable GPU health checks from fresh GryviaGpuNode observations.")
	flag.BoolVar(&enableGPURemediation, "enable-gpu-remediation", false, "Allow requested cordon/quarantine and PDB-respecting drain on measured GPU health failures.")
	flag.BoolVar(&enableGPUSharing, "enable-gpu-sharing", false, "Reconcile GryviaGPUSharingPolicy: label matching GPU nodes for time-slicing, MIG and fractional sharing (nvidia.com/device-plugin.config, nvidia.com/mig.config and gryvia.io/* labels). Off by default; it writes node labels.")
	flag.StringVar(&devicePluginConfigName, "device-plugin-config-name", "", "ConfigMap the NVIDIA device plugin reads (GPU Operator devicePlugin.config.name). With --device-plugin-config-namespace, a time-slicing GryviaGPUSharingPolicy writes the data key for its maxPodsPerGPU there. Empty = only label nodes with the fixed key gryvia-time-slicing.")
	flag.StringVar(&devicePluginConfigNamespace, "device-plugin-config-namespace", "", "Namespace of --device-plugin-config-name.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")

	flag.BoolVar(&autoRegister, "auto-register", true,
		"Create a GryviaGpuNode for every node labelled nvidia.com/gpu.present=true by GPU Feature Discovery "+
			"(only objects labelled gryvia.io/auto-registered=true are ever modified or deleted).")

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
		LeaderElectionID:       "gpu-operator.gryvia.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.GryviaGpuNodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Log:    ctrl.Log.WithName("controllers").WithName("GryviaGpuNode"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaGpuNode")
		os.Exit(1)
	}

	if autoRegister {
		if err = (&controllers.NodeDiscoveryReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Log:    ctrl.Log.WithName("controllers").WithName("NodeDiscovery"),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "NodeDiscovery")
			os.Exit(1)
		}
	}

	if enableGPUHealth {
		if err = (&controllers.GryviaHealthCheckReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Log: ctrl.Log.WithName("gpu-health"), EnableRemediation: enableGPURemediation}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "health controller")
			os.Exit(1)
		}
	}
	if err = (&controllers.GryviaGpuMemoryOptimizerReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Log:       ctrl.Log.WithName("controllers").WithName("GryviaGpuMemoryOptimizer"),
		Predictor: memory.NewPredictor(24 * time.Hour),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaGpuMemoryOptimizer")
		os.Exit(1)
	}

	if enableGPUSharing {
		if err = (&controllers.GryviaGPUSharingPolicyReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Log:    ctrl.Log.WithName("controllers").WithName("GryviaGPUSharingPolicy"),

			DevicePluginConfigName:      devicePluginConfigName,
			DevicePluginConfigNamespace: devicePluginConfigNamespace,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaGPUSharingPolicy")
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

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
