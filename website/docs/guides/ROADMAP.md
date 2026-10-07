# Gryvia Roadmap

Where Gryvia stands and what is still open. This page replaces an earlier month-by-month plan that carried invented dates, effort estimates, targets and budgets. There are no committed dates here. Anything under "Open work" is aspirational, not a promise.

The project is alpha: the API is `gryvia.io/v1alpha1` and can change. For what each release actually contains, the source of truth is [CHANGELOG.md](https://github.com/zyvorai/gryvia/blob/main/CHANGELOG.md). Nothing below has been verified on real GPU, RDMA or InfiniBand hardware unless stated; most checks are unit tests against fake clients, CI on kind or k3s without GPUs, and eBPF load tests on Linux.

## Status by area

### Implemented (with the caveats stated)

| Area | What exists | Caveat |
|------|-------------|--------|
| `GryviaAIJob` operator | Node selection, run-to-completion Indexed Job (training, fine-tuning, evaluation) or StatefulSet (inference), headless Service and PVC per job, distributed-training environment variables, status phases including `Succeeded`/`Failed`, `retryLimit` and `timeout`, cancel, validating admission webhook, opt-in quota/budget admission gate | Node choice is recorded in status, pods are placed by the Kubernetes scheduler; cooperative preStop checkpoint hooks and a checksum-verified PyTorch recovery example; distributed checkpoint coordination is not implemented. Kind e2e (`e2e-jobs.yml`, CPU pods) passes in CI; nothing on GPUs. See [Scheduling](SCHEDULING.md) and [AIJob lifecycle](https://github.com/zyvorai/gryvia/blob/main/docs/aijob-lifecycle.md) |
| ML controllers | `GryviaWorkspace`, `GryviaInferenceService`, `GryviaModelRegistry`, `GryviaWorkflow`, `GryviaAutoTuner` in the ai-operator | Unit-tested; kind e2e with tiny CPU images passes in CI; no GPU, real model server or real metrics. See [ML Workflows](ML_WORKFLOWS.md) |
| Model factory | Opt-in `GryviaModelWatch`: polls the Hugging Face Hub and runs a workflow per new open model (download, LoRA fine-tune, evaluation, register); workflow step outputs, `register` steps and cron schedules; registry `promotionPolicy` and shared-service canary rollout of each better version | Unit-tested; e2e flow with a stand-in hub and busybox steps passed on a k3s cluster without GPUs and in kind CI; no real fine-tune, evaluation or vLLM serving of a fine-tuned model. See [model factory](https://github.com/zyvorai/gryvia/blob/main/docs/model-factory.md) |
| Datasets | Opt-in `GryviaDataset` controller in the storage-operator: http (with checksum), s3 and nfs sources downloaded into a PVC, one directory per version, retention | Unit-tested; the http path runs in kind CI against a stand-in file server; s3 and nfs only in unit tests. See [datasets](https://github.com/zyvorai/gryvia/blob/main/docs/datasets.md) |
| Model evaluation and quantization | Workflow `registry` steps, registry `rollbackPolicy` and requested rollbacks of a shared service, scheduled evaluation example, AWQ/GPTQ quantize step | Rollback loop runs in kind CI with busybox evaluation; no real evaluation or quantization has run. See [model evaluation](https://github.com/zyvorai/gryvia/blob/main/docs/model-evaluation.md) |
| LLM gateway | Opt-in OpenAI-compatible endpoint (`/manager llm-gateway`) in front of annotated inference services: per-tenant hashed keys, token metering into usage records, `tokensPerDay` quotas, streaming | Runs in kind CI against a stand-in OpenAI server; not tried with vLLM. See [LLM gateway](https://github.com/zyvorai/gryvia/blob/main/docs/llm-gateway.md) |
| RAG | Opt-in `GryviaVectorIndex`: managed or external Qdrant, ingestion Jobs embedding a dataset through the gateway, `/v1/retrieve` | Runs in kind CI with the real Qdrant and a stand-in embedding server; no real embedding model. See [RAG](https://github.com/zyvorai/gryvia/blob/main/docs/rag.md) |
| Agents | Opt-in `GryviaAgent`: tool-calling runtime (retrieval and allowlisted HTTP tools) per agent with its own gateway key and an egress NetworkPolicy; chat through the api-gateway, CLI and dashboard | Runs in kind CI with a stand-in tool-calling model; no real tool-calling model; NetworkPolicy enforcement depends on the CNI. See [agents](https://github.com/zyvorai/gryvia/blob/main/docs/agents.md) |
| Job hooks | Opt-in `GryviaJobHook`: signed webhook (or Slack message) when an AI job or workflow reaches chosen phases, with retries and a private-address guard; gateway and CLI | e2e step with a stand-in receiver; no third-party receiver tried; at-least-once delivery. See [job hooks](https://github.com/zyvorai/gryvia/blob/main/docs/job-hooks.md) |
| GPU operator | `GryviaGpuNode` registration from NVIDIA GPU feature discovery labels, `GryviaGpuMemoryOptimizer` | No NVML; per-GPU numbers come from the DCGM exporter. Unverified on hardware |
| GPU node preparation | Optional bundled NVIDIA GPU Operator, opt-in Network Operator and NIM Operator sub-charts ([NVIDIA one-click](NVIDIA_ONE_CLICK.md)), `scripts/install-k3s-gpu.sh` | CI runs it on k3s without a GPU; see [GPU nodes](GPU_NODES.md) |
| Checkpoint guard | `GryviaCheckpointGuard` (opt-in `aiOperator.checkpointGuard`): injects checkpoint directory, cadence, replica target and a status ConfigMap into matching training jobs; reports committed steps; replaces pods of non-elastic jobs on a lost node | Unit-tested; kind e2e (`e2e-elastic.yml` node-loss, CPU torchrun on NFS) in CI; not on GPUs or S3. See [checkpoint guard](https://github.com/zyvorai/gryvia/blob/main/docs/checkpoint-guard.md) |
| ai-operator extras | `GryviaTrainingTimeMachine`, `GryviaLiveExperiment`, `GryviaTrainingProfiler`, `GryviaModelLineage` | Unit-tested; not run against real training jobs |
| Quota, tenants, metering | `GryviaQuota`, `GryviaTenant` (namespace `tenant-<name>`, ResourceQuota, LimitRange, optional NetworkPolicy, opt-in RoleBindings), `GryviaUsageRecord`, `GryviaCostPredictor`, `GryviaGpuSku` catalog, `GryviaBudget` controller, opt-in `GryviaReservation` node reservations, estimate invoices and a signed invoice webhook, opt-in immutable billing ledger with finalized `GryviaInvoice`s and Stripe test mode | Estimates unless the billing ledger is on; Stripe test mode only, no live payments or tax; reservations, tenant RBAC and the admission gate are off by default and unverified on a real cluster. See [GPU as a service](GPU_AS_A_SERVICE.md) |
| Storage and network operators | `GryviaStorage`, `GryviaNetwork` controllers | Real VAST, Weka, DDN or RDMA/SR-IOV hardware not tested |
| Network intelligence | 10 kinds reconciled by the network-intelligence operator (separate chart) | See the eBPF row below |
| API gateway and dashboard | REST API under `/api/...`, API key and session login, OIDC for tenant users, Flight Recorder cluster view, CRUD for several kinds | OIDC not verified against a real identity provider. See [Auth and TLS](AUTH_AND_TLS.md) |
| CLI | Rust `gryvia` talking to the Kubernetes API through your kubeconfig | |
| Helm charts and releases | `helm/gryvia`, `helm/network-intelligence`, observability chart, tag-driven multi-arch image release signed with cosign | |
| Python and Go SDKs | Python REST client, Go Kubernetes client | Install from source |

### Experimental

- **eBPF collector.** 47 CO-RE programs. Compile and pass the kernel verifier on Linux 7.0 x86_64 and in CI; arm64 is compile-only. XDP chaining (`-xdp-mux`) is tested in CI on the loopback interface only. Off by default, runs privileged with host networking, and its image is not part of the release images. GPU, NCCL, RDMA and GPUDirect Storage behaviour is unverified on hardware. Flight Recorder and fabric signals are node-local previews.
- **Fabric signal CRD (`gryviafabricsignals`).** CRD and collector endpoint exist; the collector can patch its status (`-publish-fabric-status`, opt-in) and, with `-fabric-status-per-node` and the ai-operator's `--merge-fabric-signals`, per-node entries are folded into the top-level status. The penalty function in the scheduler package is used only by the opt-in fabric-aware scheduling. None of it has run on real GPU or RDMA hardware.
- **Terraform and Ansible automation** under the repository's infrastructure directories: marked experimental and incomplete; they do not install Kubernetes, a CNI or GPU drivers.
- **No in-tree gang, queue or elastic scheduler.** The unused gang scheduler, DRF queue and elastic helpers were removed. Gang admission, queueing and preemption are done by Kueue instead, opt-in ([Kueue integration](https://github.com/zyvorai/gryvia/blob/main/docs/kueue-integration.md)): unit-tested with fake clients, the kind workflow (real Kueue, CPU pods) passes in CI, never run on GPUs.

### Legacy APIs without supported runtime behavior

The legacy kinds without a runtime, `GryviaSLA`, `GryviaAutoScaler`, `GryviaRetryPolicy`, `GryviaBenchmark`, `GryviaAudit`, `GryviaDRTest`, `GryviaMetric` and `GryviaQuotaPolicy`, have been removed (their CRDs are no longer shipped; the CHANGELOG has the upgrade steps). `GryviaJobHook` was rebuilt as a namespaced kind with a controller that delivers webhooks (opt-in, see [job hooks](https://github.com/zyvorai/gryvia/blob/main/docs/job-hooks.md)), and `GryviaDataset` has a real controller again (see Datasets above). `GryviaGPUSharingPolicy` keeps its controller (opt-in, `gpuOperator.gpuSharing`) for the NVIDIA one-click labels. `GryviaGpuSku` is catalog data and needs no controller. PriorityClass reconciliation, template validation, federation readiness probes, actual-usage chargeback and optional GPU health checks now have registered controllers. See the [completion bundle](https://github.com/zyvorai/gryvia/blob/main/docs/platform-completion.md) for their limits.

### Not implemented

- Automatic resizing from free capacity and guaranteed checkpoints on abrupt node loss (an elastic job resizes live on request through `elastic.desiredNodes`, the gateway or the dashboard, within `distributed.nodes`; a checkpoint guard replaces a non-elastic job's pods on a lost node and resumes from the last committed step, but steps after that commit are lost). Cooperative preStop requests, a durable single-process recovery example, a file-based all-ranks commit protocol (`examples/training/coordinated_checkpoint.py`) and DCP checkpoints that reshard model and optimizer state to another world size (`examples/training/dcp_checkpoint.py`, used by the elastic `torchrun` trainer; multi-process tested on CPU with DDP and FSDP2, never on GPUs) are implemented.
- A separate hierarchical/DRF/global scheduler. Native Kueue fair-sharing, topology and MultiKueue configuration is available; a two-kind-cluster MultiKueue e2e with CPU pods passes in CI (a job submitted on the manager runs on the worker and reports back); opt-in federation failover (fence a failed member, evict and redispatch its Workloads, resume from an S3 checkpoint replica) has a three-cluster kind e2e. GPU placement across clusters remains unverified.
- Cilium as the default CNI or any cluster-wide CNI replacement
- Multi-cluster control plane and global scheduler
- Verified inference serving (Triton, vLLM, TensorRT-LLM) managed by Gryvia: the controller exists, with a pod-count canary, but opt-in Gateway API weighted routing, custom GPU/RPS HPA metrics and Prometheus SLO gating of canaries are implemented; real-image/data-plane behaviour is not verified (see [inference serving](https://github.com/zyvorai/gryvia/blob/main/docs/inference-serving.md))
- E-mail/PDF/pager delivery, live payments and tax (an opt-in hash-chained billing ledger with finalized invoices and Stripe test mode, budget Events, an opt-in signed budget-alert webhook (`quotaOperator.budgetWebhook`), the signed invoice webhook and metered showback/chargeback estimates are implemented)
- A *verified* GPU reset, and driver reload/reboot (opt-in quarantine, PDB-respecting drain and an opt-in node-local reset agent, dry-run by default and unit-tested only, are implemented; see [docs/gpu-reset-agent.md](https://github.com/zyvorai/gryvia/blob/main/docs/gpu-reset-agent.md))
- Payment processing and billing

## Open work

Listed roughly in the order they would make the platform more useful; no dates.

1. **Verify on real hardware.** Run the operators, the eBPF collector and the RDMA paths on GPU and InfiniBand or RoCE nodes and record measured results. Until then storage, NCCL and training performance are unknown: the earlier targets (for example 20 GB/s storage, 30 percent faster training, 99.9 percent uptime) were goals, never measurements. The procedures are [GPU validation](https://github.com/zyvorai/gryvia/blob/main/docs/gpu-validation.md) for the nodes and the [GPU-cluster runbook](https://github.com/zyvorai/gryvia/blob/main/docs/gpu-ai-runbook.md) for the AI features.
2. **Prove the Kueue integration** (the kind workflow already passes in CI; next a GPU cluster) and wire what is still unwired (topology-aware placement beyond the opt-in fabric penalty, which is wired); elastic training is not implemented. The decision to integrate Kueue rather than build queueing in-tree is made.
3. **Prove the ML controllers** on real workloads (`e2e-ml.yml` already passes in CI with tiny CPU images; next real Jupyter, vLLM and Triton images on GPUs) and decide the kinds that have no controller: build them or remove them. For the model factory: one real watch on a GPU cluster, a fine-tune of a small model (for example a 0.5B instruct model) through to a vLLM canary, with the measured GPU memory compared with the sizing estimate. For the AI features: a real AWQ quantization served by vLLM, the LLM gateway in front of vLLM, a real embedding model behind a vector index, and an agent on a vLLM model with tool calling turned on.
4. **Reliability of jobs**: validate cooperative checkpoint recovery with real distributed trainers and run the new elastic bounds (`distributed.elastic.minNodes`, unit-tested only) with a real `torchrun`.
5. **Cost controls**: validate budget Events and actual-usage chargeback on a real cluster, then add external notification/report delivery.
6. **Multi-cluster** and federation, if demand justifies it.
7. **Release quality**: publish the collector image once the eBPF programs are verified on more kernels and on arm64, add end-to-end tests that exercise GPUs, and broaden CI.

## Verification cadence

Ideas for measurements worth running regularly once hardware is available; none is currently automated: LLM training throughput, `nccl-tests` bandwidth, storage I/O with `fio`, job success rate, scheduler and API latency. No targets are set until a baseline exists.

## Contributing and feedback

- **Issues**: https://github.com/zyvorai/gryvia/issues
- **Discussions**: https://github.com/zyvorai/gryvia/discussions

## Runtime completion updates

The [platform completion bundle](https://github.com/zyvorai/gryvia/blob/main/docs/platform-completion.md) adds telemetry injection, live Prometheus SLO tests, verified single-process PyTorch recovery, native priority classes, Kueue fair-sharing/topology/MultiKueue configuration, verified federation probes, optional GPU quarantine/drain and metered cost-center estimates. Legacy API reporting is a capability condition, not an implementation of those APIs. Hardware qualification and distributed elastic checkpoint coordination remain open.
