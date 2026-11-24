# Job insights: queue, waits, failures, right-sizing, quota use

Five read-only views over `GryviaAIJob` and `GryviaTenant` objects, served by the API gateway and available as
`gryvia insights ...`. They read what the operators already record; nothing here changes a job, a quota or a
queue. A tenant user sees only its own jobs and tenants; the provider admin sees all. Jobs carrying a marking
the caller is not cleared for are left out, as everywhere else in the gateway.

**Status:** verified with endpoint tests against a fake Kubernetes API and Rust unit tests; not run against a live
cluster. Comparable products: Kueue's pending-workloads API (position in queue), Kubecost's GPU efficiency report
and request sizing, Run:ai's allocated-versus-deserved quota view, and the transient-versus-deterministic failure
split from GPU-cluster failure studies. See docs/RESEARCH.md.

## Queue positions: `GET /api/queue`

Jobs in `Pending`, `Queued` or `Scheduling` with no start time, in the order they are considered: `spec.priority`
descending, then creation time, then name. Each row has `position` (cluster-wide, from 0), `positionInQueue` (among
jobs of the same `spec.queueName` or `gryvia.io/queue-name`, else `default`), `positionInTenant`, `gpusAhead` (GPUs
requested by every job in front, per-pod GPUs times `distributed.nodes`), `waitingSeconds` and `reason` (Kueue's
`queueExplanation.category`, else the status message). Positions are computed over all waiting jobs, so a tenant sees
its real place in line while seeing only its own rows. Filters: `queue`, `tenant` (admin), `limit`.

The order is Gryvia's reading of priority and age. Kueue and KAI order by their own rules (fair sharing, borrowing),
so for jobs they manage the position is an estimate, not a promise.

## Wait times: `GET /api/queue/stats?days=7`

For jobs whose `status.startTime` falls in the window: wait = start minus creation, summarized as count, p50, p90
(nearest rank), max and mean, overall, per tenant and per queue (slowest p90 first).

## Failure causes: `GET /api/failures?days=7`

Jobs in `Failed`, `Preempted` or `Rejected` that ended in the window, classified from the phase, status message and
condition reasons by fixed rules (first match wins):

| Category | Matches | Transient |
|---|---|---|
| `oom` | OOMKilled, out of memory | no |
| `image-pull` | ImagePullBackOff, ErrImagePull | yes |
| `node-failure` | NodeLost, NotReady, Xid | yes |
| `preempted` | phase Preempted, evicted | yes |
| `deadline` | DeadlineExceeded, timed out | no |
| `quota` | quota, admission, denied, budget, phase Rejected | no |
| `scheduling` | Unschedulable, no nodes, insufficient | no |
| `app-error` | BackoffLimitExceeded, exit code, error | no |
| `unknown` | none of the above | no |

`transient` means a retry can succeed without a change; the others will fail again until something is fixed. The
classifier reads text, not logs, so a job whose cause is only in the container log lands in `app-error` or `unknown`.
Filters: `category`, `tenant` (admin), `limit`, `format=json|csv` (text cells starting with `=`, `+`, `-` or `@` are
quoted for spreadsheets).

## GPU right-sizing: `GET /api/rightsizing?days=14&targetUtilPct=70`

For `Running` and `Succeeded` jobs that started in the window and carry `status.metrics.gpuUtilization`: a job holding
`g` GPUs at `u` percent needs about `g * u / target`. A one-GPU job below the target is advised to share a GPU
(`gryvia.io/gpu-share` 0.25, 0.5 or 0.75; above 0.75 it stays whole); a multi-GPU job is advised to ask for
`ceil(g * u / target)` per pod. `savedGpuHours` = freed GPUs x nodes x hours run. Jobs shorter than `minHours` (default
0.5) are skipped. Jobs without a utilization sample are counted nowhere. The sample is a job-level average, so a job
that is idle half the time and saturated the other half looks half-used: apply the advice to the next run, not
mid-run.

## Quota use: `GET /api/quota-usage`

Per tenant: `GryviaTenant.spec.quotas.concurrentGPUs` against GPUs held by `Running` and `Scheduling` jobs and GPUs
requested by waiting jobs. `utilizationPct`, `headroomGpus`, `borrowing` (holding more than the quota), `blocked`
(at or over the quota with jobs waiting). A tenant with no `concurrentGPUs` shows no percentage.

## CLI

```bash
gryvia insights queue
gryvia insights waits --days 30
gryvia insights failures --category oom
gryvia insights rightsizing --target-util-pct 60
gryvia insights quota -o json
```
