---
sidebar_position: 0
slug: /intro
---

# Gryvia Documentation

Documentation for Gryvia, a Kubernetes-native GPU platform. Gryvia is **alpha** software (`gryvia.io/v1alpha1`): pages say what
is verified, what is experimental and what is only a CRD or design so far. Start with the [status section of the README](https://github.com/zyvorai/gryvia#status-and-security).

## Documentation Structure

### Getting Started
- [Quick Start Guide](getting-started/quickstart.md) - kind demo without GPUs, Helm install, k3s + GPU bootstrap, remote k3s

### User Guides
- [Submitting Jobs](user-guide/jobs.md) - How to submit and manage AI workloads
- [GPU as a Service](guides/GPU_AS_A_SERVICE.md) - Tenants, GPU SKU catalog, usage metering and estimate invoices

### Administrator Guides
- [Cluster Setup](admin-guide/cluster-setup.md) - Setting up a Gryvia cluster
- [GPU Nodes](guides/GPU_NODES.md) - Drivers, CUDA and the device plugin through the bundled NVIDIA GPU Operator
- [Authentication and TLS](guides/AUTH_AND_TLS.md) - API key, sessions, OIDC roles and tenants, certificates
- [Deployment Guide](guides/COMPLETE_DEPLOYMENT_GUIDE.md) - Complete deployment instructions
- [Operations](guides/OPERATIONS.md) - Upgrade, uninstall, backup, troubleshooting
- [Operational Playbooks](guides/OPERATIONAL_PLAYBOOKS.md) - Day-2 operations

### Developer Guides
- [API Reference](developer-guide/api-reference.md) - Every API gateway route and who may call it
- [CRD Reference](reference/crds.md) - The 49 custom resources and which ones have a controller
- [CLI Guide](guides/CLI_GUIDE.md) - Command-line interface usage
- [Storage & Network Operators](guides/STORAGE_NETWORK_OPERATORS.md) - Operator internals

### Feature Guides
- [ML Workflows](guides/ML_WORKFLOWS.md) - Hyperparameter tuning, DAG pipelines, model registry, inference serving, workspaces
- [Network Intelligence](guides/NETWORK_INTELLIGENCE.md) - eBPF-powered observability, security, and traffic analysis
- [Scheduling](guides/SCHEDULING.md) - What the scheduler does today and the design for gang scheduling, fair-share queues and elastic training
- [Native queue scheduler](guides/NATIVE_SCHEDULER.md) - Opt-in queues, gang admission, priority preemption and fractional GPU requests (alpha)

### Reference
- [FAQ](guides/FAQ.md) - Frequently asked questions
- [Roadmap](guides/ROADMAP.md) - Feature roadmap
- [Advanced Features](guides/ADVANCED_FEATURES.md) - Advanced platform features
- [Integrations](guides/INTEGRATIONS.md) - Third-party integrations
- [Budget Management](https://github.com/zyvorai/gryvia/blob/main/features/budget-management.md) - Budget design (see its status note)
- [Flight Recorder](https://github.com/zyvorai/gryvia/blob/main/docs/flight-recorder.md) - Node-local, job-attributed eBPF timeline (preview)
- [GPU validation checklist](https://github.com/zyvorai/gryvia/blob/main/docs/gpu-validation.md) - What to run on real GPU hardware

### Examples
- [Job Examples](https://github.com/zyvorai/gryvia/tree/main/examples/jobs/) - Sample job configurations
- [Workflow Examples](https://github.com/zyvorai/gryvia/tree/main/examples/workflows/) - Argo Workflow templates
- [Integration Examples](https://github.com/zyvorai/gryvia/tree/main/examples/integrations/) - JupyterHub, VSCode, Ray
- [Templates](https://github.com/zyvorai/gryvia/tree/main/examples/templates/) - Pre-configured job templates
- [Complete Setup](https://github.com/zyvorai/gryvia/tree/main/examples/complete-setup/) - Full production example

## Quick Links

### For End Users
- **New to Gryvia?** Start with the [Quick Start Guide](getting-started/quickstart.md)
- **Submit your first job:** See [Job Submission Guide](user-guide/jobs.md)

### For Administrators
- **Install Gryvia:** Start with the [Quick Start](getting-started/quickstart.md), then the [Deployment Guide](guides/COMPLETE_DEPLOYMENT_GUIDE.md)
- **Configure cluster:** See [Cluster Setup](admin-guide/cluster-setup.md)

### For Developers
- **REST API:** Browse [API Reference](developer-guide/api-reference.md)
- **CLI commands:** See [CLI Guide](guides/CLI_GUIDE.md)

## Support

- **Issues:** https://github.com/zyvorai/gryvia/issues
- **Discussions:** https://github.com/zyvorai/gryvia/discussions

## Contributing

We welcome contributions! See [Contributing Guide](https://github.com/zyvorai/gryvia/blob/main/CONTRIBUTING.md) for details.

## License

Apache License 2.0 - see [LICENSE](https://github.com/zyvorai/gryvia/blob/main/LICENSE) for details.
