package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/controllers"
	usageadmission "github.com/zyvorai/gryvia/operators/quota-operator/pkg/admission"
	quotametrics "github.com/zyvorai/gryvia/operators/quota-operator/pkg/metrics"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/notify"
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
	var kueueIntegration, kueueGPUTypeFlavors bool
	var kueueQuotaResources, kueueTopology, kueueAdmissionCheck string
	var kueueFairSharing bool
	var tenantRBAC, enableReservations bool
	var enableWebhooks, billingLedger bool
	var webhookCertDir string
	var budgetWebhookURL string
	var slurmIntegration, slurmOversubscribe bool
	var slurmNamespace, slurmController, slurmdImage, slurmRestURL, slurmTokenSecret, slurmTokenKey string
	var slurmNodes, slurmGPUsPerNode int
	var slurmAccountingInterval time.Duration

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&kueueIntegration, "kueue-integration", false,
		"Create a Kueue LocalQueue (tenant-<name>/gryvia), ClusterQueue (gryvia-<tenant>, cohort gryvia) and ResourceFlavors for every GryviaTenant. Needs Kueue installed. Off by default.")
	flag.BoolVar(&kueueFairSharing, "kueue-fair-sharing", false, "Configure equal-weight Kueue fair sharing; enable fairSharing in Kueue manager configuration too.")
	flag.StringVar(&kueueTopology, "kueue-topology-name", "", "Existing Kueue Topology for managed ResourceFlavors.")
	flag.StringVar(&kueueAdmissionCheck, "kueue-admission-check", "", "Existing administrator-owned admission check, e.g. MultiKueue dispatcher.")
	flag.StringVar(&kueueQuotaResources, "kueue-quota-resources", controllers.DefaultKueueQuotaResources,
		"With --kueue-integration: comma-separated resources the tenant's nominal quota (concurrentGPUs, else the GryviaQuota maxGPUs) is applied to. The kind e2e sets cpu.")
	flag.BoolVar(&kueueGPUTypeFlavors, "kueue-gpu-type-flavors", false,
		"With --kueue-integration: one ResourceFlavor per enabled GryviaGpuSku gpuType (node label gryvia.io/gpu=<type>) in every ClusterQueue. Each flavor gets the full nominal quota; see docs/kueue-integration.md.")
	flag.BoolVar(&slurmIntegration, "slurm-integration", false,
		"Create a Slinky NodeSet (and so the Slurm partition gryvia-<tenant>) for every GryviaTenant and record finished Slurm jobs as GryviaUsageRecords. Needs SchedMD's slurm-operator and a Controller CR. Off by default.")
	flag.StringVar(&slurmNamespace, "slurm-namespace", "", "With --slurm-integration: namespace of the Slurm Controller CR, the NodeSets and the token Secret.")
	flag.StringVar(&slurmController, "slurm-controller", "slurm", "With --slurm-integration: name of the Slinky Controller CR.")
	flag.StringVar(&slurmdImage, "slurm-slurmd-image", controllers.DefaultSlurmdImage, "With --slurm-integration: slurmd image of the tenant NodeSets.")
	flag.IntVar(&slurmNodes, "slurm-nodes-per-tenant", 1, "With --slurm-integration: slurmd pods per tenant when there is no GPU quota or --slurm-gpus-per-node is 0.")
	flag.IntVar(&slurmGPUsPerNode, "slurm-gpus-per-node", 0, "With --slurm-integration: nvidia.com/gpu per slurmd pod; the tenant GPU quota divided by it is the node count. 0 is CPU only.")
	flag.BoolVar(&slurmOversubscribe, "slurm-oversubscribe", false, "With --slurm-integration: allow several slurmd pods on one Kubernetes node (test clusters).")
	flag.StringVar(&slurmRestURL, "slurm-restapi-url", "", "With --slurm-integration: slurmrestd URL (default http://slurm-restapi.<slurm-namespace>.svc:6820); empty token secret disables accounting.")
	flag.StringVar(&slurmTokenSecret, "slurm-token-secret", "gryvia-slurm-token", "With --slurm-integration: Secret in --slurm-namespace holding a slurmrestd JWT; empty disables accounting.")
	flag.StringVar(&slurmTokenKey, "slurm-token-key", "auth-token", "With --slurm-integration: key of the JWT in --slurm-token-secret.")
	flag.DurationVar(&slurmAccountingInterval, "slurm-accounting-interval", 30*time.Second, "With --slurm-integration: how often finished Slurm jobs are read (keep it well under slurmctld's MinJobAge, 300s by default).")
	flag.BoolVar(&tenantRBAC, "tenant-rbac", false,
		"Create RoleBindings in tenant namespaces from GryviaTenant spec.members and spec.oidcGroups, bound to the ClusterRoles gryvia-tenant-viewer|member|admin (shipped by the Helm chart when quotaOperator.tenantRbac is set). Off by default.")
	flag.BoolVar(&enableReservations, "enable-reservations", false,
		"Run the GryviaReservation controller, which taints and labels reserved nodes (gryvia.io/reserved:NoSchedule). Off by default: an existing GryviaReservation object starts reserving nodes as soon as this is on.")

	flag.StringVar(&budgetWebhookURL, "budget-webhook-url", "",
		"POST every GryviaBudget threshold alert as JSON to this URL (https, or loopback). HMAC-SHA256 signed with the secret in the environment variable GRYVIA_BUDGET_WEBHOOK_SECRET when set. Delivery is at least once. Empty = Kubernetes Events only.")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", false,
		"Serve the GryviaUsageRecord validating webhook that rejects spec changes once spec.final is true (needs TLS certs in --webhook-cert-dir).")
	flag.BoolVar(&billingLedger, "billing-ledger", false,
		"Append a hash-chained GryviaLedgerEntry for every sealed GryviaUsageRecord; with --enable-webhooks also reject edits and deletes of ledger entries, of non-Draft GryviaInvoices and deletes of sealed usage records.")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"Directory holding tls.crt and tls.key for the webhook server.")

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "quota-operator.gryvia.io",
	}
	if enableWebhooks {
		mgrOpts.WebhookServer = webhook.NewServer(webhook.Options{Port: 9443, CertDir: webhookCertDir})
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.GryviaQuotaReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaQuota")
		os.Exit(1)
	}

	if err = (&controllers.GryviaCostPredictorReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaCostPredictor")
		os.Exit(1)
	}

	if err = (&controllers.GryviaTenantReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		TenantRBAC: tenantRBAC,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaTenant")
		os.Exit(1)
	}

	if err = (&controllers.GryviaUsageRecordReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaUsageRecord")
		os.Exit(1)
	}
	if billingLedger {
		if err = (&controllers.GryviaLedgerReconciler{
			Client: mgr.GetClient(),
			Reader: mgr.GetAPIReader(),
			Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaLedger")
			os.Exit(1)
		}
	}

	if kueueIntegration {
		var resources []string
		for _, n := range strings.Split(kueueQuotaResources, ",") {
			if n = strings.TrimSpace(n); n != "" {
				resources = append(resources, n)
			}
		}
		if err = (&controllers.GryviaKueueReconciler{
			Client:         mgr.GetClient(),
			Scheme:         mgr.GetScheme(),
			QuotaResources: resources,
			GPUTypeFlavors: kueueGPUTypeFlavors,
			FairSharing:    kueueFairSharing, TopologyName: kueueTopology, AdmissionCheck: kueueAdmissionCheck,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaKueue")
			os.Exit(1)
		}
	}

	if slurmIntegration {
		if slurmNamespace == "" {
			setupLog.Error(nil, "--slurm-integration needs --slurm-namespace")
			os.Exit(1)
		}
		if err = (&controllers.GryviaSlurmReconciler{
			Client: mgr.GetClient(), Namespace: slurmNamespace, Controller: slurmController, SlurmdImage: slurmdImage,
			NodesPerTenant: int32(slurmNodes), GPUsPerNode: int32(slurmGPUsPerNode), Oversubscribe: slurmOversubscribe,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaSlurm")
			os.Exit(1)
		}
		if slurmTokenSecret != "" {
			if slurmRestURL == "" {
				slurmRestURL = fmt.Sprintf("http://slurm-restapi.%s.svc:6820", slurmNamespace)
			}
			if err = mgr.Add(&controllers.SlurmAccounting{
				Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Namespace: slurmNamespace, URL: slurmRestURL,
				TokenSecret: slurmTokenSecret, TokenKey: slurmTokenKey, Interval: slurmAccountingInterval,
			}); err != nil {
				setupLog.Error(err, "unable to add Slurm accounting")
				os.Exit(1)
			}
		}
	}

	if err = (&controllers.GryviaChargebackReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "chargeback controller")
		os.Exit(1)
	}
	budgetReconciler := &controllers.GryviaBudgetReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Reader: mgr.GetAPIReader(),
	}
	if budgetWebhookURL != "" {
		if err := notify.CheckURL(budgetWebhookURL); err != nil {
			setupLog.Error(err, "invalid --budget-webhook-url")
			os.Exit(1)
		}
		budgetReconciler.Webhook = &notify.Webhook{URL: budgetWebhookURL, Secret: os.Getenv("GRYVIA_BUDGET_WEBHOOK_SECRET")}
	}
	if err = budgetReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GryviaBudget")
		os.Exit(1)
	}

	if enableReservations {
		if err = (&controllers.GryviaReservationReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GryviaReservation")
			os.Exit(1)
		}
	}

	if err := quotametrics.Register(mgr.GetClient()); err != nil { // scrape-time quota/usage/tenant gauges
		setupLog.Error(err, "unable to register quota metrics")
		os.Exit(1)
	}

	if enableWebhooks {
		mgr.GetWebhookServer().Register(usageadmission.UsageRecordPath,
			&admission.Webhook{Handler: usageadmission.NewUsageRecordHandler(mgr.GetScheme(), billingLedger)})
		setupLog.Info("registered validating webhook", "path", usageadmission.UsageRecordPath, "certDir", webhookCertDir)
		if billingLedger {
			mgr.GetWebhookServer().Register(usageadmission.LedgerEntryPath,
				&admission.Webhook{Handler: usageadmission.NewLedgerEntryHandler(mgr.GetScheme())})
			mgr.GetWebhookServer().Register(usageadmission.InvoicePath,
				&admission.Webhook{Handler: usageadmission.NewInvoiceHandler(mgr.GetScheme())})
			setupLog.Info("registered billing webhooks", "paths", []string{usageadmission.LedgerEntryPath, usageadmission.InvoicePath})
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
