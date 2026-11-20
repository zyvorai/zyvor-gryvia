# Cluster Setup Guide

How to install and configure Gryvia on a Kubernetes cluster. Everything here is installed by the Helm chart
(`helm/gryvia`) unless it says otherwise. Gryvia is alpha software: read the [threat model](https://github.com/zyvorai/gryvia/blob/main/SECURITY.md)
before exposing an install beyond a lab.

:::caution What has and has not been run
The chart, the operators and the gateway are covered by unit tests, chart rendering and a kind job in CI that installs the chart with demo data and reads it back through the API and CLI (it does not run a GPU workload). The
GPU path (NVIDIA GPU Operator sub-chart, `install-k3s-gpu.sh`, auto-registration), RDMA/InfiniBand handling and the eBPF
collector have **not** been run on real GPU or RDMA hardware. See [GPU validation](https://github.com/zyvorai/gryvia/blob/main/docs/gpu-validation.md).
:::

## Prerequisites

- Kubernetes 1.30 or newer (the chart sets `kubeVersion: ">=1.30.0"`), `kubectl` and Helm 3.14 or newer.
- For GPU work: nodes with NVIDIA GPUs. Either let the bundled NVIDIA GPU Operator install the driver and container
  toolkit (`nvidia.enabled=true`) or have a working driver and the NVIDIA container runtime already on the hosts. See
  [GPU nodes](../guides/GPU_NODES.md).
- Optional and **not installed by Gryvia**: Multus and SR-IOV for RDMA networking, a parallel filesystem and its CSI
  driver, cert-manager for trusted certificates, Prometheus for cost and metric history, an OIDC identity provider.
- Sizing: there is no tested sizing guidance yet. The defaults request 50 to 200 millicores and 64 to 256 MiB per
  component (`helm/gryvia/values.yaml`); size real clusters yourself.

## Installation methods

### Helm (recommended)

```bash
helm install gryvia oci://ghcr.io/zyvorai/charts/gryvia \
  --namespace gryvia-system --create-namespace \
  --set auth.apiKey='a-long-random-secret'
```

The chart is also published as a classic repository:

```bash
helm repo add gryvia https://zyvorai.github.io/zyvor-gryvia/charts && helm repo update
helm install gryvia gryvia/gryvia --namespace gryvia-system --create-namespace
```

From a checkout, run `helm dependency build helm/gryvia` first (the GPU Operator sub-chart) and install `./helm/gryvia`.
The chart installs the CRDs, the GPU, AI workload and quota operators, the API gateway and the dashboard. The storage
and network operators are off by default (`storageOperator.enabled`, `networkOperator.enabled`). The sixth operator,
network intelligence, has its own chart (`helm/network-intelligence`, see [Network Intelligence](../guides/NETWORK_INTELLIGENCE.md)).

### A fresh GPU server

```bash
sudo ./scripts/install-k3s-gpu.sh server
```

Installs k3s, Gryvia and the NVIDIA GPU Operator. See [GPU nodes](../guides/GPU_NODES.md).

### Remote k3s host over SSH

`./scripts/deploy-remote.sh <host> <user>` builds the images on the host and installs the same chart. See the
[Quick Start](../getting-started/quickstart.md).

### Manual install (not supported)

There are no raw manifests for the operators. `manifests/deploy/` holds only the API gateway and dashboard
Deployments used by an older flow. To get plain YAML, render the chart: `helm template gryvia ./helm/gryvia -n gryvia-system`.

### Terraform and Ansible (experimental)

`terraform/bare-metal` and `ansible/` are experimental and incomplete. They do not install Kubernetes, Calico, Multus
or GPU drivers end to end; do not rely on them for a production setup.

## Configuration

Every option is in `helm/gryvia/values.yaml`; the chart [README](https://github.com/zyvorai/gryvia/tree/main/helm/gryvia)
lists the common ones. Highlights:

| Need | Values |
|---|---|
| Operator replicas and leader election | `<component>.replicas`, `ha.leaderElection` (leader election is on by default; run more than one replica only with it on) |
| Enable storage or network operator | `storageOperator.enabled`, `networkOperator.enabled` |
| Dashboard exposure | `ui.service.type` / `ui.service.nodePort`, `ui.ingress.*` |
| TLS | `tls.mode` = `selfSigned` (default), `certManager` or `existingSecret`; see [Authentication and TLS](../guides/AUTH_AND_TLS.md) |
| API key | `auth.apiKey`, `auth.existingSecret` |
| OIDC roles and tenants | `apiGateway.oidc.adminGroups`, `apiGateway.oidc.legacyNamespaces`; see [GPU as a Service](../guides/GPU_AS_A_SERVICE.md) |
| Prometheus for cost/metric history | `apiGateway.prometheusUrl` |
| Netra as the flow source | `apiGateway.netra.url`, `apiGateway.netra.tokenSecret`, `apiGateway.netra.insecureTLS` |
| Flight Recorder cluster view | `apiGateway.flightTokenSecret`, `apiGateway.flightCollectorNamespace` |
| Hardening | `podDisruptionBudget.enabled`, `networkPolicy.enabled`, `webhook.failurePolicy` |
| GPU components | `nvidia.enabled`, `nvidia.driver.enabled`, `nvidiaDevicePlugin.*`, `dcgmExporter.*`, `gpuOperator.autoRegister` |

`monitoring.*` exists in `values.yaml` but no template reads it today (`ha.enabled`, `crds.install`/`crds.keep`, `gpuOperator.healthCheck.*`, `quotaOperator.pricing.*`, `labels` and `annotations` were removed for the same reason); enable metrics
scraping and dashboards through the [observability chart](https://github.com/zyvorai/gryvia/tree/main/helm/observability)
and `monitoring/` instead. The chart does not deploy PostgreSQL or any other database: Gryvia keeps its state in
custom resources.

### Storage and network

Storage backends are declared with `GryviaStorage` and network fabrics with `GryviaNetwork`; both are reconciled by
the optional storage and network operators. Provisioning depends on your CSI driver, filesystem and fabric, which
Gryvia does not install. See [Storage and network operators](../guides/STORAGE_NETWORK_OPERATORS.md). None of this has
been validated against real VAST, Weka, DDN, Lustre, CephFS, InfiniBand or RoCE hardware.

### GPU prices

Prices come from the `GryviaGpuSku` catalog (`gryvia catalog`, the dashboard Catalog page, `POST /api/skus`). With no
SKUs, a built-in default table is used. The old `gpu-pricing` ConfigMap is not read.

## GPU nodes

With the NVIDIA GPU Operator (or your own GPU Feature Discovery) the nodes carry `nvidia.com/gpu.*` labels and the
Gryvia GPU operator creates a `GryviaGpuNode` for each one (`gpuOperator.autoRegister`, on by default). Check:

```bash
kubectl get gryviagpunodes
gryvia status
```

You can also create a `GryviaGpuNode` by hand (objects you create are never touched by auto-registration):

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaGpuNode
metadata:
  name: gpu-node-01
spec:
  nodeName: gpu-node-01     # must be a real Kubernetes node
  gpuType: A100
  gpuCount: 8
  memoryGB: 80
  rdma: true
  interconnect: NVLink
```

The GPU operator copies each `GryviaGpuNode` onto its Kubernetes Node as labels (`gryvia.io/gpu` = `gpuType`,
`gryvia.io/gpu-count`, `gryvia.io/rdma`, `gryvia.io/sriov`, `gryvia.io/interconnect`), and the AI operator's node
selection reads those labels: a job with `spec.gpuType: H100` only lands on nodes whose `gryvia.io/gpu` label is
`H100`. Auto-registration normalizes NVIDIA's product name to a short type (`H100`, `A100-80G`, `A100-40G`, `L40`,
`A10`, `T4`, `V100`) and leaves unknown products unchanged, so check `kubectl get gryviagpunodes` for the exact
string to use in jobs and SKUs. The operator's node selection is a filter, score and select over those labels and the nodes' free
`nvidia.com/gpu`, recorded in the job status; the job's pods are then pinned by node selector and placed by the default
Kubernetes scheduler. Gang admission, per-tenant queues and priority preemption are available only through the opt-in Kueue integration
(`kueue.enabled`, `aiOperator.kueueIntegration`, `quotaOperator.kueueIntegration`; see [Kueue integration](https://github.com/zyvorai/gryvia/blob/main/docs/kueue-integration.md) and the
[Scheduling guide](../guides/SCHEDULING.md)), unit-tested but not yet verified on a real cluster.

Tainting GPU nodes and other scheduling policy are your cluster's business; Gryvia does not add taints.

## Monitoring

- The Gryvia chart ships an NVIDIA device plugin and DCGM exporter that only run on nodes labelled
  `nvidia.com/gpu.present=true`; with `nvidia.enabled=true` the GPU Operator's own components are used instead.
- `helm/observability` wraps `kube-prometheus-stack`; `monitoring/` has Grafana dashboards, a ServiceMonitor and
  Prometheus rules as templates. Not every metric they query is guaranteed to exist in your cluster.
- The gateway reads Prometheus (`apiGateway.prometheusUrl`) for GPU metrics and cost history; without it those pages
  say so.

## Security

- Change the API key, decide how tenants sign in (OIDC), and use a trusted certificate: [Authentication and TLS](../guides/AUTH_AND_TLS.md).
- Enable `networkPolicy.enabled` on shared clusters so only the dashboard pods reach the gateway.
- The operators and gateway run with broad RBAC on `gryvia.io` resources, nodes and pods. The eBPF collector, when you
  enable it, is a privileged, hostNetwork DaemonSet; most of its HTTP endpoints are unauthenticated. See
  [SECURITY.md](https://github.com/zyvorai/gryvia/blob/main/SECURITY.md).
- `manifests/security/` contains sample policies; the PodSecurityPolicy file targets an API removed in Kubernetes 1.25
  and is not applicable on supported versions.

## Verification

```bash
kubectl -n gryvia-system get pods
```

With defaults you should see the `gryvia-gpu-operator`, `gryvia-ai-operator`, `gryvia-quota-operator`,
`gryvia-api-gateway` and `gryvia-ui` deployments (plus `gryvia-storage-operator` and `gryvia-network-operator` when
enabled, and the NVIDIA operator pods with `nvidia.enabled=true`).

```bash
kubectl get crd | grep -c '\.gryvia\.io'     # 49 with this release
gryvia status                                # component banner, workloads, per-node table
./scripts/doctor.sh                          # prerequisite and health checks
```

Submit a test job (needs a schedulable GPU node):

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: gpu-test
spec:
  type: training
  gpus: 1
  gpuType: any
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command: ["python", "-c", "import torch; print(torch.cuda.is_available())"]
```

```bash
kubectl apply -f gpu-test.yaml
gryvia get job gpu-test
gryvia logs gpu-test
```

## Backup and upgrade

State lives in custom resources: see [Operations](../guides/OPERATIONS.md) for backup, restore, upgrade and uninstall.
`scripts/backup-crs.sh` exports and restores Gryvia resources.

## Maintenance

```bash
gryvia maintenance list                 # nodes marked for maintenance
gryvia maintenance start <node> --reason "driver update"          # cordon and mark
gryvia maintenance start <node> --reason "driver update" --drain  # also evict pods through the Eviction API
gryvia maintenance end <node>           # uncordon
```

To update GPU drivers with the GPU Operator, change `nvidia.driver.version` in the chart values; on hosts with
their own driver, drain the node, update, reboot and uncordon as usual.

## Troubleshooting

| Symptom | Check |
|---|---|
| Operator does not start | `kubectl -n gryvia-system logs deploy/gryvia-<operator>`, `kubectl -n gryvia-system get events --sort-by=.lastTimestamp` |
| No `GryviaGpuNode` appears | Nodes lack `nvidia.com/gpu.present=true`; `gpuOperator.autoRegister` is off |
| Node stays `WaitingForDrivers` | Allocatable `nvidia.com/gpu` is 0; check the NVIDIA operator pods and the device plugin |
| A job stays `Pending` | `kubectl describe gryviaaijob <name>` shows the scheduling reason (GPU type, capacity, quota) |
| The dashboard shows no jobs you created | The admin sees one namespace (`GRYVIA_JOB_NAMESPACE`, the release namespace by default); create jobs there or use `-n` |

More in [GPU nodes](../guides/GPU_NODES.md) and [Operations](../guides/OPERATIONS.md).

## Support

- Issues: https://github.com/zyvorai/gryvia/issues
