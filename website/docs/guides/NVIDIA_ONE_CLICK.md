# NVIDIA one-click install

Gryvia turns **open** NVIDIA operators on (GPU Operator, Network Operator, NIM Operator).
It does **not** replace them and does **not** copy Run:ai’s proprietary UI.
Unit tests cover Gryvia’s sharing labels and ConfigMap writes; CUDA/NCCL/NVSwitch/IB
paths are unproven on real GPUs in this repository.

## Base (already in install-k3s-gpu.sh)

```bash
sudo ./scripts/install-k3s-gpu.sh server --api-key '<secret>'
```

GPU Operator: driver, toolkit, device plugin, DCGM. Checklist: docs/gpu-validation.md.

## Training fabric

```bash
helm upgrade gryvia oci://ghcr.io/zyvorai/charts/gryvia -n gryvia-system \
  --set nvidia.enabled=true \
  --set nvidia.driver.rdma.enabled=true \
  --set nvidia.gds.enabled=true \
  --set nvidia.mig.strategy=mixed \
  --set nvidia.migManager.enabled=true \
  --set nvidiaNetwork.enabled=true \
  --set nvidiaNetwork.nicClusterPolicy.enabled=true \
  --set nvidiaPlatform.timeSlicing.enabled=true \
  --set nvidia.devicePlugin.config.name=gryvia-time-slicing \
  --set nvidiaPlatform.validate.enabled=true
```

With `nicClusterPolicy.enabled` the chart renders the NicClusterPolicy itself; the Network
Operator chart installs or upgrades its CRDs in a pre-install/pre-upgrade hook first. To manage
the policy by hand instead, leave the flag off and `kubectl apply -f examples/nvidia/nic-cluster-policy.yaml`.

## Inference (NIM)

```bash
helm upgrade gryvia ... \
  --set nvidiaNim.enabled=true \
  --set nvidiaNim.ngcApiKey="$NGC_API_KEY"
kubectl apply -f examples/nvidia/nim-service.yaml
```

`k8s-nim-operator` is pinned to 2.0.1 in Chart.yaml (NGC publishes it without a `v`). The
example NIMService is not a licensed catalog and needs an image pull secret `ngc-secret`
(see the comment in the example). The API key passed with `--set` is stored in the Helm
release Secret; create the `ngc-api` Secret yourself if that is not acceptable.

## Flags

| Flag | What it actually starts |
|---|---|
| `nvidia.enabled` | GPU Operator |
| `nvidia.driver.rdma.enabled` | GPUDirect RDMA in the driver container |
| `nvidia.gds.enabled` | `nvidia-fs` (GDS) |
| `nvidia.mig.strategy` / `migManager.enabled` | MIG manager |
| `nvidiaPlatform.timeSlicing` | ConfigMap `gryvia-time-slicing` |
| `nvidiaNetwork.enabled` | Network Operator |
| `nvidiaNetwork.nicClusterPolicy.enabled` | NicClusterPolicy (DOCA, RDMA shared plugin, Multus) |
| `nvidiaNim.enabled` | NIM Operator |
| `nvidiaPlatform.validate` | post-install `nvidia-smi` Job, not NCCL |

Sharing: enabling `nvidiaPlatform.timeSlicing.enabled` **also** turns on Gryvia’s sharing controller and
device-plugin ConfigMap writes (no triple `--set`). Equivalent explicit flags: `gpuOperator.gpuSharing` and
`gpuOperator.devicePluginConfig`.

- time-slicing → policy writes `gryvia-ts-<maxPodsPerGPU>` (or the chart’s `gryvia-time-slicing` key) and labels nodes; Ready=False `ConfigWriteFailed` if the write fails. Device-plugin advertising that many GPUs is unproven without a GPU node.
- mig → `nvidia.com/mig.config=<first profile>` (must be a mig-parted name)

## Fabric Manager

There is no flag for it. On NVSwitch/HGX nodes the GPU Operator driver container runs Fabric
Manager; look for it inside the `nvidia-driver-daemonset` pod rather than for a new DaemonSet.

## Enable GPU sharing (one switch)

```bash
helm upgrade gryvia ... \
  --set nvidiaPlatform.timeSlicing.enabled=true \
  --set nvidiaPlatform.timeSlicing.replicas=4
```

That enables Gryvia’s sharing controller. Apply a `GryviaGPUSharingPolicy` (see `examples/nvidia/` and
`examples/crds/gryviagpusharing-examples.yaml`). Prefer KAI `--gpu 0.5` for schedule-time packing
([kai-scheduler](https://github.com/zyvorai/zyvor-gryvia/blob/main/docs/kai-scheduler.md)) when you want
Run:ai-like density without NVIDIA’s closed product.

