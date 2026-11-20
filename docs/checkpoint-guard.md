# Checkpoint guard and node-loss recovery

`GryviaCheckpointGuard` ties a batch training job to the checkpoints its trainer actually commits. It is
opt-in: the ai-operator only acts on guards with `--checkpoint-guard` (chart value
`aiOperator.checkpointGuard: true`). Without the flag a guard is still reconciled for its health checks, but no
job is changed and nothing is recovered.

The operator never writes a checkpoint itself. A process outside the trainer cannot read the model's memory;
the trainer saves, commits and reports. The guard gives it a place to save, a channel to report, and replaces
its pods when their node is lost.

## What a guard does to a job

When a new `GryviaAIJob` is created, the first guard (by name) in its namespace whose `jobSelector.matchLabels`
matches the job's labels (an empty selector matches every job) is applied. Inference jobs, StatefulSet jobs
and jobs without `spec.storage` are skipped and listed in the guard's `status.jobs[].message`. Workload
templates are immutable, so a guard created after a job's batch Job does not change it.

The trainer container gets:

| Variable | From |
|---|---|
| `GRYVIA_CHECKPOINT_DIR` | the job's `gryvia.io/checkpoint-directory` annotation, else `spec.checkpointPolicy.directory`, else `/data/checkpoints` (below the `spec.storage` volume) |
| `GRYVIA_RESUME_IF_PRESENT` | `true` |
| `GRYVIA_CHECKPOINT_EVERY` | `spec.checkpointPolicy.everySteps` |
| `GRYVIA_CHECKPOINT_INTERVAL_SECONDS` | `spec.checkpointPolicy.intervalMinutes` × 60 |
| `GRYVIA_CHECKPOINT_REPLICA` | `spec.checkpointPolicy.replication.target` (`file:///…` or `s3://bucket/prefix`) |
| `GRYVIA_CHECKPOINT_STATUS_CONFIGMAP` | `<job>-checkpoint-status` |
| `POD_NAMESPACE` | the downward API |

The operator also creates, owned by the job, the ConfigMap `<job>-checkpoint-status` and a Role and RoleBinding
of the same name that let the namespace's `default` service account `get` and `update` only that ConfigMap.
This is why the flag grants the operator `create` on Roles and RoleBindings; RBAC escalation rules let it
grant only verbs it holds itself.

## The trainer's side

`examples/training/checkpoint_status.py` (standard library only) is the reporter. `StatusReporter.from_env()`
uses the in-cluster service account; `committed(step)` writes `committedStep` and `committedAt` after a global
commit, `progress(step)` writes `currentStep` (throttled), and `requested()` returns true when the operator
asked for an early checkpoint (`requestedAt` newer than the last commit). Writes retry on a conflict.

`examples/training/checkpoint_replica.py` copies each committed step to the replica target and, with an empty
checkpoint directory, restores the latest replicated step before resuming. `file:///` copies to a mounted path;
`s3://` needs `boto3` and credentials in the pod.

`examples/training/elastic_train.py` uses both: it saves every `GRYVIA_CHECKPOINT_EVERY` steps through the
commit protocol, reports each commit, replicates it, and checkpoints early (all ranks together) when the
operator requests it.

## What the guard reports

- `GryviaAIJob.status.checkpoint`: guard, directory, status ConfigMap, `lastCommittedStep`, `lastCommittedAt`,
  `currentStep`, `nodeLossRecoveries`, `lostSteps` and `lastNodeLoss {node, at, pods, resumeStep, lostSteps}`.
- `GryviaCheckpointGuard.status.jobs[]`: per job, whether it was injected, its last committed step and time, and
  its node-loss recoveries. `totalCheckpoints` and `validCheckpoints` count newly committed steps;
  `lastValidCheckpoint` is `<job>/step-<N>`.
- Condition `CheckpointValid`: `Committed`, `Stale` (no commit for twice the interval, default 30 minutes) or
  `NotInjected`.
- On an emergency trigger (`spec.checkpointPolicy.emergencyCheckpoint.triggers`), the guard writes `requestedAt`
  and `requestReason` into each running job's status ConfigMap, once per commit.

## Node loss

With `spec.restore.autoRestore: true`, a pod of a guarded **non-elastic** job whose node is gone, or whose node's
`Ready` condition has not been `True` for `spec.restore.nodeLossGraceSeconds` (15-3600, default 60), is
force-deleted (grace 0, UID precondition). Without this the pod waits for taint-based eviction (5 minutes) and
then stays `Terminating` until the kubelet returns, so the Indexed Job does not replace it. The Job recreates the
index on another node, and the trainer resumes from the last committed step. The AIJob gets condition
`RecoveredFromNodeLoss` (reason `PodsReplaced`), a `NodeLost` event, and `lostSteps` = reported current step minus
committed step.

Elastic jobs are left alone: their survivors re-form the group (see [elastic training](elastic-training.md)).
A force deletion assumes the node is really gone. If it comes back, its kubelet kills the old container, but
both may write until then; the commit protocol keeps the previous step as the resume point.

## Fields not acted on

`validation`, `restore.preferNearStorage`, `restore.injectEnvVars`, `replication.backend/copies/asyncUpload`
and `monitoring` are accepted but not implemented. `storageUsed` and `avgCheckpointDuration` are not filled.

## Tests

- ai-operator unit tests (`gryviaaijob_checkpoint_test.go`, `gryviacheckpointguard_observe_test.go`) cover guard
  matching, env injection, the ConfigMap/Role/RoleBinding, status sync, staleness, request de-duplication and
  node-loss detection, grace and force deletion.
- `examples/training/test_checkpoint_status.py` runs the reporter against a local fake API server;
  `test_checkpoint_replica.py` covers `file://` and a fake S3 client.
- `scripts/tests/checkpoint-guard-chart.test.sh` renders the flag and the RBAC.
- The kind workflow `e2e-elastic.yml` job **node-loss** runs a real torchrun trainer in a non-elastic guarded
  job on an NFS-backed `spec.storage` volume, stops its node's container after a reported commit, and checks the
  operator replaced the pod, the trainer resumed from the committed step, and the job finished. It has not run
  locally; it runs in CI.
- Not run on GPUs, real node failures in a cloud, or S3.
