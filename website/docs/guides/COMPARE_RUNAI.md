# Gryvia and NVIDIA Run:ai

On October 13, 2026 NVIDIA released the full [NVIDIA Run:ai platform](https://github.com/dsx-ai-factory/run-ai) as open
source under Apache 2.0, with four parts that can be adopted on their own: KAI Scheduler (a CNCF Sandbox project), the
GPU Fractioning Operator, KAI Resource Management and Workload Map
([announcement](https://developer.nvidia.com/blog/nvidia-runai-is-now-open-source-as-part-of-nvidia-dsx-ai-factory-platform/)).
Both projects are now open source and self-hosted, so the choice is about what each one does, how proven it is, and
how they fit together.

This page is written by the Gryvia project. Run:ai facts come from its public announcement and documentation as of
October 2026; corrections are welcome as issues. Gryvia's status column follows the rule used across this repository:
nothing is claimed beyond how it was tested.

## Short version

- **Run:ai is ahead on GPU scheduling and sharing, and on production use.** KAI Scheduler, hard-isolated GPU fractions
  (CUDA 13.4), dynamic fractions, GPU memory swap and years of production deployments. Gryvia's scheduling pieces are
  alpha and have not run on GPUs.
- **Gryvia is broader above the scheduler.** It runs a GPU cloud as a business (tenants, price catalog, metering,
  budgets, invoices) and an AI stack (an OpenAI-compatible LLM gateway with per-tenant keys and token metering, RAG,
  agents, a model factory, Slurm on Kubernetes, eBPF network signals) from one Helm chart.
- **They are not exclusive.** Gryvia can hand its jobs to KAI Scheduler (opt-in, `aiOperator.kaiScheduler`): KAI
  does the placement, gang scheduling and GPU fractions, and Gryvia keeps tenants, metering and billing on top. This
  was run on kind with a simulated GPU, not on GPU hardware ([KAI Scheduler integration](https://github.com/zyvorai/zyvor-gryvia/blob/main/docs/kai-scheduler.md)).
  Using the GPU Fractioning Operator and mapping tenants to KAI projects are on the [roadmap](ROADMAP.md).

## Side by side

| Capability | Gryvia | NVIDIA Run:ai |
|---|---|---|
| License | Apache 2.0 | Apache 2.0 (community-supported; NVIDIA-supported editions are sold separately) |
| Scheduler | Kubernetes scheduler for placement; gang admission, fair sharing and preemption through Kueue (kind CI, CPU pods), the alpha native queue gate (unit tests), or KAI Scheduler underneath (kind, simulated GPU) | KAI Scheduler: gang scheduling with in-memory simulation, time-based fair share, hierarchical quotas, topology-aware placement, bin-packing, DRA |
| Org hierarchy and quotas | Tenants, per-team quotas and queues, budgets | Projects, departments, node pools (KAI Resource Management) |
| GPU fractions | `--gpu 0.5` on MIG, MPS (compute share, and an MPS memory limit with `--gpu-memory`) or DRA intent with the native queue gate, or KAI's fractions with the KAI backend (two halves shared one simulated GPU on kind); not run on GPUs | Fractions with a hard memory limit enforced by the CUDA driver (CUDA 13.4), dynamic fractions (request and limit), GPU memory swap to host RAM |
| One-line submit | `gryvia submit --image ... --gpu 0.5 -- cmd` | `runai` CLI |
| Workspaces | Jupyter and VS Code with pause and resume, idle pause, browser access through the gateway with per-workspace sessions (run on kind with a real JupyterLab; code-server and GPUs not run) | Workspaces as a first-class workload type |
| Training and inference | `GryviaAIJob` (training, fine-tuning, evaluation, distributed), `GryviaInferenceService` with canaries and scale to zero; kind CI with CPU images | Training, distributed training and inference workloads; Model Streamer for fast weight loading |
| GPU-as-a-service billing | SKU price catalog, GPU-hour and token metering, budgets, estimate invoices, an opt-in hash-chained billing ledger and Stripe test mode | Usage dashboards per project and department; the announcement lists partners (for example Saturn Cloud, Rafay) that add metering, billing and self-service catalogs |
| LLM platform | OpenAI-compatible LLM gateway with per-tenant keys and `tokensPerDay` quotas, RAG with Qdrant, tool-calling agents, a model factory that fine-tunes and promotes new open models | Not part of the platform described in the announcement |
| HPC | Opt-in Slurm on Kubernetes (Slinky) and `gryvia submit --sbatch` | Not described |
| Network visibility | eBPF collector and network intelligence (experimental, separate chart) | Not described |
| Try without GPUs | `scripts/kind-demo.sh` on a laptop | Requires a cluster; GPU-dependent features need GPUs |
| Proven on GPUs | **No.** See [what works today](https://github.com/zyvorai/zyvor-gryvia#what-works-today) | Yes, in production at many organizations |
| Support | Community | Community, or NVIDIA enterprise support through its commercial editions |

## When to pick which

- **You need proven GPU scheduling and sharing today:** Run:ai.
- **You sell or charge back GPU capacity to tenants and want metering, budgets and invoices next to the scheduler,
  or want an LLM gateway, RAG and agents in the same install:** Gryvia, with the caveats in its status column.
- **You want to evaluate on a laptop first:** Gryvia's kind demo runs without GPUs.

## Coming from Run:ai

The [README](https://github.com/zyvorai/zyvor-gryvia#coming-from-runai) maps `runai` commands to `gryvia` ones.
