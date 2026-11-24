# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""GPU right-sizing recommendations from each job's measured utilization (``status.metrics.gpuUtilization``).

Kubecost's request sizing and Run:ai's utilization views do the same for CPU and memory: compare what a workload
asked for with what it used and say what it could have asked for. A job that held ``gpus`` GPUs at ``u`` percent
average utilization needs about ``gpus * u / target`` of them. A one-GPU job below the target is advised to share
a GPU (a quarter, half or three quarters, the ``gryvia.io/gpu-share`` values); a multi-GPU job is advised to ask
for fewer. Saved GPU-hours are the GPUs it could give back times the hours it ran. Advice only: nothing is changed,
and a job without a utilization sample is not reported.
"""
import math
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Optional

from fastapi import APIRouter, Depends, Query, Request

from .common import Deps
from .jobview import load_jobs, name_of, ns_of, num, queue_of, spec_of, status_of, tenant_of_ns
from .uiutil import parse_ts

SHARES = (0.25, 0.5, 0.75)


def recommend(gpus: int, util_pct: float, target_pct: float) -> Optional[Dict[str, Any]]:
    """Smaller request for ``gpus`` GPUs used at ``util_pct`` percent, or None when the request is already right."""
    if gpus < 1 or util_pct <= 0 or util_pct >= target_pct:
        return None
    need = gpus * util_pct / target_pct  # GPUs worth of work, at the target utilization
    if gpus == 1:
        share = next((s for s in SHARES if need <= s), None)
        return {"gpus": 1, "gpuShare": share, "freedGpus": round(1 - share, 4)} if share else None
    rec = max(1, math.ceil(need))
    return {"gpus": rec, "gpuShare": None, "freedGpus": gpus - rec} if rec < gpus else None


def rightsizing(jobs: List[Dict[str, Any]], lo: datetime, hi: datetime, *, target_pct: float,
                min_hours: float) -> Dict[str, Any]:
    items: List[Dict[str, Any]] = []
    sampled = 0
    for j in jobs:
        st, spec = status_of(j), spec_of(j)
        if st.get("phase") not in ("Running", "Succeeded"):
            continue
        start = parse_ts(st.get("startTime"))
        gpus = int(num(spec.get("gpus")))
        util = num((st.get("metrics") or {}).get("gpuUtilization"))
        if start is None or gpus < 1 or util <= 0 or not lo <= start <= hi:
            continue
        sampled += 1
        end = parse_ts(st.get("completionTime")) or hi
        hours = max(0.0, (end - start).total_seconds() / 3600.0)
        rec = recommend(gpus, min(util, 100.0), target_pct)
        if rec is None or hours < min_hours:
            continue
        nodes = max(1, int(num((spec.get("distributed") or {}).get("nodes")) or 1))
        items.append({"name": name_of(j), "namespace": ns_of(j), "tenant": tenant_of_ns(ns_of(j)),
                      "queue": queue_of(j), "phase": st.get("phase"), "gpus": gpus, "nodes": nodes,
                      "utilizationPct": round(util, 2), "hours": round(hours, 3), "recommendation": rec,
                      "savedGpuHours": round(rec["freedGpus"] * nodes * hours, 4)})
    items.sort(key=lambda i: (-i["savedGpuHours"], i["name"]))
    by_tenant: Dict[str, float] = {}
    for i in items:
        by_tenant[i["tenant"]] = by_tenant.get(i["tenant"], 0.0) + i["savedGpuHours"]
    return {"targetUtilPct": target_pct, "jobsWithTelemetry": sampled, "oversized": len(items),
            "savedGpuHours": round(sum(i["savedGpuHours"] for i in items), 4),
            "byTenant": [{"tenant": t, "savedGpuHours": round(h, 4)}
                         for t, h in sorted(by_tenant.items(), key=lambda kv: (-kv[1], kv[0]))],
            "items": items}


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/rightsizing")
    @deps.limiter.limit("30/minute")
    async def get_rightsizing(request: Request, days: int = Query(default=14, ge=1, le=90),
                              targetUtilPct: float = Query(default=70.0, gt=0, le=100),
                              minHours: float = Query(default=0.5, ge=0, le=720),
                              tenant: Optional[str] = Query(default=None, max_length=63),
                              limit: int = Query(default=100, ge=1, le=1000),
                              _=Depends(deps.verify_auth)):
        hi = datetime.now(timezone.utc)
        jobs = await load_jobs(request, deps)
        if tenant:
            jobs = [j for j in jobs if tenant_of_ns(ns_of(j)) == tenant]
        body = rightsizing(jobs, hi - timedelta(days=days), hi, target_pct=targetUtilPct, min_hours=minHours)
        body["days"] = days
        body["truncated"] = len(body["items"]) > limit
        body["items"] = body["items"][:limit]
        return body

    return router
