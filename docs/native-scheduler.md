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
| Fair-share accounting | Per-queue usage, guarantee and weighted dominant share are computed (`used / guarantee`, divided by `weight`, halved while under guarantee). The gate orders waiting jobs by priority, weighted share and FIFO (job ID breaks timestamp ties); it also orders preemption victims |
| Guarantees and ceilings | `guaranteeGPU` is the queue's entitlement, `maxGPU` a hard cap (0 = none) |
| Borrowing | A queue with `borrow: true` may exceed its guarantee into idle capacity; with `borrow: false` it is held at the guarantee |
| Gang admission | The gate reserves the full declared replica shape. The engine can plan elastic minimums, but the gate holds partial plans until workload resizing can consume them safely |
| Priority preemption | A higher `spec.priority` job may evict lower-priority allocations that are explicitly marked preemptible. Equal or higher priority is never evicted, and another queue's guarantee is preserved |
| Fractional GPU | `gryvia.io/gpu-share` requests part of a GPU; accounted in milli-GPU (1 GPU = 1000) |

Waiting jobs are ordered before each admission, independently of reconcile order. The gate skips candidates whose quota/gang plan cannot be admitted, so an oversized head does not block smaller fitting jobs. Already-admitted Pending jobs retain their reservation while placement retries. This ordering checks cluster capacity and queue policy; it does not guarantee that a candidate's exact node requirements fit.

What it does **not** do: backfill by predicted duration, hierarchical queues, multi-resource DRF, or time-sliced fairness. It remains an opt-in Kubernetes admission gate, not a macOS/Metal runtime.

GPU jobs fail closed when policy, queue identity, active allocation accounting or capacity reads cannot be validated: they stay native-Queued with a reason and retry every 15 seconds. CPU-only jobs bypass the GPU queue gate. Fixing the policy releases held jobs normally; running jobs are unaffected by this Pending-job gate.

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
| `gryvia.io/gpu-memory` | annotation | GPU memory MiB (picks a MIG profile; with MPS, becomes the client's device-memory limit) |
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
5. **Fail-closed:** missing/invalid policy, unknown queues and unreadable capacity/accounting hold GPU jobs in `Queued` with a `NativeQueued` reason. No new workload is created. CPU-only jobs bypass this GPU gate.

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
| `mps` | `nvidia.com/gpu: 1` + `CUDA_MPS_ACTIVE_THREAD_PERCENTAGE`, and `CUDA_MPS_PINNED_DEVICE_MEM_LIMIT=0=<MiB>M` when `gpu-memory` is set | compute share and, with `gpu-memory`, a per-client memory limit; both are honoured only when an MPS control daemon serves the GPU (not verified on GPUs) |
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
- Partial elastic admission is held until the full declared shape fits; the current builder cannot apply the engine's partial replica allocation safely. This prevents launching beyond queue ceilings.
- Not validated on GPU hardware.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Job stays `Pending`, no `NativeAdmitted` | Gate off (check the ai-operator args for `--native-queue-scheduler=true`) or not reached |
| `NativeQueued` with `NativePolicyUnavailable` | ConfigMap `gryvia-native-scheduler` missing or `policy.yaml` invalid |
| `NativeQueueUnknown` | Queue not defined in the policy |
| Everything `Queued: insufficient capacity` | No node advertises `nvidia.com/gpu`, or capacity is fully used and nothing is preemptible |
| Preemption never happens | Victims need `gryvia.io/native-preemptible: "true"` and strictly lower `spec.priority`; checkpoint guard defers it |

## Opt-in fresh telemetry ranking

Set `gryvia.io/live-telemetry: "true"` on a `GryviaAIJob` to add up to 25 advisory placement points through `LiveTelemetryScore`. Default placement is unchanged. Nodes must have both labels `gryvia.io/gpu-utilization` and `gryvia.io/gpu-memory-used-percent` (finite 0..100), and annotation `gryvia.io/telemetry-observed-at` (RFC3339, no more than 60 seconds old and not in the future). Optional NVLink/RDMA/storage health labels must be valid if present. Missing, stale or invalid observations contribute zero points and never change admission capacity.

An external collector must publish these observations; this change does not add a DCGM scraper. Runtime/hardware validation is still required before relying on the ranking.
