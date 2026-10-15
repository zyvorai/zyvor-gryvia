package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/controllers"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/modelhub"
)

// mlOptions are the flags of the ML controllers: GryviaWorkspace, GryviaInferenceService, GryviaModelRegistry,
// GryviaWorkflow and GryviaAutoTuner. Image defaults can be replaced per cluster here and per object with
// spec.image; the built-in defaults are large GPU images that are unverified here (see docs/ml-controllers.md).
type mlOptions struct {
	enabled bool

	workspaceJupyterImage string
	workspaceCodeImage    string
	workspacePublicURL    string
	workspaceProxyKeyFile string
	platformNamespace     string

	inferenceImageVLLM       string
	inferenceImageTriton     string
	inferenceImageTensorRT   string
	inferenceImageTorchServe string
	inferenceHealthPath      string
	canaryStartupGrace       time.Duration
	inferenceGatewayRouting  bool
	inferencePrometheusURL   string
	inferenceTelemetryImage  string
	autoServeGPUCount        int

	workflowMaxParallelSteps int
	workflowAllowWebhooks    bool

	tunerMaxTrials      int
	tunerMaxParallelism int

	modelWatchEnabled      bool
	modelWatchHubURL       string
	modelWatchMinPollEvery time.Duration

	ragEnabled      bool
	ragQdrantImage  string
	ragIngestImage  string
	agentsEnabled   bool
	agentImage      string
	llmGatewayURL   string
	llmKeyNamespace string
}

// workspaceProxy reads the proxy key; nil when the proxy is not configured.
func (o *mlOptions) workspaceProxy() (*controllers.WorkspaceProxy, error) {
	if o.workspacePublicURL == "" && o.workspaceProxyKeyFile == "" {
		return nil, nil
	}
	if o.workspacePublicURL == "" || o.workspaceProxyKeyFile == "" {
		return nil, fmt.Errorf("--workspace-public-url and --workspace-proxy-key-file must be set together")
	}
	if u, err := url.Parse(o.workspacePublicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("--workspace-public-url %q is not an http(s) URL", o.workspacePublicURL)
	}
	key, err := os.ReadFile(o.workspaceProxyKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read --workspace-proxy-key-file: %w", err)
	}
	key = bytes.TrimSpace(key)
	if len(key) < 32 {
		return nil, fmt.Errorf("--workspace-proxy-key-file must hold at least 32 bytes")
	}
	return &controllers.WorkspaceProxy{
		PublicURL: o.workspacePublicURL, TokenKey: key, PlatformNamespace: o.platformNamespace,
	}, nil
}

func (o *mlOptions) bind(fs *flag.FlagSet) {
	fs.BoolVar(&o.enabled, "enable-ml-controllers", true,
		"Run the GryviaWorkspace, GryviaInferenceService, GryviaModelRegistry, GryviaWorkflow and GryviaAutoTuner controllers.")
	fs.StringVar(&o.workspaceJupyterImage, "workspace-jupyter-image", controllers.DefaultJupyterImage,
		"Image of jupyter workspaces whose spec.image is empty.")
	fs.StringVar(&o.workspaceCodeImage, "workspace-code-image", controllers.DefaultVSCodeImage,
		"Image of vscode workspaces whose spec.image is empty.")
	fs.StringVar(&o.workspacePublicURL, "workspace-public-url", "",
		"Dashboard origin (e.g. https://gryvia.example.com). With --workspace-proxy-key-file, workspaces are opened through the api-gateway at <url>/ws/<namespace>/<name>/.")
	fs.StringVar(&o.workspaceProxyKeyFile, "workspace-proxy-key-file", "",
		"File holding the key shared with the api-gateway (GRYVIA_WORKSPACE_PROXY_KEY) that derives Jupyter workspace tokens.")
	fs.StringVar(&o.platformNamespace, "platform-namespace", "",
		"Namespace of the api-gateway and ai-operator; workspace NetworkPolicies admit only their pods. Empty: no NetworkPolicy.")
	fs.StringVar(&o.inferenceImageVLLM, "inference-image-vllm", controllers.DefaultInferenceImages[gryviav1.BackendVLLM],
		"Image of vllm inference services whose spec.image is empty.")
	fs.StringVar(&o.inferenceImageTriton, "inference-image-triton", controllers.DefaultInferenceImages[gryviav1.BackendTriton],
		"Image of triton inference services whose spec.image is empty.")
	fs.StringVar(&o.inferenceImageTensorRT, "inference-image-tensorrt-llm", controllers.DefaultInferenceImages[gryviav1.BackendTensorRTLLM],
		"Image of tensorrt-llm inference services whose spec.image is empty.")
	fs.StringVar(&o.inferenceImageTorchServe, "inference-image-torchserve", controllers.DefaultInferenceImages[gryviav1.BackendTorchServe],
		"Image of torchserve inference services whose spec.image is empty.")
	fs.StringVar(&o.inferenceHealthPath, "inference-health-path", "",
		"HTTP path of the inference readiness/liveness probes for every backend (default: per backend, e.g. /health for vllm). spec.healthCheck.path still wins.")
	fs.BoolVar(&o.inferenceGatewayRouting, "inference-gateway-routing", false, "Manage opt-in Gateway API HTTPRoutes for inference canaries. Requires Gateway API v1 and a Gateway controller.")
	fs.StringVar(&o.inferenceTelemetryImage, "inference-telemetry-image", "", "AI operator image containing the inference-proxy subcommand; enables opt-in telemetry sidecars.")
	fs.StringVar(&o.inferencePrometheusURL, "inference-prometheus-url", "", "Administrator-configured Prometheus base URL for opt-in canary SLO analysis.")
	fs.DurationVar(&o.canaryStartupGrace, "inference-canary-startup-grace", 5*time.Minute,
		"How long a new canary may take to become ready before health checks count it as failing.")
	fs.IntVar(&o.autoServeGPUCount, "autoserve-default-gpu-count", 1,
		"GPUs per replica of a service auto-created by a GryviaModelRegistry whose servingConfig sets no gpuCount (0 serves on CPU).")
	fs.IntVar(&o.workflowMaxParallelSteps, "workflow-max-parallel-steps", controllers.DefaultWorkflowMaxParallelSteps,
		"Most steps of one GryviaWorkflow that run at the same time.")
	fs.BoolVar(&o.workflowAllowWebhooks, "workflow-allow-webhooks", false,
		"Run webhook steps of GryviaWorkflow (the operator makes the HTTP call). Off by default: a webhook step lets whoever can create a workflow make the operator send requests inside the cluster network.")
	fs.IntVar(&o.tunerMaxTrials, "tuner-max-trials", controllers.DefaultMaxTrialsCap,
		"Largest spec.maxTrials a GryviaAutoTuner may ask for; larger tuners are marked Failed.")
	fs.IntVar(&o.tunerMaxParallelism, "tuner-max-parallelism", controllers.DefaultMaxParallelismCap,
		"Most trials of one GryviaAutoTuner that run at the same time (lowers spec.parallelism).")
	fs.BoolVar(&o.modelWatchEnabled, "enable-model-watch", false,
		"Run the GryviaModelWatch controller, which polls a model hub and creates a GryviaWorkflow per new model. Off by default: it makes outbound requests to the hub. Needs --enable-ml-controllers.")
	fs.StringVar(&o.modelWatchHubURL, "model-watch-hub-url", modelhub.DefaultHuggingFaceURL,
		"Base URL of the Hugging Face compatible hub GryviaModelWatch polls (a mirror or proxy may be used).")
	fs.DurationVar(&o.modelWatchMinPollEvery, "model-watch-min-poll-interval", controllers.MinModelWatchPollInterval,
		"Shortest spec.pollInterval a GryviaModelWatch may use.")
	fs.BoolVar(&o.ragEnabled, "enable-rag", false,
		"Run the GryviaVectorIndex controller (managed Qdrant, ingestion Jobs embedding through the LLM gateway). Needs --enable-ml-controllers and --llm-gateway-url.")
	fs.StringVar(&o.ragQdrantImage, "rag-qdrant-image", controllers.DefaultQdrantImage,
		"Image of managed vector stores whose spec.store.managed.image is empty.")
	fs.StringVar(&o.ragIngestImage, "rag-ingest-image", controllers.DefaultRAGIngestImage,
		"Image of ingestion Jobs (python3 /app/ingest.py, see examples/rag) whose spec.ingestImage is empty.")
	fs.BoolVar(&o.agentsEnabled, "enable-agents", false,
		"Run the GryviaAgent controller (agent runtime Deployments calling models through the LLM gateway). Needs --enable-ml-controllers and --llm-gateway-url.")
	fs.StringVar(&o.agentImage, "agent-image", controllers.DefaultAgentImage,
		"Image of agent runtimes (see examples/agents) whose spec.image is empty.")
	fs.StringVar(&o.llmGatewayURL, "llm-gateway-url", "",
		"Base URL of the LLM gateway (http://<release>-llm-gateway.<namespace>.svc.cluster.local:8080) that ingestion Jobs and agents call.")
	fs.StringVar(&o.llmKeyNamespace, "llm-key-namespace", "gryvia-llm-keys",
		"Namespace of the LLM gateway's key Secrets, where the operator stores the hashed keys of indexes and agents.")
}
