# Fabric-aware scheduling (opt-in)

Connects fresh per-node fabric health from the eBPF collector to the ai-operator's node ranking. **Everything here is off
by default**; with the defaults no flag, RBAC rule, status field or pod-spec field changes.

Signal quality depends on the collector's fabric probes (NCCL, RDMA, GDS, RoCE CNP, PFC). They have been exercised only
against fakes (canned signals, fake API servers, fake clients) and are **unverified on real GPU/RDMA/NCCL hardware**. Treat
the penalty as a nudge, not a health verdict.

## What is wired

1. **Per-node signal.** With `-publish-node-fabric` (chart `ebpf.publishNodeFabric=true` in `helm/network-intelligence`)
   each collector writes ONE cluster-scoped `GryviaNodeFabric` named after its `NODE_NAME` every 30 s: merge-patch, and a
   create on 404. Fields (all under `spec`): `nodeName`, `scoreDelta` in [0,1], `reasons` (at most 8), `measuredAt`,
   `ttlSeconds` (300), `expiresAt`. `scoreDelta` is the worst per-group `ScoreDelta` of the collector's fold window
   (any attributed job, `_unattributed` or node-level `_node/*` group on that node); the reasons are the thresholds that
   group crossed, each tagged with its group. Chart RBAC (only when on): `create`, `patch` on `gryvianodefabrics`.
   Kubernetes RBAC cannot restrict `create` to one name; the collector only ever addresses its own node's object.
   A CRD was chosen over status fields on `GryviaGpuNode` because that kind is owned and rewritten by the gpu-operator
   and is copied into two Go modules.
2. **Expiry.** The operator ignores a signal without `measuredAt`, one older than its `ttlSeconds` (default 5 min, at most
   24 h) and one dated more than 1 min in the future. A stopped collector therefore stops influencing placement by itself.
   A missing CRD, missing RBAC or list error (bounded to 5 s) also means "no signal": nothing is ever penalised on stale or
   missing data.
3. **Ranking.** With `--fabric-aware-scheduling` (chart `aiOperator.fabricAwareScheduling=true`) or the job annotation
   `gryvia.io/fabric-aware: "true"`, `FindOptimalNodesFabric` subtracts `25 * scoreDelta` points (hard cap 25, lowerable with
   `--fabric-max-penalty`) from each node's score before choosing nodes. Annotation `"false"` opts a job out even when
   the operator flag is on. Base scores are capacity and topology points (typically tens to a few hundred), so this
   reorders near-ties and mildly different nodes; it is never a filter and never removes a node from the candidate set.
   Ordering is stable: equal scores fall back to the node name.
4. **Placement effect.** Before this change `status.nodesAllocated` was only a record: the StatefulSet is placed by the
   default kube-scheduler from `nodeSelector` (`gryvia.io/gpu`, `gryvia.io/rdma`, the job's own) and `spec.affinity`, and
   never reads `nodesAllocated`. When a fabric penalty changed the ranking, the generated pod template now gets a **soft**
   `preferredDuringSchedulingIgnoredDuringExecution` node-affinity term (weight 100, `matchFields metadata.name In [node]`)
   for each selected node, appended to the job's own affinity (which is deep-copied, never mutated).
5. **Explanation.** `status.placementExplanation` (bounded to 16 entries: the top 8 by final score plus penalised nodes
   of the pre-fabric top 8) lists `node, baseScore, fabricPenalty, finalScore, reasons[], signalAgeSeconds`, and a
   `FabricAwarePlacement` Event is emitted on the job. Neither appears when no fresh signal changed a score. The gateway's
   `GET /api/jobs/{name}` returns the job object unchanged, so the field is already visible there; there is no dashboard card yet.

## Why the placement mechanism is safe

- Preferred affinity is a scoring hint: it can never make a pod unschedulable. If no selected node fits, the kube-scheduler
  places it elsewhere exactly as before.
- Required terms, `nodeSelector` and tolerations are untouched, so no eligibility rule is loosened or tightened.
- It is applied only at workload **creation** (Job or StatefulSet). Reconcile of an existing workload does not compare affinity, so
  toggling the feature or a changing signal never edits a running job's pod template (which would roll its pods).
- It is applied only when a penalty actually changed something, so unaffected jobs are byte-identical.

## Enabling

```
# collector chart:  ebpf.enabled=true, ebpf.publishNodeFabric=true
# ai-operator chart: aiOperator.fabricAwareScheduling=true      (adds get/list/watch on gryvianodefabrics)
```

A per-job `gryvia.io/fabric-aware: "true"` with the operator flag off also works, but needs the read RBAC, which the chart
adds only when `aiOperator.fabricAwareScheduling` is true. Without it the lookup fails open and nothing changes. Example
manifests: `examples/crds/gryvianodefabric-examples.yaml`.

## Limits

- Only new scheduling decisions are affected (phase Pending, or Scheduling with no nodes). Running jobs are never moved.
- The kube-scheduler still decides: a soft preference competes with its other scores and with pod spreading.
- `hostname`-style names: the term matches `metadata.name`, so it needs the operator's node names to be real node names
  (they are: they come from the Node list).
- No hard threshold or `NotIn` exclusion is implemented; it would need explicit configuration and a failure story for
  jobs that then cannot schedule, so it was left out.
- One node value per collector: a straggler job on a node marks the whole node, whatever the cause. Multi-tenant nodes
  can therefore be penalised for one job's behaviour.
- Verified with unit and fake-client tests only; not run against a real cluster or hardware.
