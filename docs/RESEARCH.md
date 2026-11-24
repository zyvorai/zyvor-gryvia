# Research: GPU scheduling and ML platforms

Date: 2026-11-23. Statements marked [U] were not read from a page fetched for this document (written from background
knowledge) and need checking before anyone relies on them. Unmarked statements come from the pages listed under Sources.

## What the field does

| System | What it does |
|---|---|
| Kueue | ClusterQueues own quota; cohorts let queues borrow each other's unused quota; borrowed quota can be reclaimed by evicting the borrower; preemption is classic (priority, nominal quota) or fair sharing, where each queue's share is borrowed usage scaled by `fairSharing.weight` (lowest share admitted first, highest preempted); preemption strategies `LessThanOrEqualToFinalShare` and `LessThanInitialShare`; admission checks gate admission; topology-aware scheduling. |
| Volcano | Gang scheduling (all tasks start together), multi-level queues with inheritance, multi-resource quotas, proportion/capacity scheduling, DRF fair share across multi-dimensional resources, borrowing, reclaim and preemption between queues, pluggable actions. |
| Run:ai (NVIDIA) | Per project and department deserved quota per node pool; over-quota use limited to preemptible workloads; over-quota weights divide leftover capacity; reclaim is fairness-driven, not priority-driven; time-based fairshare smooths using historical GPU-hours; bin-pack or spread per node pool. |
| OpenCost | Workload GPU cost is max(requested, used); node cost is the sum of CPU, GPU, RAM and network; idle cost = cluster asset cost minus workload cost and can be distributed to tenants. |
| KubeRay | Ray head and workers as Pods through RayCluster, RayJob, RayService CRDs; optional autoscaling by adding and removing Pods. |
| NVIDIA GPU Operator, DCGM, MIG, time-slicing | [U] GPU Operator installs drivers, device plugin and DCGM exporter; MIG partitions a GPU into isolated slices; time-slicing oversubscribes a GPU without isolation. |
| SkyPilot | [U] Launches jobs across clouds and clusters, picks the cheapest capacity and retries on preemption. |
| Slurm on Kubernetes | [U] Runs Slurm controllers and nodes as Pods so HPC users keep sbatch while the cluster stays Kubernetes. |
| Checkpoint/restart | [U] Periodic checkpoints plus restart on another node make preemption cheap; schedulers use them to preempt safely. |

## What Gryvia already has (from the repo docs)

Native queue scheduler, Kueue and KAI Scheduler integration with per-tenant queues, gang admission, GPU fractions and MIG,
time-window fairshare weights (`docs/fairshare.md`), checkpoint guard (`docs/checkpoint-guard.md`), Slurm accounting,
budgets, a cost predictor, a metering ledger and chargeback by cost center, and SkyPilot and elastic training docs.

## What it lacks

1. Idle capacity cost: chargeback billed only metered job cost, so the sum of invoices was always below the GPU bill and
   idle cost was invisible (OpenCost distributes idle cost to tenants).
2. A tested decision core for fair-share reclaim (which borrowed workloads to preempt, never below a tenant's quota).
   Fairshare weights exist, but the victim selection lives inside Kueue or KAI, and the native scheduler has no
   weighted-share reclaim.
3. max(requested, used) GPU cost: Gryvia bills wall-clock reserved GPUs, which matches OpenCost for allocation-billed
   GPUs, but there is no utilization-based view of waste per tenant.
4. Topology-aware admission as a quota concept (Kueue TAS), and per-pool bin-pack versus spread policy (Run:ai): both
   need cluster or hardware to verify.

## Ranked ideas

| # | Idea | Why | Verifiable headlessly | Status |
|---|---|---|---|---|
| 1 | Idle-cost allocation in chargeback | Closes the gap between invoices and the GPU bill; small, pure maths; fits the metering identity | Yes | Built (`docs/idle-cost.md`) |
| 2 | Fair-share reclaim planner (weighted share, victim selection) | Reusable core for Kueue/Run:ai-style reclaim; pure logic | Yes | Built as a library, not wired |
| 3 | Utilization-aware waste report (requested vs DCGM-used GPU per tenant) | Shows over-requesting; needs live DCGM data | No | Not built |
| 4 | Topology-aware admission, bin-pack/spread per pool | Matches Kueue TAS and Run:ai | No (needs a cluster or GPUs) | Not built |
| 5 | Auto-checkpoint before preemption | Makes reclaim safe | No (needs training jobs) | Not built; checkpoint guard exists |
| 6 | Multi-cloud price-aware placement (SkyPilot-style) | Cost routing | Partly; large scope | Not built |

Rejected for now: replacing the scheduler or copying Kueue's preemption code (Gryvia delegates to Kueue and KAI by
design); a payment or invoicing engine (SECURITY.md says there is no payment processing).

## Caveats

The strategy rules in `pkg/fairshare` are our reading of the strategy names and the share definition on the Kueue page; the
page did not give formulas, so behaviour may differ from Kueue in edge cases. The Run:ai page is NVIDIA's self-hosted docs
and the OpenCost page is the specification page.

## Sources

- https://kueue.sigs.k8s.io/docs/concepts/
- https://kueue.sigs.k8s.io/docs/concepts/preemption/
- https://volcano.sh/en/docs/
- https://www.opencost.io/docs/specification
- https://run-ai-docs.nvidia.com/self-hosted/platform-management/runai-scheduler/scheduling/concepts-and-principles
- https://docs.ray.io/en/latest/cluster/kubernetes/index.html

## Round 3 (2026-11-24)

Pages read for this round: the Kueue pending-workloads API page (each pending workload gets `positionInClusterQueue` and
`positionInLocalQueue` so users can estimate when a job starts), and the Kubecost 2.4 GPU page (an efficiency report with
"GPU workload idle" per workload and "idle by cluster" for GPUs no container requested). Search results only, not read as
pages: Run:ai documents a Reports page with CSV download and time-based fairshare but no wait-time or quota-utilization
report; AMD's Resource Manager project dashboard shows average wait time and a quota-utilization panel; Slurm exit states
(OUT_OF_MEMORY, NODE_FAIL, PREEMPTED, TIMEOUT) and GPU-cluster failure studies split failures into deterministic ones
(OOM, application bugs) and transient ones (node or network faults, preemption); SkyPilot searches found nothing new.

Built from this (docs/job-insights.md): queue positions, wait-time percentiles, failure classification with a
transient flag, per-job GPU right-sizing from measured utilization, and quota use per tenant. Not built: Kubecost-style
infrastructure idle (GPUs no job requested; needs node inventory and DCGM), request sizing applied automatically, and
log-based failure root cause.
