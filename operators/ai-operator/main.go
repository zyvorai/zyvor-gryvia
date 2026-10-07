package main

import (
	"flag"
	"fmt"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/servingproxy"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/controllers"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/jobhook"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/llmgateway"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/modelhub"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/timemachine"
	jobwebhook "github.com/zyvorai/gryvia/operators/ai-operator/pkg/webhook"
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
	if len(os.Args) > 1 && os.Args[1] == "inference-proxy" {
		if err := servingproxy.Run(); err != nil {
			setupLog.Error(err, "inference proxy")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "llm-gateway" {
		if err := llmgateway.Run(os.Args[2:]); err != nil {
			setupLog.Error(err, "llm gateway")
			os.Exit(1)
		}
		return
	}
	var federationServers, federationNamespace string
	var reportUnsupportedAPIs bool
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var enableWebhooks bool
	var webhookCertDir string
	var fabricAware bool
	var fabricMaxPenalty float64
	var kueueIntegration bool
	var kueueStrictAdmission bool
	var kueueDefaultQueue string
	var admissionGate bool
	var checkpointGuard bool
	var preflightEnforce bool
	var admissionDefaultHours float64
	var ml mlOptions
	var mergeFabricSignals bool
	var placementHolds bool
	var enableJobHooks bool
	var jobHookAllowedCIDRs string
	var jobHookWorkers int

	flag.StringVar(&federationServers, "federation-allowed-servers", "", "Comma-separated administrator-allowed HTTPS Kubernetes API servers; empty disables federation probes.")
	flag.StringVar(&federationNamespace, "federation-credentials-namespace", "gryvia-system", "Namespace containing trusted inline federation kubeconfig secrets.")
	flag.BoolVar(&reportUnsupportedAPIs, "report-unsupported-apis", false, "Report unsupported legacy APIs with Ready=False instead of silently leaving them pending.")
	flag.BoolVar(&enableJobHooks, "enable-job-hooks", false, "Run the GryviaJobHook controller: webhooks when GryviaAIJobs and GryviaWorkflows change phase.")
	flag.IntVar(&jobHookWorkers, "job-hook-workers", 8, "Goroutines that POST job hook deliveries; reconciles only queue them.")
	flag.StringVar(&jobHookAllowedCIDRs, "job-hook-allowed-cidrs", "", "Comma-separated CIDRs job hooks may reach although private (and over plain http), e.g. the cluster's service CIDR; empty allows public https receivers only.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager.")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", false,
		"Serve the GryviaAIJob validating admission webhook (needs TLS certs in --webhook-cert-dir).")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"Directory holding tls.crt and tls.key for the webhook server.")

	flag.BoolVar(&fabricAware, "fabric-aware-scheduling", false,
		"Rank nodes with the fresh per-node fabric health published by the collector (GryviaNodeFabric). Per-job override: annotation gryvia.io/fabric-aware=true|false. Off by default.")
	flag.Float64Var(&fabricMaxPenalty, "fabric-max-penalty", 25,
		"Most points fabric health may subtract from a node score (0 or above 25 means 25).")

	flag.BoolVar(&kueueIntegration, "kueue-integration", false,
		"Create the batch Job of a job with a queue suspended and labelled kueue.x-k8s.io/queue-name so Kueue admits all its pods together, and report Kueue's admission state as phase Queued. Needs Kueue installed. Off by default: nothing changes without it.")
	flag.BoolVar(&kueueStrictAdmission, "kueue-strict-admission", false, "Require Kueue admission for tenant batch jobs even if the default LocalQueue is missing. Requires --kueue-integration.")
	flag.StringVar(&kueueDefaultQueue, "kueue-default-queue", "gryvia",
		"With --kueue-integration: LocalQueue used by jobs in tenant-* namespaces that name no queue (only if that LocalQueue exists).")
	flag.BoolVar(&checkpointGuard, "checkpoint-guard", false,
		"Inject a matching GryviaCheckpointGuard's checkpoint environment into new batch jobs, create their status ConfigMap and Role, and replace pods stuck on lost nodes for AutoRestore guards.")
	flag.BoolVar(&admissionGate, "admission-gate", false,
		"Before creating a job's workload, check the quotas and hard budgets covering its namespace (spend from usage records plus a forecast for the job) and reject it instead of creating it. Fails open on lookup errors. Off by default.")
	flag.BoolVar(&preflightEnforce, "preflight-enforce", false,
		"Before creating a job's workload, reject a job whose preflight annotations (gryvia.io/model-params-billions and friends) fit no node pool by GPU type, node shape, estimated memory per GPU, RDMA or interconnect. Jobs without the annotations, an empty cluster and unknown GPU memory are allowed. Fails open on lookup errors. Off by default.")
	flag.Float64Var(&admissionDefaultHours, "admission-default-hours", 1,
		"Hours a job without spec.timeout is assumed to run for the admission gate's cost forecast.")
	flag.BoolVar(&placementHolds, "placement-holds", false,
		"Hold the GPUs of a job's chosen nodes until its pods are up, so two jobs placed in the same window cannot pick the same free GPUs. A single job is already placed all-or-nothing; this closes the race between jobs. In memory, expires after 5 minutes, off by default.")
	ml.bind(flag.CommandLine)
	flag.BoolVar(&mergeFabricSignals, "merge-fabric-signals", false,
		"Fold the per-node entries collectors write into GryviaFabricSignal status.nodes[] into the top-level status (max for degradation metrics, sample-weighted means for ratios, stale entries ignored). Off by default.")

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if kueueStrictAdmission && !kueueIntegration {
		fmt.Fprintln(os.Stderr, "--kueue-strict-admission requires --kueue-integration")
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgrOpts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "ai-operator.gryvia.io",
	}
	if enableWebhooks {
		mgrOpts.WebhookServer = webhook.NewServer(webhook.Options{
			Port:    9443,
			CertDir: webhookCertDir,
		})
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.GryviaAIJobReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Log:    ctrl.Log.WithName("controllers").WithName("GryviaAIJob"),

		FabricAware:          fabricAware,
		FabricMaxPenalty:     fabricMaxPenalty,
		KueueIntegration:     kueueIntegration,
		KueueStrictAdmission: kueueStrictAdmission,
		KueueDefaultQueue:    kueueDefaultQueue,
		Recorder:             mgr.GetEventRecorderFor("gryviaaijob-controller"),

		AdmissionGate:         admissionGate,
		CheckpointGuard:       checkpointGuard,
		PreflightEnforce:      preflightEnforce,
		AdmissionDefaultHours: admissionDefaultHours,
		PlacementHolds:        placementHolds,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaAIJob")
		os.Exit(1)
	}

	if mergeFabricSignals {
		if err = (&controllers.GryviaFabricSignalReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaFabricSignal")
			os.Exit(1)
		}
	}

	// Create kubernetes clientset for pod log access
	clientset, err := kubernetes.NewForConfig(ctrl.GetConfigOrDie())
	if err != nil {
		setupLog.Error(err, "unable to create kubernetes clientset")
		os.Exit(1)
	}

	if err = (&controllers.GryviaLiveExperimentReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Log:       ctrl.Log.WithName("controllers").WithName("GryviaLiveExperiment"),
		Clientset: clientset,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaLiveExperiment")
		os.Exit(1)
	}

	if err = (&controllers.GryviaCheckpointGuardReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Log:     ctrl.Log.WithName("controllers").WithName("GryviaCheckpointGuard"),
		Enabled: checkpointGuard,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaCheckpointGuard")
		os.Exit(1)
	}

	if err = (&controllers.GryviaTrainingProfilerReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Log:      ctrl.Log.WithName("controllers").WithName("GryviaTrainingProfiler"),
		Recorder: mgr.GetEventRecorderFor("gryviatrainingprofiler-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaTrainingProfiler")
		os.Exit(1)
	}

	if err = (&controllers.GryviaModelLineageReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaModelLineage")
		os.Exit(1)
	}

	if err = (&controllers.GryviaTrainingTimeMachineReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Log:         ctrl.Log.WithName("controllers").WithName("GryviaTrainingTimeMachine"),
		ForkHandler: timemachine.NewForkHandler(mgr.GetClient()),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaTrainingTimeMachine")
		os.Exit(1)
	}

	// The ML controllers: their flags and defaults are in ml_controllers.go, docs/ml-controllers.md explains them.
	if federationServers != "" {
		if err = (&controllers.GryviaFederationReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Log: ctrl.Log.WithName("federation"), AllowedServers: strings.Split(federationServers, ","), CredentialsNamespace: federationNamespace}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "federation controller")
			os.Exit(1)
		}
	}
	if ml.enabled {
		if err = (&controllers.GryviaPriorityReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Log: ctrl.Log.WithName("priority")}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "priority controller")
			os.Exit(1)
		}
		if err = (&controllers.GryviaTemplateReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Log: ctrl.Log.WithName("template")}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "template controller")
			os.Exit(1)
		}
		if ml.autoServeGPUCount < 0 {
			setupLog.Error(nil, "--autoserve-default-gpu-count must not be negative")
			os.Exit(1)
		}
		if err = (&controllers.GryviaWorkspaceReconciler{
			Client:       mgr.GetClient(),
			Scheme:       mgr.GetScheme(),
			Log:          ctrl.Log.WithName("controllers").WithName("GryviaWorkspace"),
			JupyterImage: ml.workspaceJupyterImage,
			CodeImage:    ml.workspaceCodeImage,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaWorkspace")
			os.Exit(1)
		}

		if err = (&controllers.GryviaInferenceServiceReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Log:    ctrl.Log.WithName("controllers").WithName("GryviaInferenceService"),
			Images: map[gryviav1.InferenceBackend]string{
				gryviav1.BackendVLLM:        ml.inferenceImageVLLM,
				gryviav1.BackendTriton:      ml.inferenceImageTriton,
				gryviav1.BackendTensorRTLLM: ml.inferenceImageTensorRT,
				gryviav1.BackendTorchServe:  ml.inferenceImageTorchServe,
			},
			HealthPath:         ml.inferenceHealthPath,
			CanaryStartupGrace: ml.canaryStartupGrace,
			GatewayRouting:     ml.inferenceGatewayRouting,
			PrometheusURL:      ml.inferencePrometheusURL,
			TelemetryImage:     ml.inferenceTelemetryImage,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaInferenceService")
			os.Exit(1)
		}

		if err = (&controllers.GryviaModelRegistryReconciler{
			Client:            mgr.GetClient(),
			Scheme:            mgr.GetScheme(),
			Log:               ctrl.Log.WithName("controllers").WithName("GryviaModelRegistry"),
			AutoServeGPUCount: int32(ml.autoServeGPUCount),
			Recorder:          mgr.GetEventRecorderFor("gryviamodelregistry-controller"),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaModelRegistry")
			os.Exit(1)
		}

		if err = (&controllers.GryviaWorkflowReconciler{
			Client:           mgr.GetClient(),
			Scheme:           mgr.GetScheme(),
			Log:              ctrl.Log.WithName("controllers").WithName("GryviaWorkflow"),
			MaxParallelSteps: ml.workflowMaxParallelSteps,
			AllowWebhooks:    ml.workflowAllowWebhooks,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaWorkflow")
			os.Exit(1)
		}

		if err = (&controllers.GryviaAutoTunerReconciler{
			Client:            mgr.GetClient(),
			Scheme:            mgr.GetScheme(),
			Log:               ctrl.Log.WithName("controllers").WithName("GryviaAutoTuner"),
			MaxTrialsCap:      int32(ml.tunerMaxTrials),
			MaxParallelismCap: int32(ml.tunerMaxParallelism),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaAutoTuner")
			os.Exit(1)
		}

		if ml.modelWatchEnabled {
			if err = (&controllers.GryviaModelWatchReconciler{
				Client:          mgr.GetClient(),
				Scheme:          mgr.GetScheme(),
				Log:             ctrl.Log.WithName("controllers").WithName("GryviaModelWatch"),
				Hub:             &modelhub.Client{BaseURL: ml.modelWatchHubURL},
				MinPollInterval: ml.modelWatchMinPollEvery,
			}).SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "GryviaModelWatch")
				os.Exit(1)
			}
		}
		if ml.ragEnabled {
			if ml.llmGatewayURL == "" {
				setupLog.Error(nil, "--enable-rag needs --llm-gateway-url (ingestion embeds through the LLM gateway)")
				os.Exit(1)
			}
			if err = (&controllers.GryviaVectorIndexReconciler{
				Client:       mgr.GetClient(),
				Scheme:       mgr.GetScheme(),
				Log:          ctrl.Log.WithName("controllers").WithName("GryviaVectorIndex"),
				QdrantImage:  ml.ragQdrantImage,
				IngestImage:  ml.ragIngestImage,
				GatewayURL:   ml.llmGatewayURL,
				KeyNamespace: ml.llmKeyNamespace,
			}).SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "GryviaVectorIndex")
				os.Exit(1)
			}
		}
		if ml.agentsEnabled {
			if ml.llmGatewayURL == "" {
				setupLog.Error(nil, "--enable-agents needs --llm-gateway-url (agents call models through the LLM gateway)")
				os.Exit(1)
			}
			if err = (&controllers.GryviaAgentReconciler{
				Client:       mgr.GetClient(),
				Scheme:       mgr.GetScheme(),
				Log:          ctrl.Log.WithName("controllers").WithName("GryviaAgent"),
				Image:        ml.agentImage,
				GatewayURL:   ml.llmGatewayURL,
				KeyNamespace: ml.llmKeyNamespace,
			}).SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "GryviaAgent")
				os.Exit(1)
			}
		}
	} else if ml.modelWatchEnabled || ml.ragEnabled || ml.agentsEnabled {
		setupLog.Error(nil, "--enable-model-watch, --enable-rag and --enable-agents need --enable-ml-controllers")
		os.Exit(1)
	}

	if enableWebhooks {
		mgr.GetWebhookServer().Register(jobwebhook.ValidatePath,
			&admission.Webhook{Handler: jobwebhook.NewGryviaAIJobValidator(mgr.GetClient())})
		setupLog.Info("registered validating webhook", "path", jobwebhook.ValidatePath, "certDir", webhookCertDir)
	}

	if enableJobHooks {
		allowed, err := jobhook.ParseCIDRs(jobHookAllowedCIDRs)
		if err != nil {
			setupLog.Error(err, "--job-hook-allowed-cidrs")
			os.Exit(1)
		}
		if err = (&controllers.GryviaJobHookReconciler{
			Client:  mgr.GetClient(),
			Scheme:  mgr.GetScheme(),
			Log:     ctrl.Log.WithName("controllers").WithName("GryviaJobHook"),
			Guard:   jobhook.Guard{Allowed: allowed},
			Workers: jobHookWorkers,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaJobHook")
			os.Exit(1)
		}
	}

	if reportUnsupportedAPIs && !enableJobHooks {
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

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
