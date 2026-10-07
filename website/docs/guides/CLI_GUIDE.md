# Gryvia CLI Guide

The Gryvia CLI (Rust, in `cli/`) provides a command-line interface for managing GPU clusters, submitting jobs, monitoring quotas, and analyzing costs.

The CLI talks to the **Kubernetes API using your kubeconfig** (or `--context`); it does not call the Gryvia API gateway.
It therefore needs the `gryvia.io/v1alpha1` CRDs installed and RBAC to read and create them. Commands only show what the
cluster's controllers have written: kinds without a registered controller (see [reference/crds.md](../reference/crds.md))
are never filled in, and the network, security and GPU-analysis commands depend on optional components described in
[Network Intelligence](./NETWORK_INTELLIGENCE.md).

Run `gryvia <command> --help` for the authoritative list of options for any command; this guide may lag behind the CLI.

## Installation

### From Release Binary

Tagged releases attach CLI binaries named `gryvia-<tag>-<target>` for `linux-amd64`, `linux-arm64` and
`darwin-arm64`, each with a `.sha256` file. Download the one for your platform from the GitHub releases page, verify the
checksum and install it:

```bash
# example for linux-amd64; replace <tag> with a real release tag
curl -LO https://github.com/zyvorai/gryvia/releases/download/<tag>/gryvia-<tag>-linux-amd64
curl -LO https://github.com/zyvorai/gryvia/releases/download/<tag>/gryvia-<tag>-linux-amd64.sha256
shasum -a 256 -c gryvia-<tag>-linux-amd64.sha256
chmod +x gryvia-<tag>-linux-amd64
sudo mv gryvia-<tag>-linux-amd64 /usr/local/bin/gryvia
```

### From Source

```bash
cd cli
cargo build --release
sudo cp target/release/gryvia /usr/local/bin/
```

### Verify Installation

```bash
gryvia --version
gryvia --help
```

## Platform status

`gryvia status` with no argument reports on the whole platform, in the style of `cilium status`: one line per
component with an `OK`, `Warning`, `Error` or `disabled` marker, then workload readiness, cluster totals, image
versions, any errors, and a per-node table. It reads only the Kubernetes API, so it needs no agent on the nodes.

```bash
gryvia status
```

Example output (from the CLI's own snapshot tests; the image tags and node data are test fixtures, not real releases):

```text
    ______      Gryvia:       OK
   /      \     Operators:    OK  gpu, ai, quota
  /   G    \    API gateway:  OK  2/2 ready
  \        /    Dashboard:    OK  2/2 ready
   \______/     GPU nodes:    OK  2/2 ready
                GPU add-ons:  OK  dcgm-exporter 2/2, nvidia-device-plugin 2/2
                Collectors:   disabled  not installed

Deployment gryvia-gpu-operator          Desired: 1, Ready: 1/1, Available: 1/1
Deployment gryvia-ai-operator           Desired: 1, Ready: 1/1, Available: 1/1
Deployment gryvia-quota-operator        Desired: 1, Ready: 1/1, Available: 1/1
Deployment gryvia-api-gateway           Desired: 2, Ready: 2/2, Available: 2/2
Deployment gryvia-ui                    Desired: 2, Ready: 2/2, Available: 2/2
DaemonSet  gryvia-dcgm-exporter         Desired: 2, Ready: 2/2, Available: 2/2
DaemonSet  gryvia-nvidia-device-plugin  Desired: 2, Ready: 2/2, Available: 2/2

Cluster Pods:   5/5 running
Jobs:           1 running, 2 pending, 3 completed, 0 failed
Namespace:      gryvia-system

Image versions
  gryvia-ai-operator           ghcr.io/zyvorai/gryvia-ai-operator:1.0.0
  gryvia-api-gateway           ghcr.io/zyvorai/gryvia-api-gateway:1.0.0
  gryvia-dcgm-exporter         ghcr.io/zyvorai/gryvia-dcgm-exporter:1.0.0
  gryvia-gpu-operator          ghcr.io/zyvorai/gryvia-gpu-operator:1.0.0
  gryvia-nvidia-device-plugin  ghcr.io/zyvorai/gryvia-nvidia-device-plugin:1.0.0
  gryvia-quota-operator        ghcr.io/zyvorai/gryvia-quota-operator:1.0.0
  gryvia-ui                    ghcr.io/zyvorai/gryvia-ui:1.0.0

Nodes
  NODE    STATE  GPUS      DRIVER              GPU HEALTH  DCGM  PLUGIN  COLLECTOR  PODS
  node-a  Ready  8 x H100  550.54 / CUDA 12.4  8/8         ✓     ✓       -          3
  node-b  Ready  8 x H100  550.54 / CUDA 12.4  8/8         ✓     ✓       -          2
```

The per-node table has one row per Kubernetes node (plus a row for any `GryviaGpuNode` whose node does not exist):
readiness, GPUs, driver and CUDA versions, how many GPUs report healthy, whether the DCGM exporter, NVIDIA device
plugin and collector pods run on that node (`✓` running, `✗` present but not ready, `-` none), pod count, and a note
when the node is cordoned or in maintenance. When something is wrong the platform line turns `Warning` or `Error`
and the problems are listed:

```text
    ______      Gryvia:       Warning
   /      \     Operators:    OK  gpu, ai
  /   G    \    API gateway:  Warning  1/2 ready
  \        /    Dashboard:    OK  2/2 ready
   \______/     GPU nodes:    Warning  2/3 ready
                GPU add-ons:  OK  dcgm-exporter 2/2, nvidia-device-plugin 2/2
                Collectors:   disabled  not installed

Deployment gryvia-gpu-operator          Desired: 1, Ready: 1/1, Available: 1/1
Deployment gryvia-ai-operator           Desired: 1, Ready: 1/1, Available: 1/1
Deployment gryvia-api-gateway           Desired: 2, Ready: 1/2, Available: 1/2
Deployment gryvia-ui                    Desired: 2, Ready: 2/2, Available: 2/2
DaemonSet  gryvia-dcgm-exporter         Desired: 2, Ready: 2/2, Available: 2/2
DaemonSet  gryvia-nvidia-device-plugin  Desired: 2, Ready: 2/2, Available: 2/2

Cluster Pods:   5/6 running
Jobs:           1 running, 2 pending, 3 completed, 0 failed
Namespace:      gryvia-system

Image versions
  gryvia-ai-operator           ghcr.io/zyvorai/gryvia-ai-operator:1.0.0
  gryvia-api-gateway           ghcr.io/zyvorai/gryvia-api-gateway:1.0.0
  gryvia-dcgm-exporter         ghcr.io/zyvorai/gryvia-dcgm-exporter:1.0.0
  gryvia-gpu-operator          ghcr.io/zyvorai/gryvia-gpu-operator:1.0.0
  gryvia-nvidia-device-plugin  ghcr.io/zyvorai/gryvia-nvidia-device-plugin:1.0.0
  gryvia-ui                    ghcr.io/zyvorai/gryvia-ui:1.0.0

Warnings
  ! could not list jobs: forbidden

Errors
  ✗ pod gryvia-quota-operator-q1: ImagePullBackOff

Nodes
  NODE    STATE         GPUS      DRIVER              GPU HEALTH  DCGM  PLUGIN  COLLECTOR  PODS  NOTES
  ghost   no such node  8 x H100  550.54 / CUDA 12.4  Failed      -     -       -          0
  node-a  Ready         8 x H100  550.54 / CUDA 12.4  8/8         ✓     ✓       -          3     driver update
  node-b  NotReady      8 x H100  550.54 / CUDA 12.4  6/8         ✓     ✓       -          3
```

Only the required components (operators, API gateway, dashboard) decide the result: `gryvia status` exits with
code 1 when one of them is in `Error`. GPU nodes, GPU add-ons and collectors are reported but never fail the
command, so a cluster without GPUs (for example the kind demo) still reports healthy.

Useful flags:

```bash
gryvia status --brief                       # OK, or the failing components; exit code 1 on failure
gryvia status --wait --wait-timeout 5m      # block until the platform is OK (for scripts and CI)
gryvia status --node gpu-node-01            # only that node in the per-node table
gryvia status -o json                       # the same data as JSON (or -o yaml)
gryvia status --platform-namespace my-ns    # when Gryvia is not installed in gryvia-system
gryvia status my-job                        # with a job name it shows that job instead
```

## Global options, colors and output formats

`gryvia --help` lists the commands in groups (Workloads, Cluster, Operations, Observe, Utilities) and every
command's `--help` ends with examples. Help and output are colored when stdout is a terminal.

| Option | Effect |
|--------|--------|
| `--no-color` | Turn colors off. The `NO_COLOR` environment variable does the same. |
| `CLICOLOR_FORCE=1` | Keep colors when piping (for example into `less -R`). |
| `-o, --output table\|json\|yaml` | Per-command option (not global) on commands that print data; `GRYVIA_OUTPUT` sets the default. `usage` and `invoice` also accept `csv`. |
| `--context`, `-n/--namespace` | Kubernetes context and namespace. |

Piped output has no color codes, so `gryvia list jobs | grep Running` and `gryvia status -o json | jq` work as
expected. Errors are printed to stderr with a short hint, for example when no kubeconfig is found.

```bash
gryvia --no-color status
NO_COLOR=1 gryvia cluster --detailed
```

## Shell completion and versions

```bash
gryvia completion bash > /etc/bash_completion.d/gryvia
gryvia completion zsh > ~/.zsh/completions/_gryvia
gryvia completion fish > ~/.config/fish/completions/gryvia.fish
```

`gryvia version` prints the client version and the image of every workload in the platform namespace;
`gryvia version --client` needs no cluster.

## Quick Start

### 1. Submit a Training Job

```bash
# Create job YAML
cat > llm-training.yaml <<EOF
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: llm-training
spec:
  type: training
  gpus: 8
  gpuType: H100
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command: ["python", "train.py"]
EOF

# Submit job
gryvia submit -f llm-training.yaml

# Submit and wait for completion
gryvia submit -f llm-training.yaml --wait
```

### 2. Monitor Cluster Status

```bash
# View cluster overview
gryvia cluster

# Watch mode (refresh every 5 seconds)
gryvia cluster --watch 5

# Detailed node information
gryvia cluster --detailed
```

### 3. Check Team Quota

```bash
# View all team quotas
gryvia quota

# Specific team with budget details
gryvia quota ml-research --budget
```

### 4. Analyze Costs

```bash
# Monthly cost summary for all teams
gryvia cost

# Specific team with details
gryvia cost ml-research --detailed
```

## Common Workflows

### Data Scientist Workflow

```bash
# 1. Check available GPU quota
gryvia quota my-team

# 2. View available GPU nodes
gryvia list nodes

# 3. Submit training job
gryvia submit -f my-experiment.yaml --wait

# 4. Monitor job progress
gryvia status my-experiment

# 5. View logs
gryvia logs my-experiment --follow

# 6. Check cost impact
gryvia cost my-team
```

### Team Manager Workflow

```bash
# 1. View team quota and budget status
gryvia quota computer-vision --budget

# 2. List running jobs
gryvia list jobs

# 3. Check monthly spending
gryvia cost computer-vision --period month

# 4. Cancel expensive job if needed (sets status to "Cancelled", preserves job record)
gryvia cancel expensive-job

# 5. Monitor budget alerts
gryvia quota computer-vision
```

### Cluster Admin Workflow

```bash
# 1. Check overall cluster health
gryvia health

# 2. View all nodes and their status
gryvia list nodes

# 3. Monitor all team quotas
gryvia list quotas

# 4. View cost breakdown across teams
gryvia cost

# 5. Watch cluster in real-time
gryvia cluster --watch 10
```

## Command Reference

### Job Management

#### Submit Jobs

```bash
# Basic submission
gryvia submit -f job.yaml

# Submit and wait for completion
gryvia submit -f job.yaml --wait

# Submit and follow logs
gryvia submit -f job.yaml --logs

# Use specific namespace
gryvia -n production submit -f job.yaml
```

#### Import a Slurm batch script

```bash
# Print the GryviaAIJob(s) the script becomes, with warnings for directives that have no equivalent
gryvia submit --sbatch train.sh --image pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime --dry-run

# Submit it (one job per --array index) and wait
gryvia submit --sbatch train.sh --image pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime --wait
```

`#SBATCH` directives map to job fields and `SLURM_*` variables, `srun` and `scontrol show hostnames` work inside the
script. See [Slurm batch scripts](https://github.com/zyvorai/gryvia/blob/main/docs/slurm.md).

#### List Jobs

```bash
# List all jobs in current namespace
gryvia list jobs

# All namespaces
gryvia list jobs --all-namespaces

# JSON output
gryvia list jobs --output json

# YAML output
gryvia list jobs --output yaml
```

#### Job Status

```bash
# View detailed job status
gryvia status my-training-job

# With log following
gryvia status my-job --follow
```

#### View Logs

```bash
# View last 100 lines
gryvia logs my-job

# Follow logs in real-time
gryvia logs my-job --follow

# Show last 500 lines
gryvia logs my-job --tail 500

# Specific replica for distributed jobs
gryvia logs my-job --replica 0
```

#### Cancel Jobs

Cancelling a job patches its status to "Cancelled" rather than deleting the resource. The job record is preserved for auditing and cost tracking.

```bash
# Cancel single job (sets status to "Cancelled")
gryvia cancel my-job

# Cancel multiple jobs
gryvia cancel job1 job2 job3

# Skip confirmation prompt
gryvia cancel my-job --yes
```

### Quota Management

#### View Quotas

```bash
# List all team quotas
gryvia list quotas

# Specific team quota
gryvia quota ml-research

# With budget details
gryvia quota ml-research --budget
```

#### Quota Details

```bash
# Get full quota specification
gryvia get quota team-ml

# YAML format
gryvia get quota team-ml --output yaml

# JSON format
gryvia get quota team-ml --output json
```

### Cost Analysis

#### View Costs

The `--period` parameter is validated and only accepts `day`, `week`, or `month`; any other value produces an error.

```bash
# All teams monthly costs
gryvia cost

# Specific team
gryvia cost ml-research

# Different time periods
gryvia cost --period day
gryvia cost --period week
gryvia cost --period month

# Detailed breakdown
gryvia cost ml-research --detailed
```

#### Budget Alerts

The CLI marks how much of a budget or quota is used: no marker below 80%, a warning at 80% or more, an error at 100% or
more (the quota's own `alertThreshold` is also shown as a warning when reached). Costs are estimates from job run time
and the SKU catalog.

### Cluster Overview

#### Cluster Status

```bash
# Basic cluster overview
gryvia cluster

# Detailed view with GPU metrics
gryvia cluster --detailed

# Watch mode (refresh every 5 seconds, interval must be > 0)
gryvia cluster --watch 5

# Refresh every 30 seconds
gryvia cluster --watch 30
```

#### GPU Nodes

```bash
# List all GPU nodes
gryvia list nodes

# Get node details
gryvia get node gpu-worker-01

# JSON output
gryvia get node gpu-worker-01 --output json
```

### Queue Management

```bash
# View job queue (pending/queued/scheduling jobs)
gryvia queue

# Show a single queue by name
gryvia queue my-experiment

# Watch mode (refresh every 5 seconds)
gryvia queue --watch 5
```

### Interactive Creation

```bash
# Interactive job creation wizard
gryvia create job

# Interactive quota creation wizard
gryvia create quota
```

The wizard prompts for framework, GPU type/count, image, distributed config, and more.

### Health Checks

```bash
# Check all components
gryvia health

# Check specific components
gryvia health gpu
gryvia health storage
gryvia health network
```

### Resource Management

#### Get Resources

```bash
# Get job details
gryvia get job my-training-job

# Get quota details
gryvia get quota team-ml

# Storage and network objects (GryviaStorage, GryviaNetwork)
gryvia get storage my-storage
gryvia get network my-network

# Get node details
gryvia get node gpu-worker-01

# Different output formats
gryvia get job my-job --output yaml
gryvia get job my-job --output json
```

#### Delete Resources

```bash
# Delete job
gryvia delete job old-experiment

# Delete quota
gryvia delete quota team-dev

# Delete storage
gryvia delete storage my-storage

# Delete network
gryvia delete network my-network

# Skip confirmation
gryvia delete job my-job --yes
```

#### Validate YAML

Validates the YAML file structure and checks that `apiVersion` and `kind` match known Gryvia types (e.g., `gryvia.io/v1alpha1` / `GryviaAIJob`, `GryviaQuota`, `GryviaGpuNode`, etc.).

```bash
# Validate job YAML before submission
gryvia validate job.yaml
```

### GPU Capacity

`gryvia capacity` is a read-only snapshot computed from cluster objects. Supply is the sum of
`GryviaGpuNode` `spec.gpuCount` per `spec.gpuType`; allocation is the GPUs held by Running jobs; demand is the
GPUs requested by Pending, Queued and Scheduling jobs (the same phase grouping the gateway uses, where
Succeeded and Completed both mean completed). Per type it reports free GPUs, the pending shortfall (demand
that free GPUs of that type cannot cover) and the headroom left after pending demand.

```bash
# Capacity per GPU type
gryvia capacity

# One GPU type only (case-insensitive)
gryvia capacity --gpu-type H100

# Machine-readable
gryvia capacity --output json
```

Notes on what the numbers mean:

- Jobs that do not name a GPU type (or set it to `any`) cannot be attributed to one type. They are counted only
  in the cluster-wide totals, shortfall and headroom, and are excluded when `--gpu-type` is set.
- A job type that no node registers appears as its own row with 0 total GPUs.
- If running jobs hold more GPUs than nodes provide (for example after a node was removed), free is shown as 0
  and the row is flagged with the excess.
- It does not forecast growth or estimate purchases; it counts GPUs as they are right now.

### Node Maintenance

`gryvia maintenance` wraps the cordon workflow. It marks nodes with two annotations,
`gryvia.io/maintenance` (RFC 3339 start time) and `gryvia.io/maintenance-reason`, so that `list` can show who is
out of service and for how long. The node names are Kubernetes node names.

```bash
# Cordon a node and record why
gryvia maintenance start gpu-node-05 --reason "replace failed PSU"

# Cordon and also evict the pods on it (asks for confirmation; add --yes to skip)
gryvia maintenance start gpu-node-05 --reason "firmware update" --drain

# Show nodes currently marked, with age and reason
gryvia maintenance list

# Uncordon and remove both annotations
gryvia maintenance end gpu-node-05
```

`--drain` only uses the Kubernetes Eviction API, so PodDisruptionBudgets are honoured. It skips DaemonSet pods,
mirror (static) pods and pods that already finished or are terminating. A pod whose eviction is refused (HTTP 429,
usually a PodDisruptionBudget) is reported as blocked and left running; the command then exits with an error and
the node stays cordoned. Pods are never deleted directly. Running `start` again on a node that is already marked
keeps the original start time, which makes retrying a blocked drain safe.

## Tenants, catalog, usage and invoices

These commands cover the [GPU as a Service](./GPU_AS_A_SERVICE.md) flow: a provider publishes SKUs, creates tenants and reads metered usage. They talk to the Kubernetes API directly, like the other commands, and need the `gryvia.io/v1alpha1` CRDs installed.

### `gryvia catalog` (alias `skus`)

Lists the GPU SKUs on offer (GryviaGpuSku): GPU type, GPUs per unit, rate per hour, spot discount and whether the SKU is enabled, with a total line. Output formats: `table`, `json`, `yaml`.

```bash
gryvia catalog
gryvia skus -o json
```

### `gryvia tenant`

A tenant is a cluster-scoped GryviaTenant. Its workloads run in the namespace `tenant-<name>`, so the name must be lowercase letters, digits and `-`, at most 56 characters.

```bash
gryvia tenant list
gryvia tenant get acme
gryvia tenant create acme --display-name "Acme Corp" --allowed-sku h100-8x --allowed-sku a100 --max-gpus 16 --isolated true
gryvia tenant delete acme --yes
```

- `list` and `get` accept `-o table|json|yaml`.
- `create` accepts `--display-name`, repeatable `--allowed-sku` (default: all enabled SKUs), `--max-gpus` (maximum concurrent GPUs) and `--isolated true|false` (network isolation from other tenants). It applies the object, so running it again updates the tenant.
- `delete` asks for confirmation unless `--yes` is given. Deleting a tenant can remove its namespace and everything in it.

### `gryvia usage`

Aggregates the metered usage records (GryviaUsageRecord) into GPU hours, cost and the number of distinct jobs per tenant, SKU or day, with a total row.

```bash
gryvia usage
gryvia usage --tenant acme --from 2026-09-01 --to 2026-09-30
gryvia usage --group-by sku -o json
gryvia usage --group-by day -o csv
gryvia usage --network --group-by zone-class
```

- `--tenant` restricts to one tenant.
- `--from` and `--to` filter on each record's start time. A date-only `--to` includes that whole day (UTC). RFC 3339 timestamps are also accepted.
- `--group-by tenant|sku|day` (default `tenant`).
- `-o table|json|yaml|csv`. CSV has a header row and a final `total` row, and cells starting with `=`, `+`, `-` or `@` are prefixed with `'` so spreadsheets do not run them as formulas. When records use different currencies the currency shows as `MIXED`.
- Costs are estimates: job run time times the SKU rate. No invoices or payments are processed.
- `--network` reports measured network egress (GryviaNetworkUsageRecord, written by the opt-in collector feature described in [`docs/network-cost-attribution.md`](https://github.com/zyvorai/gryvia/blob/main/docs/network-cost-attribution.md)) instead of GPU usage: bytes per tenant, `peer-class`, `zone-class` or `day`, plus an estimated cost when a GryviaNetworkRate exists. Only egress is counted; `--group-by sku` is rejected in this mode and `peer-class`/`zone-class` are rejected without it.

### `gryvia invoice`

Builds monthly invoice estimates from the metered usage records (GryviaUsageRecord) in all namespaces, with the same rules as the gateway: one invoice per tenant with usage in the month, one line per SKU.

```bash
gryvia invoice
gryvia invoice --tenant acme --month 2026-09
gryvia invoice --month 2026-09 -o csv
gryvia invoice -o json
```

- `--tenant` restricts to one tenant.
- `--month YYYY-MM` selects the UTC month (default: the current month). A malformed month is a usage error. Records count toward the month of their start time.
- Each invoice has the number `INV-<tenant>-<YYYYMM>`, the period, the currency (`MIXED` when records use different currencies), the status `estimate`, the lines (SKU, GPU type, jobs, GPU hours, rate, amount), the subtotal and the job count. Amounts are rounded to 2 decimals and GPU hours to 4.
- `open` is true while any included record is not final (a job is still running), so the amounts may still change. The table marks this with a warning symbol.
- `-o table|json|yaml|csv`. JSON and YAML are always an array of invoices. CSV has the header `invoice,tenant,period_from,period_to,sku,gpuType,jobs,gpuHours,rate,amount,currency`, one row per line and a `TOTAL` row per invoice; cells starting with `=`, `+`, `-` or `@` are prefixed with `'`.
- A month without usage prints a message and exits 0. This command always computes estimates from job run time. With the opt-in billing ledger, finalized invoices (hash-chained ledger lines, Stripe test mode only) are in the gateway (`GET /api/invoices`) and the dashboard; see [billing ledger](https://github.com/zyvorai/gryvia/blob/main/docs/billing-ledger.md). Gryvia does not process live payments.

## Network Intelligence Commands

:::info Where these commands get their data
Commands whose data lives in the eBPF collectors or behind the gateway (`network flows`, `network graph`,
`gpu memory`, `security alerts`) go through the **API gateway** when `GRYVIA_GATEWAY_URL` (or `--gateway URL`) is set:
they send the API key from `GRYVIA_API_KEY` as a bearer token, verify TLS against the system roots (or the CA bundle in
`GRYVIA_CA_FILE`; `--insecure` turns verification off), and need no kubeconfig. Without a gateway, `network flows`
and `network graph` read the `GryviaServiceGraph` objects of the namespace, `security alerts` shows the counters of
the `GryviaSecurityPolicy` objects, and `gpu memory` only says what it needs (the counters exist only in the
collectors). Everything else reads Kubernetes objects: `gpu rdma` the `GryviaFabricSignal` status, `network policy
suggest` the `GryviaFlowPolicy` objects labelled `gryvia.io/suggested=true` that a `GryviaAutoPolicy` writes.
The objects are filled by the network-intelligence operator (separate chart; see
[Network Intelligence](./NETWORK_INTELLIGENCE.md) and `docs/network-intelligence-sources.md`). Nothing here has been
run against a live gateway or collector fleet; the CLI is tested against an in-process mock gateway.
:::

### Network Overview

```bash
# Network health overview
gryvia network status
```

### Network Tracing

`gryvia network trace` takes the target service as a positional argument. The capture level is `l3`, `l4` or `l7` (default `l7`), and the duration defaults to `2m`.

```bash
# Trace a service for 5 minutes
gryvia network trace training-service --duration 5m

# Trace at L4 in a specific namespace
gryvia network trace training-service --level l4 --trace-namespace ml-research --duration 2m

# Stream results live
gryvia network trace training-service --follow
```

### Flow Analysis

```bash
# Recent flows in a namespace (service-graph edges when no gateway is configured)
gryvia network flows --flow-namespace ml-research

# Flows through the gateway (Netra history when the gateway has Netra, else graph edges), for one service
gryvia network flows --gateway https://gryvia.example.com --service training-service

# Export flows as JSON
gryvia network flows --flow-namespace ml-research --output json
```

Through the gateway the answer covers every namespace over the gateway's fixed window, so `--flow-namespace` and
`--last` do not apply there; `--service` filters client-side. Edges carry no verdict when the source has none: the
verdict shows `-`, never `FORWARDED` by default.

### Service Graph

```bash
# View ASCII service dependency graph
gryvia network graph --graph-namespace ml-research

# JSON output
gryvia network graph --graph-namespace ml-research --format json

# Merged graph of every namespace through the gateway
gryvia network graph --gateway https://gryvia.example.com
```

### Network Policy Management

```bash
# List flow policies
gryvia network policy list

# Show auto-generated policy suggestions (from a GryviaAutoPolicy in that namespace)
gryvia network policy suggest --policy-namespace ml-research

# Approve a suggested policy by name (removes the suggested label; the operator then enforces it)
gryvia network policy apply suggestion-name --policy-namespace ml-research
```

Suggestions are never applied automatically. Applying one means the source pods' egress is limited to what the
allow policy lists, so review the whole set before approving.

### Anomaly Detection

```bash
# View detected network anomalies
gryvia network anomalies

# Filter by severity (critical, high, medium, low)
gryvia network anomalies --severity critical

# Filter by service, as JSON
gryvia network anomalies --service training-service --output json
```

For full documentation on network intelligence features, see [Network Intelligence Guide](NETWORK_INTELLIGENCE.md).

## Security Commands

```bash
# View security alerts
gryvia security alerts

# Filter by severity
gryvia security alerts --severity critical

# Filter by alert type (escape, mining, exfiltration, privesc)
gryvia security alerts --alert-type mining

# Through the gateway when it serves per-event alerts (falls back to the policy counters otherwise)
gryvia security alerts --gateway https://gryvia.example.com

# View security status overview
gryvia security status

# List security policies
gryvia security policy list

# Create a security policy
gryvia security policy create my-policy \
  --namespaces ml-research,ml-platform --rules escape,mining
```

`--auto-block` is deprecated and ignored: the operator never blocks traffic on an alert.

## GPU Network Analysis Commands

`gpu nccl` and `gpu training` read a `GryviaTrainingInsight` object for the job (create one and run the network-intelligence
operator and collector). `gpu memory` reads cumulative host/device transfer counters, summed over all collectors, through
the gateway (`--node` is accepted but not applied). `gpu rdma` reads the `GryviaFabricSignal` status of the jobs in the
namespace (retry ratio, CNP and PFC rates, NCCL p99, score delta); a value that was not measured shows `-`. With
`--node` it also shows that node's `GryviaNodeFabric`. RDMA figures need the collector with `ebpf.nicCounters` and
`ebpf.publishFabricStatus` on RDMA hardware (unverified on hardware).

```bash
# View NCCL communication metrics for a training job
gryvia gpu nccl --job llm-distributed-training

# View GPU memory transfer counters (through the gateway)
gryvia gpu memory

# View RDMA / fabric signals of a job, or of all jobs in the namespace
gryvia gpu rdma --job llm-distributed-training
gryvia gpu rdma --node gpu-node-01

# Training insights for a job
gryvia gpu training --job llm-distributed-training
```

For full documentation on GPU-level network analysis, see [Network Intelligence Guide](NETWORK_INTELLIGENCE.md#gpu-programs).

## Advanced Usage

### Using Different Contexts

```bash
# Use specific Kubernetes context
gryvia --context production list jobs

# Use staging context
gryvia --context staging cluster
```

### Custom Namespaces

```bash
# Use specific namespace
gryvia -n ml-production list jobs

# Override default namespace
gryvia -n experiments submit -f job.yaml
```

### Verbose Logging

```bash
# Enable debug logging
gryvia --verbose submit -f job.yaml

# Verbose output for troubleshooting
gryvia -v list jobs
```

### Output Formats

The CLI supports three output formats: `table`, `json`, and `yaml`. The `--output` flag is validated and any other value produces an error.

```bash
# Pretty table (default)
gryvia list jobs

# JSON (machine-readable)
gryvia list jobs --output json | jq '.items[].status.phase'

# YAML
gryvia list jobs --output yaml
```

## Examples

### Complete Training Workflow

```bash
#!/bin/bash
set -e

# Check quota
echo "Checking quota..."
gryvia quota ml-research --budget

# Submit job
echo "Submitting job..."
gryvia submit -f llm-training.yaml

# Wait a bit for job to start
sleep 10

# Monitor status
echo "Monitoring job..."
gryvia status llm-training

# Follow logs
gryvia logs llm-training --follow
```

### Monitor Multiple Jobs

```bash
#!/bin/bash

JOBS=("job1" "job2" "job3")

for job in "${JOBS[@]}"; do
    echo "=== $job ==="
    gryvia status $job
    echo ""
done
```

### Cost Report Script

```bash
#!/bin/bash

# Generate monthly cost report
echo "Gryvia Monthly Cost Report"
echo "=============================="
echo ""

gryvia cost --period month

echo ""
echo "Per-Team Breakdown:"
echo "-------------------"

for team in ml-research computer-vision nlp; do
    echo ""
    echo "Team: $team"
    gryvia cost $team
done
```

### Cluster Health Dashboard

```bash
#!/bin/bash

while true; do
    clear
    echo "Gryvia Cluster Dashboard"
    echo "============================"
    echo ""

    gryvia cluster

    echo ""
    gryvia health

    sleep 30
done
```

## Troubleshooting

### Connection Issues

```bash
# Verify Kubernetes connection
kubectl cluster-info

# Check current context
kubectl config current-context

# List available contexts
kubectl config get-contexts

# Use specific context
gryvia --context my-cluster list jobs
```

### Permission Errors

```bash
# Check if you can access CRDs
kubectl auth can-i list gryviaaijobs
kubectl auth can-i get gryviaquotas

# View your permissions
kubectl auth can-i --list
```

### CRD Not Found

```bash
# Verify CRDs are installed
kubectl get crds | grep gryvia

# Expected output:
# gryviaaijobs.gryvia.io
# gryviagpunodes.gryvia.io
# gryvianetworks.gryvia.io
# gryviaquotas.gryvia.io
# gryviastorages.gryvia.io
```

### Debug Mode

```bash
# Enable verbose logging
gryvia --verbose list jobs

# Check CLI version
gryvia --version

# View help for specific command
gryvia submit --help
```

## Tips and Best Practices

### 1. Use Watch Mode for Monitoring

```bash
# Watch cluster status
gryvia cluster --watch 5

# Monitor job in terminal
watch -n 5 "gryvia status my-job"
```

### 2. Combine with jq for Filtering

```bash
# Get all running jobs
gryvia list jobs --output json | \
  jq -r '.items[] | select(.status.phase=="Running") | .metadata.name'

# Count jobs by status
gryvia list jobs --output json | \
  jq -r '.items | group_by(.status.phase) | map({status: .[0].status.phase, count: length})'
```

### 3. Create Aliases

```bash
# Add to ~/.bashrc or ~/.zshrc
alias gr="gryvia"
alias grj="gryvia list jobs"
alias grq="gryvia quota"
alias grc="gryvia cluster"

# Usage
grj                    # List jobs
grq my-team            # Check quota
grc --watch 5          # Watch cluster
```

### 4. Validate Before Submitting

```bash
# Always validate YAML
gryvia validate job.yaml

# Then submit if valid
gryvia submit -f job.yaml
```

## Integration with CI/CD

### GitLab CI Example

```yaml
submit_training:
  stage: train
  script:
    - gryvia submit -f jobs/llm-training.yaml --wait
    - gryvia logs llm-training
  only:
    - main
```

### GitHub Actions Example

```yaml
name: Submit Training Job
on:
  push:
    branches: [main]

jobs:
  train:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Install Gryvia CLI
        run: |
          TAG=<release tag>
          curl -LO https://github.com/zyvorai/gryvia/releases/download/${TAG}/gryvia-${TAG}-linux-amd64
          chmod +x gryvia-${TAG}-linux-amd64
          mv gryvia-${TAG}-linux-amd64 gryvia
      - name: Submit Job
        # needs a kubeconfig for a service account allowed to create GryviaAIJob objects
        run: ./gryvia submit -f job.yaml --wait
        env:
          KUBECONFIG: ${{ github.workspace }}/kubeconfig
```

## Not implemented

The CLI has no job template library, multi-cluster mode or job history and analytics commands. Shell completion
(bash, zsh, fish) is implemented (`gryvia completion <shell>`).

## Support

- **Issues**: https://github.com/zyvorai/gryvia/issues
- **Discussions**: https://github.com/zyvorai/gryvia/discussions

## Contributing

See [CONTRIBUTING.md](https://github.com/zyvorai/gryvia/blob/main/CONTRIBUTING.md) for development guidelines.
