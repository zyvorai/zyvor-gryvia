# Gryvia Platform Setup Guide

This guide covers everything **after** the Kubernetes cluster exists: installing the operators, dashboard and
gateway, and optionally wiring storage, RDMA and monitoring. To prepare hardware and get a cluster with working GPUs,
start with the [Bare Metal Deployment Guide](./DEPLOYMENT_GUIDE.md) and [GPU nodes](./GPU_NODES.md). For a quick trial
without GPUs see the [Quick Start](../getting-started/quickstart.md). The shorter, canonical install reference is the
[Cluster Setup Guide](../admin-guide/cluster-setup.md).

:::caution What has and has not been run
The chart, operators and gateway are covered by unit tests, chart rendering and a kind job in CI. The GPU path, RDMA and
InfiniBand handling, the storage and network operators against real systems, and the eBPF collector have **not** been
run on real GPU, RDMA or parallel-filesystem hardware. Steps 4 and 5 below are templates for that hardware, not
verified procedures.
:::

## Prerequisites

### Hardware

Gryvia has no fixed hardware requirements and there is no tested sizing guidance. The chart's default requests are
50 to 200 millicores and 64 to 256 MiB per component (`helm/gryvia/values.yaml`). For real GPU work you need NVIDIA
GPU nodes; RDMA (InfiniBand or RoCE) and a shared parallel filesystem are optional and entirely yours to provide.

### Software

- A Kubernetes 1.30+ cluster (the chart sets `kubeVersion: ">=1.30.0"`), `kubectl`, and Helm 3.14+
- NVIDIA driver and container toolkit on GPU nodes: installed by the bundled GPU Operator (`nvidia.enabled=true`) or already on the host
- For RDMA: NVIDIA OFED (or your distribution's equivalent), Multus and, if you use SR-IOV, SR-IOV support. Gryvia does not install any of these.

## Step-by-Step Deployment

### 1. Get a Kubernetes cluster with working GPUs

Gryvia needs a Kubernetes cluster whose GPU nodes can run GPU containers. Pick one path:

| You have | Do this |
|---|---|
| A fresh Ubuntu 22.04/24.04 server with an NVIDIA GPU | `sudo ./scripts/install-k3s-gpu.sh server` installs k3s, Gryvia and NVIDIA's GPU Operator (driver, container toolkit, device plugin, DCGM) in one step. Add more GPU nodes with `agent`. |
| An existing Kubernetes cluster | Install the chart with `--set nvidia.enabled=true` to have NVIDIA's GPU Operator prepare the GPU nodes, or leave it off if your cluster already has the driver, toolkit and device plugin. |

Both paths are described in [GPU nodes](./GPU_NODES.md). What Gryvia does **not** install: Kubernetes itself, a CNI
plugin, Multus, NVIDIA OFED/InfiniBand drivers or Fabric Manager. The Terraform and Ansible directories are experimental
and incomplete (see `ansible/README.md`); do not rely on them.

Check the cluster:

```bash
kubectl get nodes
kubectl get nodes -o json | jq '.items[].status.capacity'   # GPU nodes should list nvidia.com/gpu
```

### 2. Install Gryvia

One chart installs the CRDs, the GPU, AI workload and quota operators, the API gateway and the dashboard. The storage and
network operators are off by default; enable them if you use them (steps 4 and 5). The network-intelligence operator has
its own chart (`helm/network-intelligence`, see [Network Intelligence](./NETWORK_INTELLIGENCE.md)).

```bash
helm install gryvia oci://ghcr.io/zyvorai/charts/gryvia \
  --namespace gryvia-system --create-namespace \
  --set auth.apiKey='a-long-random-secret'

kubectl get pods -n gryvia-system
```

From a checkout, run `helm dependency build helm/gryvia` first and install `./helm/gryvia`.

Expected pods with the defaults (names include a hash suffix; the NVIDIA device plugin and DCGM exporter run on GPU
nodes only):

```
gryvia-gpu-operator-...
gryvia-ai-operator-...
gryvia-quota-operator-...
gryvia-api-gateway-... (x2)
gryvia-ui-... (x2)
```

`gryvia-storage-operator` and `gryvia-network-operator` appear when enabled. Chart options (Ingress, TLS, NodePort,
PodDisruptionBudgets) are in the [chart README](https://github.com/zyvorai/gryvia/tree/main/helm/gryvia) and the
[Cluster Setup Guide](../admin-guide/cluster-setup.md); sign-in and certificates are in
[Authentication and TLS](./AUTH_AND_TLS.md).

There is no supported "manual" install: the repository has no raw manifests for the operators. To get plain YAML,
render the chart with `helm template gryvia ./helm/gryvia -n gryvia-system`.

### 3. Register GPU nodes

Nodes labelled `nvidia.com/gpu.present=true` (by the GPU Operator or your own GPU Feature Discovery) get a
`GryviaGpuNode` automatically (`gpuOperator.autoRegister`, on by default):

```bash
kubectl get gryviagpunodes
gryvia status
```

The reported GPU type, count and other fields depend on what your nodes' labels say; check
`kubectl get gryviagpunodes -o yaml`. See [GPU nodes](./GPU_NODES.md).

### 4. Optional: storage backend

Enable the storage operator, then declare a backend. `spec.backend`, `spec.endpoint` and `spec.capacity` are required;
the credentials Secret must exist first. See [Storage and Network Operators](./STORAGE_NETWORK_OPERATORS.md) for what the
controller does and its limits (never run against a real VAST, Weka, DDN, Lustre or Ceph system).

```bash
helm upgrade gryvia oci://ghcr.io/zyvorai/charts/gryvia -n gryvia-system --reuse-values \
  --set storageOperator.enabled=true

kubectl create secret generic vast-credentials -n gryvia-system \
  --from-literal=username=admin --from-literal=password='<your-password>'
```

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaStorage
metadata:
  name: vast-production
spec:
  backend: vast
  endpoint: vast-mgmt.example.com
  capacity: 500Ti
  storageClass:
    name: vast-production
  credentials:
    secretName: vast-credentials
    secretNamespace: gryvia-system
```

```bash
kubectl wait --for=jsonpath='{.status.phase}'=Ready gryviastorage/vast-production --timeout=300s
kubectl get storageclass vast-production
```

### 5. Optional: RDMA network

RDMA needs InfiniBand or RoCE hardware, its drivers and Multus, all installed by you. Label the nodes the network
should cover, enable the network operator and declare the network:

```bash
kubectl label nodes gpu-worker-01 gpu-worker-02 gryvia.io/rdma=true

helm upgrade gryvia oci://ghcr.io/zyvorai/charts/gryvia -n gryvia-system --reuse-values \
  --set networkOperator.enabled=true
```

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaNetwork
metadata:
  name: rdma-network
spec:
  networkType: rdma
  mtu: 9000
  targetNamespace: ml-training
  nodeSelector:
    gryvia.io/rdma: "true"
  rdma:
    mode: infiniband
    devices: [mlx5_0, mlx5_1]
    subnet: 10.100.0.0/16
    gateway: 10.100.0.1
```

```bash
kubectl wait --for=condition=Ready gryvianetwork/rdma-network --timeout=300s
kubectl get nodes -o json | jq '.items[].status.allocatable' | grep rdma
```

The AI operator adds the pod annotation `k8s.v1.cni.cncf.io/networks: rdma-network` for jobs with `spec.network: rdma`,
so the NetworkAttachmentDefinition must be named `rdma-network` and live in the job's namespace (`targetNamespace`).

### 6. Create team quotas

```bash
kubectl create namespace ml-training
kubectl apply -f examples/quota/team-ml-quota.yaml
kubectl get gryviaquotas
```

The other files in `examples/quota/` cover further teams. Quota semantics, tenants and billing are in
[GPU as a Service](./GPU_AS_A_SERVICE.md).

### 7. Monitoring

The Gryvia chart does not install Prometheus or Grafana. `helm/observability` wraps `kube-prometheus-stack`:

```bash
helm dependency build helm/observability
helm install monitoring ./helm/observability --namespace monitoring --create-namespace \
  --set grafana.adminPassword='<choose one>'
```

Point the gateway at Prometheus with `apiGateway.prometheusUrl` for GPU metrics and cost history. `monitoring/` holds
dashboards, a ServiceMonitor and Prometheus rules as templates; not every metric they query is guaranteed to exist in
your cluster. Service names depend on the release name you choose; list them with `kubectl get svc -n monitoring`.

### 8. Run a test workload

A minimal GPU job (needs a schedulable GPU node):

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: test-training
  namespace: ml-training
spec:
  type: training
  gpus: 1
  gpuType: any
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command: ["python", "-c", "import torch; print(torch.cuda.is_available())"]
```

```bash
kubectl apply -f test-training.yaml
gryvia get job test-training -n ml-training
gryvia logs test-training -n ml-training
```

For a multi-node job see `examples/distributed/multi-gpu-training.yaml`. Relevant `GryviaAIJob` fields, as implemented by
the AI operator: `distributed.{enabled,nodes,gpusPerNode}` (the operator sets `MASTER_ADDR`, `MASTER_PORT`,
`WORLD_SIZE` and NCCL defaults), `storage` (a StorageClass name; the operator creates a PVC `<job>-data` mounted at
`/data`) and `network` (`rdma` or `sriov`, see step 5). Gang scheduling and fair-share are not wired into the running
operator; see the [Scheduling guide](./SCHEDULING.md).

## Verification Checklist

```bash
kubectl get pods -n gryvia-system            # operators, gateway and dashboard Running
kubectl get gryviagpunodes                   # GPU nodes registered
kubectl get crd | grep -c '\.gryvia\.io'     # 49 with this release
gryvia status                                # component banner, workloads, per-node table
./scripts/doctor.sh                          # prerequisite and health checks
```

If you enabled the optional pieces:

```bash
kubectl get gryviastorage                    # PHASE Ready
kubectl get storageclass
kubectl get gryvianetwork                    # then check nodes advertise the RDMA resource
kubectl get gryviaquotas
```

## Production Configuration

### High availability

Leader election is on by default (`ha.leaderElection`), so running more than one replica of a component is safe.
Set replicas per component; the values are `gpuOperator.replicas`, `aiOperator.replicas`, `quotaOperator.replicas`,
`storageOperator.replicas`, `networkOperator.replicas`, `apiGateway.replicas` and `ui.replicas`:

```bash
helm upgrade gryvia ./helm/gryvia -n gryvia-system --reuse-values \
  --set gpuOperator.replicas=2 --set aiOperator.replicas=2 --set quotaOperator.replicas=2
```

There is no `ha.enabled` value (only `ha.leaderElection`); `podDisruptionBudget.enabled` is the related switch. There are no `highAvailability.*` or `networkIntelligenceOperator.*` values; the network-intelligence
operator has its own chart (`operator.replicas`, `ha.*`). No failover test has been published.

### GPU pricing

Prices come from the `GryviaGpuSku` catalog (`gryvia catalog`, the dashboard Catalog page, `POST /api/skus`). With no
SKUs, a built-in default table is used. The old `gpu-pricing` ConfigMap is not read, and there is no `quotaOperator.pricing` value. See [GPU as a Service](./GPU_AS_A_SERVICE.md).

### Backup

Gryvia keeps its state in custom resources; see [Operations](./OPERATIONS.md) for export, restore, upgrade and
uninstall, and `scripts/backup-crs.sh`. A cluster-level tool such as Velero can also back up the namespaces; that is
independent of Gryvia and not tested with it.

## Troubleshooting

### GPU not detected

```bash
nvidia-smi                                       # on the node
kubectl get nodes -o jsonpath='{.items[*].metadata.labels}' | grep nvidia
kubectl describe node <gpu-node>                 # look for nvidia.com/gpu under Capacity
./tools/gpu-diagnostics.sh
```

With `nvidia.enabled=true` look at the pods of the GPU Operator; otherwise at the `nvidia-device-plugin` DaemonSet in
`gryvia-system`. See [GPU nodes](./GPU_NODES.md).

### Storage PVC stuck in Pending

```bash
kubectl get storageclass
kubectl get pods -A | grep csi
kubectl describe pvc <pvc-name>
kubectl describe gryviastorage <name>            # phase and conditions
```

### RDMA not working

```bash
ibstat                                           # on the node
kubectl get gryvianetwork -o yaml                # phase, conditions
kubectl get network-attachment-definitions -A
```

### Job not scheduling

```bash
kubectl get gryviaquota -o yaml
gryvia get job <job-name>
kubectl describe gryviaaijob <job-name>
kubectl logs -n gryvia-system deploy/gryvia-ai-operator
```

## Performance tuning

These are general NVIDIA and NCCL practices, not settings Gryvia applies or has validated:

```bash
nvidia-smi -pm 1          # persistence mode (not enabled automatically)
```

NCCL environment variables go in the job's `spec.env`; the right values depend on your fabric (`NCCL_IB_HCA`,
`NCCL_IB_GID_INDEX`, `NCCL_SOCKET_IFNAME` and so on). The AI operator already sets `NCCL_DEBUG=INFO` for distributed jobs
and `NCCL_IB_DISABLE=0`, `NCCL_NET_GDR_LEVEL=5` when `spec.network` is `rdma`. See `benchmarks/suite.yaml`
for how to measure on your own cluster; no results are published.

## Scaling

- **Add GPU nodes:** join them to the cluster (for k3s, `install-k3s-gpu.sh agent`); labelled nodes register themselves.
- **Increase storage capacity:** edit `spec.capacity` of the `GryviaStorage` and expand PVCs through your CSI driver
  (whether the driver supports expansion depends on the backend).

## Support

- GitHub Issues: https://github.com/zyvorai/gryvia/issues
- Discussions: https://github.com/zyvorai/gryvia/discussions
