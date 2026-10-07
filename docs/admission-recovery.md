# Reliable admission and cooperative recovery

## Strict tenant admission

Enable both `aiOperator.kueueIntegration=true` and `aiOperator.kueueStrictAdmission=true`.
The operator flag is `--kueue-strict-admission`. Helm and the binary reject strict mode
without Kueue integration.

Tenant (`tenant-*`) batch jobs with no explicit queue always receive the configured
`kueueDefaultQueue` label and are created suspended, even if its LocalQueue or the
Kueue controller is missing. Kueue owns suspension after creation. Explicit queues
also stay suspended when missing. Non-tenant jobs without a queue retain their normal
behaviour. Strict mode rejects new tenant StatefulSet workloads because this integration
supports batch Jobs only; inference services need their own serving quota controls.

The compatibility default is `false`. Enabling strict mode does not retrofit protection
onto Jobs that already exist: inspect/cancel existing unqueued jobs before enforcement.
Only provider administrators should grant tenants permission to modify the underlying
batch Jobs, otherwise a tenant could change their suspension directly.

Queue-managed jobs bypass Gryvia's advisory free-GPU placement check. Their suspended
Jobs must reach Kueue even while every GPU is busy, so Kueue can see demand, borrow
quota or preempt. `Scheduled=True/QueueManaged` explains delegation; it is not proof
that pods are on nodes. `nodesAllocated` and fabric placement explanations are empty
for these jobs; topology and fabric scoring are not combined with Kueue in this patch.
Queued jobs report zero allocated GPUs; unsuspended jobs report the requested total.
The existing dashboard job detail shows the Kueue message and status conditions.

## Worker placement

Set `gryvia.io/spread-workers: "true"` on a distributed job to add required pod
anti-affinity by `kubernetes.io/hostname`, scoped to this job and namespace. Each
worker then needs a distinct node. Existing user affinity, selectors and tolerations
are preserved. This is an explicit constraint: fewer usable nodes than workers leaves
pods Pending. Kueue's admission is all-or-nothing quota admission, not a guarantee
that all admitted pods become runnable simultaneously. Configure Kueue's
`waitForPodsReady` recovery and sufficient node capacity. This is not topology-aware
Kueue scheduling, GPU-ID binding or elastic world-size recovery.

## Cooperative checkpoints

For a new batch training job, use:

```yaml
metadata:
  annotations:
    gryvia.io/checkpoint-command: '["python", "/app/checkpoint.py", "--request"]'
    gryvia.io/checkpoint-directory: /data/checkpoints
    gryvia.io/checkpoint-grace-seconds: "180"
    gryvia.io/spread-workers: "true"
spec:
  storage: shared-rwx  # a real StorageClass supporting ReadWriteMany
```

The command is a JSON argv array (1-32 arguments, maximum 4096 bytes); it is executed
as the trainer's `preStop` hook without an implicit shell. The hook and trainer share
`GRYVIA_CHECKPOINT_DIR` and `GRYVIA_RESUME_IF_PRESENT=true`. The directory must be a
clean absolute path below the persistent `/data` volume. Grace is 30-3600 seconds,
default 120. Checkpoint options without a persistent volume are rejected before
creating resources. The grace budget includes both the hook and normal termination.

**The trainer must implement the contract.** Save model, optimizer, RNG, data-loader
and step state atomically to persistent storage; load an existing compatible checkpoint
on startup. A hook should request a save from the running process and wait for its
acknowledgement. A second process cannot directly read the trainer's in-memory model.
For distributed training, `examples/training/coordinated_checkpoint.py` is a reference for the missing global commit on a ReadWriteMany volume: each rank publishes its blob, rank 0 writes one COMMIT record listing every rank's sha256 and only then moves the COMMITTED pointer, so a dead rank or a timeout leaves the previous step as the resume point for every rank (`save_global` / `load_global` / `prune`). It does not elect a new rank 0 or verify contents, and its opaque blobs do not reshard; `dcp_checkpoint.py` stores model and optimizer state through `torch.distributed.checkpoint` on the same commit protocol and loads it at another world size ([DCP checkpoints](elastic-training.md#dcp-checkpoints-and-resharding)). `load_replicated` resumes a data-parallel run with another world size from rank 0's blob; `examples/training/elastic_train.py` uses the module under a real `torchrun` in the kind e2e ([elastic training](elastic-training.md#reference-trainer)). Otherwise coordinate ranks and checkpoint commits in the framework;
independent per-rank files are not proof of a consistent global checkpoint.

`examples/training/checkpoint_worker.py` is a CPU-only executable example of the request,
acknowledgement, atomic persistence and resume protocol, not a model training benchmark.
Package it in your job image as `/app/checkpoint.py` to exercise the annotation above.

Kueue suspension deletes the running pods; graceful deletion runs this hook. The
same batch Job and PVC remain, and replacement pods read the checkpoint on startup.
`scripts/e2e-kueue.sh preempt-checkpoint` checks this with a real Kueue preemption on kind
(busybox trainer that saves only on the hook's request, two workers, both resume).
A crash, OOM kill, unreachable node, forced deletion or exhausted grace period can
prevent a hook from running or finishing. Periodic framework checkpoints are still
required. Gryvia does not verify checkpoint contents or wait for a global checkpoint
commit before Kueue evicts a workload. It does not promise recovery without data loss.

Workload templates are immutable after batch Job creation. Set these annotations
before submitting a job; later edits do not retrofit hooks or placement constraints.

## Verification

- AI operator unit tests cover missing queues, queued GPU demand without nodes,
  preserved Kueue suspension ownership, unsupported strict workload kinds,
  checkpoint validation/hooks, and worker affinity preservation.
- `scripts/tests/e2e-kueue-pods.test.sh` checks retained terminal pods vs active pods.
- `examples/training/test_checkpoint_worker.py` runs real worker/request subprocesses,
  verifies persisted progress and restarts from the checkpoint.
- `scripts/tests/kueue-chart.test.sh` renders strict flags and rejects invalid combinations.
- The kind Kueue workflow now exercises missing tenant queues and checks non-terminal
  pod clearance during preemption. The high-priority job runs long enough to observe
  eviction before re-admission. The workflow passes in CI; it has not run locally.
- No GPU/RDMA, real model checkpoint or cluster validation is claimed by this patch.
