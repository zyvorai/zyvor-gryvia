# Gryvia as the platform of a GPU cloud

GPU clouds ("AI superclouds", "neoclouds") sell two things: hardware they operate (data centers, GPUs, fabric,
power) and a platform on top (tenants, quotas, jobs, billing, inference endpoints, fine-tuning, a CLI and a
console). Gryvia is the second part, as software you install on Kubernetes clusters you run. It does not supply
the first.

This page maps what such a provider advertises to what Gryvia has, with the test or CI run that checks it, and
lists what Gryvia does not provide.

## What the provider brings

- Data centers, power, cooling, and the hardware itself (GPUs, CPUs, NVMe, InfiniBand/RoCE fabric).
- Hardware provisioning: racking, firmware, OS images, bringing a node into a cluster. Gryvia starts at a
  Kubernetes node; [GPU nodes](../website/docs/guides/GPU_NODES.md) covers drivers and the device plugin.
- VM and bare-metal instances sold as products. Gryvia schedules containers and workspaces, not VMs.
- Network, peering, data residency of the facility, certifications, and support.

## Capability map

| Advertised capability | In Gryvia | Checked by |
|---|---|---|
| GPU capacity per tenant, quotas | `GryviaTenant` (namespace, quotas, allowed SKUs), `GryviaQuota`, SKU catalog | [GPU as a Service](../website/docs/guides/GPU_AS_A_SERVICE.md); `scripts/e2e-gpuaas.sh` in `e2e-gpuaas.yml` |
| Jobs that queue instead of failing, gang admission, priorities, preemption | Kueue integration: per-tenant ClusterQueues, all-or-nothing gangs, priority preemption with requeue | [Kueue integration](kueue-integration.md); `e2e-kueue.yml` (passes on kind) |
| Checkpoints, surviving node loss | Checkpoint hooks run as `preStop` on eviction; elastic training re-forms the group after losing a worker, including index 0 | [Admission and recovery](admission-recovery.md), [Elastic training](elastic-training.md); `e2e-kueue.sh preempt-checkpoint`, `e2e-elastic.yml` ([run 36990965624](https://github.com/zyvorai/gryvia/actions/runs/36990965624)) |
| Transparent billing | One usage record per admitted run (requeued time is not billed), SKU rates, budgets that can block, chargeback, invoices | [GPU as a Service](../website/docs/guides/GPU_AS_A_SERVICE.md), [Kueue integration](kueue-integration.md#priorities-and-preemption); quota-operator unit tests, `e2e-kueue.sh preempt` |
| One CLI, API and console | `gryvia` CLI (jobs, queues, quotas, usage, invoices, models, LLM keys, datasets, logs), the API gateway, the web UI | [CLI guide](../website/docs/guides/CLI_GUIDE.md), [API reference](../website/docs/developer-guide/api-reference.md) |
| Inference endpoints, autoscaling, canaries | `GryviaInferenceService` (any serving image, such as vLLM or llama.cpp), HPA on GPU or requests per second, Gateway API canaries with SLO gating | [Inference serving](inference-serving.md) |
| Serverless inference (scale to zero) | `spec.scaleToZero`: idle services go to 0 replicas; a request through the LLM gateway wakes the service and waits for it (503 with `Retry-After` after the cold-start timeout) | [Inference serving](inference-serving.md#scale-to-zero), [LLM gateway](llm-gateway.md#scale-to-zero); controller and gateway unit tests; the scale-to-zero step of `e2e-ml.yml` (not yet passed) |
| Prompt playground | Console page `/playground`: pick a ready model, set the system prompt and sampling, stream replies with token usage, copy the call as curl or Python; chats go through the LLM gateway with your key and are metered | [LLM gateway](llm-gateway.md#playground-through-the-api-gateway); api-gateway and web-ui unit tests; the playground checks in `e2e-ml.yml` (not yet passed) |
| Model APIs (OpenAI-compatible, streaming, per-key metering) | LLM gateway: per-tenant keys, routing by model, streaming with usage, token quotas, token usage records | [LLM gateway](llm-gateway.md); `e2e-ml.yml` with llama.cpp ([run 36990962579](https://github.com/zyvorai/gryvia/actions/runs/36990962579)) |
| Fine-tuning | Model factory: download, LoRA fine-tune, lm-eval, GGUF conversion, registry, serving from the registry, scoring the live endpoint | [Model factory](model-factory.md); `e2e-ml.yml` ([run 37016697378](https://github.com/zyvorai/gryvia/actions/runs/37016697378)) |
| Agents and RAG | `GryviaAgent` (tool-calling runtime behind the gateway), `GryviaVectorIndex` | [Agents](agents.md), [RAG](rag.md) |
| Datasets and storage | `GryviaDataset` from http, S3-compatible stores and NFS, versioned and incremental | [Datasets](datasets.md); `e2e-ml.yml` |
| Fabric and network | RDMA/NVLink-aware scheduling, network intelligence, per-tenant network cost | [Scheduling](../website/docs/guides/SCHEDULING.md), [Network intelligence](../website/docs/guides/NETWORK_INTELLIGENCE.md) |
| Sovereign deployment, no lock-in | Runs on your own clusters; one release with Zyntra and Netra; Apache-licensed, standard Kubernetes objects | [Sovereign AIOS](sovereign-aios.md) |
| Several clusters or regions | MultiKueue admission check binding | [Kueue integration](kueue-integration.md) (`e2e-multikueue.yml`, [run 36875403171](https://github.com/zyvorai/gryvia/actions/runs/36875403171)) |
| Slurm users | `gryvia submit --sbatch` runs batch scripts as jobs with `SLURM_*` set; opt-in real Slurm (SchedMD's Slinky operator) with a partition per tenant and finished jobs metered as usage records | [Slurm](slurm.md); `scripts/e2e-jobs.sh sbatch` in `e2e-jobs.yml` (passes on kind); `e2e-slurm.yml` for Slinky (not yet passed) |
| Upgrades without losing state | CRDs applied server-side (scope changes handled), Helm upgrade, rollback, backup and restore | [Operations](../website/docs/guides/OPERATIONS.md); `upgrade.yml` |

## Competitors and neoclouds

For how Gryvia compares to Run:ai/KAI, Volcano, SkyPilot, Kubeflow, Determined, KServe/Seldon and neocloud
platforms, see [competitors](competitors.md). Gryvia is the installable platform layer; neoclouds sell hardware
plus a managed platform. Competitor-sourced product waves are listed in the
[roadmap](../website/docs/guides/ROADMAP.md#competitor-sourced-waves).

## What Gryvia does not provide

- Hardware, facilities, provisioning of machines, or VM and bare-metal instance products (see above).
- Multi-cloud cost brokerage (use SkyPilot *on* Gryvia clusters if you need that; see competitors doc).
- Most end-to-end checks run on kind with CPU stand-ins. What has and has not run on GPUs is stated on each page
  (for example [GPU validation](gpu-validation.md)); nothing here claims GPU throughput or latency numbers.
