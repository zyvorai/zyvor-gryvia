# Native Queue Scheduler

An opt-in, in-tree admission gate for `GryviaAIJob`: queue guarantees and ceilings with borrowing, all-or-nothing gang admission, conservative priority preemption, and fractional GPU requests. **Alpha:** unit-tested against fake clients, not run on GPUs, and off by default. Kueue remains the supported production path; the two are mutually exclusive.

Full reference: [docs/native-scheduler.md](https://github.com/zyvorai/gryvia/blob/main/docs/native-scheduler.md). For how it sits next to Kueue and the other scheduling pieces, see [Scheduling](SCHEDULING.md).

## 1. Turn it on

```bash
helm upgrade --install gryvia ./helm/gryvia -n gryvia-system \
  --set aiOperator.nativeQueueScheduler=true
kubectl apply -f deploy/native-scheduler-config.yaml
```

The operator flag is `--native-queue-scheduler=true`. Combining it with `--kueue-integration` is rejected.

## 2. Define queues

The ConfigMap `gryvia-native-scheduler` (namespace `gryvia-system`, key `policy.yaml`) holds the queues. Amounts are **milli-GPU**: `8000` is 8 GPUs. It is re-read on every decision.

```yaml
queues:
  - name: research
    guaranteeGPU: 8000   # entitlement
    maxGPU: 16000        # hard cap, 0 = none
    weight: 1
    borrow: true         # may exceed the guarantee into idle capacity
  - name: production
    guaranteeGPU: 8000
    maxGPU: 32000
    weight: 2
    borrow: true
```

## 3. Submit jobs

The queue is the annotation `gryvia.io/native-queue`, else `spec.queueName`, else the job's namespace.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: train-a
  annotations:
    gryvia.io/native-queue: research
    gryvia.io/native-preemptible: "true"
spec:
  type: training
  image: registry.example/train:1
  gpus: 2
  priority: 0
```

| Setting | Meaning |
|---------|---------|
| `gryvia.io/native-preemptible: "true"` | The job may be evicted by a strictly higher `spec.priority` job (default: never) |
| `spec.distributed.nodes`, `gpusPerNode` | Gang size; all members must fit or the job waits |
| `gryvia.io/gpu-share` | `0.25`, `0.5`, `2`, ... accounted in milli-GPU |
| `gryvia.io/gpu-isolation` | `auto`, `full`, `mig`, `mps`, `dra` (set it explicitly for MIG or DRA) |
| `gryvia.io/gpu-memory`, `gryvia.io/mig-profile` | Memory request (picks a MIG profile, or sets the MPS device-memory limit) and explicit MIG profile |

## 4. Read the result

```bash
kubectl get gryviaaijob -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,MSG:.status.message
kubectl describe gryviaaijob train-a
```

| You see | Meaning |
|---------|---------|
| Condition `NativeAdmitted=True` | The gate admitted it; placement proceeds as usual |
| Phase `Queued`, condition `NativeQueued`, event `NativeQueued` | Did not fit now (queue max, gang, or no safe preemption); re-checked every 15 s |
| Phase `Preempted`, event `NativePreempted` | Evicted by a higher-priority job |
| Condition `NativeQueued` with a policy/accounting/list reason | Admission is blocked until policy or capacity can be validated |

## Know the limits

- Capacity is the sum of node allocatable `nvidia.com/gpu`. A node with no GPU resource contributes zero, so on a GPU-less cluster every GPU job queues unless capacity is advertised.
- Waiting jobs are ordered by priority, weighted share and FIFO before admission; blocked quota/gang candidates are skipped. Existing admitted reservations are retained.
- Preemption is deferred while a `GryviaCheckpointGuard` is active; the gate does not checkpoint victims itself.
- Isolation is the runtime's job: with MPS the pod gets `CUDA_MPS_ACTIVE_THREAD_PERCENTAGE` and, when `gpu-memory` is set, `CUDA_MPS_PINNED_DEVICE_MEM_LIMIT`; both only take effect when an MPS control daemon serves the GPU (not verified on GPUs). DRA only emits intent annotations.
- Per-job `gryvia.io/live-telemetry: "true"` enables bounded ranking from validated node telemetry no more than 60 seconds old; missing/invalid observations have no effect. An external collector must supply labels and the observation timestamp.

GPU jobs now wait on invalid/missing policy, unknown queues or unreadable capacity/accounting. CPU-only jobs bypass GPU admission. See `docs/native-scheduler.md` in the repository for observation fields and recovery behavior.
