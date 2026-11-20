# Ansible playbooks

## Maintained: RKE2 for the Sovereign AI OS

`playbooks/sovereign-aios-rke2.yaml` with the role `roles/rke2_hardened`: RKE2 with the CIS profile, FIPS checks,
encrypted Secrets, Pod Security, audit logs and a Harbor registry mirror, online or air-gapped. Linted and its templates
rendered in CI (`tests/rke2_hardened_templates.yaml`); not yet run against real hosts. See
[Sovereign AI OS](../docs/sovereign-aios.md#hardening) and `inventory/sovereign-aios.example.ini`.

## Experimental, incomplete

These playbooks were a starting point for bare-metal provisioning. They are **not** a working one-command install
and have never been run in CI.

`playbooks/site.yaml` references roles that do not exist in this repository:
`kernel-tuning`, `container-runtime`, `kubernetes-control-plane`, `kubernetes-worker`, `gryvia-configure`.
The roles that do exist (`common`, `nvidia-drivers`, `rdma`, `gpu-optimization`, `gryvia-install`) are untested;
`nvidia-drivers` targets Ubuntu 22.04 only, does not handle reboots and does not make `nvidia` the default
container runtime. There is no Fabric Manager or OFED handling.

## What to use instead

| You have | Use |
|---|---|
| A fresh Ubuntu server with an NVIDIA GPU | `sudo ./scripts/install-k3s-gpu.sh server` (k3s, Gryvia and NVIDIA's GPU Operator) |
| An existing Kubernetes cluster | `helm install gryvia ... --set nvidia.enabled=true` |

See `website/docs/guides/GPU_NODES.md`.
