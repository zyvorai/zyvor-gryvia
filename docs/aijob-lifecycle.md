# GryviaAIJob lifecycle

How the ai-operator turns a `GryviaAIJob` into pods, which phases a job goes through, who sets them, and which
environment variables the pods get. Behaviour described here is covered by Go unit tests against fake clients
(`operators/ai-operator/controllers/gryviaaijob_workload_test.go`, `operators/quota-operator/controllers/jobstatus_test.go`)
and by the kind end-to-end workflow `.github/workflows/e2e-jobs.yml`. **Nothing here has been run on GPUs or RDMA
hardware**, and Kubernetes behaviour that only a real cluster shows (Job controller, garbage collection, DNS) is
verified only by that workflow.

> **Quota rejection is not race-free by default.** The quota operator rejects a job only while its phase is `Pending` or
> `Queued`. Without the admission gate the ai-operator can create the PVC, Service and workload in the same second, so a job
> that violates a quota (for example an `allowedGPUTypes` mismatch) may already be running when the quota operator looks.
> Turn on `aiOperator.admissionGate` (see below) to check quotas and budgets before anything is created; the kind e2e
> (`e2e-jobs.yml`) enables it for that reason.

## Workload kinds

| `spec.workloadKind` | Workload | Default for | Completes? |
|---|---|---|---|
| `job` | Indexed `batch/v1` Job named `<job>` | `training`, `fine-tuning`, `evaluation` | yes: `Succeeded` / `Failed` |
| `statefulset` | StatefulSet named `<job>-training` (`restartPolicy: Always`) | `inference` | no: pods restart forever |

**Never migrated in place.** If a `<job>-training` StatefulSet (or a Job) controlled by the job already exists, the
controller keeps using it, whatever `spec.workloadKind` or the type says. A StatefulSet created before this change
also keeps its old environment (no `RANK`, unqualified `MASTER_ADDR`) so that an operator upgrade never restarts running
pods (new StatefulSets carry the annotation `gryvia.io/env-version: "2"` on their pod template).

### The batch Job

- `completionMode: Indexed`, `parallelism = completions = distributed.nodes` (1 when not distributed).
- `backoffLimit = spec.retryLimit` (0 means the first failed pod fails the job), `activeDeadlineSeconds` from
  `spec.timeout` (`90m`, `24h`, `7d`, `1d12h`; an invalid value marks the job `Failed`, the webhook rejects it earlier).
- `restartPolicy: Never`. A failed pod is replaced by a **new pod with the same completion index**, so the hostname
  (`<job>-<index>`), the DNS name and `RANK` never change, and every failure is counted against `backoffLimit`.
  `OnFailure` would also keep the rank (the container restarts in the same pod) but hides the failure count, cannot move
  the pod to another node and is incompatible with `podFailurePolicy`. Neither policy restarts the *other* ranks: this
  is not elastic training, and a rank that dies mid-collective leaves its peers hanging until the job's deadline.
- `podFailurePolicy`: a pod failure with condition `DisruptionTarget=True` (eviction, drain, preemption) is ignored, so
  it does not consume `backoffLimit`. Needs Kubernetes 1.26 or newer (unverified on older clusters).
- Pod `subdomain` is the headless Service `<job>-headless`, so pod DNS is `<job>-<index>.<job>-headless.<ns>.svc`. The
  Service publishes not-ready addresses so ranks can resolve each other before they are Ready.
- Same container, volumes (data PVC, memory-backed `/dev/shm`), NCCL/RDMA annotations, `nodeSelector`, tolerations and
  affinity (including the soft fabric-aware preferred terms) as the StatefulSet.
- The pod template is immutable once the Job exists: later edits of image, command or env do not reach it. Only
  `spec.suspend` is synchronised. Delete and resubmit to change a job.
- The Job is owned by the GryviaAIJob and deleted with it (background propagation).

### CPU-only jobs

`spec.gpus: 0` (the webhook allows it; negative is rejected) adds no `nvidia.com/gpu` limit and no `gryvia.io/gpu` node
selector, and skips the GPU placement step (nothing to advise on; `Scheduled` is set with reason `CPUOnly`). GPU
behaviour is unchanged: a job with `gpus >= 1` still stays `Pending` until a node qualifies. The quota operator ignores
jobs with `gpus <= 0`.

### Suspend and Kueue

`spec.suspend: true` creates the Job suspended and keeps it suspended; clearing it unsuspends. If the GryviaAIJob carries
the label `kueue.x-k8s.io/queue-name`, the label is copied to the Job, the Job is created suspended and the controller
stops touching `spec.suspend` afterwards (Kueue admits it), whatever `spec.suspend` says later. With the ai-operator flag
`--kueue-integration` (default off) the queue also comes from `spec.queueName`, the annotation `gryvia.io/queue-name` or
the tenant default LocalQueue, and the Kueue Workload state is reflected in the phase (`Queued` while Kueue holds the job,
`Queued` again after a Kueue eviction). All of that is in [Kueue integration](kueue-integration.md).

## Phase state machine

```
                      (quota operator)             (quota operator)
   "" ──> Pending ──────────────> Queued ──?──>    Rejected*        Cancelled*  <── gryvia cancel / annotation
            │  scheduling ok                          ▲                  ▲
            ▼                                         │ (any non-terminal phase, race)
        Scheduling ──> Running ──> Succeeded*      Preempted (priority controller; workload deleted, parked)
            │             │
            └─────────────┴──────> Failed*                       * = terminal, sticky
```

| Phase | Set by | Meaning | What the ai-operator does |
|---|---|---|---|
| (empty) | - | just created | sets `Pending`, requeues |
| `Pending` | ai-operator | not scheduled yet, or no node qualifies (GPU jobs) | runs the placement step; on failure stays `Pending`, retried every 30 s |
| `Scheduling` | ai-operator | placement done (or CPU-only), workload created, pods not ready; also a suspended Job | creates PVC, Service, workload; requeue 30 s |
| `Running` | ai-operator | Job: a pod is ready or an index succeeded. StatefulSet: a replica is ready | requeue 30 s; never falls back to an earlier phase |
| `Succeeded` | ai-operator | Job condition `Complete` (StatefulSet: all pods reached `Succeeded`, which never happens with `restartPolicy: Always`) | nothing, no requeue |
| `Failed` | ai-operator | Job condition `Failed` (`BackoffLimitExceeded`, `DeadlineExceeded`, ...) with the reason in `status.message`; also an invalid spec | nothing, no requeue |
| `Queued` | quota operator (quota policy `onExceeded: queue`) | held | **creates nothing** (no PVC, Service or workload); returns |
| `Queued` | ai-operator, only with `--kueue-integration` and only for a Job created with a Kueue queue label | Kueue has not admitted the job (or evicted it again) | the Job exists **suspended**, no pods; the controller keeps reconciling (marked by the condition `KueueAdmitted`) and polls every 10 s. See [Kueue integration](kueue-integration.md) |
| `Rejected` | quota operator (quota, budget, quota policy) | refused | creates nothing; deletes a workload, Service and PVC that raced ahead (only objects the job controls); terminal |
| `Preempted` | priority controller | evicted for a higher-priority job | deletes the workload, **keeps the PVC**, sets `completionTime`; the job stays parked (it is not resumed automatically; delete and resubmit) |
| `Cancelled` | `gryvia cancel` (patches `status.phase`), or annotation `gryvia.io/cancel: "true"` (the controller sets the phase) | user stopped it | deletes the workload, **keeps the PVC**, sets `completionTime`; terminal |
| `Unknown` | - | unused | - |

Notes:

- **Terminal states are sticky**: `Succeeded`/`Failed` are never touched again, not even if a workload later reports
  something else; a cancel request on a finished job changes nothing. (`gryvia cancel` writes the status directly, so
  it can still overwrite a finished job's phase with `Cancelled`; usage records that are already final are immutable.)
- **Who sets `Rejected`/`Queued`**: the quota operator (`gryviaquota_controller.go`, `gryviabudget_controller.go`), and only for jobs it sees in `Pending` or `Queued`. Nothing moves a job out of
  `Queued` when the quota operator set it: it is a hold, not a queue with admission (known gap). Queueing with real admission exists only through the opt-in Kueue integration.
- **Race**: the quota operator can see a job in `Pending` while the ai-operator has already moved on. Its patch is not
  optimistic-locked, so `Rejected` wins, and the ai-operator then tears down whatever it had created.
- **Preemption** is marked by the priority controller; nothing checkpoints first (see the code comment there).
- **Usage metering** (`GryviaUsageRecord`) needs `startTime` (set when the job first runs or terminates, taken from the
  Job's `status.startTime`) and `completionTime`; it finalizes on `Succeeded`, `Failed`, `Cancelled` and `Preempted`.
  A job that never started is not metered.
- `status.replicasReady` is the Job's ready pod count, `status.retries` the number of failed pods.
- The gateway's `DELETE /api/jobs/{name}` deletes the GryviaAIJob; the Job/StatefulSet, Service and PVC are
  garbage-collected through their owner references (background deletion of a custom resource; verified by the e2e
  workflow, not on a production cluster).

### Who writes the job status

The ai-operator writes the whole status with optimistic locking. The quota operator holds only a narrow copy of the
status type, so it **never** calls `Status().Update` on a job: it sends a JSON merge patch containing only `phase`,
`message` and the `Rejected` condition (`patchJobPhase`), so `gpusAllocated`, `replicasReady`, `nodesAllocated`,
`placementExplanation`, `metrics`, `startTime` and the ai-operator's own conditions are never dropped. The priority
controller patches `phase` and `message` the same way. Tests capture the exact patch and apply it to a status carrying
all those fields.

## Environment variables

Set only when `distributed.enabled: true`. Your own `spec.env` comes first; the variables below are appended after it
(a later duplicate wins in Kubernetes). Non-distributed jobs get only `spec.env`.

| Variable | Value | Notes |
|---|---|---|
| `MASTER_ADDR` | Job: `<job>-0.<job>-headless.<ns>.svc.cluster.local`. StatefulSet (new): `<job>-training-0.<job>-headless.<ns>.svc.cluster.local`. StatefulSet (created before this change): `<job>-training-0.<job>-headless` | Cluster domain defaults to `cluster.local` (reconciler field `ClusterDomain`, no flag) |
| `MASTER_PORT` | `29500` | port of the headless Service |
| `WORLD_SIZE` | `nodes * gpusPerNode` | number of GPU processes; a CPU-only job (`gpus: 0`) counts one process per node, so `nodes` |
| `RANK` | the pod's index | Job: `fieldRef metadata.annotations['batch.kubernetes.io/job-completion-index']`. StatefulSet: `fieldRef metadata.labels['apps.kubernetes.io/pod-index']` (Kubernetes 1.28+; empty on older clusters) |
| `NODE_RANK` | same as `RANK` | with `gpusPerNode > 1` this is the *node* index; torchrun/deepspeed derive per-process ranks (`--node_rank=$(NODE_RANK)`) and set `RANK` themselves |
| `NNODES` | `distributed.nodes` | Gryvia convenience, not a framework standard |
| `NPROC_PER_NODE` | `gpusPerNode` (1 for a CPU-only job) | Gryvia convenience, not a framework standard |
| `GRYVIA_DIST_FRAMEWORK` | `distributed.framework` | only when set; informational |
| `GRYVIA_DIST_BACKEND` | `distributed.backend` | only when set; informational |
| `NCCL_DEBUG` | `INFO` | not set when `backend: gloo` |
| `NCCL_IB_DISABLE` | `0` | only with `network: rdma` and not `backend: gloo` |
| `NCCL_NET_GDR_LEVEL` | `5` | only with `network: rdma` and not `backend: gloo` |
| `JOB_COMPLETION_INDEX` | the index | injected by Kubernetes itself into every container of an Indexed Job (not by Gryvia) |

Per framework: `framework` is only exported as `GRYVIA_DIST_FRAMEWORK`. The variables above are the PyTorch
`env://` rendezvous set (`MASTER_ADDR`, `MASTER_PORT`, `WORLD_SIZE`, `RANK`), which is what `pytorch` (and the default)
needs. PyTorch reads no backend variable (the backend is an argument of `init_process_group`), so `backend` only
decides whether the NCCL tuning variables are set. TensorFlow (`TF_CONFIG`), Horovod/MPI (hostfile, ssh) and DeepSpeed
(hostfile) get **no** framework-specific configuration: build it in your entrypoint from `MASTER_ADDR`, `NODE_RANK`,
`NNODES` and the pod DNS names.

## RBAC

The chart's manager ClusterRole gains `batch/jobs` (get, list, watch, create, update, patch, delete) and `batch/jobs/status`
(get). The controller watches Jobs it owns (`Owns(&batchv1.Job{})`).

## Order of the gates in `reconcileAIJob`

The reconciler runs these steps in order; each one only ever short-circuits, so a later feature cannot undo an earlier
decision (documented at `reconcileAIJob` in `gryviaaijob_controller.go`):

1. **Cancel annotation**, then the **phase gate**: `Succeeded`/`Failed` are sticky; `Cancelled`/`Preempted` tear the
   workload down (PVC kept); `Rejected` is sticky and also removes anything created before the rejection landed.
2. **`Queued`**: a job held by the quota operator (no `KueueAdmitted` condition) creates nothing; a job waiting for Kueue
   (condition `KueueAdmitted=False`) falls through so its suspended Job keeps being reconciled.
3. **Spec validation** (an invalid `spec.timeout` marks the job `Failed`).
4. **Admission gate** (`--admission-gate`): runs only in phase `Pending`, so it can never act on a job that is `Queued` for
   Kueue or already running; on rejection it sets the sticky `Rejected` phase and creates nothing. It fails open.
5. **Scheduling advice** (`Pending`/`Scheduling`), PVC, headless Service.
6. **Workload**: `buildPodTemplate` applies the reservation toleration and node selector (`gryvia.io/reservation`); the batch
   Job is created suspended with the Kueue queue and priority labels when `--kueue-integration` is on; the phase (including
   `Queued` from Kueue's admission state) is then derived from the workload.

## Unverified

- Everything on GPU and RDMA nodes, and multi-node NCCL rendezvous over the Job pod DNS names.
- `podFailurePolicy` on clusters older than 1.26 and `RANK` from the StatefulSet label before 1.28.
- Kueue interaction: unit-tested with fake clients; the kind workflow `.github/workflows/e2e-kueue.yml` passes in CI (real Kueue, CPU pods). Never run on GPUs.
- Operator behaviour under concurrent writers beyond what optimistic locking and the fake-client tests show.
