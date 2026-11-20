# Gryvia Network Operator

The Network Operator manages high-performance networking for AI workloads in Gryvia. It automates RDMA and SR-IOV configuration for ultra-low latency distributed training.

> **Status.** The `GryviaNetwork` controller is registered and, for `rdma` and `sriov` networks, creates device-plugin DaemonSets, ConfigMaps, node labels/annotations and a Multus NetworkAttachmentDefinition. It does **not** install Kubernetes, Multus, Whereabouts, NVIDIA OFED or GPU drivers, and does not enable IOMMU or SR-IOV firmware settings for you; those are prerequisites (see below). Everything here is unit-tested with a fake client only. It has **not** been verified on real InfiniBand/RoCE or SR-IOV hardware. The NCCL bandwidth figure in the benchmarking section is an expectation, not a measurement.

## Supported Network Types

- **RDMA (InfiniBand/RoCE)** - Remote Direct Memory Access for distributed training
- **SR-IOV** - Single Root I/O Virtualization for high-performance networking
- **Standard** - Traditional bridge networking

## Features

- **RDMA Device Plugin** - Exposes RDMA devices as Kubernetes resources
- **SR-IOV Device Plugin** - Manages SR-IOV VF allocation
- **Multus Integration** - Creates NetworkAttachmentDefinitions automatically
- **Node Auto-Configuration** - Labels and annotates nodes with network capabilities
- **Status** - Reports `configuredNodes` / `totalNodes` and a phase; there is no continuous device-level health monitoring

## Architecture

```
GryviaNetwork CR
       ↓
Network Operator
       ↓
  ┌────┴────┬───────────┐
  ↓         ↓           ↓
RDMA      SR-IOV    Standard
Plugin    Plugin    Bridge
  ↓         ↓           ↓
Multus NetworkAttachmentDefinition
  ↓
Pod Network Interfaces
```

## Installation

```bash
# Apply CRD
kubectl apply -f crds/   # or install the Helm chart, which ships the CRDs

# Deploy operator
kubectl apply -f operators/network-operator/config/

# Verify deployment
kubectl get pods -n gryvia-system -l app=network-operator
```

## Prerequisites

### For RDMA (InfiniBand)

```bash
# Install NVIDIA OFED drivers on all nodes
wget https://content.mellanox.com/ofed/MLNX_OFED-24.01-0.3.3.1/MLNX_OFED_LINUX-24.01-0.3.3.1-ubuntu22.04-x86_64.tgz
tar xzf MLNX_OFED_LINUX-*.tgz
cd MLNX_OFED_LINUX-*
./mlnxofedinstall --upstream-libs --dpdk

# Verify RDMA devices
ibv_devices
```

### For SR-IOV

```bash
# Enable IOMMU in GRUB
echo 'GRUB_CMDLINE_LINUX="intel_iommu=on iommu=pt"' >> /etc/default/grub
update-grub
reboot

# Enable VFs on physical interface
echo 32 > /sys/class/net/ens1f0/device/sriov_numvfs
```

## Usage

### RDMA InfiniBand Network

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaNetwork
metadata:
  name: rdma-infiniband
spec:
  networkType: rdma
  mtu: 9000
  nodeSelector:
    gryvia.io/rdma: "true"
  rdma:
    mode: infiniband
    devices:
      - mlx5_0
      - mlx5_1
    subnet: 10.100.0.0/16
    gateway: 10.100.0.1
```

### SR-IOV Network

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaNetwork
metadata:
  name: sriov-highspeed
spec:
  networkType: sriov
  mtu: 9000
  nodeSelector:
    gryvia.io/sriov: "true"
  sriov:
    physicalInterface: ens1f0
    numVfs: 32
    vfDriver: vfio-pci
    resourceName: intel_sriov_netdevice
```

### Using RDMA in Pods

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
    resources:
      limits:
        nvidia.com/gpu: 8
        rdma/rdma_shared_device_a: 1
```

## Implementation Details

### Reconciliation Behavior

- SR-IOV label cleanup uses `strings.HasPrefix` for accurate prefix matching (fixes an off-by-one bug in earlier versions).
- Node configuration failures are tracked per-node: the reconciler returns an error if **any** target node fails to configure (not just when all fail), reporting the count and names of failed nodes. This ensures partial failures are never silently ignored.
- RDMA device lists are serialized as JSON in ConfigMaps (not Go `fmt %v`), producing valid structured data for consumers.
- Node updates (labels, annotations) are wrapped in `retry.RetryOnConflict` to handle concurrent modifications gracefully.
- The controller no longer uses `Owns()` for DaemonSets or ConfigMaps, since cross-namespace owner references are not supported by Kubernetes. Resources are managed via explicit reconciliation logic instead.

### RDMA Configuration

**Device Plugin Deployment:**
- DaemonSet running on all RDMA-capable nodes
- Uses Mellanox k8s-rdma-shared-dev-plugin
- ConfigMap-based device configuration (device list serialized as JSON)

**Files:**
- `controllers/gryvianetwork_controller.go` - Main reconciliation
- `pkg/rdma/rdma.go` - RDMA device plugin and node config

### SR-IOV Configuration

**Components Deployed:**
- SR-IOV CNI DaemonSet
- SR-IOV Device Plugin DaemonSet
- Per-network ConfigMaps for device selection

**Files:**
- `pkg/sriov/sriov.go` - SR-IOV setup and VF management

### Multus Integration

**NetworkAttachmentDefinition Auto-Creation:**
- Generates CNI config based on network type
- RDMA: Uses macvlan with Whereabouts IPAM
- SR-IOV: Uses SR-IOV CNI with host-local IPAM

**Files:**
- `pkg/multus/multus.go` - NAD creation and config generation

## RDMA Verification

```bash
# Check device plugin
kubectl get pods -n kube-system -l app=rdma-device-plugin

# Verify node resources
kubectl get node <node-name> -o json | jq '.status.allocatable'
# Should show: "rdma/rdma_shared_device_a": "1000"

# Test RDMA in pod
kubectl exec -it <pod> -- ibv_devinfo
```

## SR-IOV Verification

```bash
# Check VFs created
lspci | grep -i virtual

# Verify device plugin
kubectl get pods -n kube-system -l app=sriov-device-plugin

# Check node resources
kubectl get node <node-name> -o json | jq '.status.allocatable'
# Should show: "intel.com/intel_sriov_netdevice": "32"
```

## Performance Benchmarking

### NCCL Bandwidth Test (RDMA)

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: nccl-test
  annotations:
    k8s.v1.cni.cncf.io/networks: rdma-infiniband
spec:
  containers:
  - name: nccl
    image: nvcr.io/nvidia/pytorch:24.01-py3
    command: ["/opt/nccl_tests/build/all_reduce_perf"]
    args: ["-b", "8", "-e", "4G", "-f", "2", "-g", "8"]
    resources:
      limits:
        nvidia.com/gpu: 8
        rdma/rdma_shared_device_a: 1
EOF

# Design expectation on suitable hardware (not measured by this project); compare your own results against NVIDIA's published nccl-tests numbers
```

## Troubleshooting

**RDMA Device Not Found**
```bash
# Check OFED installation
ofed_info -s

# Verify IB interfaces
ip link show

# Check IB status
ibstat
```

**SR-IOV VFs Not Created**
```bash
# Check IOMMU enabled
dmesg | grep -i iommu

# Manually create VFs
echo 32 > /sys/class/net/ens1f0/device/sriov_numvfs

# Verify VFs
ip link show | grep vf
```

**Pod Not Getting RDMA Device**
```bash
# Check device plugin logs
kubectl logs -n kube-system -l app=rdma-device-plugin

# Verify NAD exists
kubectl get network-attachment-definitions

# Check pod events
kubectl describe pod <pod-name>
```

## NCCL Optimization

For optimal distributed training performance:

```bash
# Set in pod env vars
NCCL_IB_HCA=mlx5_0,mlx5_1
NCCL_IB_GID_INDEX=3
NCCL_IB_TC=106
NCCL_IB_DISABLE=0
NCCL_NET_GDR_LEVEL=5
NCCL_SOCKET_IFNAME=eth0
```

## Roadmap

- [ ] RoCE v2 priority flow control automation
- [ ] Network QoS policy enforcement
- [ ] GPUDirect RDMA validation
- [ ] Network topology awareness for job placement
- [ ] Automated NCCL tuning per GPU model
- [ ] Network telemetry collection (latency, bandwidth)
