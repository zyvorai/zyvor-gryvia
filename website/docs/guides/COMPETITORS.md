---
sidebar_position: 35
---

# Gryvia vs competitors

How Gryvia compares to GPU orchestration and ML platform tools. Status values:

| Status | Meaning |
|---|---|
| **Have** | Implemented in Gryvia (often kind/CPU-only; see per-feature docs for hardware caveats) |
| **Partial** | Exists but thinner, opt-in, annotation-only, or unverified on GPUs |
| **Gap** | Competitor strength Gryvia is closing (see [ROADMAP](ROADMAP.md#competitor-sourced-waves) waves) |
| **Out of scope** | Hardware, cloud brokerage, or product surface Gryvia deliberately does not own |

Gryvia is the **platform layer** you install on Kubernetes clusters you run. It does not sell GPUs, VMs, or data centers. See [GPU as a Service](GPU_AS_A_SERVICE.md) and the [GPU cloud platform](https://github.com/zyvorai/zyvor-gryvia/blob/main/docs/gpu-cloud-platform.md) note.

## Matrix

| Capability | Run:ai / KAI | Volcano | SkyPilot / dstack | Kubeflow / Determined | KServe / Seldon | Neoclouds (CoreWeave / Lambda) | Gryvia |
|---|---|---|---|---|---|---|---|
| Kubernetes-native GPU jobs | Have | Have | Partial (multi-cloud launcher) | Have | Serving-focused | Have (managed K8s) | **Have** (`GryviaAIJob`) |
| Gang scheduling | Have (KAI) | Have | Partial | Partial | — | Via Slurm/Kueue | **Have** (KAI / Kueue / native) |
| Fractional GPUs / MIG | Have (KAI + proprietary isolation) | Partial (vGPU/MIG) | — | — | — | Provider-specific | **Have** (KAI schedule packing on kind; MIG/MPS/DRA) · Gap only for CUDA `NvFractions`/`HamiCore` on HW |
| Time-based fairshare | Have (Run:ai product) | DRF | — | — | — | — | **Have** (lookback + CLI/API apply → `fairShareWeight` / KAI `overQuotaWeight`; opt-in Helm) |
| Hierarchical queues / tenants | Have | Have | Team pools | Profiles | — | Tenants | **Have** (`parentTenant` → nested KAI `parentQueue` + Kueue) |
| Quotas, budgets, chargeback | Have | Quotas | Cost-aware placement | — | — | Full billing | **Have** (estimates; ledger opt-in; no live payments) |
| Fractional FinOps accuracy | Strong (closed product) | — | — | — | — | Provider billing | **Have** (`effectiveGpus` → `gpuHours`; MIG heuristic) |
| Browser notebooks / IDEs | Have | — | SSH / IDE sync | Notebooks | — | Varies | **Have** (Jupyter / VS Code proxy) |
| SSH + code sync into workspace | Have | — | Have | Partial | — | SSH to nodes | **Partial** (`gryvia workspace ssh/sync` via kubectl) |
| Idle autostop / reclaim | Have | — | Have (autostop) | — | Scale-to-zero | Autoscale | **Partial** (workspaces, inference, job `idleTimeout`) |
| Multi-cloud cost broker | — | — | Have | — | — | Native cloud | **Out of scope** (MultiKueue federation only) |
| Experiment tracking / HPO UX | Partial | — | — | Have | — | — | **Partial** (AutoTuner, profiler; panels → Wave 4) |
| Inference serving | Via platform | — | Launch only | KServe | Have | Endpoints | **Partial** (controller + canary; not GPU-proven) |
| Intent / KV-aware routing | — | — | — | — | Emerging | — | **Partial** (LLM IntentPolicy + optional burst) |
| DRA ResourceClaims | Emerging | Emerging | — | — | — | — | **Partial** (Pod claims + example template; needs DRA driver) |
| Slurm bridge | — | HPC roots | Slurm backend | — | — | SUNK / Slurm | **Have** (`--sbatch`, Slinky opt-in) |
| Open source Apache-2.0 platform | KAI OSS; Run:ai product | Have | Have | Have | Have | Proprietary cloud | **Have** |
| Hardware + IB fabric as product | — | — | — | — | — | Have | **Out of scope** |

## Competitor notes

### NVIDIA Run:ai / KAI Scheduler

Run:ai is NVIDIA’s **closed** commercial orchestration layer; **KAI Scheduler** is the open scheduling engine (CNCF Sandbox) Gryvia already vendors. NVIDIA’s brochure stacks schedule packing, hierarchical projects, and CUDA memory isolation into one “enforced fractions” line — Gryvia ships the open pieces (KAI packing, nested tenants, fairshare weights, sharing labels) and refuses to pretend proprietary `NvFractions`/`HamiCore` are proven without GPUs.

**Policy:** Gryvia does **not** copy proprietary NVIDIA / Run:ai product code or UI. It vendors **open** components (KAI Scheduler API `scheduling.run.ai`, NVIDIA GPU Operator / device plugin charts) as optional backends and builds original Gryvia CRDs, metering, and researcher UX on top.

Gryvia already uses KAI as an **opt-in backend** ([KAI Scheduler](https://github.com/zyvorai/zyvor-gryvia/blob/main/docs/kai-scheduler.md)). Shipped vs Run:ai: lookback fairshare with apply, fractional FinOps, nested `parentTenant` → KAI `parentQueue`, job idle reclaim, DRA claim wiring, one-switch time-slicing→sharing. Only CUDA driver isolation and GPU-proven device-plugin ads remain hardware-gated.

### Volcano

CNCF batch scheduler with mature gangs, DRF, framework plugins (PyTorch, MPI, Ray, Spark). Gryvia integrates **Kueue and KAI**, not Volcano by default ([Scheduling](SCHEDULING.md)). Wave 4 adds an optional Volcano backend rather than forking Volcano ([volcano](https://github.com/zyvorai/zyvor-gryvia/blob/main/docs/volcano.md)).

### SkyPilot / dstack

Portable job launch across clouds, Kubernetes, and Slurm; cost/availability placement; autostop; SSH and code sync. Gryvia stays **cluster-local**. Wave 2 mirrors researcher UX (SSH/sync, job autostop). Wave 4 adds export/docs so SkyPilot can target Gryvia clusters — not a multi-cloud broker ([skypilot](https://github.com/zyvorai/zyvor-gryvia/blob/main/docs/skypilot.md)).

### Kubeflow / Determined

Full ML lifecycle (pipelines, notebooks, trainer, Katib, registry, KServe) or training-focused experiment platform. Gryvia ML controllers exist ([ROADMAP](ROADMAP.md)) but are CPU/kind-proven only. Side-by-side install is fine; combination untested. See also the [FAQ](FAQ.md).

### KServe / Seldon

Production inference: canaries, autoscaling, drift/explainability. Gryvia’s `GryviaInferenceService` covers canary, HPA, Gateway API, scale-to-zero; data plane unverified on GPUs. Wave 3 adds lean intent routing in the LLM gateway ([dra-and-intent](https://github.com/zyvorai/zyvor-gryvia/blob/main/docs/dra-and-intent.md)).

### CoreWeave / Lambda (neoclouds)

Sell GPUs, fabric, and managed orchestration. Gryvia is the installable platform layer only.

## Gryvia differentiators (double down)

- Workload intelligence and operator copilot
- Network intelligence, fabric signals, Flight Recorder
- Opt-in billing ledger and sovereign placement
- Unified token + GPU-hour FinOps (training and inference)
- Scheduler choice: KAI, Kueue, or native queue gate ([Native scheduler](NATIVE_SCHEDULER.md))

## Program waves

See [ROADMAP — Competitor-sourced waves](ROADMAP.md#competitor-sourced-waves). No committed dates.

| Wave | Focus |
|---|---|
| 0 Docs | This page, ROADMAP waves |
| 1 Fairshare + FinOps | Lookback weights, fractional/MIG billing |
| 2 SSH / autostop | Workspace SSH/sync, job idleTimeout |
| 3 DRA + intent | ResourceClaims, LLM intent routing / burst |
| 4 Interop | Volcano backend, SkyPilot export, experiment panels |
