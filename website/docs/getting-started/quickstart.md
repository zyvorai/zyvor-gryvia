# Quick Start Guide

Get Gryvia running in minutes. Gryvia is alpha software; the [changelog](https://github.com/zyvorai/gryvia/blob/main/CHANGELOG.md) and the
[threat model](https://github.com/zyvorai/gryvia/blob/main/SECURITY.md) say what is and is not verified.

## Prerequisites

- `kubectl` and `helm` 3.14+
- A Kubernetes cluster 1.30+ (kind, k3s, EKS/GKE/AKS). GPU nodes are only needed to run real GPU work.

## Option 1: Try it on your laptop (no GPUs)

`scripts/kind-demo.sh` creates a [kind](https://kind.sigs.k8s.io) cluster, installs Gryvia and loads
**fictional** demo data (GPU nodes, a quota, a job) so the dashboard is populated:

```bash
git clone https://github.com/zyvorai/gryvia && cd gryvia
./scripts/kind-demo.sh
kubectl -n gryvia-system port-forward svc/gryvia-ui 8443:443
```

Open `https://localhost:8443`, accept the self-signed certificate warning, and sign in as
**`admin`** / **`Admin@321`**. Remove it with `./scripts/kind-demo.sh --delete`.

## Option 2: Install on your cluster with Helm

```bash
helm install gryvia oci://ghcr.io/zyvorai/charts/gryvia \
  --namespace gryvia-system --create-namespace \
  --set auth.apiKey='a-long-random-secret'
kubectl -n gryvia-system port-forward svc/gryvia-ui 8443:443
```

Or run `./scripts/install.sh`, which checks the prerequisites and prints how to open the dashboard.
The chart installs the operators, the API gateway and the dashboard. See the
[chart README](https://github.com/zyvorai/gryvia/tree/main/helm/gryvia) for exposing the dashboard
(NodePort, Ingress), TLS with cert-manager and other options.

### Signing in

Sign in as **`admin`**; the password is the API key you set with `auth.apiKey` (or `GRYVIA_API_KEY`
for the scripts). If you set nothing, the key is the well-known lab default **`Admin@321`**, so always
set your own for anything reachable from an untrusted network. The sign-in page shows the command that reads the key
from its Secret:

```bash
kubectl -n gryvia-system get secret gryvia-api-key -o jsonpath='{.data.GRYVIA_API_KEY}' | base64 -d
```

## Option 3: A fresh GPU server (k3s, drivers and Gryvia in one step)

On Ubuntu 22.04/24.04 with an NVIDIA GPU, as root:

```bash
sudo ./scripts/install-k3s-gpu.sh server
```

This installs k3s, Gryvia and NVIDIA's GPU Operator (driver, container toolkit, device plugin), then prints the
dashboard URL. See [GPU nodes](../guides/GPU_NODES.md) for options, extra nodes and troubleshooting. The script is
covered by dry-run tests and a GPU-less k3s job in CI; it has not been run on real GPU hardware, so follow the
[validation checklist](https://github.com/zyvorai/gryvia/blob/main/docs/gpu-validation.md) and report differences.

## Option 4: Deploy to a remote k3s host over SSH

Deploy to a single-node k3s server over SSH (key authentication; the SSH user needs `sudo`,
`podman`, `helm`, and access to the k3s cluster). The script builds the images on the host and
installs the same chart:

```bash
# Full deployment: rsync, build images on the host, CRDs, operators, API gateway, dashboard
./scripts/deploy-remote.sh <host> <user>        # or: user@host

# Skip the image builds (only when the images were already built for this exact code)
./scripts/deploy-remote.sh <host> <user> --quick

# Re-run only the health and API checks
./scripts/deploy-remote.sh <host> <user> --verify-only
```

It finishes with a smoke test (deployments, dashboard, API authentication, every dashboard endpoint and a
custom-resource round trip) and prints the dashboard URL, `https://<host>:32443`. Use
`GRYVIA_API_KEY='...' ./scripts/deploy-remote.sh <host> <user>` to set your own key; a later run without it keeps the
installed key.

## Access the Dashboard

The dashboard follows your system light or dark setting and includes:
- Cluster overview, job pipeline (pending, running, completed, failed) and GPU utilization, each linking to the page behind it
- Jobs with search, filters, sorting, and per-job pods, logs and events
- Quotas, GPU nodes and costs, with drill-down links to the jobs involved
- GPU as a Service pages: Catalog (GPU SKUs and prices), Usage (metered GPU hours and cost), Tenants and Invoices (monthly estimates, no payments); see [GPU as a Service](../guides/GPU_AS_A_SERVICE.md)
- Network flows and policies, security policies and GPU communication analysis, which say so when no collector is feeding them
- Workspaces, models, inference services, workflows and the auto-tuner, with structured create forms. The ai-operator's ML controllers (on by default) run these objects; nothing on these pages has been verified with real GPU images (see [ML Workflows](../guides/ML_WORKFLOWS.md))

Signed in with the API key you are the provider **admin**: you see cluster-wide pages, but jobs and similar objects only
from one namespace, the gateway's `GRYVIA_JOB_NAMESPACE` (the release namespace, `gryvia-system`, with the chart). OIDC
users are **tenant** users limited to their own tenant namespace; see [Authentication and TLS](../guides/AUTH_AND_TLS.md).

## Register GPU Nodes

With the GPU Operator (Option 3, or `nvidia.enabled=true`) every GPU node is registered automatically from its NVIDIA
feature-discovery labels. Otherwise register nodes yourself; `nodeName` must be a real Kubernetes node:

```yaml
# gpu-node.yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaGpuNode
metadata:
  name: gpu-node-01
spec:
  nodeName: gpu-node-01
  gpuType: A100
  gpuCount: 8
  rdma: true
  interconnect: NVLink
  memoryGB: 80
```

```bash
kubectl apply -f gpu-node.yaml
kubectl get gryviagpunodes
```

The GPU operator copies these fields onto the node as `gryvia.io/*` labels, which job scheduling reads. On a cluster
with fictional or no GPUs (the kind demo) jobs will not actually run.

## Submit Your First Job

```yaml
# training-job.yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: pytorch-test
spec:
  type: training
  gpus: 1
  gpuType: any
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command:
    - python
    - -c
    - "import torch; print(f'CUDA: {torch.cuda.is_available()}')"
```

```bash
kubectl apply -n gryvia-system -f training-job.yaml
kubectl get gryviaaijobs -n gryvia-system
```

`-n gryvia-system` puts the job where the dashboard's admin view looks; see the note above. Or use the CLI:
`gryvia -n gryvia-system submit --file training-job.yaml`. The AI operator checks that enough matching GPU nodes
exist (filtering and scoring node labels and free GPUs; the job stays `Pending` otherwise), then runs the job as a
StatefulSet pinned by node selector to nodes of the requested `gpuType`.

## Set Up Team Quota

```yaml
# team-quota.yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaQuota
metadata:
  name: ml-research
spec:
  team: ml-research
  namespaces:
    - ml-training
  gpuQuota:
    maxGPUs: 16
    maxRunningJobs: 10
    allowedGPUTypes:
      - H100
      - A100
  budget:
    monthlyBudget: 5000.00
    alertThreshold: 80.0
    hardLimit: false
```

```bash
kubectl apply -f team-quota.yaml
kubectl get gryviaquotas
```

## Install CLI (Optional)

```bash
cd cli
cargo build --release
sudo cp target/release/gryvia /usr/local/bin/

# Usage
gryvia cluster
gryvia list jobs
gryvia quota --budget
gryvia cost --period month
```

## Uninstall

```bash
# Via deploy script
./scripts/deploy-remote.sh <host> <user> --uninstall

# Or with Helm (CRDs and your objects are kept on purpose)
helm uninstall gryvia -n gryvia-system

# Remove everything, including all Gryvia objects
kubectl get crd -o name | grep '\.gryvia\.io$' | xargs kubectl delete
kubectl delete namespace gryvia-system
```

## Troubleshooting

### Pods in ImagePullBackOff

Container images need to be built locally and imported into K3s:
```bash
podman build --network=host -t <image> -f <Dockerfile> .
podman save <image> -o /tmp/img.tar
k3s ctr images import /tmp/img.tar
```

### DNS Failures During Build

Use `--network=host` with podman/docker build:
```bash
podman build --network=host -t myimage .
```

### API Returns 401/403

Set the API key on the gateway deployment:
```bash
kubectl set env deployment/gryvia-api-gateway -n gryvia-system \
  GRYVIA_API_KEY=your-key
```

### Web UI Shows "Failed to load cluster stats"

Check that the API gateway pods are running and the nginx proxy is configured:
```bash
kubectl get pods -n gryvia-system -l app=gryvia-api-gateway
kubectl logs -n gryvia-system deployment/gryvia-api-gateway
```

## Next Steps

- [Bare Metal Deployment Guide](../guides/DEPLOYMENT_GUIDE.md) - Full production setup
- [CLI Guide](../guides/CLI_GUIDE.md) - Command-line reference
- [API Reference](../developer-guide/api-reference.md) - REST API docs
- [Examples](https://github.com/zyvorai/gryvia/tree/main/examples/README.md) - Job templates and workflows
