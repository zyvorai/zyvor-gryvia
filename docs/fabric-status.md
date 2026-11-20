# Fabric status and quota pacing

Two opt-in features of the eBPF collector (`collector/`, chart `helm/network-intelligence`). Both are **off by default**;
with the defaults the chart renders no new flags and no new RBAC.

## 1. Fabric status in `GryviaFabricSignal`

Fabric signals are attributed to a job through the Flight Recorder's resolver: host PID, the pod UID in
`/host/proc/<pid>/cgroup`, the node's pod list, its `gryvia.io/job` label. Anything that does not resolve stays under
`_unattributed/<process>` (node counters under `_node/cnp` and `_node/pfc`); the resolver fails closed and never guesses.

With `-publish-fabric-status` (chart `ebpf.publishFabricStatus=true`) the collector, every 30 s and only inside a cluster:

- lists `GryviaFabricSignal` objects in the namespace of each attributed job,
- and for those whose `spec.jobRef` equals the job name sends a merge-patch to `.../gryviafabricsignals/<name>/status`.

It creates nothing, never touches `spec`, skips `_unattributed` and `_node/*`, handles at most 256 jobs per cycle, and
logs (never fails on) API errors. RBAC added to the collector ClusterRole only when the value is on: `list` on
`gryviafabricsignals`, `patch` on `gryviafabricsignals/status`.

### Several nodes per job

A job spanning nodes has one collector per node, each seeing only its own pods. Two modes:

**Default (top-level merge patch).** The patch carries only the fields THIS node measured. It never sends `0` or `null`
for something it did not measure, so a node that saw nothing cannot erase another node's values. Fields with no
"measured" flag (`stragglerRank`, `ncclP99ms`, `rdmaRetryRate`, `overlapIdleRatio`, `cnpRate`, `pfcRate`,
`inferWaitP99ms`, `exfilEvents`, `collectiveMaxSkewMs`, `scoreDelta`) count as measured when non-zero; the flagged ones
(`gdsHitRatio`, the DCGM ratios, the engine fields) when the probe delivered a reading. A field this same collector sent
earlier and no longer measures is cleared once with an explicit `null`, so a recovered value goes away. Still, two nodes
that both measure a field overwrite each other (last writer wins for that field), so the top-level value can flap
between nodes.

**Per node (`-fabric-status-per-node`, chart `ebpf.fabricStatusPerNode=true`, needs `publishFabricStatus`).** Each
collector server-side-applies (`Content-Type: application/apply-patch+yaml`, field manager `gryvia-collector-<node>`,
`force=true`) only its own entry of `status.nodes[]` (a list map keyed by `node`), so writers never conflict:

```yaml
status:
  nodes:
  - node: gpu-1
    measuredAt: "2026-05-01T12:00:00Z"
    ttlSeconds: 300
    sampleCount: 412
    ncclP99ms: 84
    stragglerRank: 3
    gdsHitRatio: 0.9
```

The entry lists only measured fields, and is released (removed) when the job leaves the node. No extra RBAC beyond the
default mode (`patch` on `gryviafabricsignals/status`). The list is capped at 256 nodes. Without the merger below only
`status.nodes[]` is filled and the top-level fields stay empty.

**Merger (ai-operator `--merge-fabric-signals`, chart `aiOperator.mergeFabricSignals=true`, default off).** A small
reconciler on `GryviaFabricSignal` folds the fresh entries into the top-level status:

| Field(s) | Rule |
| --- | --- |
| `scoreDelta`, `ncclP99ms`, `rdmaRetryRate`, `pfcRate`, `cnpRate`, `exfilEvents`, `collectiveMaxSkewMs`, `inferWaitP99ms`, `ttftP99ms`, `itlP99ms`, `queueTimeP99ms`, `e2eP99ms`, queue/e2e means, `requestsWaiting`, `kvCacheUsage` | maximum over fresh nodes |
| `stragglerRank` | from the node with the highest `ncclP99ms` (ties: lowest node name); without any NCCL figure, the newest entry with a rank |
| `gdsHitRatio`, `overlapIdleRatio`, `gpuIdleDuringCommRatio`, `smActiveDuringCompute`, `gpuCorrelationCoverage` | mean weighted by `sampleCount` (minimum weight 1), skipping nodes that did not measure it |
| `engine` | the engine name, or `mixed` when nodes differ |
| `updatedAt` | newest `measuredAt` |

An entry older than its `ttlSeconds` (default 300) is ignored, so a dead collector drops out by itself; when every entry
is stale, or there are none (collectors in the default mode), the top-level status is left untouched. Fields no fresh node
measured are cleared. Limits: because `overlapIdleRatio` has no measured flag, a node reporting zero is treated as not
measured and does not pull the mean down. Verified with unit tests and a
fake API server/client only; server-side apply on a real API server and the merger against real CRDs are not exercised
in this repository's tests.

See also [fabric-scheduling.md](fabric-scheduling.md) for the per-node signal used by opt-in fabric-aware scheduling.

## 2. Quota-driven pacing (MUTATING)

`GryviaQuota.spec.network.maxEgressMbps` (optional, 1 to 32000) caps the outbound TCP rate of **each new connection**
opened by pods in `spec.namespaces` (`SO_MAX_PACING_RATE` via the `quota_pace` sockops program). It is not an aggregate
bandwidth limit, does not apply to UDP, RDMA or connections that already exist, and a new container can be unpaced for up
to one sync interval (30 s).

It runs only when all of these are set: `-quota-pace`, `-cgroup-path` (cgroup v2 root, for example
`/host/sys/fs/cgroup`), `-quota-pace-sync`, a cluster (`KUBERNETES_SERVICE_HOST`), `NODE_NAME` and a readable service
account namespace. Chart values: `ebpf.cgroupPath`, `ebpf.quotaPace.enabled`, `ebpf.quotaPace.sync`; the chart adds `list`
on `gryviaquotas` to the collector ClusterRole only when `sync` is on. Start with `ebpf.quotaPace.dryRun=true` (flag
`-quota-pace-dry-run`): it logs `would grant` / `would revoke` and never writes the map.

Every 30 s the collector lists quotas with a cap, lists the Running pods of each listed namespace on this node, resolves
each pod's cgroup id (inode of the pod's cgroup directory and its container directories under `-cgroup-path`; cgroupfs
and systemd layouts) and grants the `Pacer` a 2-minute lease. Safety rules, each covered by a unit test:

- an API error (quota list or any pod list) grants nothing new and revokes nothing; leases then expire on their own
  (fail open);
- only cgroups below `-cgroup-path` are granted (validated UID and QoS, symlinks and escapes refused, root never granted);
- `kube-system`, `gryvia-system`, `kube-public`, `kube-node-lease` and the collector's own namespace are never paced,
  and the collector refuses to sync when it cannot tell its own namespace;
- caps outside 1..32000 Mbit/s are skipped (never clamped up); of several quotas the strictest wins; the program also
  floors any rate at 1 Mbit/s;
- a removed quota, a cap of 0 or a vanished pod is revoked; expiry deletes the map entry; shutdown deletes every entry;
- every grant, renewal, revoke, expiry and shutdown deletion is logged;
- nothing is exposed over HTTP.

An already paced socket keeps its rate until it closes; revoking only unpaces new connections.

Verified on the Linux 7.0 test host with the real `pace_rate` map and real sockops attached to a throwaway cgroup (never
the root): the granted cgroup's new loopback connections were clamped to the cap, a sibling cgroup stayed unlimited, and
revoke, a failed list, expiry and shutdown behaved as described. The Kubernetes side was faked there. Unverified: a real
API server, real quotas on real workloads, and GPU or RDMA traffic.
