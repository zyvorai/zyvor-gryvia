mod client;
mod commands;
mod display;
mod gateway;
mod output;
mod platform;
mod types;
mod ui;

use anyhow::Result;
use clap::builder::styling::{AnsiColor, Effects};
use clap::builder::Styles;
use clap::{ColorChoice, CommandFactory, FromArgMatches, Parser, Subcommand};
use clap_complete::Shell;
use commands::usage::{GroupBy, UsageFormat};
use output::{OutputFormat, StructuredFormat};

/// An "Examples:" block for a command's help, colored like the rest of the help.
fn examples(lines: &[&str]) -> clap::builder::StyledStr {
    use clap::builder::styling::Style;
    let head: Style = AnsiColor::Cyan.on_default().effects(Effects::BOLD);
    let cmd: Style = AnsiColor::Green.on_default();
    let mut out = format!("{head}Examples:{head:#}\n");
    for line in lines {
        out.push_str(&format!("  {cmd}{line}{cmd:#}\n"));
    }
    out.into()
}

/// The one-line job flags of `gryvia submit --image ...`; None for other commands or without --image.
fn inline_job(cmd: &Commands) -> Option<commands::submit::InlineJob> {
    let Commands::Submit {
        image: Some(image),
        name,
        job_type,
        gpu,
        gpu_memory,
        isolation,
        gpu_type,
        mig,
        nodes,
        queue,
        priority,
        cpu,
        memory,
        env,
        command,
        ..
    } = cmd
    else {
        return None;
    };
    Some(commands::submit::InlineJob {
        image: image.clone(),
        name: name.clone(),
        job_type: job_type.clone(),
        gpu: *gpu,
        gpu_memory_mib: *gpu_memory,
        isolation: isolation.clone(),
        gpu_type: gpu_type.clone(),
        mig: mig.clone(),
        nodes: *nodes,
        queue: queue.clone(),
        priority: *priority,
        cpu: cpu.clone(),
        memory: memory.clone(),
        env: env.clone(),
        command: command.clone(),
    })
}

/// Help colors: cyan headings, green commands and flags, yellow placeholders.
const STYLES: Styles = Styles::styled()
    .header(AnsiColor::Cyan.on_default().effects(Effects::BOLD))
    .usage(AnsiColor::Cyan.on_default().effects(Effects::BOLD))
    .literal(AnsiColor::Green.on_default().effects(Effects::BOLD))
    .placeholder(AnsiColor::Yellow.on_default())
    .error(AnsiColor::Red.on_default().effects(Effects::BOLD))
    .valid(AnsiColor::Green.on_default())
    .invalid(AnsiColor::Yellow.on_default());

#[derive(Parser)]
#[command(name = "gryvia")]
#[command(about = "Gryvia CLI - Manage GPU clusters for AI workloads", long_about = None)]
#[command(version, styles = STYLES)]
struct Cli {
    #[command(subcommand)]
    command: Commands,

    /// Kubernetes context to use
    #[arg(long, global = true)]
    context: Option<String>,

    /// Namespace to use
    #[arg(short, long, global = true)]
    namespace: Option<String>,

    /// Enable verbose logging
    #[arg(short, long, global = true)]
    verbose: bool,

    /// Disable colored output (also honours NO_COLOR)
    #[arg(long, global = true)]
    no_color: bool,

    /// Gateway base URL for the commands whose data lives behind it (network flows/graph, gpu memory, security alerts). The API key is read from GRYVIA_API_KEY
    #[arg(long, global = true, env = "GRYVIA_GATEWAY_URL", value_name = "URL")]
    gateway: Option<String>,

    /// Do not verify the gateway's TLS certificate (a CA bundle can be set with GRYVIA_CA_FILE instead)
    #[arg(long, global = true)]
    insecure: bool,
}

#[derive(Subcommand)]
enum Commands {
    /// Submit an AI training or inference job in one line, from a YAML file, or from a Slurm batch script
    #[command(after_help = examples(&[
        "gryvia submit --image pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime --gpu 1 -- python train.py",
        "gryvia submit --image vllm/vllm-openai:v0.6.3 --gpu 0.5 --gpu-memory 20480 --type inference",
        "gryvia submit --image my/trainer:1.0 --gpu 8 --nodes 4 --gpu-type H100 --queue research --wait --logs -- torchrun train.py",
        "gryvia submit --image busybox:1.36 --dry-run -- echo hello",
        "gryvia submit --file job.yaml --wait --logs",
        "gryvia submit --sbatch train.sh --image pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime --dry-run",
    ]))]
    Submit {
        /// Path to job YAML file
        #[arg(
            short,
            long,
            required_unless_present_any = ["sbatch", "image"],
            conflicts_with_all = ["sbatch", "image"]
        )]
        file: Option<String>,

        /// A Slurm batch script: #SBATCH directives become GryviaAIJob fields (see docs/slurm.md)
        #[arg(long, value_name = "SCRIPT")]
        sbatch: Option<String>,

        /// Container image. Alone: a one-line job (no YAML). With --sbatch: overrides #SBATCH --container-image
        #[arg(long)]
        image: Option<String>,

        /// Print the GryviaAIJob YAML without submitting (one-line or --sbatch)
        #[arg(long, conflicts_with = "file")]
        dry_run: bool,

        /// Job name (default: generated from the image name)
        #[arg(long, requires = "image", conflicts_with = "sbatch")]
        name: Option<String>,

        /// Workload type: training, fine-tuning, evaluation or inference
        #[arg(long = "type", default_value = "training", requires = "image", conflicts_with = "sbatch",
              value_parser = ["training", "fine-tuning", "evaluation", "inference"])]
        job_type: String,

        /// GPUs per pod: a fraction of one GPU (0.25, 0.5) or a whole number (1, 8)
        #[arg(long, value_name = "N", requires = "image", conflicts_with = "sbatch")]
        gpu: Option<f64>,

        /// GPU memory limit in MiB for a fractional --gpu (MPS memory limit, or picks the MIG profile)
        #[arg(long, value_name = "MiB", requires = "gpu")]
        gpu_memory: Option<u64>,

        /// How a fractional --gpu is isolated
        #[arg(long, requires = "gpu", value_parser = ["auto", "mps", "mig", "dra"])]
        isolation: Option<String>,

        /// GPU model, e.g. H100, A100
        #[arg(long, requires = "image", conflicts_with = "sbatch")]
        gpu_type: Option<String>,

        /// MIG profile, e.g. 1g.10gb (requests MIG slices instead of whole GPUs)
        #[arg(
            long,
            value_name = "PROFILE",
            requires = "image",
            conflicts_with = "sbatch"
        )]
        mig: Option<String>,

        /// Number of nodes for a distributed job (each gets --gpu GPUs)
        #[arg(long, requires = "image", conflicts_with = "sbatch")]
        nodes: Option<u32>,

        /// Queue (Kueue LocalQueue or native queue)
        #[arg(long, requires = "image", conflicts_with = "sbatch")]
        queue: Option<String>,

        /// Priority; higher wins
        #[arg(long, requires = "image", conflicts_with = "sbatch")]
        priority: Option<i32>,

        /// CPU request, e.g. 8
        #[arg(long, requires = "image", conflicts_with = "sbatch")]
        cpu: Option<String>,

        /// Memory request, e.g. 64Gi
        #[arg(long, requires = "image", conflicts_with = "sbatch")]
        memory: Option<String>,

        /// Environment variable KEY=VALUE (repeatable)
        #[arg(
            short = 'e',
            long = "env",
            value_name = "KEY=VALUE",
            requires = "image",
            conflicts_with = "sbatch"
        )]
        env: Vec<String>,

        /// Command and arguments after `--`
        #[arg(last = true, value_name = "COMMAND")]
        command: Vec<String>,

        /// Wait for job to complete
        #[arg(short, long)]
        wait: bool,

        /// Follow logs after submission
        #[arg(long)]
        logs: bool,
    },

    /// List jobs, quotas, or nodes
    #[command(visible_alias = "ls")]
    #[command(after_help = examples(&["gryvia list jobs", "gryvia list jobs --all-namespaces -o json", "gryvia list nodes"]))]
    List {
        /// Resource type to list (jobs, quotas, nodes, experiments)
        resource: String,

        /// Show all namespaces
        #[arg(short, long)]
        all_namespaces: bool,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Get detailed information about a resource
    #[command(visible_alias = "describe")]
    #[command(after_help = examples(&["gryvia get job my-job", "gryvia get node gpu-node-01 -o json"]))]
    Get {
        /// Resource type (job, quota, node, storage, network, experiment)
        resource: String,

        /// Resource name
        name: String,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = StructuredFormat::Yaml)]
        output: StructuredFormat,
    },

    /// Delete a resource
    #[command(visible_alias = "rm")]
    #[command(after_help = examples(&["gryvia delete job my-job --yes"]))]
    Delete {
        /// Resource type (job, quota, storage, network)
        resource: String,

        /// Resource name
        name: String,

        /// Skip confirmation
        #[arg(short, long)]
        yes: bool,
    },

    /// Show platform status, or the status of one job
    ///
    /// With no argument: a report of the whole platform (operators, gateway, dashboard, GPU nodes, add-ons,
    /// collectors), workload readiness and a per-node table, like `cilium status`. Exits 1 when a required
    /// component is failing. With a job name: that job's details.
    #[command(after_help = examples(&["gryvia status", "gryvia status --brief", "gryvia status --wait --wait-timeout 5m", "gryvia status --node gpu-node-01", "gryvia status -o json", "gryvia status my-job --follow"]))]
    Status {
        /// Job name (omit to show the platform)
        job: Option<String>,

        /// Follow logs (job status)
        #[arg(short, long, requires = "job")]
        follow: bool,

        /// Only this node in the per-node report
        #[arg(long, conflicts_with = "job")]
        node: Option<String>,

        /// Print only OK, or the failing components (exit code 1 on failure)
        #[arg(long, conflicts_with = "job")]
        brief: bool,

        /// Wait until the platform is OK (for scripts and CI)
        #[arg(long, conflicts_with = "job")]
        wait: bool,

        /// How long to wait, like 90s or 5m
        #[arg(long, default_value = "5m", requires = "wait")]
        wait_timeout: String,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, conflicts_with = "job")]
        output: OutputFormat,

        /// Namespace the platform is installed in
        #[arg(long, default_value = "gryvia-system", conflicts_with = "job")]
        platform_namespace: String,
    },

    /// View and follow job logs
    #[command(after_help = examples(&["gryvia logs my-job --follow", "gryvia logs my-job --tail 100"]))]
    Logs {
        /// Job name
        job: String,

        /// Follow logs
        #[arg(short, long)]
        follow: bool,

        /// Number of lines to show from the end
        #[arg(long, default_value = "100")]
        tail: usize,

        /// Show logs for specific pod replica
        #[arg(long)]
        replica: Option<usize>,
    },

    /// Cancel a running or queued job
    #[command(after_help = examples(&["gryvia cancel my-job", "gryvia cancel job-a job-b --yes"]))]
    Cancel {
        /// Job name(s) to cancel
        jobs: Vec<String>,

        /// Skip confirmation
        #[arg(short, long)]
        yes: bool,
    },

    /// Show GPU cluster overview
    #[command(after_help = examples(&["gryvia cluster", "gryvia cluster --detailed", "gryvia cluster -o json"]))]
    Cluster {
        /// Show detailed node information
        #[arg(short, long)]
        detailed: bool,

        /// Refresh interval in seconds (watch mode)
        #[arg(short, long)]
        watch: Option<u64>,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Show GPU quota status for teams
    #[command(after_help = examples(&["gryvia quota", "gryvia quota ml-research --budget", "gryvia quota -o yaml"]))]
    Quota {
        /// Team name (shows all teams if not specified)
        team: Option<String>,

        /// Show budget details
        #[arg(short, long)]
        budget: bool,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Show cost analysis and spending
    #[command(after_help = examples(&["gryvia cost --period month", "gryvia cost ml-research --detailed"]))]
    Cost {
        /// Team name (shows all teams if not specified)
        team: Option<String>,

        /// Time period (day, week, month)
        #[arg(short, long, default_value = "month")]
        period: String,

        /// Show detailed breakdown
        #[arg(short, long)]
        detailed: bool,
    },

    /// Show queue status
    #[command(after_help = examples(&["gryvia queue", "gryvia queue --watch 5", "gryvia queue -o json"]))]
    Queue {
        /// Queue name (shows all queues if not specified)
        name: Option<String>,

        /// Watch mode with refresh interval
        #[arg(short, long)]
        watch: Option<u64>,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// List the GPU SKUs on offer, with hourly rates
    ///
    /// Read-only. Shows every GryviaGpuSku: GPU type, GPUs per unit, rate per hour, spot discount and
    /// whether it is enabled. Rates are what usage metering multiplies GPU hours by.
    #[command(visible_alias = "skus")]
    #[command(after_help = examples(&["gryvia catalog", "gryvia skus -o json"]))]
    Catalog {
        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Manage GPU node reservations: list, create, cancel
    ///
    /// A reservation is a GryviaReservation object. The quota operator taints the reserved nodes so only
    /// jobs annotated gryvia.io/reservation=<name> by the owner can run there.
    #[command(after_help = examples(&["gryvia reservation list", "gryvia reservation create --owner ml --gpu-type H100 --gpus 16 --duration 8h", "gryvia reservation cancel resv-1 --yes"]))]
    Reservation {
        #[command(subcommand)]
        action: ReservationCommands,
    },

    /// Show budgets and spend against limits
    ///
    /// Reads GryviaBudget objects. Spend is estimated from usage records; it is not an invoice.
    #[command(after_help = examples(&["gryvia budget", "gryvia budget --scope ml", "gryvia budget -o json"]))]
    Budget {
        /// Only budgets whose scope name is this
        #[arg(long)]
        scope: Option<String>,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Manage tenants: list, get, create, delete
    ///
    /// A tenant is a GryviaTenant object; its workloads run in the namespace `tenant-<name>`.
    #[command(after_help = examples(&["gryvia tenant list", "gryvia tenant get acme", "gryvia tenant create acme --display-name Acme --allowed-sku h100-8x --max-gpus 16 --isolated true", "gryvia tenant delete acme --yes"]))]
    Tenant {
        #[command(subcommand)]
        action: TenantCommands,
    },

    /// Show metered GPU usage and estimated cost
    ///
    /// Aggregates GryviaUsageRecord objects: GPU hours, cost and distinct jobs per tenant, SKU or day. With --network it reports measured egress bytes per tenant, peer class, zone class or day from GryviaNetworkUsageRecord objects (egress only; estimates).
    /// Records are filtered on their start time; a date-only --to covers that whole day. Costs are
    /// estimates from job run time multiplied by the SKU rate, not invoices.
    #[command(after_help = examples(&["gryvia usage", "gryvia usage --tenant acme --from 2026-09-01 --to 2026-09-30", "gryvia usage --group-by sku -o json", "gryvia usage --group-by day -o csv", "gryvia usage --network --group-by zone-class", "gryvia usage --network --tenant acme --from 2026-09-01 -o csv"]))]
    Usage {
        /// Only this tenant
        #[arg(long)]
        tenant: Option<String>,

        /// Earliest record start: YYYY-MM-DD or an RFC 3339 timestamp
        #[arg(long)]
        from: Option<String>,

        /// Latest record start: YYYY-MM-DD (the whole day is included) or an RFC 3339 timestamp
        #[arg(long)]
        to: Option<String>,

        /// How to group the rows
        #[arg(long, value_enum, default_value_t = GroupBy::Tenant)]
        group_by: GroupBy,

        /// Show measured network egress (GryviaNetworkUsageRecord) instead of GPU usage
        #[arg(long)]
        network: bool,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = UsageFormat::Table, env = "GRYVIA_OUTPUT")]
        output: UsageFormat,
    },

    /// Build monthly invoice estimates from metered usage
    ///
    /// Groups GryviaUsageRecord objects (all namespaces) into one invoice per tenant for a UTC month, with
    /// one line per SKU. Records are assigned to the month of their start time. Invoices are estimates
    /// from job run time; `open` is true while an included job is still running.
    #[command(after_help = examples(&["gryvia invoice", "gryvia invoice --tenant acme --month 2026-09", "gryvia invoice --month 2026-09 -o csv", "gryvia invoice -o json"]))]
    Invoice {
        /// Only this tenant
        #[arg(long)]
        tenant: Option<String>,

        /// Month to invoice, YYYY-MM (UTC; default: the current month)
        #[arg(long, value_parser = commands::invoice::parse_month)]
        month: Option<(i32, u32)>,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = UsageFormat::Table, env = "GRYVIA_OUTPUT")]
        output: UsageFormat,
    },

    /// Model factory: watch a model hub and fine-tune, evaluate and serve new models
    ///
    /// A model watch is a GryviaModelWatch object. The ai-operator (with --enable-model-watch) polls the hub
    /// and starts one GryviaWorkflow per new model that passes the license and size filters.
    #[command(after_help = examples(&["gryvia models watch list", "gryvia models watch create -f examples/model-factory/model-watch.yaml -n ml-team", "gryvia models watch runs open-llms -n ml-team", "gryvia models watch suspend open-llms", "gryvia models rollback chat-v2 -n ml-team"]))]
    Models {
        #[command(subcommand)]
        action: ModelsCommands,
    },

    /// Datasets: download an http, s3 or nfs source into a PVC, one directory per version
    ///
    /// A dataset is a cluster-scoped GryviaDataset. The storage-operator (with --enable-datasets) materializes
    /// it in spec.namespace; jobs there mount the PVC in status.pvcName at status.subPath.
    #[command(after_help = examples(&["gryvia datasets list", "gryvia datasets create -f examples/datasets/http-dataset.yaml", "gryvia datasets get corpus", "gryvia datasets delete corpus --yes"]))]
    Datasets {
        #[command(subcommand)]
        action: CrdCommands,
    },

    /// Job hooks: a webhook when an AI job or workflow fails, succeeds or changes phase
    ///
    /// A job hook is a namespaced GryviaJobHook. The ai-operator (chart value aiOperator.jobHooks.enabled) POSTs a
    /// signed JSON body, or a Slack message, when a GryviaAIJob or GryviaWorkflow in its namespace reaches one of its
    /// events; failed POSTs are retried with backoff.
    #[command(after_help = examples(&["gryvia job-hooks create -f examples/crds/gryviajobhook-examples.yaml -n ml-research", "gryvia job-hooks list -n ml-research", "gryvia job-hooks get slack-failures -n ml-research", "gryvia job-hooks delete slack-failures --yes"]))]
    JobHooks {
        #[command(subcommand)]
        action: CrdCommands,
    },

    /// LLM gateway: per-tenant API keys, published models and token usage
    ///
    /// The gateway (chart value llmGateway.enabled) is one OpenAI-compatible endpoint for every
    /// GryviaInferenceService annotated gryvia.io/llm-model. Keys belong to the namespace (-n) they are created in.
    #[command(after_help = examples(&["gryvia llm keys create ci -n tenant-alpha", "gryvia llm keys list -A", "gryvia llm models", "gryvia llm usage --group-by day --days 7", "gryvia llm keys delete ci -n tenant-alpha --yes"]))]
    Llm {
        #[command(subcommand)]
        action: LlmCommands,
    },

    /// RAG: vector indexes built from datasets, and retrieval through the LLM gateway
    ///
    /// A GryviaVectorIndex (namespaced, -n) names a GryviaDataset, an embedding model published on the LLM gateway
    /// and a store (a managed Qdrant or an external one). The ai-operator (chart value aiOperator.rag.enabled)
    /// ingests each dataset version; `query` calls the gateway's /v1/retrieve with a key of the same namespace.
    #[command(after_help = examples(&["gryvia rag index create -f examples/rag/vector-index.yaml -n tenant-alpha", "gryvia rag index list -n tenant-alpha", "gryvia rag reingest handbook -n tenant-alpha", "gryvia rag query handbook quotas --top-k 3", "gryvia rag index delete handbook -n tenant-alpha --yes"]))]
    Rag {
        #[command(subcommand)]
        action: RagCommands,
    },

    /// Agents: tool-calling agents served as OpenAI-compatible endpoints
    ///
    /// A GryviaAgent (namespaced, -n) names an LLM gateway model, a system prompt and tools (retrieval over a
    /// vector index, HTTP calls to allowlisted URLs, or a Zyntra ontology to search and propose actions on). The ai-operator (chart value aiOperator.agents.enabled)
    /// runs its runtime; `chat` goes through the api-gateway (GRYVIA_GATEWAY_URL), which proxies to the agent.
    #[command(after_help = examples(&["gryvia agents create -f examples/agents/agent.yaml -n tenant-alpha", "gryvia agents list -n tenant-alpha", "gryvia agents chat helper what is the GPU quota", "gryvia agents delete helper -n tenant-alpha --yes"]))]
    Agents {
        #[command(subcommand)]
        action: AgentsCommands,
    },

    /// Workspaces: Jupyter and VS Code environments with GPUs, opened in the browser
    ///
    /// A GryviaWorkspace (namespaced, -n) runs one notebook or code-server pod with optional GPUs and storage that
    /// survives pause and resume. `open` goes through the api-gateway (GRYVIA_GATEWAY_URL) when the chart's
    /// workspaceProxy is on.
    #[command(visible_alias = "ws")]
    #[command(after_help = examples(&["gryvia workspace new nb --type jupyter --gpus 1 --storage 50Gi --idle-timeout 60 --idle-pause", "gryvia workspace list", "gryvia workspace open nb", "gryvia workspace pause nb", "gryvia workspace delete nb --yes"]))]
    Workspace {
        #[command(subcommand)]
        action: WorkspaceCommands,
    },

    /// Interactive job creation wizard
    #[command(after_help = examples(&["gryvia create job"]))]
    Create {
        /// Resource type (job, quota)
        resource: String,
    },

    /// Validate a job YAML file
    #[command(after_help = examples(&["gryvia validate job.yaml"]))]
    Validate {
        /// Path to job YAML file
        file: String,
    },

    /// Show cluster health and diagnostics
    #[command(after_help = examples(&["gryvia health", "gryvia health gpu"]))]
    Health {
        /// Check specific component (gpu, storage, network, all)
        #[arg(default_value = "all")]
        component: String,
    },

    /// Report GPU capacity per type: free, pending demand and shortfall
    ///
    /// Read-only. Shows total, allocated and free GPUs per GPU type. Supply comes from GryviaGpuNode objects, allocation from Running jobs and demand
    /// from Pending/Queued/Scheduling jobs. Jobs that do not name a GPU type (or say "any") are
    /// counted only in the cluster totals. This is a snapshot, not a forecast.
    #[command(visible_alias = "cap")]
    #[command(after_help = examples(&["gryvia capacity", "gryvia capacity --gpu-type H100 -o json"]))]
    Capacity {
        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,

        /// Only report this GPU type (case-insensitive); jobs without a type are excluded
        #[arg(long)]
        gpu_type: Option<String>,
    },

    /// Generate shell completions
    #[command(after_help = examples(&["gryvia completion bash > /etc/bash_completion.d/gryvia", "gryvia completion zsh > ~/.zsh/completions/_gryvia"]))]
    Completion {
        /// Shell to generate completions for
        #[arg(value_enum)]
        shell: Shell,
    },

    /// Show client and platform versions
    #[command(after_help = examples(&["gryvia version", "gryvia version --client"]))]
    Version {
        /// Only show the client version (needs no cluster)
        #[arg(long)]
        client: bool,

        /// Namespace the platform is installed in
        #[arg(long, default_value = "gryvia-system")]
        platform_namespace: String,
    },

    /// Mark nodes for maintenance: cordon, optionally drain, and list
    ///
    /// Uses the standard Kubernetes cordon flag plus the annotations gryvia.io/maintenance (start
    /// time) and gryvia.io/maintenance-reason. Draining uses the Eviction API only, so
    /// PodDisruptionBudgets are respected; pods are never deleted directly.
    #[command(after_help = examples(&["gryvia maintenance start gpu-node-01 --reason driver-update --drain", "gryvia maintenance list"]))]
    Maintenance {
        #[command(subcommand)]
        action: MaintenanceCommands,
    },

    /// Network intelligence commands
    #[command(after_help = examples(&["gryvia network status", "gryvia network flows --gateway https://gryvia.example.com", "gryvia network graph --format json", "gryvia network anomalies --severity high"]))]
    Network {
        #[command(subcommand)]
        action: NetworkCommands,
    },

    /// Security detection and enforcement commands
    #[command(after_help = examples(&["gryvia security status", "gryvia security alerts --alert-type blocked"]))]
    Security {
        #[command(subcommand)]
        action: SecurityCommands,
    },

    /// GPU communication and training analysis commands
    #[command(after_help = examples(&["gryvia gpu memory", "gryvia gpu rdma --job llm-training", "gryvia gpu nccl --job llm-training"]))]
    Gpu {
        #[command(subcommand)]
        action: GpuCommands,
    },
}

/// The verbs of the kinds that only need list, get, create and delete.
#[derive(Subcommand)]
enum CrdCommands {
    /// List them
    List {
        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Show one: its spec and status
    Get {
        /// Name
        name: String,

        /// Output format (table prints YAML)
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Create the objects of this kind in a YAML file (other kinds in the file are skipped)
    Create {
        /// Path to the YAML file
        #[arg(short, long)]
        file: String,
    },

    /// Delete one
    Delete {
        /// Name
        name: String,

        /// Skip confirmation
        #[arg(short, long)]
        yes: bool,
    },
}

async fn run_crd(
    client: &client::GryviaClient,
    k: &commands::crd::KindSpec,
    action: CrdCommands,
    next: &str,
    consequence: &str,
) -> Result<()> {
    match action {
        CrdCommands::List { output } => commands::crd::list(client, k, output.as_str()).await,
        CrdCommands::Get { name, output } => {
            commands::crd::get(client, k, &name, output.as_str()).await
        }
        CrdCommands::Create { file } => commands::crd::create(client, k, &file, next).await,
        CrdCommands::Delete { name, yes } => {
            commands::crd::delete(client, k, &name, yes, consequence).await
        }
    }
}

#[derive(Subcommand)]
enum ModelsCommands {
    /// Manage model watches: list, runs, create, suspend, resume, delete
    Watch {
        #[command(subcommand)]
        action: WatchCommands,
    },

    /// Roll a model's shared service back to the version it replaced; the entry is archived
    Rollback {
        /// Model registry entry (a production entry with servingConfig.serviceName)
        name: String,

        /// Skip the confirmation prompt
        #[arg(short, long)]
        yes: bool,
    },
}

#[derive(Subcommand)]
enum WorkspaceCommands {
    #[command(flatten)]
    Crd(CrdCommands),

    /// Create a workspace from flags (no YAML)
    New {
        /// Workspace name
        name: String,

        /// jupyter or vscode
        #[arg(long = "type", default_value = "jupyter", value_parser = ["jupyter", "vscode"])]
        ws_type: String,

        /// Whole GPUs (0 for a CPU workspace)
        #[arg(long, default_value_t = 0)]
        gpus: u32,

        /// GPU model, e.g. A100
        #[arg(long)]
        gpu_type: Option<String>,

        /// Image (default: the operator's image for the type)
        #[arg(long)]
        image: Option<String>,

        /// Persistent storage kept across pause and resume, e.g. 50Gi
        #[arg(long)]
        storage: Option<String>,

        /// Minutes without activity before the workspace is marked Idle
        #[arg(long, value_name = "MINUTES")]
        idle_timeout: Option<u32>,

        /// Pause (free the GPUs, keep storage) once Idle
        #[arg(long, requires = "idle_timeout")]
        idle_pause: bool,
    },

    /// Stop the workspace pod and free its GPUs; storage is kept
    Pause {
        /// Workspace name
        name: String,
    },

    /// Start a paused workspace again on the same storage
    Resume {
        /// Workspace name
        name: String,
    },

    /// Open the workspace in the browser through the api-gateway
    Open {
        /// Workspace name
        name: String,

        /// Print the one-time link instead of opening a browser
        #[arg(long)]
        print: bool,
    },
}

#[derive(Subcommand)]
enum AgentsCommands {
    #[command(flatten)]
    Crd(CrdCommands),

    /// Send a message to an agent through the api-gateway and print its answer and tool calls
    Chat {
        /// Agent name
        name: String,

        /// Message (the remaining words, joined with spaces)
        #[arg(required = true, trailing_var_arg = true)]
        message: Vec<String>,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },
}

#[derive(Subcommand)]
enum RagCommands {
    /// Manage vector indexes: list, get, create, delete
    Index {
        #[command(subcommand)]
        action: CrdCommands,
    },

    /// Run a fresh ingestion of an index (sets the gryvia.io/reingest annotation)
    Reingest {
        /// Index name
        name: String,
    },

    /// Retrieve the chunks nearest to a query through the LLM gateway's /v1/retrieve
    Query {
        /// Index name (in the namespace of the key)
        index: String,

        /// Query text
        query: String,

        /// Number of chunks (at most 50)
        #[arg(long, default_value_t = 4)]
        top_k: u32,

        /// LLM gateway base URL
        #[arg(long, env = "GRYVIA_LLM_GATEWAY_URL")]
        llm_gateway_url: Option<String>,

        /// LLM gateway key of the index's namespace
        #[arg(long, env = "GRYVIA_LLM_KEY", hide_env_values = true)]
        key: Option<String>,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },
}

#[derive(Subcommand)]
enum LlmCommands {
    /// Manage API keys: create (prints the key once), list, delete
    Keys {
        /// Namespace of the key Secrets (chart value llmGateway.keyNamespace)
        #[arg(
            long,
            global = true,
            default_value = "gryvia-llm-keys",
            env = "GRYVIA_LLM_KEY_NAMESPACE"
        )]
        key_namespace: String,

        #[command(subcommand)]
        action: LlmKeyCommands,
    },

    /// List the models a key of this namespace can call (its own and the shared ones)
    Models {
        /// Every published model, in all namespaces
        #[arg(short = 'A', long)]
        all_namespaces: bool,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Token usage from the gateway's hourly usage records (written every 5 minutes)
    Usage {
        /// Usage of every namespace
        #[arg(short = 'A', long)]
        all_namespaces: bool,

        /// Group by
        #[arg(long, value_parser = ["model", "tenant", "namespace", "day"], default_value = "model")]
        group_by: String,

        /// Days back, today included
        #[arg(long, default_value_t = 30)]
        days: u32,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },
}

#[derive(Subcommand)]
enum LlmKeyCommands {
    /// Create a key for the namespace; the key is printed once and only its hash is stored
    Create {
        /// Key name (lowercase letters, digits and '-')
        name: String,

        /// What the key is for
        #[arg(long, default_value = "")]
        description: String,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// List the keys of the namespace (never the keys themselves)
    List {
        /// Keys of every namespace
        #[arg(short = 'A', long)]
        all_namespaces: bool,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Revoke a key of the namespace
    Delete {
        /// Key name
        name: String,

        /// Skip the confirmation prompt
        #[arg(short, long)]
        yes: bool,
    },
}

#[derive(Subcommand)]
enum WatchCommands {
    /// List model watches in the namespace
    List {
        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Show the models a watch found and the workflow started for each, newest first
    Runs {
        /// Model watch name
        name: String,

        /// Also show baseline models (those that existed before the watch)
        #[arg(long)]
        all: bool,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Create the GryviaModelWatch objects in a YAML file (other kinds in the file are skipped)
    Create {
        /// Path to the YAML file
        #[arg(short, long)]
        file: String,
    },

    /// Stop polling the hub; running workflows continue
    Suspend {
        /// Model watch name
        name: String,
    },

    /// Resume polling the hub
    Resume {
        /// Model watch name
        name: String,
    },

    /// Delete a model watch and its workflows (registered models are kept)
    Delete {
        /// Model watch name
        name: String,

        /// Skip confirmation
        #[arg(short, long)]
        yes: bool,
    },
}

#[derive(Subcommand)]
enum ReservationCommands {
    /// List reservations
    List {
        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Create a reservation (immediate unless --start is given)
    Create {
        /// Reservation name (default: resv-<unix time>)
        #[arg(long)]
        name: Option<String>,

        /// Owner type: user, team, project or namespace
        #[arg(long, default_value = "team")]
        owner_type: String,

        /// Owner name; only this owner's jobs may use the reservation
        #[arg(long)]
        owner: String,

        /// GPU type to reserve (matches the node label gryvia.io/gpu)
        #[arg(long)]
        gpu_type: String,

        /// Number of GPUs to reserve (whole nodes are reserved)
        #[arg(long)]
        gpus: u32,

        /// Start time, RFC 3339 (default: now)
        #[arg(long)]
        start: Option<String>,

        /// End time, RFC 3339
        #[arg(long, conflicts_with = "duration")]
        end: Option<String>,

        /// Length, e.g. 8h, 90m, 2d (counted from the start, or from now)
        #[arg(long)]
        duration: Option<String>,

        /// Keep the nodes exclusive to the owner
        #[arg(long, value_name = "BOOL", action = clap::ArgAction::Set, default_value_t = true)]
        exclusive: bool,
    },

    /// Cancel (delete) a reservation; the nodes are released
    Cancel {
        /// Reservation name
        name: String,

        /// Skip confirmation
        #[arg(short, long)]
        yes: bool,
    },
}

#[derive(Subcommand)]
enum TenantCommands {
    /// List tenants
    List {
        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Show one tenant
    Get {
        /// Tenant name
        name: String,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table)]
        output: OutputFormat,
    },

    /// Create (or update) a tenant
    Create {
        /// Tenant name (becomes the namespace tenant-<name>)
        name: String,

        /// Human-readable name (defaults to the tenant name)
        #[arg(long)]
        display_name: Option<String>,

        /// SKU the tenant may use; repeat for several (default: all enabled SKUs)
        #[arg(long = "allowed-sku")]
        allowed_sku: Vec<String>,

        /// Maximum concurrent GPUs
        #[arg(long)]
        max_gpus: Option<u32>,

        /// Isolate the tenant's network from other tenants
        #[arg(long, value_name = "BOOL", action = clap::ArgAction::Set)]
        isolated: Option<bool>,
    },

    /// Delete a tenant
    Delete {
        /// Tenant name
        name: String,

        /// Skip confirmation
        #[arg(short, long)]
        yes: bool,
    },
}

#[derive(Subcommand)]
enum MaintenanceCommands {
    /// Cordon a node and mark it as under maintenance
    Start {
        /// Kubernetes node name
        node: String,

        /// Why the node is being taken out of service (stored in the annotation)
        #[arg(long)]
        reason: Option<String>,

        /// Also evict the node's pods via the Eviction API (skips DaemonSet and mirror pods;
        /// pods blocked by a PodDisruptionBudget are reported and left running)
        #[arg(long)]
        drain: bool,

        /// Skip the confirmation prompt for --drain
        #[arg(short, long)]
        yes: bool,
    },

    /// Uncordon a node and remove the maintenance annotations
    End {
        /// Kubernetes node name
        node: String,
    },

    /// List nodes marked for maintenance, with age and reason
    List {
        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },
}

#[derive(Subcommand)]
enum NetworkCommands {
    /// Trace network flows for a service
    Trace {
        /// Target service name
        service: String,

        /// Trace duration (e.g., 2m, 5m, 1h)
        #[arg(short, long, default_value = "2m")]
        duration: String,

        /// Capture level (l3, l4, l7)
        #[arg(short, long, default_value = "l7")]
        level: String,

        /// Target namespace
        #[arg(long, default_value = "default")]
        trace_namespace: String,

        /// Follow / live stream mode
        #[arg(short, long)]
        follow: bool,
    },

    /// Show recent network flows
    Flows {
        /// Filter by service name
        #[arg(short, long)]
        service: Option<String>,

        /// Target namespace
        #[arg(long, default_value = "default")]
        flow_namespace: String,

        /// Time window (e.g., 5m, 1h, 24h)
        #[arg(short, long, default_value = "5m")]
        last: String,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Show service dependency graph
    Graph {
        /// Target namespace
        #[arg(long, default_value = "default")]
        graph_namespace: String,

        /// Output format (ascii, json)
        #[arg(short, long, default_value = "ascii")]
        format: String,
    },

    /// Manage flow policies
    Policy {
        #[command(subcommand)]
        action: PolicyCommands,
    },

    /// Show network health overview
    Status {
        /// Target namespace
        #[arg(long, default_value = "default")]
        status_namespace: String,
    },

    /// Show detected network anomalies
    Anomalies {
        /// Filter by service
        #[arg(short, long)]
        service: Option<String>,

        /// Filter by severity (critical, high, medium, low)
        #[arg(long)]
        severity: Option<String>,

        /// Target namespace
        #[arg(long, default_value = "default")]
        anomaly_namespace: String,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },
}

#[derive(Subcommand)]
enum PolicyCommands {
    /// Show auto-generated policy suggestions
    Suggest {
        /// Target namespace
        #[arg(long, default_value = "default")]
        policy_namespace: String,
    },

    /// Apply a suggested policy
    Apply {
        /// Policy name to apply
        policy_name: String,

        /// Target namespace
        #[arg(long, default_value = "default")]
        policy_namespace: String,
    },

    /// List all flow policies
    List {
        /// Target namespace
        #[arg(long, default_value = "default")]
        policy_namespace: String,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },
}

#[derive(Subcommand)]
enum SecurityCommands {
    /// Show security alerts
    Alerts {
        /// Filter by severity (critical, high, medium, low)
        #[arg(long)]
        severity: Option<String>,

        /// Filter by alert type (escape, mining, exfiltration, privesc)
        #[arg(long, name = "type")]
        alert_type: Option<String>,

        /// Target namespace
        #[arg(long, default_value = "default")]
        security_namespace: String,
    },

    /// Show security status overview
    Status {
        /// Target namespace
        #[arg(long, default_value = "default")]
        security_namespace: String,
    },

    /// Manage security policies
    Policy {
        #[command(subcommand)]
        action: SecurityPolicyCommands,
    },
}

#[derive(Subcommand)]
enum SecurityPolicyCommands {
    /// List security policies
    List {
        /// Target namespace
        #[arg(long, default_value = "default")]
        security_namespace: String,

        /// Output format
        #[arg(short, long, value_enum, default_value_t = OutputFormat::Table, env = "GRYVIA_OUTPUT")]
        output: OutputFormat,
    },

    /// Create a security policy
    Create {
        /// Policy name
        name: String,

        /// Target namespaces (comma-separated)
        #[arg(long)]
        namespaces: String,

        /// Detection rules to enable (comma-separated: escape,mining,exfiltration,privesc,driver_fim)
        #[arg(long)]
        rules: String,

        /// Deprecated and ignored by the operator: nothing is ever blocked automatically
        #[arg(long)]
        auto_block: bool,

        /// Target namespace for the policy CR
        #[arg(long, default_value = "default")]
        security_namespace: String,
    },
}

#[derive(Subcommand)]
enum GpuCommands {
    /// Show NCCL collective operation stats for a job
    Nccl {
        /// Job name
        #[arg(long)]
        job: String,

        /// Follow / live stream mode
        #[arg(short, long)]
        follow: bool,

        /// Target namespace
        #[arg(long, default_value = "default")]
        gpu_namespace: String,
    },

    /// Show host/device memory transfer counters (through the gateway)
    Memory {
        /// Node name (not applied: the gateway sums the counters over all collectors)
        #[arg(long)]
        node: Option<String>,

        /// Target namespace
        #[arg(long, default_value = "default")]
        gpu_namespace: String,
    },

    /// Show RDMA / fabric signals (GryviaFabricSignal status; with --node also the node's fabric health)
    Rdma {
        /// Node name (adds that node's GryviaNodeFabric)
        #[arg(long)]
        node: Option<String>,

        /// Only the fabric signal of this job
        #[arg(long)]
        job: Option<String>,

        /// Target namespace
        #[arg(long, default_value = "default")]
        gpu_namespace: String,
    },

    /// Show training insights for a job
    Training {
        /// Job name
        #[arg(long)]
        job: String,

        /// Target namespace
        #[arg(long, default_value = "default")]
        gpu_namespace: String,
    },
}

#[tokio::main]
async fn main() {
    if let Err(err) = run().await {
        for line in display::fatal_lines(&err, ui::color_enabled()) {
            eprintln!("{line}");
        }
        std::process::exit(1);
    }
}

async fn run() -> Result<()> {
    let args: Vec<String> = std::env::args().collect();
    let no_color = ui::args_request_no_color(&args);
    ui::init(no_color);

    // Root help is rendered here so commands can be shown in named groups.
    if ui::help::wants_root_help(&args) {
        let color = colored::control::SHOULD_COLORIZE.should_colorize();
        print!("{}", ui::help::render_root_help(&Cli::command(), color));
        return Ok(());
    }

    let mut command = Cli::command();
    if no_color {
        command = command.color(ColorChoice::Never);
    }
    let matches = command.get_matches_from(&args);
    let cli = Cli::from_arg_matches(&matches).unwrap_or_else(|e| e.exit());

    // Setup logging
    // Library log lines (for example the kube client's) are noise in normal use; show them with --verbose or RUST_LOG.
    let default_level = if cli.verbose { "debug" } else { "error" };
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new(default_level));
    tracing_subscriber::fmt().with_env_filter(filter).init();

    // Commands that never touch the cluster run before a kube client is built, so they work with no kubeconfig.
    match &cli.command {
        Commands::Completion { shell } => {
            clap_complete::generate(
                *shell,
                &mut Cli::command(),
                "gryvia",
                &mut std::io::stdout(),
            );
            return Ok(());
        }
        Commands::Validate { file } => {
            commands::validate::execute(file).await?;
            return Ok(());
        }
        Commands::Version { client: true, .. } => {
            commands::version::print_client();
            return Ok(());
        }
        // Talks only to the LLM gateway.
        Commands::Rag {
            action:
                RagCommands::Query {
                    index,
                    query,
                    top_k,
                    llm_gateway_url,
                    key,
                    output,
                },
        } => {
            let cfg = commands::rag::llm_gateway(llm_gateway_url.as_deref(), key.as_deref())?;
            commands::rag::query(cfg, index, query, *top_k, output.as_str()).await?;
            return Ok(());
        }
        // Talks only to the api-gateway.
        Commands::Agents {
            action:
                AgentsCommands::Chat {
                    name,
                    message,
                    output,
                },
        } => {
            let cfg = gateway::GatewayConfig::resolve(cli.gateway.as_deref(), cli.insecure);
            commands::agents::chat(cfg, name, message, output.as_str()).await?;
            return Ok(());
        }
        Commands::Workspace {
            action: WorkspaceCommands::Open { name, print },
        } => {
            let cfg = gateway::GatewayConfig::resolve(cli.gateway.as_deref(), cli.insecure);
            commands::workspace::open(cfg, name, *print).await?;
            return Ok(());
        }
        _ => {}
    }

    // Commands whose data lives behind the gateway run through it when one is configured, before (and
    // without) a kube client, so they work from a laptop with only GRYVIA_GATEWAY_URL and GRYVIA_API_KEY.
    let gateway = gateway::GatewayConfig::resolve(cli.gateway.as_deref(), cli.insecure)
        .map(gateway::GatewayClient::new);
    if let Some(gw) = &gateway {
        match &cli.command {
            Commands::Network {
                action:
                    NetworkCommands::Flows {
                        service, output, ..
                    },
            } => {
                commands::flows::execute_gateway(gw, service.as_deref(), output.as_str()).await?;
                return Ok(());
            }
            Commands::Network {
                action: NetworkCommands::Graph { format, .. },
            } => {
                commands::graph::execute_gateway(gw, format).await?;
                return Ok(());
            }
            Commands::Gpu {
                action: GpuCommands::Memory { node, .. },
            } => {
                commands::gpu_trace::execute_memory_gateway(gw, node.as_deref()).await?;
                return Ok(());
            }
            Commands::Security {
                action:
                    SecurityCommands::Alerts {
                        severity,
                        alert_type,
                        ..
                    },
            } => {
                let printed = commands::security::alerts_via_gateway(
                    gw,
                    severity.as_deref(),
                    alert_type.as_deref(),
                )
                .await?;
                if printed {
                    return Ok(());
                }
                // no per-event source at the gateway: fall through to the policy counters (kube)
            }
            _ => {}
        }
    }

    // `gpu memory` has no Kubernetes source at all: without a gateway it only says what it needs.
    if gateway.is_none() {
        if let Commands::Gpu {
            action: GpuCommands::Memory { node, .. },
        } = &cli.command
        {
            commands::gpu_trace::execute_memory_without_gateway(node.as_deref())?;
            return Ok(());
        }
    }

    // An sbatch dry run only converts the script: no cluster needed.
    if let Commands::Submit {
        sbatch: Some(script),
        image,
        dry_run: true,
        ..
    } = &cli.command
    {
        commands::submit::print_sbatch(script, image.clone(), cli.namespace.clone())?;
        return Ok(());
    }
    if let Commands::Submit {
        sbatch: None,
        image: Some(_),
        dry_run: true,
        ..
    } = &cli.command
    {
        let job = inline_job(&cli.command).expect("submit with --image");
        let ns = cli
            .namespace
            .clone()
            .unwrap_or_else(|| "default".to_string());
        commands::submit::print_inline(&job, &ns)?;
        return Ok(());
    }

    // Save namespace flag before passing ownership to client
    let namespace_flag = cli.namespace.clone();

    // Create Kubernetes client
    let client = client::GryviaClient::new(cli.context, cli.namespace).await?;

    // Execute command
    match cli.command {
        ref submit @ Commands::Submit {
            ref file,
            ref sbatch,
            ref image,
            wait,
            logs,
            ..
        } => match (file, sbatch) {
            (_, Some(script)) => {
                commands::submit::execute_sbatch(&client, script, image.clone(), wait, logs).await?
            }
            (Some(file), None) => commands::submit::execute(&client, file, wait, logs).await?,
            (None, None) => {
                let job = inline_job(submit).expect("clap requires --file, --sbatch or --image");
                commands::submit::execute_inline(&client, &job, wait, logs).await?
            }
        },
        Commands::List {
            resource,
            all_namespaces,
            output,
        } => {
            commands::list::execute(&client, &resource, all_namespaces, output.as_str()).await?;
        }
        Commands::Get {
            resource,
            name,
            output,
        } => {
            commands::get::execute(&client, &resource, &name, output.as_str()).await?;
        }
        Commands::Delete {
            resource,
            name,
            yes,
        } => {
            commands::delete::execute(&client, &resource, &name, yes).await?;
        }
        Commands::Status {
            job: Some(job),
            follow,
            ..
        } => {
            commands::status::execute(&client, &job, follow).await?;
        }
        Commands::Status {
            job: None,
            node,
            brief,
            wait,
            wait_timeout,
            output,
            platform_namespace,
            ..
        } => {
            let opts = commands::platform_status::Options {
                namespace: platform_namespace,
                node,
                brief,
                wait,
                wait_timeout: commands::platform_status::parse_duration(&wait_timeout)?,
                output,
            };
            let code = commands::platform_status::execute(&client, opts).await?;
            if code != 0 {
                std::process::exit(code);
            }
        }
        Commands::Logs {
            job,
            follow,
            tail,
            replica,
        } => {
            commands::logs::execute(&client, &job, follow, tail, replica).await?;
        }
        Commands::Cancel { jobs, yes } => {
            commands::cancel::execute(&client, &jobs, yes).await?;
        }
        Commands::Cluster {
            detailed,
            watch,
            output,
        } => {
            commands::cluster::execute(&client, detailed, watch, output.as_str()).await?;
        }
        Commands::Quota {
            team,
            budget,
            output,
        } => {
            commands::quota::execute(&client, team, budget, output.as_str()).await?;
        }
        Commands::Cost {
            team,
            period,
            detailed,
        } => {
            commands::cost::execute(&client, team, &period, detailed).await?;
        }
        Commands::Queue {
            name,
            watch,
            output,
        } => {
            commands::queue::execute(&client, name, watch, output.as_str()).await?;
        }
        Commands::Reservation { action } => match action {
            ReservationCommands::List { output } => {
                commands::reservation::list(&client, output.as_str()).await?;
            }
            ReservationCommands::Create {
                name,
                owner_type,
                owner,
                gpu_type,
                gpus,
                start,
                end,
                duration,
                exclusive,
            } => {
                commands::reservation::create(
                    &client,
                    name.as_deref(),
                    &owner_type,
                    &owner,
                    &gpu_type,
                    gpus,
                    start.as_deref(),
                    end.as_deref(),
                    duration.as_deref(),
                    exclusive,
                )
                .await?;
            }
            ReservationCommands::Cancel { name, yes } => {
                commands::reservation::cancel(&client, &name, yes).await?;
            }
        },
        Commands::Models {
            action: ModelsCommands::Watch { action },
        } => match action {
            WatchCommands::List { output } => {
                commands::models::list(&client, output.as_str()).await?;
            }
            WatchCommands::Runs { name, all, output } => {
                commands::models::runs(&client, &name, all, output.as_str()).await?;
            }
            WatchCommands::Create { file } => {
                commands::models::create(&client, &file).await?;
            }
            WatchCommands::Suspend { name } => {
                commands::models::set_suspend(&client, &name, true).await?;
            }
            WatchCommands::Resume { name } => {
                commands::models::set_suspend(&client, &name, false).await?;
            }
            WatchCommands::Delete { name, yes } => {
                commands::models::delete(&client, &name, yes).await?;
            }
        },
        Commands::Models {
            action: ModelsCommands::Rollback { name, yes },
        } => {
            commands::models::rollback(&client, &name, yes).await?;
        }
        Commands::Datasets { action } => {
            run_crd(
                &client,
                &commands::datasets::DATASETS,
                action,
                "follow it with: gryvia datasets list",
                "Its PVC and every downloaded version are deleted",
            )
            .await?;
        }
        Commands::JobHooks { action } => {
            run_crd(
                &client,
                &commands::jobhooks::JOB_HOOKS,
                action,
                "follow it with: gryvia job-hooks list",
                "Transitions after this are no longer delivered",
            )
            .await?;
        }
        Commands::Rag { action } => match action {
            RagCommands::Index { action } => {
                run_crd(
                    &client,
                    &commands::rag::VECTOR_INDEXES,
                    action,
                    "follow it with: gryvia rag index list",
                    "Its managed Qdrant (with its volume), ingestion Jobs and gateway key are deleted",
                )
                .await?;
            }
            RagCommands::Reingest { name } => commands::rag::reingest(&client, &name).await?,
            RagCommands::Query { .. } => unreachable!("handled before the kube client is created"),
        },
        Commands::Agents { action } => match action {
            AgentsCommands::Crd(action) => {
                run_crd(
                    &client,
                    &commands::agents::AGENTS,
                    action,
                    "follow it with: gryvia agents list",
                    "Its runtime Deployment, Service, NetworkPolicy and gateway key are deleted",
                )
                .await?;
            }
            AgentsCommands::Chat { .. } => {
                unreachable!("handled before the kube client is created")
            }
        },
        Commands::Workspace { action } => match action {
            WorkspaceCommands::Crd(action) => {
                run_crd(
                    &client,
                    &commands::workspace::WORKSPACES,
                    action,
                    "follow it with: gryvia workspace list",
                    "Its pod, Service and NetworkPolicy are deleted; the storage claim is deleted with it",
                )
                .await?;
            }
            WorkspaceCommands::New {
                name,
                ws_type,
                gpus,
                gpu_type,
                image,
                storage,
                idle_timeout,
                idle_pause,
            } => {
                let w = commands::workspace::NewWorkspace {
                    name,
                    ws_type,
                    gpus,
                    gpu_type,
                    image,
                    storage,
                    idle_timeout_minutes: idle_timeout,
                    idle_pause,
                };
                commands::workspace::new(&client, &w).await?;
            }
            WorkspaceCommands::Pause { name } => {
                commands::workspace::set_paused(&client, &name, true).await?;
            }
            WorkspaceCommands::Resume { name } => {
                commands::workspace::set_paused(&client, &name, false).await?;
            }
            WorkspaceCommands::Open { .. } => {
                unreachable!("handled before the kube client is created")
            }
        },
        Commands::Llm { action } => match action {
            LlmCommands::Keys {
                key_namespace,
                action,
            } => match action {
                LlmKeyCommands::Create {
                    name,
                    description,
                    output,
                } => {
                    commands::llm::keys_create(
                        &client,
                        &key_namespace,
                        &name,
                        &description,
                        output.as_str(),
                    )
                    .await?;
                }
                LlmKeyCommands::List {
                    all_namespaces,
                    output,
                } => {
                    commands::llm::keys_list(
                        &client,
                        &key_namespace,
                        all_namespaces,
                        output.as_str(),
                    )
                    .await?;
                }
                LlmKeyCommands::Delete { name, yes } => {
                    commands::llm::keys_delete(&client, &key_namespace, &name, yes).await?;
                }
            },
            LlmCommands::Models {
                all_namespaces,
                output,
            } => {
                commands::llm::models(&client, all_namespaces, output.as_str()).await?;
            }
            LlmCommands::Usage {
                all_namespaces,
                group_by,
                days,
                output,
            } => {
                commands::llm::usage(&client, all_namespaces, &group_by, days, output.as_str())
                    .await?;
            }
        },
        Commands::Budget { scope, output } => {
            commands::budget::execute(&client, scope, output.as_str()).await?;
        }
        Commands::Catalog { output } => {
            commands::catalog::execute(&client, output.as_str()).await?;
        }
        Commands::Tenant { action } => match action {
            TenantCommands::List { output } => {
                commands::tenant::list(&client, output.as_str()).await?;
            }
            TenantCommands::Get { name, output } => {
                commands::tenant::get(&client, &name, output.as_str()).await?;
            }
            TenantCommands::Create {
                name,
                display_name,
                allowed_sku,
                max_gpus,
                isolated,
            } => {
                commands::tenant::create(
                    &client,
                    &name,
                    display_name.as_deref(),
                    &allowed_sku,
                    max_gpus,
                    isolated,
                )
                .await?;
            }
            TenantCommands::Delete { name, yes } => {
                commands::tenant::delete(&client, &name, yes).await?;
            }
        },
        Commands::Usage {
            tenant,
            from,
            to,
            group_by,
            network,
            output,
        } => {
            if network {
                let opts = commands::network_usage::Options {
                    tenant,
                    from,
                    to,
                    group_by,
                    output,
                };
                commands::network_usage::execute(&client, opts).await?;
            } else {
                let opts = commands::usage::Options {
                    tenant,
                    from,
                    to,
                    group_by,
                    output,
                };
                commands::usage::execute(&client, opts).await?;
            }
        }
        Commands::Invoice {
            tenant,
            month,
            output,
        } => {
            let opts = commands::invoice::Options {
                tenant,
                month,
                output,
            };
            commands::invoice::execute(&client, opts).await?;
        }
        Commands::Create { resource } => {
            commands::create::execute(&client, &resource).await?;
        }
        Commands::Completion { .. } | Commands::Validate { .. } => {
            unreachable!("handled before the cluster client is created")
        }
        Commands::Version {
            platform_namespace, ..
        } => {
            commands::version::execute(&client, &platform_namespace).await?;
        }
        Commands::Health { component } => {
            commands::health::execute(&client, &component).await?;
        }
        Commands::Capacity { output, gpu_type } => {
            commands::capacity::execute(&client, output.as_str(), gpu_type.as_deref()).await?;
        }
        Commands::Maintenance { action } => match action {
            MaintenanceCommands::Start {
                node,
                reason,
                drain,
                yes,
            } => {
                commands::maintenance::start(&client, &node, reason.as_deref(), drain, yes).await?;
            }
            MaintenanceCommands::End { node } => {
                commands::maintenance::end(&client, &node).await?;
            }
            MaintenanceCommands::List { output } => {
                commands::maintenance::list(&client, output.as_str()).await?;
            }
        },
        Commands::Network { action } => {
            let has_ns_flag = namespace_flag.is_some();
            let ns_default = client.namespace().to_string();
            match action {
                NetworkCommands::Trace {
                    service,
                    duration,
                    level,
                    trace_namespace,
                    follow,
                } => {
                    let effective_ns = if has_ns_flag {
                        &ns_default
                    } else {
                        &trace_namespace
                    };
                    commands::trace::execute(
                        &client,
                        &service,
                        &duration,
                        &level,
                        effective_ns,
                        follow,
                    )
                    .await?;
                }
                NetworkCommands::Flows {
                    service,
                    flow_namespace,
                    last,
                    output,
                } => {
                    let effective_ns = if has_ns_flag {
                        &ns_default
                    } else {
                        &flow_namespace
                    };
                    commands::flows::execute(
                        &client,
                        service.as_deref(),
                        effective_ns,
                        &last,
                        output.as_str(),
                    )
                    .await?;
                }
                NetworkCommands::Graph {
                    graph_namespace,
                    format,
                } => {
                    let effective_ns = if has_ns_flag {
                        &ns_default
                    } else {
                        &graph_namespace
                    };
                    commands::graph::execute(&client, effective_ns, &format).await?;
                }
                NetworkCommands::Policy {
                    action: policy_action,
                } => match policy_action {
                    PolicyCommands::Suggest { policy_namespace } => {
                        let effective_ns = if has_ns_flag {
                            &ns_default
                        } else {
                            &policy_namespace
                        };
                        commands::policy::execute(
                            &client,
                            commands::policy::PolicyAction::Suggest {
                                namespace: effective_ns.to_string(),
                            },
                        )
                        .await?;
                    }
                    PolicyCommands::Apply {
                        policy_name,
                        policy_namespace,
                    } => {
                        let effective_ns = if has_ns_flag {
                            &ns_default
                        } else {
                            &policy_namespace
                        };
                        commands::policy::execute(
                            &client,
                            commands::policy::PolicyAction::Apply {
                                policy_name,
                                namespace: effective_ns.to_string(),
                            },
                        )
                        .await?;
                    }
                    PolicyCommands::List {
                        policy_namespace,
                        output,
                    } => {
                        let effective_ns = if has_ns_flag {
                            &ns_default
                        } else {
                            &policy_namespace
                        };
                        commands::policy::execute(
                            &client,
                            commands::policy::PolicyAction::List {
                                namespace: effective_ns.to_string(),
                                output: output.as_str().to_string(),
                            },
                        )
                        .await?;
                    }
                },
                NetworkCommands::Status { status_namespace } => {
                    let effective_ns = if has_ns_flag {
                        &ns_default
                    } else {
                        &status_namespace
                    };
                    commands::network::execute_status(&client, effective_ns).await?;
                }
                NetworkCommands::Anomalies {
                    service,
                    severity,
                    anomaly_namespace,
                    output,
                } => {
                    let effective_ns = if has_ns_flag {
                        &ns_default
                    } else {
                        &anomaly_namespace
                    };
                    commands::network::execute_anomalies(
                        &client,
                        effective_ns,
                        service.as_deref(),
                        severity.as_deref(),
                        output.as_str(),
                    )
                    .await?;
                }
            }
        }
        Commands::Security { action } => {
            let has_ns_flag = namespace_flag.is_some();
            let ns_default = client.namespace().to_string();
            match action {
                SecurityCommands::Alerts {
                    severity,
                    alert_type,
                    security_namespace,
                } => {
                    let effective_ns = if has_ns_flag {
                        &ns_default
                    } else {
                        &security_namespace
                    };
                    commands::security::execute(
                        &client,
                        commands::security::SecurityAction::Alerts {
                            severity,
                            alert_type,
                            namespace: effective_ns.to_string(),
                        },
                    )
                    .await?;
                }
                SecurityCommands::Status { security_namespace } => {
                    let effective_ns = if has_ns_flag {
                        &ns_default
                    } else {
                        &security_namespace
                    };
                    commands::security::execute(
                        &client,
                        commands::security::SecurityAction::Status {
                            namespace: effective_ns.to_string(),
                        },
                    )
                    .await?;
                }
                SecurityCommands::Policy {
                    action: policy_action,
                } => match policy_action {
                    SecurityPolicyCommands::List {
                        security_namespace,
                        output,
                    } => {
                        let effective_ns = if has_ns_flag {
                            &ns_default
                        } else {
                            &security_namespace
                        };
                        commands::security::execute(
                            &client,
                            commands::security::SecurityAction::PolicyList {
                                namespace: effective_ns.to_string(),
                                output: output.as_str().to_string(),
                            },
                        )
                        .await?;
                    }
                    SecurityPolicyCommands::Create {
                        name,
                        namespaces,
                        rules,
                        auto_block,
                        security_namespace,
                    } => {
                        let effective_ns = if has_ns_flag {
                            &ns_default
                        } else {
                            &security_namespace
                        };
                        let ns_list: Vec<String> = namespaces
                            .split(',')
                            .map(|s| s.trim().to_string())
                            .collect();
                        let rule_list: Vec<String> =
                            rules.split(',').map(|s| s.trim().to_string()).collect();
                        commands::security::execute(
                            &client,
                            commands::security::SecurityAction::PolicyCreate {
                                name,
                                namespaces: ns_list,
                                rules: rule_list,
                                auto_block,
                                namespace: effective_ns.to_string(),
                            },
                        )
                        .await?;
                    }
                },
            }
        }
        Commands::Gpu { action } => {
            let has_ns_flag = namespace_flag.is_some();
            let ns_default = client.namespace().to_string();
            match action {
                GpuCommands::Nccl {
                    job,
                    follow,
                    gpu_namespace,
                } => {
                    let effective_ns = if has_ns_flag {
                        &ns_default
                    } else {
                        &gpu_namespace
                    };
                    commands::gpu_trace::execute(
                        &client,
                        commands::gpu_trace::GpuAction::Nccl {
                            job,
                            follow,
                            namespace: effective_ns.to_string(),
                        },
                    )
                    .await?;
                }
                GpuCommands::Memory { .. } => {
                    unreachable!("handled before the cluster client is created")
                }
                GpuCommands::Rdma {
                    node,
                    job,
                    gpu_namespace,
                } => {
                    let effective_ns = if has_ns_flag {
                        &ns_default
                    } else {
                        &gpu_namespace
                    };
                    commands::gpu_trace::execute(
                        &client,
                        commands::gpu_trace::GpuAction::Rdma {
                            node,
                            job,
                            namespace: effective_ns.to_string(),
                        },
                    )
                    .await?;
                }
                GpuCommands::Training { job, gpu_namespace } => {
                    let effective_ns = if has_ns_flag {
                        &ns_default
                    } else {
                        &gpu_namespace
                    };
                    commands::gpu_trace::execute(
                        &client,
                        commands::gpu_trace::GpuAction::Training {
                            job,
                            namespace: effective_ns.to_string(),
                        },
                    )
                    .await?;
                }
            }
        }
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Every `gryvia ...` line in a command's "Examples:" help must parse with the real argument definitions.
    #[test]
    fn help_examples_parse() {
        fn collect(cmd: &clap::Command, out: &mut Vec<String>) {
            if let Some(text) = cmd.get_after_help() {
                for line in text.to_string().lines() {
                    let line = line.trim();
                    if line.starts_with("gryvia ") {
                        out.push(line.split(" > ").next().unwrap().to_string());
                    }
                }
            }
            for sub in cmd.get_subcommands() {
                collect(sub, out);
            }
        }
        let mut examples = Vec::new();
        collect(&Cli::command(), &mut examples);
        assert!(
            examples.len() >= 25,
            "expected examples on most commands, got {}",
            examples.len()
        );
        for line in examples {
            let args: Vec<&str> = line.split_whitespace().collect();
            if let Err(e) = Cli::command().try_get_matches_from(args) {
                panic!("help example does not parse: `{line}`: {e}");
            }
        }
    }

    #[test]
    fn every_top_level_command_has_an_example() {
        for sub in Cli::command().get_subcommands() {
            if sub.get_name() == "help" {
                continue;
            }
            assert!(
                sub.get_after_help().is_some(),
                "`{}` needs an Examples block",
                sub.get_name()
            );
        }
    }

    #[test]
    fn visible_aliases_resolve_to_their_commands() {
        for (alias, target) in [
            ("ls", "list"),
            ("rm", "delete"),
            ("describe", "get"),
            ("cap", "capacity"),
            ("skus", "catalog"),
        ] {
            let m = Cli::command()
                .try_get_matches_from(match target {
                    "list" => vec!["gryvia", alias, "jobs"],
                    "delete" => vec!["gryvia", alias, "job", "x"],
                    "get" => vec!["gryvia", alias, "job", "x"],
                    _ => vec!["gryvia", alias],
                })
                .unwrap();
            assert_eq!(m.subcommand_name(), Some(target));
        }
    }

    #[test]
    fn data_commands_take_an_output_format() {
        for args in [
            vec!["gryvia", "cluster", "-o", "json"],
            vec!["gryvia", "quota", "-o", "yaml"],
            vec!["gryvia", "queue", "-o", "json"],
            vec!["gryvia", "maintenance", "list", "-o", "json"],
            vec!["gryvia", "capacity", "-o", "yaml"],
            vec!["gryvia", "status", "-o", "json"],
            vec!["gryvia", "catalog", "-o", "json"],
            vec!["gryvia", "tenant", "list", "-o", "yaml"],
            vec!["gryvia", "tenant", "get", "acme", "-o", "json"],
            vec!["gryvia", "usage", "-o", "csv"],
            vec!["gryvia", "usage", "--group-by", "day", "-o", "json"],
            vec![
                "gryvia",
                "usage",
                "--network",
                "--group-by",
                "zone-class",
                "-o",
                "csv",
            ],
            vec!["gryvia", "invoice", "-o", "csv"],
            vec![
                "gryvia", "invoice", "--tenant", "acme", "--month", "2026-09", "-o", "yaml",
            ],
        ] {
            assert!(
                Cli::command().try_get_matches_from(args.clone()).is_ok(),
                "{args:?} should parse"
            );
        }
        assert!(Cli::command()
            .try_get_matches_from(["gryvia", "quota", "-o", "csv"])
            .is_err());
    }

    #[test]
    fn tenant_create_takes_repeated_skus_and_a_bool() {
        let m = Cli::command()
            .try_get_matches_from([
                "gryvia",
                "tenant",
                "create",
                "acme",
                "--allowed-sku",
                "a",
                "--allowed-sku",
                "b",
                "--max-gpus",
                "8",
                "--isolated",
                "false",
            ])
            .unwrap();
        let (_, create) = m
            .subcommand_matches("tenant")
            .unwrap()
            .subcommand()
            .unwrap();
        assert_eq!(create.get_many::<String>("allowed_sku").unwrap().count(), 2);
        assert_eq!(create.get_one::<bool>("isolated"), Some(&false));
        assert!(Cli::command()
            .try_get_matches_from(["gryvia", "tenant", "create", "acme", "--max-gpus", "x"])
            .is_err());
        assert!(Cli::command()
            .try_get_matches_from(["gryvia", "usage", "--group-by", "week"])
            .is_err());
        for bad in ["2026-13", "2026-9", "sept", "2026-09-01", ""] {
            assert!(
                Cli::command()
                    .try_get_matches_from(["gryvia", "invoice", "--month", bad])
                    .is_err(),
                "--month {bad:?} should be a usage error"
            );
        }
        assert!(Cli::command()
            .try_get_matches_from(["gryvia", "catalog", "-o", "csv"])
            .is_err());
    }

    #[test]
    fn output_flag_accepts_only_known_formats() {
        assert!(Cli::command()
            .try_get_matches_from(["gryvia", "list", "jobs", "-o", "json"])
            .is_ok());
        assert!(Cli::command()
            .try_get_matches_from(["gryvia", "list", "jobs", "-o", "xml"])
            .is_err());
        assert!(Cli::command()
            .try_get_matches_from(["gryvia", "get", "job", "x", "-o", "table"])
            .is_err());
    }
}
