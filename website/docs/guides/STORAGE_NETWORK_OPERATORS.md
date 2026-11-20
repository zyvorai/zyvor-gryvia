# Storage and Network Operators

Overview of the optional Gryvia Storage and Network operators.

:::caution Status
Both operators are registered controllers (`GryviaStorage`, `GryviaNetwork`) and are **off by default** in the chart
(`storageOperator.enabled`, `networkOperator.enabled`). They are covered by unit tests against a fake Kubernetes client
only. Nothing here has been verified against real VAST, Weka, DDN, Lustre or Ceph systems, or on InfiniBand, RoCE or
SR-IOV hardware, and the operators do not install the CSI drivers' backends, Multus or the NIC drivers for you. Treat the
backend integrations as unproven. The storage operator also runs an opt-in `GryviaDataset` controller
(`storageOperator.datasets.enabled`) that downloads a dataset into a PVC; see
[Datasets](https://github.com/zyvorai/gryvia/blob/main/docs/datasets.md).
:::

## Overview

Both operators are Kubernetes controllers built with Go and the controller-runtime framework. They generate and apply the Kubernetes objects needed for these infrastructure components:

- **Storage Operator**: Generates CSI driver Deployments/DaemonSets and a StorageClass for a parallel filesystem you already run
- **Network Operator**: Deploys RDMA / SR-IOV device plugins, labels nodes and creates Multus NetworkAttachmentDefinitions

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                     Gryvia Platform                     │
├──────────────────────┬──────────────────────────────────┤
│  Storage Operator    │     Network Operator             │
├──────────────────────┼──────────────────────────────────┤
│                      │                                   │
│  • VAST CSI          │  • RDMA (InfiniBand/RoCE)        │
│  • Weka CSI          │  • SR-IOV                         │
│  • DDN CSI           │  • Multus CNI                     │
│  • Lustre CSI        │  • Device Plugins                 │
│  • Ceph CSI          │  • NAD Auto-Creation              │
│                      │                                   │
└──────────────────────┴──────────────────────────────────┘
           ↓                            ↓
    ┌─────────────┐              ┌────────────┐
    │ PVCs (RWX)  │              │  Pod NICs  │
    │ hardware-   │              │ hardware-  │
    │ dependent   │              │ dependent  │
    └─────────────┘              └────────────┘
```

## Storage Operator

### Components

**Main Controller** (`operators/storage-operator/controllers/gryviastorage_controller.go`)
- Reconciliation loop (60-second requeue when healthy)
- CSI driver install, StorageClass creation and a per-backend health probe of `spec.endpoint`
- Status `phase` (Pending, Configuring, Ready, Degraded, Failed) and conditions `CSIInstalled`, `StorageClassReady`, `Healthy`

**VAST Integration** (`operators/storage-operator/pkg/vast/vast.go`)
- CSI driver manifests: ServiceAccount + RBAC, controller Deployment, node DaemonSet

**Weka Integration** (`operators/storage-operator/pkg/weka/weka.go`)
- CSI driver manifests (quay.io/weka.io/csi-wekafs)
- ServiceAccount + RBAC setup
- Controller Deployment with provisioner and attacher sidecars
- Node DaemonSet with driver registrar
- Endpoint secret management
- Health check via Weka REST API

**DDN Integration** (`operators/storage-operator/pkg/ddn/ddn.go`)
- EXAScaler CSI driver manifests
- ServiceAccount + RBAC setup
- Controller Deployment with provisioner sidecar
- Node DaemonSet with Lustre mount support
- Endpoint secret management
- Health check via DDN REST API

**Lustre Integration** (`operators/storage-operator/pkg/lustre/lustre.go`)
- Lustre CSI driver manifests (kubernetes-sigs/lustre-csi-driver)
- ServiceAccount + RBAC setup
- Controller Deployment with provisioner sidecar
- Node DaemonSet with Lustre mount propagation
- Health check endpoint

**Ceph Integration** (`operators/storage-operator/pkg/ceph/ceph.go`)
- CephFS CSI driver manifests (cephcsi)
- ServiceAccount + RBAC setup
- ConfigMap-based Ceph cluster configuration (generated via `json.Marshal` for safe serialization)
- Controller Deployment with provisioner sidecar
- Node DaemonSet with driver registrar
- Health check endpoint

### Key Features

- **CSI manifests** - generated per backend (unverified against real systems)
- **Backends** - `vast`, `weka`, `ddn`, `lustre`, `ceph` (`spec.backend`)
- **StorageClass** - created from `spec.storageClass` (default name `<backend>-<name>`)
- **Health probe** - an HTTP check of `spec.endpoint` per backend
- **Node selection** - label-based node targeting
- **Credentials** - referenced from a Kubernetes Secret (`spec.credentials`)

### Example Usage

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaStorage
metadata:
  name: vast-production
spec:
  backend: vast
  endpoint: vast-mgmt.example.com
  capacity: 100Ti
  storageClass:
    name: vast-production
  credentials:
    secretName: vast-credentials
    secretNamespace: gryvia-system
```

What the controller is written to create (not verified against a real VAST cluster):
- VAST CSI controller Deployment and node DaemonSet
- A StorageClass (`vast-production` here; without `storageClass.name` it is `<backend>-<name>`)
- Status `phase`, `csiDriverInstalled` and `storageClassCreated`

## Network Operator

### Components

**Main Controller** (`operators/network-operator/controllers/gryvianetwork_controller.go`)
- Network type detection and routing
- Node discovery via `spec.nodeSelector`
- RDMA/SR-IOV configuration orchestration (types `rdma`, `sriov`, `standard`)
- Multus NAD creation in `spec.targetNamespace`; status `phase` and a `Ready` condition, 5-minute requeue

**RDMA Module** (`operators/network-operator/pkg/rdma/rdma.go`)
- RDMA device plugin DaemonSet deployment
- ConfigMap-based device configuration
- Node labeling with RDMA capabilities
- Mellanox/NVIDIA adapter detection (via the NFD label `feature.node.kubernetes.io/pci-15b3.present`)

**SR-IOV Module** (`operators/network-operator/pkg/sriov/sriov.go`)
- SR-IOV CNI installation
- SR-IOV device plugin deployment
- VF configuration management
- Per-network ConfigMap generation

**Multus Integration** (`operators/network-operator/pkg/multus/multus.go`)
- NetworkAttachmentDefinition generation
- CNI config templating per network type
- IPAM integration (Whereabouts, host-local); Multus itself must already be installed

### Key Features

- **RDMA** - InfiniBand and RoCE configuration (`spec.rdma.mode`)
- **SR-IOV** - VF configuration via a DaemonSet (`spec.sriov`)
- **Multus** - NAD creation
- **Device plugins** - RDMA and SR-IOV resource exposure
- **MTU** - `spec.mtu`
- **Node labels** - `gryvia.io/rdma`, `gryvia.io/rdma-mode`, `gryvia.io/sriov` and related annotations

### Example Usage

**RDMA Network:**
```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaNetwork
metadata:
  name: rdma-ib
spec:
  networkType: rdma
  mtu: 9000
  targetNamespace: default
  rdma:
    mode: infiniband
    devices: [mlx5_0, mlx5_1]
    subnet: 10.100.0.0/16
```

What the controller is written to do (needs hardware to verify):
- Deploy the RDMA shared-device plugin DaemonSet
- Label matched nodes `gryvia.io/rdma=true` and `gryvia.io/rdma-mode=infiniband`
- Create a NetworkAttachmentDefinition named `rdma-ib` (requires Multus)
- Expose the `rdma/rdma_shared_device_a` resource to the scheduler

**SR-IOV Network:**
```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaNetwork
metadata:
  name: sriov-net
spec:
  networkType: sriov
  sriov:
    physicalInterface: ens1f0
    numVfs: 32
    resourceName: intel_sriov_netdevice
```

Both network examples select nodes with `spec.nodeSelector`; with none, all nodes match.

What the controller is written to do (needs hardware to verify):
- Deploy the SR-IOV CNI and device plugin DaemonSets
- Configure 32 VFs through a DaemonSet
- Create a NetworkAttachmentDefinition for pod attachment

## File Structure

```
operators/
├── storage-operator/
│   ├── main.go                                 # Controller manager
│   ├── api/v1/gryviastorage_types.go          # CRD types
│   ├── controllers/gryviastorage_controller.go # Reconciler
│   ├── pkg/
│   │   ├── vast/vast.go
│   │   ├── weka/weka.go
│   │   ├── ddn/ddn.go
│   │   ├── lustre/lustre.go
│   │   └── ceph/ceph.go
│   ├── config/
│   │   ├── deployment.yaml                    # Operator deployment
│   │   └── namespace.yaml                     # gryvia-system NS
│   ├── Dockerfile                             # Multi-stage build
│   ├── Makefile                               # Build automation
│   └── README.md                              # Documentation
│
└── network-operator/
    ├── main.go                                # Controller manager
    ├── api/v1/gryvianetwork_types.go         # CRD types
    ├── controllers/gryvianetwork_controller.go # Reconciler
    ├── pkg/
    │   ├── rdma/rdma.go
    │   ├── sriov/sriov.go
    │   └── multus/multus.go
    ├── config/
    │   ├── deployment.yaml                   # Operator deployment
    │   └── namespace.yaml                    # gryvia-system NS
    ├── Dockerfile                            # Multi-stage build
    ├── Makefile                              # Build automation
    └── README.md                             # Documentation

examples/
├── storage/
│   ├── vast-storage-example.yaml             # VAST usage
│   ├── weka-storage-example.yaml             # Weka usage
│   └── ddn-storage-example.yaml              # DDN usage
└── network/
    ├── rdma-network-example.yaml             # RDMA + training pod
    └── sriov-network-example.yaml            # SR-IOV + inference
```

## Deployment

### Install Both Operators

The chart installs the CRDs and both operators when enabled:

```bash
helm upgrade --install gryvia oci://ghcr.io/zyvorai/charts/gryvia \
  --namespace gryvia-system --create-namespace \
  --set storageOperator.enabled=true \
  --set networkOperator.enabled=true

kubectl get pods -n gryvia-system
```

`operators/*/config/deployment.yaml` are plain manifests for the operators alone; the chart is the supported route.

### Configure Storage

```bash
# Create VAST storage backend
kubectl apply -f examples/storage/vast-storage-example.yaml

# Wait for CSI driver
kubectl wait --for=jsonpath='{.status.phase}'=Ready gryviastorage/vast-production --timeout=300s

# Create PVC
kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: training-data
spec:
  accessModes: [ReadWriteMany]
  storageClassName: vast-production
  resources:
    requests:
      storage: 10Ti
EOF
```

### Configure Network

```bash
# Create RDMA network
kubectl apply -f examples/network/rdma-network-example.yaml

# Wait for the Ready condition
kubectl wait --for=condition=Ready gryvianetwork/rdma-infiniband --timeout=300s

# Verify RDMA resources
kubectl get nodes -o json | jq '.items[].status.allocatable | select(.["rdma/rdma_shared_device_a"])'
```

## Integration with AI Workloads

### Distributed Training with RDMA + VAST

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: distributed-training
  annotations:
    k8s.v1.cni.cncf.io/networks: rdma-infiniband
spec:
  containers:
  - name: pytorch
    image: nvcr.io/nvidia/pytorch:24.01-py3
    command: ["torchrun", "--nproc_per_node=8", "train.py"]
    resources:
      limits:
        nvidia.com/gpu: 8
        rdma/rdma_shared_device_a: 1
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: training-data  # VAST storage
```

**Performance:** not measured; depends on your storage system and fabric (for example NFS/NVMe-oF to VAST, RDMA over InfiniBand). No benchmark results are published yet.

## RBAC Permissions

### Storage Operator

- `gryviastorages`: Full CRUD
- `storageclasses`, `csidrivers`: Full CRUD
- `deployments`, `daemonsets`: Full CRUD
- `persistentvolumes`, `persistentvolumeclaims`: Full CRUD
- `clusterroles`, `clusterrolebindings`: Full CRUD

### Network Operator

- `gryvianetworks`: Full CRUD
- `network-attachment-definitions`: Full CRUD
- `daemonsets`: Full CRUD
- `nodes`: Get, List, Watch, Update, Patch
- `configmaps`: Full CRUD

## Testing

### Storage Operator

```bash
# Build and test
cd operators/storage-operator
make test

# Local development
make run

# Integration test
kubectl apply -f examples/storage/vast-storage-example.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Ready gryviastorage/vast-production
```

### Network Operator

```bash
# Build and test
cd operators/network-operator
make test

# Local development
make run

# Verify RDMA
kubectl exec -it <pod-with-rdma> -- ibv_devinfo
```

## Implementation Status

Everything below means "code exists and passes unit tests against a fake client", not "works on real systems".

### Storage Operator

- CSI manifest generation for VAST, Weka, DDN EXAScaler, Lustre and CephFS
- Per-backend HTTP health probe, status phase and conditions, finalizer cleanup of the StorageClass

### Network Operator

- RDMA shared-device plugin DaemonSet and ConfigMap
- SR-IOV CNI, device plugin and VF configuration DaemonSet
- Multus NetworkAttachmentDefinition creation, node labels

## Roadmap

### Storage Operator

- [x] Complete Weka CSI implementation
- [x] Complete DDN CSI implementation
- [x] Complete Lustre CSI implementation
- [x] Complete CephFS CSI implementation
- [ ] Storage quota management
- [ ] Performance metrics (IOPS, bandwidth)
- [ ] Volume snapshots

### Network Operator

- [x] Automated VF enablement via DaemonSet
- [ ] RoCE v2 QoS configuration
- [ ] GPUDirect RDMA validation
- [ ] Network topology-aware scheduling
- [ ] NCCL auto-tuning

## Performance

No measured results are published yet. Throughput, IOPS and latency depend on the storage system, fabric and node hardware; validate with `benchmarks/suite.yaml` on your own cluster.

## Quick Start

```bash
# 1. Install the chart with both operators enabled (see above)

# 2. Declare your infrastructure (edit endpoints and secrets first)
kubectl apply -f examples/storage/vast-storage-example.yaml
kubectl apply -f examples/network/rdma-network-example.yaml   # also contains an example pod
```

Requirements you must meet yourself: a reachable storage system, Multus for NetworkAttachmentDefinitions, RDMA-capable
NICs and drivers (for example NVIDIA OFED) on the nodes, and matching node labels for `spec.nodeSelector`.
