# Federation failover

Off by default. A `GryviaFederation` lists member clusters; the ai-operator probes each member's `/readyz` through
an administrator-allowed HTTPS API server and an inline kubeconfig Secret. Workload dispatch across clusters is done
by Kueue MultiKueue (see [Kueue integration](kueue-integration.md)). Failover adds one thing on top: when a member
keeps failing its health checks, Gryvia makes sure the member's copies of the jobs are stopped (fencing) and then
moves the jobs off it right away, instead of waiting for Kueue's own lost-worker timeout (15 minutes by default).

```yaml
aiOperator:
  kueueIntegration: true
  federation:
    allowedServers: [https://worker1.example:6443, https://worker2.example:6443]
    credentialsNamespace: ""   # default: the release namespace; Secrets with key "kubeconfig"
    failover: true             # --federation-failover; grants patch on Kueue Workloads
```

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaFederation
metadata: {name: prod}
spec:
  clusters:
    - name: worker1
      enabled: true
      apiServer: https://worker1.example:6443
      credentials: {secretRef: worker1-kubeconfig}
      multiKueueCluster: worker1     # the MultiKueueCluster on the manager; defaults to name
    - name: worker2
      enabled: true
      apiServer: https://worker2.example:6443
      credentials: {secretRef: worker2-kubeconfig}
  failover:
    enabled: true
    automatic: true
    leaseTimeout: 5m                 # see "Fencing" below
    healthCheck: {interval: 30s, failureThreshold: 3}
```

The chart refuses `aiOperator.federation.failover` without `allowedServers` and `aiOperator.kueueIntegration`.

## What happens

1. Each health check updates `status.clusterStatus[]`: `state`, `consecutiveFailures` and `unhealthySince` (the first
   failure of the current streak). A healthy check resets the streak and the fence.
2. Once a member has failed `failureThreshold` checks in a row (default 3), the controller lists the manager's Kueue
   Workloads that MultiKueue placed on it (`status.clusterName`, or the admission check message
   `reservation on "<name>"` on older Kueue versions), skipping finished and already deactivated ones.
3. **Fencing.** It connects to the member with the same validated kubeconfig as the probe and deletes the member's
   copies of those Jobs and Workloads (same namespace and names as on the manager). If that works the member is
   fenced with `fenceMethod: RemoteDelete`. If the member cannot be reached, nothing moves until `leaseTimeout`
   (default 5m) has passed since `unhealthySince`; then it is fenced with `fenceMethod: LeaseExpired`. Condition
   `FailoverFenced` and the Events `MemberFenced` say which.
4. **Eviction.** Each Workload gets `spec.active: false` and the annotation `gryvia.io/failover-from: <member>`;
   Kueue evicts it. When Kueue reports it evicted (condition `Evicted`, or the quota reservation released), the
   controller sets `spec.active: true` again and replaces the annotation with `gryvia.io/failed-over-from`. Kueue
   requeues it and MultiKueue dispatches it to a member that admits it.
5. `status.failovers[]` keeps the last 20 moves (member, Workload, fence method, `evictedAt`, `requeuedAt`); Events
   `FailoverEvicted` and `FailoverRequeued` are emitted on the federation.

Without `--federation-failover`, or without both `spec.failover.enabled` and `automatic`, the controller still
tracks the failure streak but never fences or evicts.

## Fencing and the lease timeout

Moving a job while its first copy is still running would run it twice, with two writers on the same checkpoints.
Remote delete rules that out when the member's API server answers. When it does not (network partition, control
plane down), Gryvia cannot know whether the member's nodes are still running the pods. `leaseTimeout` is your
statement of how long that can last: set it no shorter than the time after which the member stops its own workloads
(for example node-lease expiry plus pod eviction, or a self-fencing agent on the member). With a short lease and a
partition that keeps the member's nodes running, both copies run until the partition heals and MultiKueue's garbage
collection removes the orphan.

## Checkpoints across clusters

A redispatched job starts on a different cluster with empty local storage. The trainer contract from
[CheckpointGuard](checkpoint-guard.md) covers that: with `GRYVIA_CHECKPOINT_REPLICA=s3://bucket/prefix` (set by a
`GryviaCheckpointGuard` with `checkpointPolicy.replication.target`, or directly in the job's `env`) rank 0 copies each
committed step to object storage (`examples/training/checkpoint_replica.py`, boto3, `AWS_ENDPOINT_URL` for
S3-compatible stores) and, when its checkpoint directory is empty, restores the latest replicated step before
resuming. The object store must be reachable from every member, and the job's credentials must exist there (MultiKueue
copies the Job spec, not the Secrets it references; plain env values or a Secret created on every member both work).

## Tested, and what is not

- Unit tests (fake clients): threshold, remote-delete fencing (remote Job and Workload deleted), lease fencing of an
  unreachable member, the two-phase deactivate/reactivate, workloads on other members and finished ones untouched,
  disabled modes, streak reset, assignment by `status.clusterName` and by the admission check message.
- Helm render test `scripts/tests/federation-chart.test.sh`.
- Kind e2e `.github/workflows/e2e-multikueue.yml` (manager plus two workers, Kueue 0.19, CPU torch): a checkpointing
  job with an S3 replica in a MinIO container runs on one worker; that worker's container is stopped; the federation
  marks it unhealthy, fences it by lease, evicts and requeues the Workload; MultiKueue runs it on the other worker,
  where the trainer restores the replicated step and finishes; the GryviaAIJob on the manager reaches Succeeded.
- Not tested: remote-delete fencing against a real outage, GPUs, more than one job per member, partitions where the
  member keeps running. Hardware verification remains open.
