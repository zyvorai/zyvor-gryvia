# Elastic training (PyTorch, run-to-completion jobs)

**Status: run on kind with a real `torchrun` on CPU; never on GPUs.** The kind e2e (`e2e-ml.yml`, "Elastic training") runs [`elastic_train.py`](../examples/training/elastic_train.py) in an elastic AIJob (`nodes: 2`, `minNodes: 1`, gloo), deletes worker index 1 after a committed checkpoint and expects torchrun to restart the workers, every rank to resume from the committed step and the job to succeed. A second kind e2e (`e2e-elastic.yml`) spreads the two workers over two nodes with the checkpoints on a ReadWriteMany NFS volume and loses a whole node: the survivor must finish alone (see [Across nodes](#across-nodes-a-lost-node)). Under Kueue, an elastic job is admitted with between `minNodes` and `nodes` workers (see [Under Kueue](#under-kueue)). NCCL and GPUs are not covered.

## What it is

```yaml
spec:
  type: training
  distributed:
    enabled: true
    framework: pytorch
    nodes: 4            # the most workers wanted (and what quota / admission count)
    gpusPerNode: 8
    elastic:
      minNodes: 2       # the fewest the training can run with
```

For such a job the operator:

- creates the Indexed batch Job with `completions = parallelism = nodes`;
- gives the launcher `NNODES=2:4` (the form `torchrun --nnodes` accepts) plus `GRYVIA_ELASTIC=true`, `GRYVIA_ELASTIC_MIN_NODES` and `GRYVIA_ELASTIC_MAX_NODES`, and `TORCH_DISABLE_SHARE_RDZV_TCP_STORE=1` unless `env` sets it ([why](#losing-index-0)). `WORLD_SIZE` stays the upper bound; the launcher recomputes it after each rendezvous;
- sets a Job `successPolicy` with `succeededCount: minNodes`, so the Job completes when `minNodes` workers succeeded even if the others never got scheduled (without it a Pending worker would keep the Job from ever completing). Needs Kubernetes with `batch/v1` Job success policy (beta and on by default in 1.31, GA in 1.33). With `minNodes == nodes` no policy is set;
- places the job as soon as `minNodes` nodes qualify and takes as many as are free up to `nodes`.

Validation (webhook and controller): `1 <= minNodes <= nodes`, PyTorch only, batch Job only (an inference StatefulSet has a fixed replica set). A bad spec fails the job with `InvalidElasticConfig`.

Your training script must be elastic itself: launch it with `torchrun --nnodes=$NNODES --nproc-per-node=$NPROC_PER_NODE --rdzv-backend=c10d --rdzv-endpoint=$MASTER_ADDR:$MASTER_PORT ...`, restart from a checkpoint on each membership change, and use [coordinated checkpoints](../examples/training/coordinated_checkpoint.py) so all ranks resume the same step. For model and optimizer state that must load at another world size (sharded optimizers, FSDP), use [DCP checkpoints](#dcp-checkpoints-and-resharding).

## Reference trainer

[`examples/training/elastic_train.py`](../examples/training/elastic_train.py) (image: `examples/training/Dockerfile.elastic`, CPU torch) is a small data-parallel trainer that does all three:

- it trains with `DistributedDataParallel` and Adam; every `CHECKPOINT_EVERY` steps all ranks call `dcp_checkpoint.save`; a step counts only once rank 0 wrote its COMMIT record;
- on (re)start rank 0 deletes half-written steps above the committed one (`discard_uncommitted`), then every rank loads the committed step with `dcp_checkpoint.load`, resharded to the current world size, so the group may resume with another world size (a directory holding an older opaque-blob checkpoint is resumed from rank 0's blob with `load_replicated`). `DONE` records `resumedFromWorld`;
- the process group gets a short timeout (`COLLECTIVE_TIMEOUT`, default 60s). A peer that disappears in the middle of an all-reduce otherwise only surfaces after gloo's default of 30 minutes, and torchrun cannot restart the workers before that.

```yaml
spec:
  type: training
  gpus: 0
  image: ghcr.io/zyvorai/gryvia-elastic-train:dev
  retryLimit: 3                     # the replacement of a lost worker counts as a retry
  distributed: {enabled: true, framework: pytorch, backend: gloo, nodes: 2, elastic: {minNodes: 1}}
  command: [torchrun]
  args: [--nnodes=$(NNODES), --nproc_per_node=$(NPROC_PER_NODE), --rdzv_backend=c10d,
         "--rdzv_endpoint=$(MASTER_ADDR):$(MASTER_PORT)", --rdzv_id=my-job, --max_restarts=3, /app/elastic_train.py]
  env: [{name: CHECKPOINT_DIR, value: /ckpt}]
  volumes: [{name: ckpt, persistentVolumeClaim: {claimName: my-checkpoints}}]   # ReadWriteMany across nodes
  volumeMounts: [{name: ckpt, mountPath: /ckpt}]
```

The operator also sets `TORCH_DISABLE_SHARE_RDZV_TCP_STORE=1` on elastic jobs. Since torch 2.4 the agent caches the workers' store address from its first rendezvous round. When another node becomes rank 0 after a loss, that agent fails `assert self._shared_tcp_store_server is not None` in `_restart_workers`; we hit this with torch 2.8 when index 0 was lost. With the variable set, the store address is rebuilt every round. If `env` sets the variable, the operator leaves it alone.

What the kind e2e checks: after worker 1 is deleted, the survivor's all-reduce fails, torchrun restarts its worker, the group re-forms (alone, or with the replacement pod the Job creates for index 1) and training continues from the last committed step. Rank 0 writes `DONE` (steps, final loss, world size, restarts, resumed step) next to the checkpoints. In the first kind run the Job's replacement pod joined in time: both ranks resumed after step 10 with world size 2 and finished all 60 steps. `restarts` stayed 0 because torchrun counts only failure restarts, not a group re-formed because a node joined; the e2e asserts the resumed step instead.

## Across nodes, a lost node

`.github/workflows/e2e-elastic.yml` runs the same trainer on a four-node kind cluster (`scripts/kind-elastic.yaml`): Gryvia on one worker, two tainted training workers, and required pod anti-affinity so each worker pod gets its own node. The checkpoints are on an NFS export of the runner, mounted ReadWriteMany by both nodes. After step 10 is committed with world size 2 (rank 0 read rank 1's manifest from the other node), the node of index 1 is stopped and deleted. The e2e then expects:

- the Job's replacement pod for index 1 to stay Pending (no training node is left);
- the survivor to re-form the group alone (`rank 0 of 1`), resume from the committed step and train to the end;
- `DONE` to record world size 1, and the Job to complete through the success policy (`succeededCount: minNodes`) with the Pending pod still unscheduled;
- a pod on a third node to read `DONE` from the same volume.

The e2e is a matrix and runs this twice: once losing index 1, once losing index 0.

The first run on main ([run 36990965624](https://github.com/zyvorai/gryvia/actions/runs/36990965624), 2026-10-02) passed: the workers ran on `worker3` (index 0) and `worker2` (index 1), `worker2` was stopped and deleted, the replacement for index 1 stayed Pending, the survivor re-formed the group alone and resumed from step 10 with one failure restart (`TORCHELASTIC_RESTART_COUNT` 1: here the worker really failed, on the collective timeout, unlike the single-node run where the replacement joined), and the reader on `worker` read `DONE`.

Use these mount options for the checkpoint volume on NFS: `noac` and `lookupcache=none` (the e2e uses both, with `nfsvers=4.2`). The commit protocol polls for files another node just created; with the default attribute and lookup caches a rank can miss them for up to a minute, past the commit timeout.

### Losing index 0

With `--rdzv_endpoint=$(MASTER_ADDR):$(MASTER_PORT)` the c10d rendezvous store lives in the torchrun agent of pod 0, so losing index 0 loses the rendezvous for everyone. To survive that, run the store on its own and make every agent a client:

- [`examples/training/rendezvous_store.py`](../examples/training/rendezvous_store.py) is a plain `TCPStore` server (port `RDZV_PORT`, 29400). It is in the elastic image; run it as a Deployment with a Service, on a node outside the training pool.
- Pass `--rdzv_endpoint=<service>:29400 --rdzv_conf=is_host=0` to torchrun. The process group's master is then the rank 0 of each round, not `MASTER_ADDR`.

The `lose index 0` e2e does this: the store runs on Gryvia's worker, index 0's node is stopped and deleted, and the survivor (index 1) re-forms the group as `rank 0 of 1`, resumes from the committed step and completes the Job. The store is in memory with a single replica, so losing the store itself still loses the rendezvous of running jobs. Before the shared-store fix above, the same test failed on the survivor with the `_shared_tcp_store_server` assertion (reproduced locally with torch 2.8).

### A replicated rendezvous (etcd)

The standalone store is a single in-memory process. To survive losing the store too, use torch's `etcd-v2` backend against an etcd cluster:

```yaml
  args: [--nnodes=$(NNODES), --nproc_per_node=$(NPROC_PER_NODE), --rdzv_backend=etcd-v2,
         --rdzv_endpoint=etcd.my-ns.svc:2379, --rdzv_id=my-job, --max_restarts=3, /app/elastic_train.py]
```

- The backend speaks etcd's **v2 API**. Run etcd 3.5 with `--enable-v2`; etcd 3.6 removed the v2 API.
- The workers need `python-etcd`; it is in the elastic image.
- Point `--rdzv_endpoint` at a Service in front of the members. The client also reconnects to other members itself.

The `rendezvous etcd` case of `e2e-elastic.yml` runs a 3-member etcd 3.5.34 StatefulSet (`quay.io/coreos/etcd`, `--enable-v2`) on Gryvia's worker. After step 10 is committed, it scales the StatefulSet to 2, so one member is gone while the cluster keeps quorum. Then it loses index 0's node. The survivor must re-form the group through the remaining members as `rank 0 of 1` and finish.

The same test passed locally with three etcd processes and two torchrun agents (torch 2.8, python-etcd 0.4.5). We killed the member the agents' endpoint named, then one agent with its worker. The other agent logged one failed keep-alive, re-rendezvoused through the other members, and finished alone from the committed step.


```yaml
apiVersion: v1
kind: PersistentVolume
metadata: {name: checkpoints}
spec:
  capacity: {storage: 100Gi}
  accessModes: [ReadWriteMany]
  storageClassName: ""
  mountOptions: [nfsvers=4.2, noac, lookupcache=none]
  nfs: {server: nfs.example.com, path: /exports/checkpoints}
```

## Under Kueue

When Kueue manages the job (see [kueue-integration.md](kueue-integration.md)), the operator puts two annotations on the batch Job:

- `kueue.x-k8s.io/job-min-parallelism: <minNodes>` turns on Kueue's partial admission. If the quota has no room for `nodes` workers, Kueue admits as many as fit, but at least `minNodes`, instead of queueing the whole job.
- `kueue.x-k8s.io/job-completions-equal-parallelism: "true"` makes Kueue lower `completions` along with `parallelism`. The Job then counts only the admitted workers, and `succeededCount: minNodes` stays within `completions`.

torchrun takes whatever group size forms within `NNODES=min:max`. The Ready condition counts the admitted workers. If Kueue evicts the job, it restores `parallelism` and `completions` to `nodes` before requeueing it. Jobs with `minNodes == nodes`, and non-elastic jobs, stay all-or-nothing.

`scripts/e2e-kueue.sh elastic`, a step of `e2e-kueue.yml`, checks two cases on kind:

- An elastic job asking for 4 workers (`minNodes: 2`) against 2 slots of quota runs with 2 workers. Its Job has parallelism and completions 2/2, the Workload's admission is for 2 pods, and no pods exist for indexes 2 and 3. The job succeeds and the quota is released.
- With 2 workers admitted and `minNodes: 1`, index 0 finishes and index 1 would sleep for 10 minutes. The success policy completes the Job (`SuccessCriteriaMet`, 1 succeeded) and the running pod is stopped. Kueue marks the Workload Finished and the ClusterQueue goes idle.
- `scripts/e2e-kueue.sh elastic-torchrun` uses the real trainer. A `torchrun` job asking for 4 workers (`minNodes: 2`) against 2 slots runs with 2 workers. `NNODES=2:4` forms a group of 2, both ranks commit every checkpoint up to the final step 20 on a shared volume, and rank 0 reports `world 2`.

## DCP checkpoints and resharding

[`examples/training/dcp_checkpoint.py`](../examples/training/dcp_checkpoint.py) stores model and optimizer state with `torch.distributed.checkpoint` under `steps/<S>/dcp/` and commits the step with the same COMMIT/COMMITTED protocol: rank 0 writes `COMMIT` (`format: dcp`, the saving world size and the sha256 of every DCP file) only after every rank's shards and the `.metadata` file exist. Loading reads the regions each rank now needs from the DCP metadata, so a step saved by 4 ranks loads at 2 or 3, and FSDP-sharded parameters and Adam moments are resharded rather than copied.

```python
import dcp_checkpoint
step_and_world = dcp_checkpoint.load(root, model, optimizer)   # None when nothing is committed
...
dcp_checkpoint.save(root, step, model, optimizer)              # collective, at the same step on every rank
```

State dicts come from `get_state_dict` / `set_state_dict`, so a plain module, DDP and FSDP2 (`fully_shard`) all work, and the names are fully qualified. A checkpoint written under DDP therefore loads into a differently wrapped model. With `verify` (the default), rank 0 checks every file against `COMMIT` before anyone loads. `checkpoint_contract.verify_committed` reports `optimizerReshardable: true` only for these checkpoints.

Tested: `examples/training/test_dcp_checkpoint.py` spawns gloo processes on CPU, saves at world size 4 and loads at 2 and 3 with DDP and with FSDP2. It compares every parameter and optimizer tensor and the Adam step counters with the saved state, checks that the ranks stay identical after one more step, ignores a half-written step and rejects a corrupted shard. The "DCP checkpoint resharding" job in `repo-checks.yml` runs it and also runs `elastic_train.py` under `torchrun` at 4 processes, then resumes it at 2. Not run on GPUs or with NCCL. Data-loader state is not resharded: the trainer must derive its data position from the step, as `elastic_train.py` does.

## What it does not do

- **It does not add or remove workers while the job runs.** The Indexed Job's `completions` is fixed at creation. Workers lost to a node failure are replaced by the Job controller (same index, same DNS name) when capacity exists; if it does not, the others carry on only if the launcher's rendezvous accepts a smaller group. There is no controller loop that resizes the Job.
- **Losing index 0 needs a standalone rendezvous store.** With the default `MASTER_ADDR` endpoint the store is in pod 0 and losing it ends the job. See [Losing index 0](#losing-index-0).
- **The Job can finish early.** Once `minNodes` indexes succeed the remaining pods are removed. In a healthy elastic run all workers finish together; if some finish a moment later they may be stopped mid-exit.
- No resharding of data-loader state (optimizer and model state reshard through [DCP](#dcp-checkpoints-and-resharding)), no scale-up of a running job, no resize of a Kueue-admitted job after admission (Kueue's partial admission picks the size once, at admission).
- Unverified: etcd with TLS (`protocol=https`, `ssl_cert`), storage other than NFS, and any NCCL behaviour on a resized group.

## Tests

`controllers/gryviaaijob_elastic_test.go` (Job shape, env, success policy, defaults, validation), `pkg/scheduler/holds_test.go` (placement between min and max), `pkg/webhook/validator_test.go`, `examples/training/test_coordinated_checkpoint.py` (commit protocol, `load_replicated`, `discard_uncommitted`), `examples/training/test_dcp_checkpoint.py` (DCP save at 4, load at 2 and 3, DDP and FSDP2), the "Elastic training" step of `e2e-ml.yml` (one node, a deleted pod) and `e2e-elastic.yml` (two nodes, NFS, losing the node of index 1 or of index 0).
