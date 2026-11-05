# NVIDIA one-click install

Gryvia turns NVIDIA operators on. It does not replace them. None of this path
has been proven on real GPUs, NVSwitch or InfiniBand in this repository.

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

Sharing policy labels the GPU Operator already watches. The controller that writes them is **opt-in**: set
`gpuOperator.gpuSharing=true` (flag `--enable-gpu-sharing`), otherwise a `GryviaGPUSharingPolicy` does nothing:

- time-slicing → `nvidia.com/device-plugin.config=gryvia-time-slicing`. That key's replica count is the chart's `nvidiaPlatform.timeSlicing.replicas`, not the policy's `maxPodsPerGPU`. Set `gpuOperator.devicePluginConfig=true` and the policy writes its own key `gryvia-ts-<maxPodsPerGPU>` into the ConfigMap and labels nodes with it (Ready=False `ConfigWriteFailed` if the write fails). Whether the device plugin then advertises that many GPUs is not verified without a GPU node.
- mig → `nvidia.com/mig.config=<first profile>` (must be a mig-parted name)

## Fabric Manager

There is no flag for it. On NVSwitch/HGX nodes the GPU Operator driver container runs Fabric
Manager; look for it inside the `nvidia-driver-daemonset` pod rather than for a new DaemonSet.

## Enable GPU sharing (Run:ai-style utilization path)

Sharing policies are inert until you turn the controller on. Gryvia does not ship NVIDIA’s proprietary
sharing UI; this is the Gryvia path that drives the same device-plugin labels the GPU Operator already watches:

```bash
helm upgrade gryvia ... \
  --set gpuOperator.gpuSharing=true \
  --set gpuOperator.devicePluginConfig=true \
  --set nvidiaPlatform.timeSlicing.enabled=true \
  --set nvidiaPlatform.timeSlicing.replicas=4
```

Then apply a `GryviaGPUSharingPolicy` (time-slicing or MIG). With `devicePluginConfig=true` the
policy writes `gryvia-ts-<maxPodsPerGPU>` into the device-plugin ConfigMap. Not verified on GPUs.

