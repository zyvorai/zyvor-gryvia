# Scheduling Guide

What Gryvia does today when a `GryviaAIJob` is submitted, and which of the more advanced scheduling ideas (gang scheduling, DRF fair share, backfill, elastic training, preemption) are Kueue-backed, opt-in, or only a design.

## Status at a glance

| Capability | Status |
|------------|--------|
| GPU-aware node selection (filter and score nodes, recorded in `status.nodesAllocated`) | Implemented and run by the GryviaAIJob controller. Advisory: see [What runs today](#what-runs-today) |
| Indexed batch Job (training, fine-tuning, evaluation) or StatefulSet (inference), headless Service and PVC creation, NCCL/`MASTER_ADDR`/`WORLD_SIZE`/`RANK` env for distributed jobs | Implemented; unit-tested against fake clients, kind end-to-end in CI, not run on GPUs. See [AIJob lifecycle](https://github.com/zyvorai/gryvia/blob/main/docs/aijob-lifecycle.md) |
| Validating admission webhook (job sanity, quota and SKU policy) | Implemented, served by the ai-operator when enabled; fails open by default |
| Gang admission, queueing, quotas, borrowing between tenants, priority preemption | **Opt-in, backed by Kueue** (`--kueue-integration`, off by default): the Job is created suspended and Kueue admits all its pods together. Unit-tested with fake clients; the kind workflow `e2e-kueue.yml` (real Kueue, CPU pods) passes in CI; not run on GPUs. See the [Kueue integration](https://github.com/zyvorai/gryvia/blob/main/docs/kueue-integration.md) |
| In-tree gang scheduler and DRF queue | Removed (they were never called); Kueue does gang admission and queueing when enabled |
| Placement holds (`--placement-holds`) | **Opt-in, off by default.** While a job's pods come up, the GPUs of the nodes it was placed on count as used for other jobs' placement, so two jobs placed at the same moment do not both take the same free GPUs. In memory, expires after 5 minutes. It does not pin pods; see [Placement holds](#placement-holds-opt-in). Unit-tested with fake clients; nothing on GPUs |
| Backfill | Not implemented (Kueue's `BestEffortFIFO` lets smaller jobs run past a blocked one, which is not time-based backfill) |
| Elastic training | Partial, unit-tested only: `distributed.elastic.minNodes` bounds the worker count (launcher `NNODES=min:max`, Job success at `minNodes`, placement between min and max); no resizing of a running job. See [Elastic training](https://github.com/zyvorai/gryvia/blob/main/docs/elastic-training.md) |
| Mutating webhook (NCCL injection and defaults) | Code exists in `pkg/webhook/mutator.go`; not registered in `main.go`, so it does not run |
| Priority preemption | With Kueue integration: a higher `spec.priority` job preempts lower-priority jobs of the same ClusterQueue (and of borrowing tenants in the cohort); the victim is requeued (`Queued`), not lost. Without Kueue: Kubernetes pod preemption only, for jobs annotated `gryvia.io/priority-class` with a `GryviaPriority` (which creates the PriorityClass) |
| Fabric-health penalty on node scores | Opt-in (`aiOperator.fabricAwareScheduling`): nodes whose fresh `GryviaNodeFabric` reports a sick fabric lose up to 25 points, and the reason is in `status.placementExplanation`. Tested against fakes only, unverified on GPU or RDMA hardware; see [Fabric scheduling](https://github.com/zyvorai/gryvia/blob/main/docs/fabric-scheduling.md) |

The remaining sections describe the running behaviour first, then the designs. Sections marked "Design" are not what a cluster does today. Nothing here has been verified on real GPU hardware; the operator logic is covered by Go unit tests against fake clients.

## What runs today

The GryviaAIJob controller (`operators/ai-operator/controllers/gryviaaijob_controller.go`) reconciles each job through these steps:

1. New jobs get `status.phase: Pending`.
2. **Node selection.** `scheduler.FindOptimalNodes` lists nodes and GPU-consuming pods, then filters and scores nodes:
   - Filters: node is Ready; `gryvia.io/gpu` label matches `spec.gpuType` (unless empty or `any`); `gryvia.io/rdma=true` when `spec.network: rdma`; `gryvia.io/sriov=true` when `spec.network: sriov`; `spec.nodeSelector` matches; enough free GPUs (from the `gryvia.io/gpu-count` label or `nvidia.com/gpu` allocatable, minus GPUs requested by running pods).
   - Score: +50 for a GPU-type match, +30 for RDMA when requested, +40 (NVSwitch) or +30 (NVLink) from the `gryvia.io/interconnect` label for multi-GPU jobs, +5 per free GPU, +1 per 10 GB of `gryvia.io/gpu-memory`, plus a small general node-resource term.
   - The top `distributed.nodes` nodes (1 when not distributed) are written to `status.nodesAllocated`. If no node qualifies, the `Scheduled` condition is set to false with the reason and the job is retried after 30 seconds.
3. Creates a PVC when `spec.storage` is set, a headless Service, and the workload with one `trainer` container. Training, fine-tuning and evaluation jobs get an **Indexed `batch/v1` Job** named `<job>` (parallelism = completions = `distributed.nodes`, `restartPolicy: Never`, `backoffLimit` = `spec.retryLimit`, `activeDeadlineSeconds` from `spec.timeout`); inference jobs get a StatefulSet named `<job>-training`. `spec.workloadKind` (`job` or `statefulset`) overrides the default, and a job that already has a StatefulSet keeps it.
4. Derives status from the workload. For a Job: `Scheduling` until a pod is ready, `Running`, then `Succeeded` when the Job is Complete or `Failed` (with a message such as `BackoffLimitExceeded` or `DeadlineExceeded`) when the Job fails. Terminal phases are final. For a StatefulSet: `Running` once a replica is ready; pods restart forever, so it never completes.

With `--kueue-integration` the Job of a job that has a queue is created suspended and Kueue decides when it starts; the job shows `Queued` with Kueue's reason while it waits (see [Gang Scheduling](#gang-scheduling)). `Queued`, `Rejected`, `Preempted` and `Cancelled` are honoured: nothing is created while a job is `Queued` or `Rejected`, and `Cancelled`/`Preempted` delete the workload (the PVC is kept). Full state machine: [AIJob lifecycle](https://github.com/zyvorai/gryvia/blob/main/docs/aijob-lifecycle.md).

Important limits of the current implementation:

- `status.nodesAllocated` is **not** turned into a node binding. The pod template carries `nodeSelector` (`gryvia.io/gpu`, `gryvia.io/rdma`, plus your own), tolerations and affinity, and the Kubernetes scheduler places the pods. The operator's node choice is recorded, not enforced. In effect the operator's step is a feasibility check plus advisory status: it fails (job stays Pending, retried every 30 seconds) when no node qualifies, but it does not decide where pods run. The `gryvia.io/gpu`, `gryvia.io/gpu-count`, `gryvia.io/rdma`, `gryvia.io/sriov` and `gryvia.io/interconnect` node labels it relies on are set by the `GryviaGpuNode` controller (from the resource's spec and GPU feature discovery data).
- Pods request `nvidia.com/gpu` (per pod: `distributed.gpusPerNode` when distributed, otherwise `spec.gpus`), so the NVIDIA device plugin is required.
- `spec.priority` and `spec.model` are accepted by the schema (priority is range-checked 0 to 100 by the webhook) but the controller does not act on them today. `spec.retryLimit` and `spec.timeout` apply to the batch Job workload only.
- `spec.gpus: 0` is a CPU-only job: no `nvidia.com/gpu` limit, no GPU node selector and no GPU placement step (used for CI on clusters without GPUs).
- A Job's pod template is immutable once created, so later edits of the image, command or env do not reach a running Job (`spec.suspend` is the exception). A StatefulSet is updated in place for image, command, args, env, GPU count and node selector.

### A distributed job that validates against the CRD

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: distributed-llm-training
spec:
  type: training
  image: nvcr.io/nvidia/pytorch:24.01-py3
  gpus: 8
  gpuType: H100
  network: rdma
  distributed:
    enabled: true
    framework: pytorch
    backend: nccl
    nodes: 4
    gpusPerNode: 8
  command: ["torchrun", "--nproc_per_node=8", "train.py"]
```

For a distributed job the controller creates 4 replicas with 8 GPUs each and injects `MASTER_ADDR`, `MASTER_PORT=29500`, `WORLD_SIZE` (nodes times GPUs per node) and `NCCL_DEBUG=INFO`; with `network: rdma` it also sets `NCCL_IB_DISABLE=0` and `NCCL_NET_GDR_LEVEL=5`, adds the `gryvia.io/rdma` annotation and the `rdma-network` network attachment annotation, and mounts a memory-backed `/dev/shm`. RDMA behaviour has not been verified on hardware.

---

## Gang Scheduling

Status: **Kueue-backed and opt-in.** Without `--kueue-integration` a multi-node job becomes one Indexed Job (all pods are created at once) or, for inference and `workloadKind: statefulset`, one StatefulSet, and the Kubernetes scheduler places each pod independently: nothing reserves the whole set of GPUs up front, so two large jobs can each end up holding part of the cluster.

With `--kueue-integration` on both operators (and Kueue installed, for example `kueue.enabled=true` in the chart) the ai-operator creates the Job **suspended** with the label `kueue.x-k8s.io/queue-name`. Kueue reserves quota for all `distributed.nodes` pods at once and only then unsuspends the Job, so a gang that does not fit stays entirely un-started (zero pods) instead of holding part of the cluster. The queue comes from `spec.queueName`, the annotation `gryvia.io/queue-name`, or the tenant's default LocalQueue `gryvia` in `tenant-*` namespaces. What "all pods admitted together" does and does not guarantee (quota, not node placement; `waitForPodsReady`; no topology-aware placement) is in the [Kueue integration](https://github.com/zyvorai/gryvia/blob/main/docs/kueue-integration.md).

**Verification:** unit tests against fake clients and a kind workflow (`.github/workflows/e2e-kueue.yml`, real Kueue, CPU pods) that passes in CI; nothing was run on GPUs or with real multi-node NCCL.

There is no in-tree gang scheduler (an unused one was removed in favour of Kueue) and no `spec.scheduling` field on `GryviaAIJob`. Volcano is not integrated.

### Placement holds (opt-in)

A single distributed job is already placed all-or-nothing: if fewer than `distributed.nodes` nodes qualify, the placement fails, `Scheduled` is false and the job stays `Pending` (retried after 30 seconds), so a placement never starts a partial set. What placement did not do is account for a job placed a moment earlier: GPUs count as used only once a pod is bound to a node, so two jobs placed within seconds of each other were both told the same GPUs were free.

With `--placement-holds` (chart `aiOperator.placementHolds`, off by default) the operator remembers, in memory, the nodes it placed each job on and counts those GPUs as used for every other job's placement until the job's pods are all ready, the job ends or is deleted, or five minutes pass. A job's own placement never counts its own hold. Fabric-aware ranking and every filter are unchanged; holds only change what other jobs see as free. Kueue-managed jobs do not use it.

**Limit.** The operator's choice is advisory. The pods carry a label node selector (GPU type, RDMA, your own `spec.nodeSelector`), not the names of the chosen nodes, and kube-scheduler decides among the nodes that match. When free GPUs are scarce the pods can only go to the nodes the operator picked, and the hold is accurate. With spare matching nodes the pods may land elsewhere, and two jobs can still compete for the same ones. Annotate the job `gryvia.io/pin-placement: "true"` to make it binding: the pods then get a required node affinity (`metadata.name` in the chosen nodes, added to every term of your own required affinity). The cost is that if a chosen node disappears the pods stay Pending until the job is rescheduled. Unit-tested with fake clients; not run on GPUs.

### Topology-aware group placement (opt-in)

With `--topology-placement` (chart `aiOperator.topologyPlacement`, off by default) a multi-node job is kept inside one InfiniBand block, else one rack. Label the nodes `gryvia.io/ib-block` and `gryvia.io/rack`. After filtering and ranking (fabric-aware ranking, dataset locality and holds included), the operator groups the eligible nodes by `gryvia.io/ib-block` and takes the **smallest** block with at least `distributed.nodes` eligible nodes, so larger blocks stay free for larger jobs. Ties go to the block whose best nodes score highest, then to the block name. Within the block it keeps the ranking order. If no block fits, racks are tried the same way; if no rack fits either, the plain ranking places the job as before.

The group is recorded in `status.placementTopology` (`gryvia.io/ib-block=b2`) and in the `Scheduled` condition message, and the pods get soft preferred node affinity (weight 80) toward it. Like the rest of the operator's placement it is advisory unless `gryvia.io/pin-placement: "true"` is set. Elastic jobs and Kueue-managed jobs are not grouped (Kueue's own topology-aware scheduling does that: see [platform completion](https://github.com/zyvorai/gryvia/blob/main/docs/platform-completion.md#scheduling-and-federation)). Per-job opt-out: annotation `gryvia.io/topology-placement: "false"`. Unit-tested with fake clients (`pkg/scheduler/topology_test.go`, `controllers/gryviaaijob_topology_test.go`); not run on an InfiniBand fabric.

### Design sketch

Design sketch, not accepted by the current CRD schema (there is no `scheduling` field). Topology preference values (`same-node`, `same-rack`, `same-zone`, `any`) are not implemented as such; the opt-in group placement above and Kueue's topology-aware scheduling cover the block and rack cases.

```text
spec:
  scheduling:
    gang:
      enabled: true
      minMembers: 4
      timeout: 10m
      topology:
        preferred: same-rack
```

---

## Queues, quotas and fair sharing

Status: **Kueue-backed and opt-in** (same switch as above; no in-tree fair-share queue runs). With the quota operator's `--kueue-integration`, every `GryviaTenant` gets a LocalQueue `gryvia` in `tenant-<name>` and a ClusterQueue `gryvia-<tenant>` whose nominal quota comes from `spec.quotas.concurrentGPUs` (else the `maxGPUs` of a `GryviaQuota` covering the namespace, else unlimited). All tenant ClusterQueues share the cohort `gryvia`: idle quota is borrowed between tenants and reclaimed by preemption, with `BestEffortFIFO` ordering. Kueue's own fair-sharing modes and DRF are **not** enabled; what you get is nominal quota + borrowing + preemption. Details, limits and the exact objects: [Kueue integration](https://github.com/zyvorai/gryvia/blob/main/docs/kueue-integration.md).

There is no in-tree DRF calculator or priority queue (an unused one was removed). There is no `GryviaQueue` kind, no hierarchical queues and no time-based backfill.

Other multi-tenant limits, independent of Kueue:

- `GryviaQuota` (reconciled by the quota-operator) tracks usage per namespace, and the admission webhook enforces per-job GPU limits and allowed GPU types from quotas and the tenant's SKU list on create.
- Kubernetes `ResourceQuota` and namespaces per tenant work as usual.

### The CLI queue view

`gryvia queue` lists jobs whose phase is Pending, Queued or Scheduling (with Kueue integration, `Queued` includes jobs waiting for quota, with Kueue's reason in the message; `kubectl get workloads,clusterqueues,localqueues` shows Kueue's own view), plus a summary of running jobs. It reads `GryviaAIJob` objects through your kubeconfig; the optional name argument filters by job name substring, it is not a queue name.

```bash
gryvia queue
gryvia queue --watch 5
gryvia queue -o json
```

### Design sketch

Design sketch, not accepted by the current CRD schema (no `GryviaQueue` kind exists; Kueue's ClusterQueue/LocalQueue objects are what exists when the integration is on):

```text
kind: GryviaQueue
spec:
  parent: organization-queue
  team: ml-research
  resources:
    guaranteed: {gpus: 32}
    limit: {gpus: 64}
  borrowing: {enabled: true}
  backfill: {enabled: true, maxJobDuration: 4h}
  weight: 10
```

Intended semantics: weighted DRF ordering across teams, guaranteed minimums with borrowing of idle capacity, and backfill of short small jobs into gaps that do not delay higher-priority jobs. Backfill would also need a way to know a job's maximum duration; `spec.timeout` exists in the schema but is not read by any scheduler today.

---

## Elastic Training

Status: partial and unit-tested only. `spec.distributed.elastic.minNodes` lets a PyTorch batch job run with between `minNodes` and `distributed.nodes` workers: the launcher gets `NNODES=min:max`, the Job succeeds once `minNodes` indexes did, and placement starts as soon as `minNodes` nodes qualify. The operator does **not** resize a running job (the Job's `completions` is fixed), does not tolerate losing rendezvous host 0, and nothing has run on a cluster or GPUs. See [docs/elastic-training.md](https://github.com/zyvorai/gryvia/blob/main/docs/elastic-training.md) and the example `examples/scheduling/elastic-job-example.yaml`.

The old design sketch below (`minWorkers`/`maxWorkers`/`checkpointOnScale`) is **not** the schema that exists; use `distributed.elastic`.

### Design sketch (not implemented)

```text
spec:
  elastic:
    enabled: true
    minWorkers: 2
    maxWorkers: 8
    checkpointOnScale: true
```

Intended behaviour of the full design: scale workers up when GPUs are free, scale down instead of killing the job when GPUs are needed elsewhere, and checkpoint before removing a worker. This would need a controller that resizes the workload, the checkpoint machinery (see `GryviaCheckpointGuard` and the coordinated-checkpoint example) and a rendezvous backend in the cluster.

---

## Admission Webhooks

The ai-operator serves a **validating** webhook for `GryviaAIJob` when started with `--enable-webhooks` (the Helm chart's `webhook.enabled`, which defaults to true, with a self-signed certificate). The chart's `webhook.failurePolicy` defaults to `Ignore`, so if the webhook is unreachable job creation still succeeds (fail open). Set it to `Fail` to reject jobs while the webhook is down.

### Validation rules that exist

| Rule | Behaviour |
|------|-----------|
| `spec.type` | Must be one of `training`, `inference`, `fine-tuning`, `evaluation` |
| `spec.gpus` | Must be greater than 0 |
| `spec.network` | If set, one of `standard`, `rdma`, `sriov` |
| `spec.priority` | Between 0 and 100 |
| `spec.distributed` | When enabled: `nodes` > 0, `gpusPerNode` not negative, `framework` one of pytorch, tensorflow, horovod, deepspeed, megatron, `backend` one of nccl, gloo, mpi, and nodes times GPUs per node at most 1024 |
| Resource requests, image | Basic well-formedness checks |
| Quota and tenant policy (create only) | Denies GPU types not allowed by the namespace's `GryviaQuota` objects, GPU counts above the smallest per-job limit, and GPU types outside the tenant's allowed SKUs (from the `GryviaGpuSku` catalog) |
| Cluster warnings | Non-blocking warnings when the request exceeds the largest node or names a GPU type no node carries |

Rules that earlier versions of this guide listed but that are **not** implemented: budget check, image allowlist, priority authorization, PVC existence, network policy validation. There is no `gryvia-operator-config` ConfigMap driving webhook behaviour.

### Mutating webhook

`operators/ai-operator/pkg/webhook/mutator.go` contains a mutator that would inject NCCL variables (defaults such as `NCCL_DEBUG=WARN`, `NCCL_IB_DISABLE`, `NCCL_NET_GDR_LEVEL`, `NCCL_SOCKET_IFNAME`), topology and RDMA/SR-IOV annotations, default CPU and memory limits and a default image pull policy. It is not registered in the operator's `main.go` and no `MutatingWebhookConfiguration` is shipped, so none of these mutations happen. The only NCCL/distributed environment that is set today is the small set the controller itself adds (see above).

---

## Priority Preemption

Status: **Kueue-backed and opt-in; without it, not implemented.** With `--kueue-integration`, `spec.priority` (0 to 100) is mapped to a Kueue `WorkloadPriorityClass` `gryvia-priority-<n>` (`n` = priority rounded down to a multiple of 10; priority below 10 uses Kueue's default of 0), and the tenant ClusterQueues are created with `withinClusterQueue: LowerPriority`, `reclaimWithinCohort: Any` and `borrowWithinCohort: LowerPriority`. A higher-priority job that cannot fit therefore evicts lower-priority admitted jobs of the same queue; a tenant reclaiming quota it lent out evicts the borrowers. An evicted job goes back to `Queued` (message "Evicted by Kueue ...") and runs again when it fits: it is requeued by Kueue, not deleted and not terminal `Preempted`. **Nothing checkpoints first**: the pods are terminated, so the job must resume from its own checkpoints. Metering caveat and details: [Kueue integration](https://github.com/zyvorai/gryvia/blob/main/docs/kueue-integration.md).

Without Kueue integration `spec.priority` is validated but not acted on. Kubernetes pod priority is available separately: a `GryviaPriority` makes the ai-operator (with the ML controllers, on by default) create an owned Kubernetes `PriorityClass` of the same name, and a job annotated `gryvia.io/priority-class: <name>` gets it as `priorityClassName` on its batch Job pods (not on the StatefulSet workload), so the Kubernetes scheduler may preempt lower-priority pods for them. There is no `priorityClassName`, `preemption` or `checkpointing` field on `GryviaAIJob`.

**Unverified:** unit tests against fake clients only; the kind workflow that preempts a running job has not been run yet.

```bash
# Kueue view of the queue (with the integration on)
kubectl get workloads,clusterqueues,localqueues -A
kubectl get workloadpriorityclasses

# Priority classes: each GryviaPriority owns a Kubernetes PriorityClass of the same name
kubectl get gryviapriorities,priorityclasses

# Real commands
gryvia queue
gryvia status my-job
kubectl describe gryviaaijob my-job
```

---

## Scheduling Summary

| Feature | Real state |
|---------|-----------|
| Node selection | Runs; recorded in status, pods are placed by the Kubernetes scheduler using selectors |
| Gang admission (all pods together) | Kueue-backed, opt-in (`--kueue-integration`); unit-tested; the kind e2e (real Kueue, CPU pods) passes in CI; never on GPUs. |
| Queues, per-tenant quota, borrowing | Kueue-backed, opt-in; nominal quota + cohort borrowing, not DRF. |
| Hierarchical queues, backfill | Not implemented |
| Elastic training | Partial: `distributed.elastic.minNodes`, unit-tested, not run on a cluster; no live resize |
| Validating webhook | Runs when enabled; quota and SKU policy; fails open by default |
| Mutating webhook (NCCL injection) | Code only, not registered |
| Priority preemption | Kueue-backed, opt-in: evicted jobs are requeued, no checkpointing. Off without the integration |

See also [GPU nodes](GPU_NODES.md), [GPU as a service](GPU_AS_A_SERVICE.md) for quota and tenancy, and [Operations](OPERATIONS.md).
