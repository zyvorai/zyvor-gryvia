# Gryvia Bare Metal Deployment Guide

Hardware and network preparation for bare metal GPU clusters. **The Terraform and Ansible material in the repository is
experimental and incomplete** (the playbook references roles that do not exist and has never been run in CI); for a
working install use `scripts/install-k3s-gpu.sh` or the Helm chart with `--set nvidia.enabled=true` (see
[GPU nodes](./GPU_NODES.md)). Once the cluster is up, continue with the [Platform Setup Guide](./COMPLETE_DEPLOYMENT_GUIDE.md) to install
Gryvia itself. Operating an install (upgrade, uninstall, backup, troubleshooting) is covered in
[Operations](./OPERATIONS.md).

## Quick Deploy (Single Server)

For a single k3s host you can reach over SSH:

```bash
./scripts/deploy-remote.sh user@10.0.1.5            # sync, build images on the host, install the chart
./scripts/deploy-remote.sh user@10.0.1.5 --quick    # skip the image builds
./scripts/deploy-remote.sh user@10.0.1.5 --verify-only
./scripts/deploy-remote.sh user@10.0.1.5 --uninstall
```

The script uses SSH (there is no password argument), rsyncs the repo, builds the images with podman on the host,
installs the CRDs and the `gryvia` chart, and smoke-tests the dashboard and API. Set `GRYVIA_API_KEY` on a first install (the default
is a well-known development key); a redeploy keeps the key that is installed. See the [Quick Start](../getting-started/quickstart.md) and `scripts/deploy-remote.sh --help`.

## Full Production Deployment

This guide covers **hardware and network preparation** for multi-node GPU clusters. Gryvia does not provision any of it:
you bring the machines, the Kubernetes cluster, the GPU drivers and the RDMA and storage stacks. The Helm chart then
installs the Gryvia components. None of the RDMA or parallel-storage steps below has been validated on real hardware by
the project.

## Prerequisites

### Hardware Requirements

These are planning suggestions, not tested minimums or maximums.

**Control Plane Nodes** (minimum 3 for HA):
- CPU: 8+ cores
- RAM: 16GB+
- Disk: 100GB+ SSD
- Network: 10Gbps+

**GPU Worker Nodes**:
- CPU: 32+ cores
- RAM: 256GB+ (512GB recommended for H100)
- GPUs: NVIDIA H100/A100/L40/V100/T4
- Disk: 500GB+ NVMe SSD
- Network: 100-400Gbps InfiniBand or RoCE
- RDMA-capable NICs (for example Mellanox ConnectX-6 or newer) if you want RDMA

**Storage**:
- Optional: a shared filesystem (VAST, Weka, DDN, Lustre, NFS and so on) and its CSI driver, if you need shared datasets or checkpoints. Gryvia does not install these; see [Storage and network operators](./STORAGE_NETWORK_OPERATORS.md)
- Size throughput to your workload

### Software Requirements

- Ubuntu 22.04 LTS (recommended) or RHEL 8+
- A recent kernel (5.15+ for typical GPU stacks; 6.6+ if you want the optional eBPF `tcx` programs)
- SSH access to all nodes
- Internet connectivity for package and image downloads
- Kubernetes 1.30+, `kubectl` and Helm 3.14+ (required by the Gryvia chart)

## Step 1: Prepare Infrastructure

### 1.1 Network Setup

**Management Network** (1GbE):
- Control plane: 10.0.1.100-10.0.1.110
- GPU nodes: 10.0.1.200-10.0.1.250

**RDMA Network** (100-400GbE InfiniBand/RoCE):
- GPU nodes: 192.168.100.10-192.168.100.250
- Storage: 192.168.100.1-192.168.100.9

### 1.2 Storage Setup

If you use an NFS-over-RDMA share, mount it on all GPU nodes (illustrative; use your vendor's mount options):
```bash
# Example for VAST
mkdir -p /mnt/vast
mount -t nfs -o rdma,port=20049 vast-vip:/gryvia /mnt/vast
```

Add to `/etc/fstab`:
```
vast-vip:/gryvia /mnt/vast nfs rdma,port=20049,hard,nointr 0 0
```

## Step 2: Install Kubernetes and the GPU stack

Use one of the paths that exist and are documented:

| You have | Use |
|---|---|
| One fresh Ubuntu server with an NVIDIA GPU | `sudo ./scripts/install-k3s-gpu.sh server` (k3s, Gryvia and NVIDIA's GPU Operator); join more with `agent` |
| An existing Kubernetes cluster | `helm install gryvia ... --set nvidia.enabled=true` |

See [GPU nodes](./GPU_NODES.md). RDMA (Multus, SR-IOV, MOFED or the NVIDIA Network Operator) and the multi-node
Kubernetes bring-up (kubeadm or your distribution's installer) are yours to set up; the repository does not automate them.

### Experimental: Terraform and Ansible

`terraform/bare-metal` and `ansible/` are experimental and **incomplete**. The Terraform module only renders files
(`generated/inventory.ini`, `kubeadm-config.yaml`, per-node GPU configs, `rdma-config.yaml`, `storage-config.yaml`,
`deploy.sh`); it provisions nothing. `ansible/playbooks/site.yaml` references roles that do not exist
(`kernel-tuning`, `container-runtime`, `kubernetes-control-plane`, `kubernetes-worker`, `gryvia-configure`) and has never
been run in CI; the roles that do exist (`common`, `nvidia-drivers`, `rdma`, `gpu-optimization`, `gryvia-install`) are
untested. Read `terraform/bare-metal/README.md` and `ansible/README.md` before touching them. They do not install
Kubernetes, Calico, Multus or GPU drivers end to end.

## Step 3: Install Gryvia

Once `kubectl` works against the cluster:

```bash
helm install gryvia oci://ghcr.io/zyvorai/charts/gryvia \
  --namespace gryvia-system --create-namespace \
  --set auth.apiKey='a-long-random-secret'
```

The full walk-through, configuration table and verification are in the [Platform Setup Guide](./COMPLETE_DEPLOYMENT_GUIDE.md)
and the [Cluster Setup Guide](../admin-guide/cluster-setup.md).

## Step 4: Verify Installation

### 4.1 Check the cluster

```bash
kubectl get nodes
kubectl get pods -n gryvia-system
kubectl get crd | grep -c '\.gryvia\.io'    # 49 with this release
./scripts/doctor.sh
```

With the default values you should see the `gryvia-gpu-operator`, `gryvia-ai-operator`, `gryvia-quota-operator`,
`gryvia-api-gateway` and `gryvia-ui` deployments, plus the NVIDIA device plugin and DCGM exporter DaemonSets on nodes
labelled `nvidia.com/gpu.present=true` (or the GPU Operator's pods with `nvidia.enabled=true`).

### 4.2 Verify GPU nodes

```bash
kubectl get gryviagpunodes
gryvia status
```

Nodes labelled `nvidia.com/gpu.present=true` (by the GPU Operator or your own GPU Feature Discovery) are registered
automatically. Column names and values depend on what your nodes report; check `kubectl get gryviagpunodes -o yaml`.

### 4.3 Test GPU access

```bash
kubectl apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: gpu-test
spec:
  containers:
  - name: cuda
    image: nvidia/cuda:12.2.0-base-ubuntu22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 1
  restartPolicy: Never
YAML

kubectl logs gpu-test
```

## Step 5: Configure Monitoring

The Gryvia chart does not install Prometheus or Grafana. `helm/observability` wraps `kube-prometheus-stack`; set the
Grafana admin password explicitly (there is no default):

```bash
helm dependency build helm/observability
helm install gryvia-observability ./helm/observability -n gryvia-system \
  --set grafana.adminPassword='<choose one>'
```

`monitoring/` holds dashboards, a ServiceMonitor and Prometheus rules as templates; `manifests/monitoring/` has a GPU
alert rule file and a dashboard JSON. Not every metric they query is guaranteed to exist in your cluster. Point the
gateway at Prometheus with `apiGateway.prometheusUrl` for GPU metrics and cost history.

## Step 6: Submit Test Job

```bash
kubectl apply -f examples/training/simple-pytorch-training.yaml
kubectl get gryviaaijob
gryvia logs pytorch-simple-training
```

## Troubleshooting

### NVIDIA Drivers Not Loading

```bash
# On GPU node
sudo modprobe nvidia
sudo nvidia-smi

# Check kernel module
lsmod | grep nvidia
```

### RDMA Not Working

```bash
# Check InfiniBand status
ibstat
ibv_devices

# Test RDMA bandwidth
ib_write_bw
```

### Pods Not Scheduling

```bash
kubectl describe gryviaaijob <job-name>   # or: gryvia get job <job-name>
kubectl get events --sort-by='.lastTimestamp'
```

### GPU Not Detected

```bash
kubectl describe node <gpu-node-name>
# Look for nvidia.com/gpu resource
```

## Next Steps

1. Configure team quotas: `kubectl apply -f examples/quota/` (see [GPU as a Service](./GPU_AS_A_SERVICE.md))
2. Declare storage: `examples/storage/` holds `GryviaStorage` examples; they need the optional storage operator
   (`storageOperator.enabled=true`) and your own CSI driver
3. Try a multi-GPU job: `kubectl apply -f examples/distributed/multi-gpu-training.yaml`
4. Alerts: sample rules in `manifests/monitoring/alerts/gpu-alerts.yaml`
5. Day-2 operations: [Operations](./OPERATIONS.md)

## Production Checklist

A generic list, not something Gryvia checks for you:

- [ ] Highly available control plane (your Kubernetes distribution)
- [ ] Regular etcd backups, and a backup of `gryvia.io` resources (see [Operations](./OPERATIONS.md))
- [ ] Monitoring and alerting
- [ ] Network policies (`networkPolicy.enabled` in the chart restricts access to the gateway)
- [ ] A non-default API key, OIDC for tenants and a trusted certificate ([Authentication and TLS](./AUTH_AND_TLS.md))
- [ ] Audit logging and log aggregation (cluster level)
- [ ] A tested disaster recovery procedure
- [ ] Runbooks

## Support

For issues and questions:
- GitHub Issues: https://github.com/zyvorai/gryvia/issues
- Discussions: https://github.com/zyvorai/gryvia/discussions
