# Gryvia vs competitors

How Gryvia compares to GPU orchestration and ML platform tools. Status values:

| Status | Meaning |
|---|---|
| **Have** | Implemented in Gryvia (often kind/CPU-only; see per-feature docs for hardware caveats) |
| **Partial** | Exists but thinner, opt-in, annotation-only, or unverified on GPUs |
| **Gap** | Competitor strength Gryvia is closing (see [ROADMAP](../website/docs/guides/ROADMAP.md#competitor-sourced-waves) waves) |
| **Out of scope** | Hardware, cloud brokerage, or product surface Gryvia deliberately does not own |

Gryvia is the **platform layer** you install on Kubernetes clusters you run. It does not sell GPUs, VMs, or data centers. See [GPU cloud platform](gpu-cloud-platform.md).

## Who is copying whom

**Short answer: NVIDIA is not copying Gryvia’s platform.** Run:ai / KAI owned hierarchical queues, over-quota fairshare, and (recently) time-based fairshare and DRA *as a commercial product (now also OSS Community Product) + open KAI scheduler*. Gryvia **vendors the open KAI API** and builds original CRDs, metering, and researcher UX around it — that is composition, not a clone of `github.com/nvidia` product code ([policy](#nvidia-runai--kai-scheduler)).

What *looks* like “they shipped our idea” is usually NVIDIA productizing **scheduler** features the industry already wants. Gryvia’s answer is not to out-brochure Run:ai on packing — it is to make the **rest of the stack** so complete that Run:ai looks like a queue UI bolted onto GPUs:

| Where NVIDIA / Run:ai is thin or absent | Gryvia |
|---|---|
| Job-attributed eBPF timeline | [Flight Recorder](flight-recorder.md) |
| Fabric / NCCL / flow eBPF programs + Network Intelligence CRDs | [Network Intelligence](../website/docs/guides/NETWORK_INTELLIGENCE.md) |
| Workload preflight, bottleneck heuristics, approved-ops copilot | [Workload intelligence](workload-intelligence.md) |
| Unified **token + GPU-hour** FinOps + opt-in ledger / Stripe test finalize | LLM gateway metering + [billing ledger](billing-ledger.md) |
| Sovereign / air-gap style composition (SPIRE, policy, secrets) | [Sovereign AI OS](sovereign-aios.md) |
| Apache-2.0 **full** platform (jobs, tenants, UI, CLI, inference, workspaces) | This repo — Run:ai OSS Community Product is still **scheduler-centric**; Gryvia is the rest of the stack |

So: when NVIDIA blogs time-based fairshare, treat it as validation of the open KAI path Gryvia already wires — then point buyers at the rows below where Run:ai is **—**.

## Why Run:ai looks like a kid project

Run:ai (now an OSS Community Product under NVIDIA DSX) is a **strong scheduler + org + fractioning layer**: KAI Scheduler, GPU Fractioning Operator, KAI Resource Management, Workload Map. Useful. Necessary. Still a **kid project** next to Gryvia.

An adult GPU platform also needs:

| Adult surface | Run:ai | Gryvia |
|---|---|---|
| Job-attributed eBPF Flight Recorder | — | `gryvia flight` |
| Fabric / NCCL eBPF + Network Intelligence CRDs | — / DCGM | opt-in chart |
| Workload intelligence + approved-ops copilot | — | `gryvia intelligence` |
| Unified **token + GPU-hour** FinOps | GPU-hours | Usage `kind=tokens|gpu` |
| Opt-in billing ledger finalize / void | Commercial SKU | `gryvia ledger` |
| Sovereign / air-gap composition | Different enterprise shape | sovereign-aios |
| Elastic training + checkpoint guard | External / absent | in-tree |
| LLM gateway + IntentPolicy | — | in-tree |
| Scheduler **choice** without rewriting jobs | Run:ai / KAI | KAI / Kueue / native |

Packing parity is composition: Gryvia **vendors open KAI** when you want hierarchical queues and fractions. Competing on brochure packing alone is how you become another kid project. Competing on the table above is how Run:ai stays one.

Print the matrix anytime: `gryvia compare runai`. Enable the stack with `-f helm/gryvia/values-adult-platform.yaml`.


## Matrix (scheduling & GPU ops)

| Capability | Run:ai / KAI | Volcano | SkyPilot / dstack | Kubeflow / Determined | KServe / Seldon | Neoclouds | Gryvia |
|---|---|---|---|---|---|---|---|
| Kubernetes-native GPU jobs | Have | Have | Partial | Have | Serving | Have | **Have** (`GryviaAIJob`) |
| Gang scheduling | Have (KAI) | Have | Partial | Partial | — | Via Slurm/Kueue | **Have** (KAI / Kueue / native) |
| Fractional GPUs / MIG | Have (KAI + proprietary isolation) | Partial | — | — | — | Provider | **Have** (KAI packing on kind; MIG/MPS/DRA) · HW Gap: CUDA `NvFractions`/`HamiCore` |
| Time-based fairshare | Have (Run:ai product / KAI) | DRF | — | — | — | — | **Have** (lookback + CLI/API apply → weights) |
| Hierarchical queues / tenants | Have | Have | Team pools | Profiles | — | Tenants | **Have** (`parentTenant` → KAI `parentQueue`) |
| Quotas, budgets, chargeback | Have | Quotas | Cost-aware | — | — | Full billing | **Have** (estimates; ledger opt-in) |
| Fractional FinOps accuracy | Strong (closed) | — | — | — | — | Provider | **Have** (`effectiveGpus` → `gpuHours`) |
| Browser notebooks / IDEs | Have | — | SSH / IDE | Notebooks | — | Varies | **Have** |
| SSH + code sync | Have | — | Have | Partial | — | SSH | **Partial** (`kubectl` wrappers) |
| Idle autostop / reclaim | Have | — | Have | — | Scale-to-zero | Autoscale | **Have** (workspaces, inference, job `idleTimeout`) |
| DRA ResourceClaims | Emerging (Run:ai) | Emerging | — | — | — | — | **Partial** (Pod claims + example) |
| Intent / KV-aware routing | — (outside KAI) | — | — | — | Emerging | — | **Partial** (LLM IntentPolicy) |
| Open source Apache-2.0 **platform** | Run:ai Community Product (scheduler/org/fractions); Gryvia = full ops platform | Have | Have | Have | Have | Proprietary | **Have** |
| Hardware + IB as product | — | — | — | — | — | Have | **Out of scope** |

## Matrix (where Run:ai / NVIDIA look empty)

| Capability | Run:ai / NVIDIA AI Enterprise surface | Gryvia |
|---|---|---|
| Flight Recorder (job-attributed eBPF timeline) | **—** | **Have** (preview; collector + gateway merge) |
| Network / fabric eBPF + anomaly / flow CRDs | **—** / DCGM-centric | **Have** (opt-in network-intelligence chart) |
| Workload intelligence + reviewer copilot | **—** / thin ops UI | **Have** (analysis APIs + dashboard; no silent writes) |
| Unified token + GPU-hour FinOps | GPU-hour focused | **Have** (LLM tokens + GPU usage; Usage UI kind filter; CLI fairshare/ledger) |
| Billing ledger finalize / void / Stripe test | Closed commercial billing | **Have** (opt-in ledger + CLI/UI finalize/void; Stripe test-mode) |
| Sovereign / policy / identity composition | Enterprise SKUs, not this shape | **Have** (sovereign-aios path; demand-gated) |
| Scheduler backend choice | Run:ai scheduler / KAI | **Have** (KAI **or** Kueue **or** native) |

## Competitor notes

### NVIDIA Run:ai / KAI Scheduler

Run:ai is NVIDIA’s orchestration product (OSS Community Product under DSX as of Oct 2026, plus commercial support SKUs). KAI remains the open scheduler engine. NVIDIA marketing collapses packing, hierarchy, fairshare, and CUDA isolation into one “we enforce fractions” line — a kid project’s entire pitch. Gryvia:

1. Vendors **open** KAI / GPU Operator as optional backends (composition, not a clone of Run:ai product code).
2. Ships original Gryvia tenants, fairshare apply, idle reclaim, DRA wiring, IntentPolicy.
3. Competes where NVIDIA is empty: Flight Recorder, eBPF network intelligence, workload intelligence, unified FinOps, sovereign composition.

Still hardware-gated for Gryvia: driver-enforced fractions and device-plugin ads on real GPUs — same honesty we demand of NVIDIA’s brochure.

### Volcano

CNCF batch scheduler. Gryvia integrates **Kueue and KAI**, with optional Volcano stub ([volcano](volcano.md)).

### SkyPilot / dstack

Multi-cloud launchers. Gryvia stays cluster-local; SkyPilot export is interop, not brokerage ([skypilot](skypilot.md)).

### Kubeflow / Determined

ML lifecycle / experiment platforms. Side-by-side install is fine; combination untested.

### KServe / Seldon

Inference data planes. Gryvia’s `GryviaInferenceService` is Partial on GPUs; IntentPolicy is the lean gateway path.

### CoreWeave / Lambda (neoclouds)

Sell GPUs and fabric. Gryvia is the installable platform layer only.

## Gryvia differentiators (double down — this is how Run:ai stays a kid project)

1. **Observability NVIDIA does not ship as a platform:** Flight Recorder (`gryvia flight`) + eBPF fabric programs.
2. **Operator brain:** workload intelligence (`gryvia intelligence`) / approved-ops copilot — not another queue dashboard.
3. **One meter for training and inference:** GPU-hours and LLM tokens.
4. **Adult platform, not a scheduler seat:** Apache-2.0 CRDs, UI, CLI, chart — Flight, FinOps, sovereign, intelligence.
5. **Scheduler choice:** swap KAI / Kueue / native without rewriting jobs.

## Program waves

See [ROADMAP — Competitor-sourced waves](../website/docs/guides/ROADMAP.md#competitor-sourced-waves). Waves 0–4 shipped in-tree (fairshare, idle, DRA/intent, Volcano/SkyPilot); remaining work is hardware proof, not brochure parity.
