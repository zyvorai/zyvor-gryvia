# Gryvia native scheduler completion patch

Base reviewed: `zyvorai/zyvor-gryvia` main at `fcc6e8d98ab9ad0ec0363dabfc7eceab3d5042a5` (2026-10-07).

## Why this patch exists

Current Gryvia already has GPU-aware node filtering/scoring, fabric-aware ranking, topology grouping,
placement holds, quota/budget admission, Kueue gang admission and priority/preemption integration,
MIG policy manifests, and operators for AI/GPU/quota/network/storage.

The repository README explicitly says the ai-operator has **no in-tree gang, DRF or preemption scheduler**.
The old root `scheduler/topology-optimizer.go` is also a demo implementation with hard-coded A100/V100
topology and a fixed utilization score.

This patch closes those gaps without replacing working Gryvia placement code.

## Added

1. Native weighted dominant-share fair ordering.
2. Atomic `minAvailable` gang admission.
3. Conservative priority preemption; equal/higher-priority allocations are never victims.
4. Queue guarantees, max GPU ceilings, borrowing.
5. Fractional GPU accounting in milli-GPU units.
6. Runtime selection: whole GPU, MIG, MPS, or DRA.
7. Live telemetry scoring contract for GPU utilization, memory pressure, NVLink, RDMA and storage.
8. `GryviaAIJob` bridge: existing jobs become native scheduler workloads; no duplicate workload CRD.
9. Pod runtime mutation helper for device-plugin/MIG/MPS plans.
10. CRD JSON-patch fragment and sample scheduler ConfigMap.

## Runtime truth

`0.25 GPU` in the scheduler is accounting, not automatically hard CUDA isolation.

- **MIG** gives hardware partitioning when a compatible profile exists.
- **MPS** provides compute sharing; GPU-memory hard limits depend on the installed runtime/device stack.
- **DRA** is the preferred long-term vendor-neutral path; the patch emits DRA intent annotations and leaves
  ResourceClaim/DeviceClaim materialization to the installed driver/controller.
- **full** uses `nvidia.com/gpu`.

## Integration order

1. Copy `overlay/operators/ai-operator/pkg/scheduler/*.go` into the existing scheduler package.
2. Copy `overlay/operators/ai-operator/controllers/gryviaaijob_native_schedule.go`.
3. Apply/regenerate the GryviaAIJob CRD fields or keep annotation-only compatibility.
4. Wire `NativeQueueEngine.PlanAdmission` immediately after the existing quota/budget preflight and before workload creation.
5. Build allocations by listing active GryviaAIJobs and calling `NativeAllocationFromAIJob`.
6. Existing `FindOptimalNodes*` remains responsible for concrete nodes; feed its available capacity into native gang admission.
7. Execute victims only when native preemption is enabled, and checkpoint preemptible training first using the existing checkpoint guard.
8. Apply `PlanGPURuntime`/`ApplyRuntimePlan` while constructing the Job/StatefulSet PodTemplate.
9. Replace/deprecate the root demo topology optimizer after the live collector/GryviaGpuNode labels are wired.

## Suggested opt-in flags

Keep this alpha-safe and preserve Kueue as the default initially:

- `--native-queue-scheduler=false`
- `--native-preemption=false`
- `--fractional-gpu=false`

When native queue scheduling is off, current behavior remains unchanged.

## Validation included

The pure policy/runtime core is formatted and unit-tested in `test-harness`.
Cluster-level GPU isolation still requires a real MIG/MPS/DRA environment.
