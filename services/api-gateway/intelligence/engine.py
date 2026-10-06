"""Deterministic analysis over explicit inputs; unknown observations never imply health."""

from datetime import datetime, timezone
from math import ceil
from statistics import median

from .models import (
    Capacity,
    Economics,
    Fabric,
    Federation,
    Laboratory,
    Locality,
    Preflight,
    Recovery,
    Serving,
    Training,
)


def fresh(observed, max_age, now):
    age = (now - observed).total_seconds()
    return 0 <= age <= max_age


def report(mode, **data):
    return {"mode": mode, **data}


def preflight(body: Preflight):
    required = body.nodes * body.gpusPerNode
    # Decimal billions of parameters; binary GiB. Extra memory is explicit, not guessed.
    weight_gib = body.parametersBillions * 1e9 * body.weightBits / 8 / (1024**3)
    per_gpu = weight_gib / body.tensorParallel + body.extraMemoryGiBPerGPU
    blockers, unknown = [], []
    if body.quotaGPUs is None:
        unknown.append("quota availability not supplied")
    elif required > body.quotaGPUs:
        blockers.append("requested GPUs exceed supplied quota availability")
    if body.storageReady is False:
        blockers.append("storage is not ready")
    elif body.storageReady is None:
        unknown.append("storage readiness not supplied")
    if body.training and body.checkpointReady is not True:
        unknown.append("training recovery contract not qualified")
    candidates = []
    for pool in body.pools:
        why = []
        if pool.gpuType != body.gpuType:
            why.append("GPU type mismatch")
        if pool.nodes < body.nodes or pool.gpusPerNode < body.gpusPerNode:
            why.append("insufficient node shape")
        full_reserved, partial = divmod(pool.reservedGPUs, pool.gpusPerNode)
        usable_nodes = pool.nodes - full_reserved - (1 if partial else 0)
        if partial and pool.gpusPerNode - partial >= body.gpusPerNode:
            usable_nodes += 1
        if usable_nodes < body.nodes:
            why.append("reservations leave insufficient per-node shapes under packed reservation assumption")
        if pool.nodes * pool.gpusPerNode - pool.reservedGPUs < required:
            why.append("insufficient unreserved GPUs")
        if pool.memoryGiB < per_gpu:
            why.append("estimated memory exceeds per-GPU capacity")
        if body.requireRDMA and not pool.rdma:
            why.append("RDMA required")
        if body.requireFastInterconnect and pool.interconnect == "none":
            why.append("fast GPU interconnect required")
        cost = required * body.durationHours * pool.hourlyRate
        if body.budget is not None and cost > body.budget:
            why.append("estimated spend exceeds supplied budget")
        candidates.append(
            {
                "pool": pool.name,
                "eligible": not why,
                "reasons": why,
                "estimatedCost": round(cost, 6),
                "currency": pool.currency,
            }
        )
    if not any(c["eligible"] for c in candidates):
        blockers.append("no eligible supplied pool")
    return report(
        "estimate",
        state="Blocked" if blockers else "Incomplete" if unknown else "Candidate",
        requiredGPUs=required,
        estimatedMemoryGiBPerGPU=round(per_gpu, 3),
        blockers=blockers,
        unknown=unknown,
        candidates=candidates,
        assumptions=[
            "Pool capacities and readiness are caller-supplied snapshots, not reservations.",
            "Extra memory must include activations, optimizer state, KV cache and runtime overhead.",
            "Framework, driver and model compatibility require a qualified runtime probe.",
            "Reservations are assumed packed into nodes; actual reservation placement must be checked.",
        ],
    )


def training(body: Training, now=None):
    now = now or datetime.now(timezone.utc)
    usable = [r for r in body.ranks if fresh(r.observedAt, body.maxAgeSeconds, now)]
    missing = sorted(set(range(body.expectedRanks)) - {r.rank for r in usable})
    findings = []
    steps = [r.stepSeconds for r in usable]
    baseline = median(steps) if steps else None
    for r in usable:
        evidence = {
            "rank": r.rank,
            "source": r.source,
            "observedAt": r.observedAt.isoformat(),
            "stepSeconds": r.stepSeconds,
            "windowSeconds": r.windowSeconds,
        }
        if r.dataWaitSeconds is not None and r.dataWaitSeconds / r.stepSeconds >= 0.25:
            findings.append(
                {
                    "kind": "data-loading",
                    "confidence": "medium",
                    "evidence": {**evidence, "dataWaitSeconds": r.dataWaitSeconds},
                    "recommendation": "Compare storage throughput and loader workers; test prefetching.",
                }
            )
        if r.collectiveSeconds is not None and r.collectiveSeconds / r.stepSeconds >= 0.25:
            findings.append(
                {
                    "kind": "communication",
                    "confidence": "medium",
                    "evidence": {**evidence, "collectiveSeconds": r.collectiveSeconds},
                    "recommendation": "Qualify NCCL transport and compare rank placement and fabric measurements.",
                }
            )
        if len(usable) >= 3 and baseline and r.stepSeconds > baseline * 1.5:
            findings.append(
                {
                    "kind": "straggler",
                    "confidence": "medium",
                    "evidence": {**evidence, "medianStepSeconds": baseline},
                    "recommendation": "Compare this rank's data loader, CPU allocation and collective timings.",
                }
            )
        if r.gpuUtilization is not None and r.gpuUtilization < 40:
            findings.append(
                {
                    "kind": "low-gpu-activity",
                    "confidence": "low",
                    "evidence": {**evidence, "gpuUtilization": r.gpuUtilization},
                    "recommendation": "Correlate framework timings; low activity alone does not identify a cause.",
                }
            )
    missing_metrics = [
        {
            "rank": r.rank,
            "metrics": [k for k in ("dataWaitSeconds", "collectiveSeconds", "gpuUtilization") if getattr(r, k) is None],
        }
        for r in usable
    ]
    return report(
        "supplied-observations",
        state="Incomplete" if missing or not usable else "Observed",
        missingRanks=missing,
        missingMetrics=[m for m in missing_metrics if m["metrics"]],
        findings=findings,
        medianStepSeconds=baseline,
        caveat=(
            "Timing ratios are heuristics; overlapping timings and " "differing sample windows can distort comparisons."
        ),
    )


def economics(body: Economics):
    return report(
        body.costBasis,
        currency=body.currency,
        totalCost=body.cost,
        failedRunCost=body.failedRunCost,
        failedRunFraction=body.failedRunCost / body.cost if body.cost else None,
        costPerCompletedRun=body.cost / body.completedRuns if body.completedRuns else None,
        costPerCommittedCheckpoint=body.cost / body.committedCheckpoints if body.committedCheckpoints else None,
        costPerSuccessfulEvaluation=body.cost / body.successfulEvaluations if body.successfulEvaluations else None,
        costPerMillionDeliveredTokens=body.cost * 1e6 / body.deliveredTokens if body.deliveredTokens else None,
        caveat=(
            "Each unit cost uses the supplied total cost; "
            "it is not incremental attribution or payment-grade billing."
        ),
    )


def serving(body: Serving, now=None):
    now = now or datetime.now(timezone.utc)
    s = body.sample
    unknown = []
    if not fresh(s.observedAt, body.maxAgeSeconds, now):
        unknown.append("sample is stale or in the future")
    if s.requests < body.minRequests:
        unknown.append("insufficient request sample")
    for name in ("ttftP95Ms", "interTokenP95Ms", "queueDepth", "kvCachePercent"):
        if getattr(s, name) is None:
            unknown.append(f"{name} unavailable")
    if unknown:
        return report(
            "advisory",
            state="Unknown",
            reasons=unknown,
            recommendedReplicas=body.replicas,
            action="Hold",
            evidence=s.model_dump(mode="json"),
        )
    breaches = []
    error_rate = s.errors / s.requests if s.requests else None
    if error_rate is not None and error_rate > body.maxErrorRate:
        breaches.append("error rate")
    if s.ttftP95Ms > body.maxTTFTMs:
        breaches.append("time to first token")
    if s.interTokenP95Ms > body.maxInterTokenMs:
        breaches.append("inter-token latency")
    overloaded = bool(breaches) and (s.queueDepth > 0 or s.kvCachePercent >= 90)
    target = min(body.maxReplicas, body.replicas + 1) if overloaded else body.replicas
    return report(
        "advisory",
        state="Breached" if breaches else "WithinPolicy",
        breaches=breaches,
        errorRate=error_rate,
        recommendedReplicas=target,
        action="ScaleUp" if target > body.replicas else "Hold",
        evidence=s.model_dump(mode="json"),
        caveat="One sample never triggers scale-down. Recommendations require approval; HPA remains independent.",
    )


def laboratory(body: Laboratory, now=None):
    now = now or datetime.now(timezone.utc)
    accepted, rejected = [], []
    for b in body.benchmarks:
        why = []
        for name in ("model", "workloadDigest", "qualitySuite", "currency"):
            if getattr(b, name) != getattr(body, name):
                why.append(f"{name} mismatch")
        if not fresh(b.observedAt, body.maxAgeSeconds, now):
            why.append("stale or future benchmark")
        if b.qualityScore < body.minQuality:
            why.append("quality below policy")
        if b.ttftP95Ms > body.maxTTFTMs:
            why.append("TTFT exceeds policy")
        if b.successfulRequests < body.minSuccessfulRequests:
            why.append("insufficient request sample")
        if b.failedRequests / (b.failedRequests + b.successfulRequests) > body.maxErrorRate:
            why.append("error rate exceeds policy")
        if why:
            rejected.append({"name": b.name, "reasons": why})
        else:
            accepted.append(
                {**b.model_dump(mode="json"), "costPerMillionTokens": b.hourlyCost * 1e6 / (3600 * b.tokensPerSecond)}
            )
    accepted.sort(key=lambda b: (b["costPerMillionTokens"], b["ttftP95Ms"], b["name"]))
    frontier = [
        a["name"]
        for a in accepted
        if not any(
            b["costPerMillionTokens"] <= a["costPerMillionTokens"]
            and b["ttftP95Ms"] <= a["ttftP95Ms"]
            and b["qualityScore"] >= a["qualityScore"]
            and (
                b["costPerMillionTokens"] < a["costPerMillionTokens"]
                or b["ttftP95Ms"] < a["ttftP95Ms"]
                or b["qualityScore"] > a["qualityScore"]
            )
            for b in accepted
        )
    ]
    return report(
        "supplied-benchmarks",
        ranked=accepted,
        rejected=rejected,
        paretoFrontier=frontier,
        recommended=accepted[0]["name"] if accepted else None,
        caveat=(
            "Cost assumes the measured token throughput is sustained. "
            "Concurrency and workload conditions remain visible."
        ),
    )


def recovery(body: Recovery, now=None):
    now = now or datetime.now(timezone.utc)
    c = body.checkpoint
    why = []
    if not c:
        return report("advisory", recoverable=False, reasons=["no committed checkpoint"], lostSteps=None)
    for name in ("modelDigest", "datasetDigest", "framework"):
        if getattr(c, name) != getattr(body, name):
            why.append(f"{name} mismatch")
    if not c.integrityVerified:
        why.append("checkpoint integrity unverified")
    if not c.globallyCommitted or set(c.ranks) != set(range(c.worldSize)):
        why.append("incomplete global commit")
    if not c.durableStorage:
        why.append("storage durability unqualified")
    if c.committedAt > now:
        why.append("checkpoint timestamp is in the future")
    if c.worldSize != body.targetWorldSize and not c.optimizerReshardable:
        why.append("world-size change requires a qualified resharding adapter")
    if body.targetWorldSize > 1 and not body.rendezvousReady:
        why.append("rendezvous unavailable")
    lost = body.currentStep - c.step
    if lost < 0 or lost > body.maxLostSteps:
        why.append("lost progress outside recovery policy")
    return report(
        "advisory",
        recoverable=not why,
        reasons=why,
        resumeStep=c.step,
        lostSteps=max(0, lost),
        caveat="Metadata declarations must come from a verified adapter; this API does not read checkpoint blobs.",
    )


def locality(body: Locality):
    options = []
    for c in body.candidates:
        local = any(
            r.cluster == c.cluster and r.pool == c.pool and r.digest == body.datasetDigest and r.ready and r.verified
            for r in body.replicas
        )
        why = []
        if c.freeGPUs < body.requestedGPUs:
            why.append("insufficient free GPUs")
        if not local and c.transferBytesPerSecond is None:
            why.append("transfer throughput unknown")
        seconds = (
            0 if local else (body.datasetGiB * 1024**3 / c.transferBytesPerSecond if c.transferBytesPerSecond else None)
        )
        options.append(
            {
                "cluster": c.cluster,
                "pool": c.pool,
                "local": local,
                "eligible": not why,
                "reasons": why,
                "estimatedTransferSeconds": seconds,
                "estimatedEgressCost": 0 if local else body.datasetGiB * c.egressCostPerGiB,
            }
        )
    options.sort(
        key=lambda o: (
            not o["eligible"],
            not o["local"],
            o["estimatedTransferSeconds"] or 0,
            o["estimatedEgressCost"],
            o["cluster"],
            o["pool"],
        )
    )
    return report(
        "estimate",
        candidates=options,
        recommended=next((o for o in options if o["eligible"]), None),
        caveat="Planning only; no data transfer, cache population or placement reservation is performed.",
    )


def fabric(body: Fabric, now=None):
    now = now or datetime.now(timezone.utc)
    links = []
    for link in body.links:
        ratio = link.measuredGbps / link.expectedGbps
        state = (
            "Unknown"
            if not fresh(link.observedAt, body.maxAgeSeconds, now)
            else ("Degraded" if link.errors or ratio < body.minimumBandwidthRatio else "Qualified")
        )
        links.append({**link.model_dump(mode="json"), "state": state, "bandwidthRatio": ratio})
    return report(
        "supplied-measurements",
        state=(
            "Unknown"
            if not links or any(link["state"] == "Unknown" for link in links)
            else "Degraded" if any(link["state"] == "Degraded" for link in links) else "Qualified"
        ),
        links=links,
        caveat="Qualification applies only to supplied links and test windows, not the entire fabric.",
    )


def capacity(body: Capacity):
    # Per-GPU availability lanes preserve per-node shape for gang placement. Earlier reservations
    # are never displaced. This is a deterministic planning heuristic, not a Kueue replay.
    if sum(p.nodes for p in body.pools) > 8192 or sum(p.nodes * p.gpusPerNode for p in body.pools) > 65536:
        raise ValueError("scenario exceeds 8192 nodes or 65536 GPU lanes")
    lanes = {}
    for p in body.pools:
        reserve = p.reservedGPUs
        nodes = []
        for _ in range(p.nodes):
            take = min(reserve, p.gpusPerNode)
            reserve -= take
            nodes.append([0.0] * (p.gpusPerNode - take))
        lanes[p.name] = nodes
    runs, blocked, busy, cost = [], [], 0.0, 0.0
    for d in sorted(body.demand, key=lambda j: (j.arrivalSeconds, j.name)):
        choices = []
        for p in body.pools:
            if p.gpuType != d.gpuType:
                continue
            shapes = sorted(
                (sorted(node)[d.gpusPerNode - 1], i)
                for i, node in enumerate(lanes[p.name])
                if len(node) >= d.gpusPerNode
            )
            if len(shapes) >= d.nodes:
                selected = shapes[: d.nodes]
                start = max(d.arrivalSeconds, selected[-1][0])
                choices.append((start, p.hourlyRate, p.name, selected))
        if not choices:
            blocked.append({"name": d.name, "reason": "no pool can satisfy the unreserved per-node GPU shape"})
            continue
        start, rate, pool, selected = min(choices)
        end = start + d.durationSeconds
        for _, index in selected:
            slots = lanes[pool][index]
            slots.sort()
            slots[: d.gpusPerNode] = [end] * d.gpusPerNode
        gpu_seconds = d.nodes * d.gpusPerNode * d.durationSeconds
        run_cost = gpu_seconds * rate / 3600
        busy += gpu_seconds
        cost += run_cost
        runs.append(
            {
                "name": d.name,
                "pool": pool,
                "startSeconds": start,
                "endSeconds": end,
                "queueSeconds": start - d.arrivalSeconds,
                "estimatedCost": run_cost,
            }
        )
    horizon = max((r["endSeconds"] for r in runs), default=0)
    free = sum(len(node) for nodes in lanes.values() for node in nodes)
    waits = sorted(r["queueSeconds"] for r in runs)
    return report(
        "simulation",
        runs=runs,
        blocked=blocked,
        estimatedCost=cost,
        currency=body.currency,
        horizonSeconds=horizon,
        utilization=busy / (free * horizon) if free and horizon else None,
        p95QueueSeconds=waits[max(0, ceil(len(waits) * 0.95) - 1)] if waits else None,
        assumptions=[
            "Non-preemptive reservations, fixed durations, one GPU type/pool per job.",
            "Reserved GPUs are removed by packing reservations into nodes first.",
            "No quota borrowing, checkpoint overhead, autoscaling or Kueue policy emulation.",
        ],
    )


def federation(body: Federation, now=None):
    now = now or datetime.now(timezone.utc)
    candidates = []
    for c in body.clusters:
        why = []
        if not fresh(c.observedAt, body.maxAgeSeconds, now):
            why.append("stale or future cluster observation")
        if c.region not in body.allowedRegions:
            why.append("data residency policy")
        if not c.ready or not c.credentialConfigured:
            why.append("cluster readiness or credentials unavailable")
        if c.gpuType != body.gpuType or c.freeGPUs < body.gpus:
            why.append("GPU capacity mismatch")
        if c.currency != body.currency:
            why.append("currency mismatch; FX conversion is not supported")
        if body.datasetDigest not in c.datasetDigests:
            why.append("verified dataset replica missing")
        candidates.append(
            {"cluster": c.name, "eligible": not why, "reasons": why, "estimatedHourlyCost": c.hourlyRate * body.gpus}
        )
    candidates.sort(key=lambda c: (not c["eligible"], c["estimatedHourlyCost"], c["cluster"]))
    return report(
        "advisory",
        candidates=candidates,
        selected=next((c["cluster"] for c in candidates if c["eligible"]), None),
        caveat=(
            "No remote dispatch or failover. Use existing MultiKueue "
            "for execution; data replication remains external."
        ),
    )


ANALYZERS = {
    "preflight": (Preflight, preflight),
    "training": (Training, training),
    "economics": (Economics, economics),
    "serving": (Serving, serving),
    "laboratory": (Laboratory, laboratory),
    "recovery": (Recovery, recovery),
    "locality": (Locality, locality),
    "fabric": (Fabric, fabric),
    "capacity": (Capacity, capacity),
    "federation": (Federation, federation),
}
