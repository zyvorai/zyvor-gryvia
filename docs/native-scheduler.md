# Native queue scheduler

An opt-in, in-tree admission gate for `GryviaAIJob`: queue guarantees and ceilings with borrowing, atomic
gang admission and conservative priority preemption, with fractional GPU requests accounted in milli-GPUs. It is off by default; with the flag off, behaviour is unchanged.

Status: **alpha, opt-in.** The engine and the gate are unit-tested against fake clients (`go test
./pkg/scheduler ./controllers`). It has not been verified on real GPUs, and its end-to-end behaviour on a
cluster is not covered by a CI workflow yet. Kueue stays the supported production path for gang admission
([kueue-integration.md](kueue-integration.md)); pick one, they are mutually exclusive.

## What it does

| Capability | Behaviour |
|---|---|
| Fair-share accounting | Per-queue usage, guarantee and weighted dominant share are computed (`used / guarantee`, divided by `weight`, halved while under guarantee). In the gate this orders **preemption victims** (most over-share first); it does not reorder waiting jobs |
| Guarantees and ceilings | `guaranteeGPU` is the queue's entitlement, `maxGPU` a hard cap (0 = none) |
| Borrowing | A queue with `borrow: true` may exceed its guarantee into idle capacity; with `borrow: false` it is held at the guarantee |
| Gang admission | A job is admitted only if its `minAvailable` pods fit at once (all-or-nothing). Distributed jobs use `distributed.nodes`; elastic jobs use `distributed.elastic` minNodes |
| Priority preemption | A higher `spec.priority` job may evict lower-priority allocations that are explicitly marked preemptible. Equal or higher priority is never evicted, and another queue's guarantee is preserved |
| Fractional GPU | `gryvia.io/gpu-share` requests part of a GPU; accounted in milli-GPU (1 GPU = 1000) |

What it does **not** do: reorder pending jobs (each job is decided on its own when it reconciles, so admission is
effectively first-come among jobs that fit; `NativeQueueEngine.Order` exists in `pkg/scheduler` but the
controller does not call it yet), pick nodes (the existing `FindOptimalNodes*` placement still does), backfill by
time, hierarchical queues, or time-sliced fairness. Live telemetry scoring (`LiveTelemetryScore`) exists as
a library contract in `pkg/scheduler` but is not called by the controller yet.

## Enable it

Helm:

```bash
helm upgrade --install gryvia ./helm/gryvia -n gryvia-system \
  --set aiOperator.nativeQueueScheduler=true
kubectl apply -f deploy/native-scheduler-config.yaml
```

Or the operator flag `--native-queue-scheduler=true`. Setting it together with `--kueue-integration` (or
`aiOperator.kueueIntegration`) is rejected at start-up / template time.

## Define queues

Policy is read from the ConfigMap `gryvia-native-scheduler` in `gryvia-system`, key `policy.yaml`. GPU values
are **milli-GPU** (8000 = 8 GPUs). It is re-read on every admission decision, so edits apply without a
restart.

```yaml
apiVersion: v1
kind: ConfigMap
metadata: {name: gryvia-native-scheduler, namespace: gryvia-system}
data:
  policy.yaml: |
    queues:
      - name: research
        guaranteeGPU: 8000
        maxGPU: 16000
        weight: 1
        borrow: true
      - name: production
        guaranteeGPU: 8000
        maxGPU: 32000
        weight: 2
        borrow: true
```

## Submit jobs

A job's queue is, in order: annotation `gryvia.io/native-queue`, `spec.queueName`, then the job's namespace.
The queue must exist in the policy.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: train-a
  annotations:
    gryvia.io/native-queue: research
    gryvia.io/native-preemptible: "true"   # may be evicted by higher-priority work
spec:
  type: training
  image: registry.example/train:1
  gpus: 2
  priority: 0
```

| Setting | Where | Meaning |
|---|---|---|
| `gryvia.io/native-queue` | annotation | Queue name |
| `gryvia.io/native-preemptible` | annotation | `"true"` allows this job to be a preemption victim (default: never preempted) |
| `spec.priority` | spec | Higher wins; only strictly lower-priority preemptible jobs can be evicted |
| `spec.distributed.nodes` / `gpusPerNode` | spec | Gang size and GPUs per member |
| `gryvia.io/gpu-share` | annotation | Fraction or count, e.g. `0.25`, `0.5`, `2` |
| `gryvia.io/gpu-memory` | annotation | GPU memory MiB (used to pick a MIG profile) |
| `gryvia.io/gpu-isolation` | annotation | `auto` (default), `full`, `mig`, `mps`, `dra` |
| `gryvia.io/mig-profile` | annotation | Explicit MIG profile, e.g. `1g.10gb` |

## Job lifecycle with the gate on

1. New job: `Pending`. The gate computes free GPUs (sum of node allocatable `nvidia.com/gpu`, minus
   milli-GPU held by `Scheduling`/`Running` jobs and by `Pending` jobs it already admitted).
2. **Admitted:** condition `NativeAdmitted=True`, the job proceeds to placement as usual.
3. **Not admissible now** (queue max exceeded, gang does not fit, no safe preemption plan): phase `Queued`,
   condition `NativeQueued=True`, message says why, event `NativeQueued`. It is re-evaluated every 15
   seconds and released back to `Pending` when it fits.
4. **Preemption:** chosen victims are set to `Preempted` (event `NativePreempted`); the existing
   `Preempted` path tears their workload down.
5. **Fail-open:** if the policy ConfigMap is missing/invalid, the queue is unknown, or the cluster cannot be
   listed, the job proceeds and gets an `AdmissionUnchecked` warning condition.

```bash
kubectl get gryviaaijob -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,MSG:.status.message
kubectl describe gryviaaijob train-a        # conditions and NativeQueued / NativePreempted events
```

## Fractional GPUs

The scheduler accounts `0.25 GPU` as 250 milli-GPU, but isolation is delegated to the runtime:

| Mode | Result on the pod | Isolation |
|---|---|---|
| `full` | `nvidia.com/gpu: N` | whole device |
| `mig` | `nvidia.com/mig-<profile>: 1` | hardware partition, needs a MIG-enabled node |
| `mps` | `nvidia.com/gpu: 1` + `CUDA_MPS_ACTIVE_THREAD_PERCENTAGE` | compute share; memory limits depend on the runtime |
| `dra` | `gryvia.io/dra-milli-gpu` annotations only | needs a DRA driver to create the claim |

`auto` picks `full` for whole GPUs, otherwise MPS. The controller does not pass node labels at template
time yet, so `auto` never selects MIG or DRA by itself: set `gryvia.io/gpu-isolation` explicitly.

## Limits and known gaps

- Capacity is read from node allocatable `nvidia.com/gpu`. A node with no GPU resource contributes 0, so on a
  GPU-less cluster every GPU job queues unless you advertise capacity yourself.
- Preemption is deferred, not executed, while a `GryviaCheckpointGuard` is active (`--checkpoint-guard`):
  the gate does not checkpoint victims itself.
- `crds/patches/gryviaaijob-native-scheduler.jsonpatch.yaml` is a reference for promoting `gpuShare`,
  `gpuIsolation` and `minAvailable` to typed spec fields; the annotations above are what the code reads today.
- Placement is unchanged: a job the gate admits can still wait in `Pending` if no node fits.
- Not validated on GPU hardware.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Job stays `Pending`, no `NativeAdmitted` | Gate off (check the ai-operator args for `--native-queue-scheduler=true`) or not reached; see `AdmissionUnchecked` |
| `AdmissionUnchecked` with `NativePolicyUnavailable` | ConfigMap `gryvia-native-scheduler` missing or `policy.yaml` invalid |
| `NativePlanFailed` | Queue not defined in the policy |
| Everything `Queued: insufficient capacity` | No node advertises `nvidia.com/gpu`, or capacity is fully used and nothing is preemptible |
| Preemption never happens | Victims need `gryvia.io/native-preemptible: "true"` and strictly lower `spec.priority`; checkpoint guard defers it |
