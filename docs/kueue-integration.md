# Kueue integration: queues, gang admission, quotas and preemption

Opt-in (default off). With it, [Kueue](https://kueue.sigs.k8s.io) decides **when** a `GryviaAIJob` starts: it holds the job
until **all** of its pods fit the tenant's quota, then lets them all start together; tenants borrow each other's idle
quota, and a higher `spec.priority` job preempts a lower-priority one. Without it nothing here applies and nothing
changes.

**Status: unit-tested with fake clients; the kind end-to-end workflow (`.github/workflows/e2e-kueue.yml`,
`scripts/e2e-kueue.sh`) installs real Kueue on kind and passes in CI with CPU pods. Nothing has run on GPUs.**
Statements about Kueue's runtime behaviour are verified only as far as that workflow goes; anything about GPUs, MultiKueue across real clusters or topology on real hardware is unverified.

> The pinned Kueue (0.19) serves `ClusterQueue` as `v1beta2`, where the cohort field is `spec.cohortName` (`v1beta1` calls it
> `spec.cohort`). The operator's objects join the `gryvia` cohort either way; use the field name of the version you query.

## How it works

```
GryviaTenant q1 ──(quota operator)──> ResourceFlavor gryvia-default, gryvia-compute
   quotas.concurrentGPUs: 2           ClusterQueue   gryvia-q1   (cohort "gryvia", nominal quota 2)
                                      LocalQueue     tenant-q1/gryvia  -> gryvia-q1

GryviaAIJob in tenant-q1 ──(ai-operator)──> batch/v1 Job created SUSPENDED
   spec.priority: 90                        labels: kueue.x-k8s.io/queue-name=gryvia
                                                    kueue.x-k8s.io/priority-class=gryvia-priority-90
                                        Kueue Workload job-<name>-<hash>  -> admitted -> Kueue unsuspends the Job
```

Both operators talk to Kueue with the **unstructured client** (`kueue.x-k8s.io/v1beta1`); there is no Kueue Go
dependency. `v1beta1` is served by Kueue 0.6 up to the pinned 0.19 (its storage version there is `v1beta2`); if a future
Kueue drops `v1beta1` the constants `kueueVersion` (ai-operator) and `kueueAPIVersion`/`kueueGVK` (quota operator) must move.

## Install

Everything on, using the chart's own Kueue sub-chart:

```bash
helm dependency build helm/gryvia          # downloads the NVIDIA and Kueue sub-charts (oci://registry.k8s.io, no repo add)
helm upgrade --install gryvia ./helm/gryvia -n gryvia-system --create-namespace \
  --set kueue.enabled=true \
  --set aiOperator.kueueIntegration=true \
  --set quotaOperator.kueueIntegration=true
```

| Value | Default | Effect |
|---|---|---|
| `kueue.enabled` | `false` | installs Kueue (chart `kueue` **0.19.6**, app v0.19.6) into the release namespace as `kueue-controller-manager`, including its CRDs |
| `aiOperator.kueueIntegration` | `false` | ai-operator flag `--kueue-integration`; adds RBAC on `workloads`, `localqueues` (get/list/watch) and `workloadpriorityclasses` (get/create) |
| `aiOperator.kueueDefaultQueue` | `gryvia` | flag `--kueue-default-queue` |
| `quotaOperator.kueueIntegration` | `false` | quota-operator flag `--kueue-integration`; adds RBAC create/update/delete on `clusterqueues`, `resourceflavors`, `localqueues` |
| `quotaOperator.kueueQuotaResources` | `nvidia.com/gpu` | flag `--kueue-quota-resources`: the resource(s) `concurrentGPUs` is applied to (the kind e2e uses a fake extended resource) |
| `quotaOperator.kueueGpuTypeFlavors` | `false` | flag `--kueue-gpu-type-flavors`, see [Flavors](#resourceflavors-and-gpu-types) |

With every value at its default the chart renders byte-identically to before (no flag, no RBAC, no Kueue objects; the
sub-chart is not rendered).

**Version pin.** The pinned chart 0.19.6 was the newest release in `registry.k8s.io/kueue/charts/kueue` when this was
written (listed from the registry tag index; the chart was pulled and its CRDs and values inspected). It was **not** run on
a cluster. To use another Kueue, install it yourself (leave `kueue.enabled=false`) or change the pin in
`helm/gryvia/Chart.yaml` and run `helm dependency update helm/gryvia` (this rewrites `Chart.lock`); the Configuration
in `values.yaml` (`kueue.managerConfig`) is written for the `config.kueue.x-k8s.io/v1beta2` API of the pinned version.

### Recommended Kueue configuration

The chart's `kueue.managerConfig.controllerManagerConfigYaml` replaces Kueue's default configuration with one that:

- enables **`waitForPodsReady`**: an admitted workload whose pods are not all Ready within `timeout` (10m) is evicted
  and requeued with backoff, and with `blockAdmission: true` no other workload is admitted meanwhile. This is what
  stops two gangs from each pinning half of the cluster's pods (image pulls failing, unschedulable pods, a node lost).
  (The `v1beta2` Configuration has no `enable` field; in the older `v1beta1` Configuration it was
  `waitForPodsReady.enable: true`.) Trade-off: `blockAdmission` serialises admission.
- registers only the **`batch/job`** integration, so Kueue does not put webhooks on every pod, Deployment and
  StatefulSet in the cluster. Jobs without the queue label are never managed (`manageJobsWithoutQueueName` is false).

If you install Kueue yourself, put the same settings in its Configuration.

## Per-tenant objects (quota operator)

For every `GryviaTenant` `t` (only with `--kueue-integration`), a controller named `gryviakueue` (a second controller on the tenant kind; the tenant controller itself is untouched) maintains:

| Object | Name | Content |
|---|---|---|
| `ResourceFlavor` | `gryvia-default` | no node labels; holds the tenant quota |
| `ResourceFlavor` | `gryvia-compute` | no node labels; holds the cpu/memory declarations (a separate flavor because Kueue is believed not to accept one flavor in two resource groups: unverified) |
| `ClusterQueue` | `gryvia-<t>` | `cohort: gryvia`; `namespaceSelector` = `kubernetes.io/metadata.name: tenant-<t>`; `queueingStrategy: BestEffortFIFO`; `preemption: {withinClusterQueue: LowerPriority, reclaimWithinCohort: Any, borrowWithinCohort: {policy: LowerPriority}}`; resource groups below |
| `LocalQueue` | `tenant-<t>/gryvia` | `clusterQueue: gryvia-<t>` |

**Nominal quota** of the quota resource (`nvidia.com/gpu` by default), in order: `spec.quotas.concurrentGPUs` of the tenant;
else `spec.gpuQuota.maxGPUs` of a `GryviaQuota` whose `namespaces` lists `tenant-<t>`; else **unlimited** (`1000000`, because 0
would starve every job). The source is recorded in the annotation `gryvia.io/quota-source` (`tenant`, `quota`, `unlimited`).

**cpu and memory.** Kueue refuses to admit a workload that requests a resource the ClusterQueue does not declare, so
`cpu` and `memory` are always declared, with effectively unlimited nominal quota (`100000` cpu, `1000Ti` memory). Kueue
therefore does **not** account CPU or memory for tenants; only the quota resource is limited. (If you set the quota
resource to `cpu`, cpu is limited and only memory is declared as unlimited.) Any other resource a job pod requests
(`ephemeral-storage`, `rdma/*`, hugepages) is **not** declared and would keep the workload inadmissible: this is
unverified for the GPU path and is the first thing to check if a job stays Queued with an "unavailable in ClusterQueue"
message.

**Borrowing.** All tenant ClusterQueues share the cohort `gryvia`, so idle quota of one tenant can be borrowed by
another (no `borrowingLimit`/`lendingLimit`), and is reclaimed by preemption when its owner needs it. Consequence: a tenant
can temporarily run more than its `concurrentGPUs`. A tenant with the "unlimited" default quota can borrow nothing
useful but also never blocks.

### ResourceFlavors and GPU types

By default there is one flavor, so `concurrentGPUs: 2` means exactly 2 GPUs whatever the type. With
`--kueue-gpu-type-flavors` a `ResourceFlavor` `gryvia-<gpuType>` with `nodeLabels: {gryvia.io/gpu: <gpuType>}` is created
for every enabled `GryviaGpuSku` type and put in every ClusterQueue (sorted, then `gryvia-default` last). Caveats:
each flavor gets the **full** nominal quota, so the total across types can exceed `concurrentGPUs`; and Kueue picks the first flavor
that fits a job's node selector, so an untyped job lands in the first type flavor and is pinned to that node label.
Use it only if jobs always name `spec.gpuType`. Types that are not valid label values are skipped. Flavors are shared and never deleted.

### Cleanup

Cluster-scoped objects cannot have a namespaced owner, so they carry the labels `app.kubernetes.io/managed-by:
gryvia-quota-operator` and `gryvia.io/tenant: <t>`, and the `GryviaTenant` gets the finalizer `gryvia.io/kueue-finalizer`
that deletes the ClusterQueue and LocalQueue when the tenant is deleted. Objects that already exist **without** our
managed-by label are never modified or deleted. Spec changes (a new `concurrentGPUs`) are applied to the ClusterQueue on
the next reconcile (on the tenant update, else every 2 minutes); fields Kueue defaults itself are not treated as drift.
Deleting a ClusterQueue that still has admitted workloads is held by Kueue's own finalizer until they finish.

### Plain namespaces (no tenant)

Not automated. For a namespace with only a `GryviaQuota` (no `GryviaTenant`), create the LocalQueue and ClusterQueue
yourself and name the queue in `spec.queueName` (or the annotation `gryvia.io/queue-name`). The quota only feeds the
tenant queues as a fallback.

## Submitting jobs

The queue of a job, first match wins: `spec.queueName`, annotation `gryvia.io/queue-name`, label
`kueue.x-k8s.io/queue-name` on the GryviaAIJob (W1 passthrough), then, in a `tenant-*` namespace, the default queue
(`gryvia`, flag `--kueue-default-queue`) **if that LocalQueue exists**. Anything else keeps today's behaviour: no
label, the Job runs immediately, nothing waits forever for a queue that does not exist (the condition
`KueueAdmitted=False, reason LocalQueueNotFound` records it). An **explicitly named** queue is trusted without checking: if the
LocalQueue does not exist the job stays `Queued` and Kueue's message says why, rather than silently running outside quota.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata: {name: train, namespace: tenant-q1}
spec:
  type: training
  image: my/trainer:1
  gpus: 8
  priority: 60            # -> WorkloadPriorityClass gryvia-priority-60
  distributed: {enabled: true, nodes: 4, gpusPerNode: 8}
  # queueName: gryvia     # optional in tenant-* namespaces
```

Only `workloadKind: job` (training, fine-tuning, evaluation) is queued. StatefulSet (inference) workloads are not managed by Kueue.
The queue is chosen when the batch Job is **created**; changing `queueName` later has no effect (the Job is immutable
in that respect). `spec.queueName` is ignored, with a log line, when the flag is off.

### Phases

| Situation | Job | GryviaAIJob |
|---|---|---|
| Kueue has not created the Workload yet (or is not running) | suspended | `Queued`, "Waiting for Kueue to create the Workload ..." |
| Workload pending | suspended | `Queued`, `Queued by Kueue: <Kueue's reason, e.g. insufficient quota for ...>` |
| Quota reserved, admission checks pending | suspended | `Queued`, "Quota reserved, waiting for admission checks" |
| Admitted | unsuspended by Kueue | `Scheduling` until a pod is Ready, then `Running` |
| Evicted (preempted, `waitForPodsReady` timeout, ...) | suspended again, pods deleted | `Queued`, `Evicted by Kueue ..., requeued` |
| Finished | Complete / Failed | `Succeeded` / `Failed` (unchanged) |

The phase is polled every 10 s while `Queued` (Workload changes do not trigger the controller; Job changes do). The condition
`KueueAdmitted` (False while waiting, True once admitted) marks a `Queued` phase as "waiting for Kueue": a `Queued` phase
**without** it is the quota operator's hold, which still creates nothing (see [AIJob lifecycle](aijob-lifecycle.md)).

`spec.suspend: true` does **not** pause a Kueue-managed job (Kueue owns `spec.suspend` once the queue label is set; the
controller never re-suspends an admitted Job). Deactivate the Workload (`spec.active: false`) or delete the job instead.

## Priorities and preemption

`spec.priority` (0 to 100) is bucketed: `n = priority / 10 * 10` (integer division), giving the cluster-scoped
`WorkloadPriorityClass` `gryvia-priority-<n>` with `value: n`. The ai-operator creates a class the first time a job
needs it (label `app.kubernetes.io/managed-by: gryvia-ai-operator`) and never modifies an existing one. Priority 0 to 9
(bucket 0, Kueue's default priority 0) gets no label and no class. If the class cannot be created (RBAC), the job queues at
the default priority and the error is logged.

Preemption is per the ClusterQueue spec above: a pending job that does not fit may evict **lower-priority** admitted
workloads in its own ClusterQueue (`withinClusterQueue: LowerPriority`), a tenant may reclaim quota lent to others
(`reclaimWithinCohort: Any`), and it may evict lower-priority borrowers in the cohort to borrow itself
(`borrowWithinCohort: LowerPriority`). Equal priority never preempts. Preemption is a decision about **quota**, whatever
the pods are doing: Kueue suspends the Job and its pods are deleted gracefully. A job with a checkpoint hook
(`gryvia.io/checkpoint-command`, see [admission-recovery.md](admission-recovery.md#cooperative-checkpoints)) checkpoints
in that window: the hook is the trainer's `preStop`, and `gryvia.io/checkpoint-grace-seconds` bounds it. Without a hook,
nothing checkpoints first. `scripts/e2e-kueue.sh preempt-checkpoint` checks this on kind: a 2-worker job with
`retryLimit: 0` is preempted, each worker saves only when its hook asks, both resume from a step above 0 after
readmission and the job Succeeds, so the eviction did not consume `retryLimit`.

**Decision: a Kueue eviction maps back to `Queued`, not to the terminal `Preempted`.** The W1 `Preempted` semantics
(workload deleted, PVC kept, job parked) would throw away Kueue's requeueing. Instead the Job stays (suspended), Kueue
requeues the Workload, and the job resumes from scratch (or from its own checkpoints on the kept PVC) when it fits.
Metering is per admitted run, so time spent requeued **is not counted**. The first run's usage record
(`usage-<job uid>`) starts at the job's `startTime`. When Kueue evicts the job, the record is finalized at the
transition time of the `KueueAdmitted` condition to False (the eviction). When Kueue admits the job again, a new
record `usage-<job uid>-<n>` starts at that condition's transition to True, and ends at the next eviction or at the
job's end. A job cancelled while requeued opens no new record. All records of a job carry its `jobUID` and the
`gryvia.io/job` label; budgets and chargebacks sum them. `scripts/e2e-kueue.sh preempt` checks the two records of the
preempted job. If the quota operator misses a whole eviction and readmission (for example, it is down for that time),
the open record keeps running across the gap, as before.

## Gang semantics

"All pods admitted together" means: Kueue reserves quota for every pod of the Job in one step (`parallelism` =
`completions` = `distributed.nodes`) and only then unsuspends the Job, so a gang that does not fit has **zero pods**
and never holds part of the quota. Limits:

- It is **quota** admission, not placement. Kueue does not look at free node capacity: an admitted gang can still have
  unschedulable pods (no node with the resources). `waitForPodsReady` turns that into an eviction and a retry with
  backoff instead of a hang. Without it, an admitted-but-unschedulable gang holds its quota forever.
- No topology-aware placement (Kueue's Topology API is not wired; `gryvia.io/interconnect` scoring is unchanged).
- Elastic jobs (`distributed.elastic.minNodes < nodes`) are the exception: Kueue may admit them with fewer pods, down to `minNodes` (partial admission; see [elastic-training.md](elastic-training.md#under-kueue)). The size is fixed at admission; a running Job is not resized.
- Multi-cluster (MultiKueue): Gryvia only binds the tenant ClusterQueues to an administrator-created MultiKueue admission check (`platformCompletion.kueueAdmissionCheck`). `.github/workflows/e2e-multikueue.yml` (two kind clusters, CPU pods) passes in CI (run 36875403171): a GryviaAIJob submitted on the manager is admitted by Kueue "on worker1", its pod runs on the worker cluster (none on the manager) and Succeeded comes back to the manager. The same workflow now runs a second worker and a GryviaFederation with automatic failover: a worker is stopped, fenced by lease timeout, its Workload evicted and redispatched, and the trainer resumes from an S3 replica (see [federation failover](federation-failover.md)). Not tested: GPUs, cost aggregation.
- CPU and memory are not accounted (see above); only the quota resource is.
- Kueue's DRF/fair-sharing modes, `admissionFairSharing` and hierarchical cohorts are not configured.
- The unused in-tree gang scheduler and queue packages were removed; Kueue replaces them.

## Troubleshooting

```bash
kubectl get workloads,clusterqueues,localqueues -A            # Kueue's own state
kubectl -n tenant-q1 describe workload <job-name>-...         # Workload named job-<name>-<hash>: conditions say why pending
kubectl describe clusterqueue gryvia-q1                       # usage, pending/admitted counts, flavors
kubectl get resourceflavors,workloadpriorityclasses
kubectl -n tenant-q1 get gryviaaijob train -o jsonpath='{.status.message}{"\n"}{.status.conditions}'
kubectl -n gryvia-system logs deploy/kueue-controller-manager
```

| Symptom | Likely cause |
|---|---|
| Job `Queued`, message "Waiting for Kueue to create the Workload" | Kueue is not running or does not manage `batch/job` (integration list), or the ai-operator lacks RBAC on workloads |
| Message "... unavailable in ClusterQueue" / "no flavors" | the job pod requests a resource the ClusterQueue does not declare (see cpu/memory above), or the GPU-type flavor does not match `spec.gpuType` |
| Message about a missing LocalQueue | `spec.queueName` names a queue that does not exist in the namespace |
| No ClusterQueue for a new tenant | quota operator not started with `--kueue-integration`, missing RBAC, or Kueue CRDs absent (it retries every minute) |
| Job runs at once, no `Queued`, no label on the Job | ai-operator flag off, namespace is not `tenant-*` without a queue name, or no LocalQueue `gryvia` (see the condition `KueueAdmitted` reason `LocalQueueNotFound`) |
| Job stays `Queued` although quota is free | a workload ahead is blocked by `waitForPodsReady` + `blockAdmission`, or the ClusterQueue is not `Active` (missing ResourceFlavor) |
| An unexpected tenant runs above its quota | borrowing inside the cohort (by design) |

## Limits and what is unverified

- **Verified on kind only**: `.github/workflows/e2e-kueue.yml` passes in CI (queueing, gang admission without partial starts, priority preemption with requeue, a checkpoint in the preStop hook of the preempted job) with CPU pods and a fake extended resource as the quota. No GPUs, no multi-node clusters, no NCCL.
- **Kueue version**: pinned 0.19.6 (`v1beta1` API and `v1beta2` Configuration). Other versions untested.
- Kueue fields relied on (all `kueue.x-k8s.io/v1beta1` unless noted): ClusterQueue `spec.cohort`, `namespaceSelector`,
  `queueingStrategy`, `preemption.{withinClusterQueue,reclaimWithinCohort,borrowWithinCohort.policy}`,
  `resourceGroups[].{coveredResources,flavors[].{name,resources[].{name,nominalQuota}}}`, `status.{pendingWorkloads,reservingWorkloads}` (e2e only);
  LocalQueue `spec.clusterQueue`; ResourceFlavor `spec.nodeLabels`; WorkloadPriorityClass top-level `value`, `description`;
  Workload `metadata.ownerReferences` (the Job), `spec.priority` (e2e), `status.conditions[]` types `Admitted`, `QuotaReserved`,
  `Evicted`, `Finished` with `reason`/`message`; Job labels `kueue.x-k8s.io/queue-name` and `kueue.x-k8s.io/priority-class`;
  Configuration (`config.kueue.x-k8s.io/v1beta2`) `waitForPodsReady.{timeout,blockAdmission,requeuingStrategy}` and `integrations.frameworks`.
- Not checked offline: that the Workload's `Evicted` condition stays visible after a requeue (the operator does not depend on it: "was
  admitted, is suspended now" is enough), Kueue's exact pending messages, and that a flavor may not be shared between resource groups.
- A checkpoint on preemption needs the job's own hook (see above); Kueue does not wait for it beyond the pod's grace period. No resize after admission, topology or multi-cluster.
- Removing the chart removes Kueue's CRDs with it (they are templated in the sub-chart) and therefore every ClusterQueue, LocalQueue and Workload.
- Quota is enforced by Kueue **only for jobs that have a queue**: a job in a namespace without a LocalQueue (or with the
  integration off) bypasses it. The existing webhook and quota-operator policies (GPU type allow-lists, per-job limits,
  `Rejected`) still apply to every job.

## Strict admission and recovery

See [Reliable admission and cooperative recovery](admission-recovery.md) for opt-in fail-closed tenant queues, queue-managed GPU demand, worker spreading and checkpoint hooks.
