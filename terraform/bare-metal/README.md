# Terraform bare-metal module: experimental, incomplete

This module only renders inventory and configuration files. It does not provision machines, install Kubernetes or
install GPU drivers, and it feeds the Ansible playbooks in `ansible/`, which are also incomplete
(see `ansible/README.md`).

For a working bare-server install use `scripts/install-k3s-gpu.sh`; for an existing cluster use the Helm chart with
`--set nvidia.enabled=true`. See `website/docs/guides/GPU_NODES.md`.
