# GPU nodes: drivers, CUDA and the device plugin

Gryvia does not install NVIDIA drivers itself. It uses NVIDIA's own [GPU Operator](https://github.com/NVIDIA/gpu-operator)
for that (bundled as an optional sub-chart) and then registers and observes the nodes it prepares.

```
fresh Ubuntu server ──scripts/install-k3s-gpu.sh──▶ k3s ──helm──▶ Gryvia chart
                                                                   │  nvidia.enabled=true
             NVIDIA GPU Operator: node feature discovery, driver container, container toolkit,
                                  device plugin, DCGM exporter
                                                                   │
        node labels nvidia.com/gpu.* + allocatable nvidia.com/gpu ─▶ Gryvia GPU operator creates a
        GryviaGpuNode per GPU node and reports its readiness; per-GPU numbers come from the node's DCGM pod
```

:::caution Not yet validated on real GPUs
This path is covered by tests and a GPU-less CI job only. Follow the [validation checklist](https://github.com/zyvorai/gryvia/blob/main/docs/gpu-validation.md) on a real machine and report differences.
:::

## Path 1: a fresh Ubuntu server

Ubuntu 22.04 or 24.04, x86_64 or arm64, an NVIDIA GPU, as root:

```bash
sudo ./scripts/install-k3s-gpu.sh server --api-key '<secret>'
```

It installs k3s, Helm and the Gryvia chart with `nvidia.enabled=true` and the k3s container-runtime settings the
toolkit needs, then prints the dashboard URL. It detects the GPU, blacklists the `nouveau` driver (and asks for a
reboot once), warns about Secure Boot, and keeps an existing host driver instead of installing a second one. It is safe
to re-run. Useful options: `--no-gpu`, `--gpu`, `--host-driver yes|no`, `--dry-run`, `--chart`, `--set key=value`,
`--uninstall`.

Join another GPU node:

```bash
sudo ./scripts/install-k3s-gpu.sh agent --server https://<server-ip>:6443 --token <node-token>
```

The node is labelled by the GPU Operator and appears in Gryvia by itself.

## Path 2: an existing cluster

```bash
# NVIDIA's driver and toolkit pods are privileged: create the namespace with the Pod Security label first
# (needed on clusters that enforce Pod Security Admission; harmless elsewhere).
kubectl create namespace gryvia-system
kubectl label namespace gryvia-system pod-security.kubernetes.io/enforce=privileged

helm install gryvia oci://ghcr.io/zyvorai/charts/gryvia \
  --namespace gryvia-system \
  --set nvidia.enabled=true
```

Add `--set nvidia.driver.enabled=false` when the drivers are already on the hosts. On k3s also set the toolkit
environment (`nvidia.toolkit.env`, see `helm/gryvia/values.yaml`). The namespace needs the `privileged` label because the
driver container needs it (the chart cannot label a namespace that Helm's `--create-namespace` created). Helm does not upgrade the GPU Operator's CRDs; apply them from its chart on upgrades.

Without `nvidia.enabled`, Gryvia's own device plugin and DCGM exporter run only on nodes labelled
`nvidia.com/gpu.present=true` and assume a host that already has driver, toolkit and the nvidia runtime.

## What Gryvia does with the GPU nodes

- **Registration:** every node labelled `nvidia.com/gpu.present=true` gets a `GryviaGpuNode` (label
  `gryvia.io/auto-registered=true`); it is removed when the label goes away. Objects you create by hand are never
  touched. Disable with `--set gpuOperator.autoRegister=false`.
- **Readiness:** phase `Ready` when the node's allocatable `nvidia.com/gpu` covers the GPU count; otherwise
  `WaitingForDrivers`. Driver and CUDA versions come from the node labels.
- **Health:** temperature, power, memory and utilisation come from the DCGM exporter pod on that node and are left
  empty when it is not reachable (the operator needs to reach pod port 9400).

## What is not automated

Kubernetes installation on bare metal (kubeadm), CNI (Calico), Multus, NVIDIA OFED/InfiniBand drivers, Fabric
Manager, RHEL-family hosts. The Terraform and Ansible directories are experimental and incomplete.

## Troubleshooting

| Symptom | Check |
|---|---|
| Node stays `WaitingForDrivers` | `kubectl get node <n> -o jsonpath='{.status.allocatable}'`; NVIDIA operator pods in `gryvia-system` |
| Driver pod loops | kernel headers for the running kernel; Secure Boot blocks unsigned modules; `nouveau` still loaded (reboot) |
| Pods cannot see the GPU on k3s | toolkit env for k3s set; `nvidia` runtime class present |
| No `GryviaGpuNode` appears | node lacks `nvidia.com/gpu.present=true` (NFD/GFD not running?); `gpuOperator.autoRegister` is on |
| `gryvia status` shows no GPU metrics | DCGM exporter pod on the node; network policy to port 9400 |

## NVIDIA one-click extras

RDMA, GDS, MIG, time-slicing and the Network Operator are opt-in flags. See [NVIDIA one-click](NVIDIA_ONE_CLICK.md).
