# Gryvia CLI

Command-line interface for managing Gryvia GPU clusters.

> **Status.** The CLI is written in Rust and talks to the Kubernetes API directly through your kubeconfig (it does not use the API gateway). It has 25 top-level commands (see the reference table at the end); `gryvia --help` and `gryvia <command> --help` are authoritative. Every `gryvia ...` command shown in the docs is checked against the built binary by `scripts/check-cli-docs.py`, which verifies that the syntax is accepted, not that the output matches. Commands were exercised against a kind cluster with demo data; behaviour on real GPU clusters is unverified. Sample output blocks below are illustrative.

## Features

- **Job Management**: Submit, list, monitor, and cancel AI training jobs
- **Quota Monitoring**: View team quotas and budget status (read from `GryviaQuota` status)
- **Cluster Overview**: Real-time cluster status and GPU utilization
- **Cost Analysis**: Spending and budget usage from `GryviaQuota` status; `usage` and `invoice` aggregate `GryviaUsageRecord` metering into estimates (not real invoices)
- **Tenants and Catalog**: `gryvia tenant list|get|create|delete` and `gryvia catalog` (alias `skus`)
- **Network, Security and GPU insight**: `gryvia network`, `gryvia security` and `gryvia gpu` read the network-intelligence custom resources; these depend on collectors that are implemented (see `gryvia network`, Flight Recorder, e2e-netintel), so they can be empty (see operators/network-intelligence/README.md)
- **Health Checks**: Monitor GPU, storage, and network health
- **Capacity Report**: `gryvia capacity` shows total/allocated/free GPUs per type, pending demand and shortfall
- **Node Maintenance**: `gryvia maintenance start|end|list` cordons, optionally drains (Eviction API), and tracks nodes
- **Platform Status**: `gryvia status` shows every component, workload and node (like `cilium status`), with `--brief`, `--wait` and `-o json|yaml`
- **Grouped, Colored Help**: commands are listed in groups, every command has examples, and colors honour `--no-color`/`NO_COLOR`
- **Shell Completions**: `gryvia completion bash|zsh|fish|powershell`
- **Consistent Output**: aligned tables with OK/Warning/Error markers

## Installation

### From Source

```bash
cd cli
cargo build --release
sudo cp target/release/gryvia /usr/local/bin/
```

### Using Cargo

```bash
cargo install --path .
```

## Usage

### Platform Status

```bash
# Every component, workload and node at a glance (exit code 1 when a required component fails)
gryvia status

# For scripts: OK or the failing components; wait until healthy
gryvia status --brief
gryvia status --wait --wait-timeout 5m

# Same data as JSON or YAML
gryvia status -o json
```

Sample output:

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

Colors follow the terminal: use `--no-color` or `NO_COLOR=1` to turn them off. Shell completions:
`gryvia completion bash|zsh|fish|powershell`. See the [CLI guide](../website/docs/guides/CLI_GUIDE.md#platform-status).

### Submit a Training Job

```bash
# Submit job from YAML file
gryvia submit -f job.yaml

# Submit and wait for completion
gryvia submit -f job.yaml --wait

# Submit and follow logs
gryvia submit -f job.yaml --logs
```

### Capacity and Maintenance

```bash
# GPU capacity per type (read-only)
gryvia capacity
gryvia capacity --gpu-type H100 --output json

# Take a node out of service, optionally evicting its pods
gryvia maintenance start gpu-node-05 --reason "firmware update" --drain
gryvia maintenance list
gryvia maintenance end gpu-node-05
```

### List Resources

```bash
# List all jobs
gryvia list jobs

# List all quotas
gryvia list quotas

# List GPU nodes
gryvia list nodes

# Output as JSON
gryvia list jobs --output json
```

### Get Detailed Information

```bash
# Get job details
gryvia get job my-training-job

# Get quota details
gryvia get quota team-ml

# Get node details
gryvia get node gpu-worker-01
```

### Monitor Job Status

```bash
# View job status
gryvia status my-training-job

# View and follow logs
gryvia logs my-training-job --follow

# View specific replica logs
gryvia logs my-training-job --replica 0
```

The `logs` and `submit --logs` commands auto-detect the container name from the
pod spec (using the first container). This works with any job container name,
not just the default "trainer".

### Cancel Jobs

Cancelling a job patches its status to "Cancelled" rather than deleting the resource, preserving the job record for auditing.

```bash
# Cancel a single job
gryvia cancel my-job

# Cancel multiple jobs
gryvia cancel job1 job2 job3

# Skip confirmation
gryvia cancel my-job --yes
```

### Cluster Overview

```bash
# View cluster status
gryvia cluster

# Detailed view
gryvia cluster --detailed

# Watch mode (refresh every 5 seconds, interval must be > 0)
gryvia cluster --watch 5
```

### View Quotas

```bash
# List all team quotas
gryvia quota

# View specific team quota
gryvia quota ml-research

# Show budget details
gryvia quota ml-research --budget
```

### Cost Analysis

The `--period` parameter is validated and only accepts `day`, `week`, or `month`.

```bash
# View monthly costs for all teams
gryvia cost

# View costs for specific team
gryvia cost ml-research

# Detailed breakdown
gryvia cost ml-research --detailed
```

### Health Checks

```bash
# Check all components
gryvia health

# Check specific component
gryvia health gpu
gryvia health storage
gryvia health network
```

### Delete Resources

```bash
# Delete a job
gryvia delete job my-training-job

# Delete a quota
gryvia delete quota team-dev

# Delete storage or network resources
gryvia delete storage my-storage
gryvia delete network my-network

# Skip confirmation
gryvia delete job my-job --yes
```

### Validate YAML

Validates the YAML file structure and checks that `apiVersion` and `kind` match known Gryvia types (e.g., `gryvia.io/v1alpha1` / `GryviaAIJob`).

```bash
gryvia validate job.yaml
```

## Global Options

```bash
# Use specific Kubernetes context
gryvia --context production list jobs

# Use specific namespace
gryvia -n ml-training list jobs

# Enable verbose logging
gryvia --verbose submit -f job.yaml
```

## Examples

### Complete Workflow

```bash
# 1. Check cluster status
gryvia cluster

# 2. View available quota
gryvia quota ml-research --budget

# 3. Submit training job
gryvia submit -f llm-training.yaml --wait

# 4. Monitor job status
gryvia status llm-training

# 5. View logs
gryvia logs llm-training --follow

# 6. Check cost impact
gryvia cost ml-research
```

### Team Manager Workflow

```bash
# View team quota status
gryvia quota computer-vision --budget

# List all running jobs
gryvia list jobs | grep Running

# Check monthly spending
gryvia cost computer-vision --period month

# Check if approaching budget
gryvia quota computer-vision
```

## Output Formats

The CLI supports multiple output formats. The `--output` flag is validated and only accepts `table`, `json`, or `yaml`; any other value produces an error.

- **table** (default): Pretty-printed tables with colors
- **json**: Machine-readable JSON output
- **yaml**: YAML format

```bash
gryvia list jobs --output table
gryvia list jobs --output json
gryvia list jobs --output yaml
```

## Configuration

The CLI uses your Kubernetes configuration (`~/.kube/config`) by default.

### Use Different Context

```bash
export KUBECONFIG=/path/to/kubeconfig
gryvia --context staging list jobs
```

### Set Default Namespace

```bash
gryvia -n production list jobs
```

## Building

### Development Build

```bash
cargo build
./target/debug/gryvia --help
```

### Release Build (Optimized)

```bash
cargo build --release
./target/release/gryvia --help
```

### Run Tests

```bash
cargo test
```

## Command Reference

| Command | Description |
|---------|-------------|
| `submit` | Submit a job from YAML file |
| `list` | List resources (jobs, quotas, nodes, experiments) |
| `get` | Get resource details (job, quota, node, storage, network, experiment) |
| `delete` | Delete a resource (supports job, quota, storage, network) |
| `status` | Show platform status, or one job's status |
| `logs` | View job logs |
| `cancel` | Cancel running jobs (patches status to "Cancelled") |
| `cluster` | Show cluster overview |
| `quota` | View team quotas |
| `cost` | Show cost analysis |
| `queue` | View job queue status |
| `create` | Interactive creation wizard (job, quota) |
| `validate` | Validate YAML files |
| `health` | Check cluster health |
| `capacity` | GPU capacity per type: free, pending demand, shortfall |
| `maintenance` | `start`, `end`, `list` nodes under maintenance |
| `tenant` | `list`, `get`, `create`, `delete` tenants |
| `catalog` | List GPU SKUs and hourly rates (alias `skus`) |
| `usage` | Metered GPU usage and estimated cost |
| `invoice` | Monthly invoice estimates from metered usage |
| `network` | Subcommands `trace`, `flows`, `graph`, `policy`, `status`, `anomalies` |
| `security` | Subcommands `alerts`, `status`, `policy` |
| `gpu` | Subcommands `nccl`, `memory`, `rdma`, `training` |
| `version` | Client and platform versions |
| `completion` | Shell completions (bash, zsh, fish, powershell) |

## Technical Notes

- GPU count fields use `u32` (unsigned 32-bit integer) since counts cannot be negative.
- The tokio runtime uses specific feature flags (`rt-multi-thread`, `macros`, `time`, etc.) rather than the `full` feature set, reducing binary size.
- Job names are validated against Kubernetes DNS-1123 subdomain rules: lowercase alphanumeric and hyphens only, max 253 characters, must not start or end with a hyphen.

## Troubleshooting

### Connection Issues

```bash
# Verify Kubernetes connection
kubectl cluster-info

# Check context
kubectl config current-context

# Use specific context
gryvia --context my-cluster list jobs
```

### Permission Errors

Ensure your Kubernetes user has permissions to access Gryvia CRDs:

```bash
kubectl auth can-i list gryviaaijobs
kubectl auth can-i get gryviaquotas
```

### CRD Not Found

Ensure Gryvia CRDs are installed:

```bash
kubectl get crds | grep gryvia
```

## Contributing

See the main [Gryvia repository](https://github.com/zyvorai/gryvia) for contribution guidelines.

## License

Apache 2.0
