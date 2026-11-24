# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Queue visibility and wait-time statistics.

``GET /api/queue`` lists the jobs that have not started with their place in line (Kueue's pending-workloads API
gives ``positionInClusterQueue`` / ``positionInLocalQueue`` for the same purpose): highest priority first, then the
oldest. ``GET /api/queue/stats`` reports how long jobs that did start waited (p50 / p90 / max, per tenant and per
queue) over a window. Both read GryviaAIJob objects only. Positions are computed over every waiting job in the
cluster, so a tenant sees its real place in line but only its own jobs.
"""
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Optional

from fastapi import APIRouter, Depends, Query, Request

from .common import Deps, list_items
from .jobview import (PLURAL, WAITING_PHASES, created_of, load_jobs, name_of, ns_of, num, percentile, queue_of,
                      spec_of, status_of, tenant_of_ns, total_gpus)
from .uiutil import parse_ts


def _reason(job: Dict[str, Any]) -> Optional[str]:
    st = status_of(job)
    return (st.get("queueExplanation") or {}).get("category") or st.get("message") or None


def waiting_order(jobs: List[Dict[str, Any]], now: datetime) -> List[Dict[str, Any]]:
    """Waiting jobs in the order they are considered: priority descending, creation time ascending, name."""
    waiting = [j for j in jobs if status_of(j).get("phase", "") in WAITING_PHASES and not status_of(j).get("startTime")]
    floor = datetime.min.replace(tzinfo=timezone.utc)
    waiting.sort(key=lambda j: (-num(spec_of(j).get("priority")), created_of(j) or floor, ns_of(j), name_of(j)))
    rows: List[Dict[str, Any]] = []
    per_queue: Dict[str, int] = {}
    per_tenant: Dict[str, int] = {}
    ahead_gpus = 0
    for pos, j in enumerate(waiting):
        q, t = queue_of(j), tenant_of_ns(ns_of(j))
        created = created_of(j)
        rows.append({
            "name": name_of(j), "namespace": ns_of(j), "tenant": t, "queue": q,
            "phase": status_of(j).get("phase") or "Pending",
            "priority": int(num(spec_of(j).get("priority"))), "gpus": total_gpus(j),
            "position": pos, "positionInQueue": per_queue.get(q, 0), "positionInTenant": per_tenant.get(t, 0),
            "gpusAhead": ahead_gpus, "reason": _reason(j),
            "waitingSeconds": max(0, int((now - created).total_seconds())) if created else None,
        })
        per_queue[q] = per_queue.get(q, 0) + 1
        per_tenant[t] = per_tenant.get(t, 0) + 1
        ahead_gpus += total_gpus(j)
    return rows


def _summarize(waits: List[float]) -> Dict[str, Any]:
    waits = sorted(waits)
    return {"started": len(waits), "p50Seconds": percentile(waits, 50), "p90Seconds": percentile(waits, 90),
            "maxSeconds": waits[-1] if waits else None,
            "meanSeconds": round(sum(waits) / len(waits), 2) if waits else None}


def wait_stats(jobs: List[Dict[str, Any]], lo: datetime, hi: datetime) -> Dict[str, Any]:
    """Wait (creation to start) of jobs that started in [lo, hi], overall and per tenant and queue."""
    samples = []
    for j in jobs:
        start, created = parse_ts(status_of(j).get("startTime")), created_of(j)
        if start is None or created is None or not lo <= start <= hi:
            continue
        samples.append((tenant_of_ns(ns_of(j)), queue_of(j), max(0.0, (start - created).total_seconds())))

    def group(idx: int, key: str) -> List[Dict[str, Any]]:
        by: Dict[str, List[float]] = {}
        for s in samples:
            by.setdefault(s[idx], []).append(s[2])
        rows = [{key: k, **_summarize(v)} for k, v in by.items()]
        rows.sort(key=lambda r: (-(r["p90Seconds"] or 0), r[key]))
        return rows

    return {"overall": _summarize([s[2] for s in samples]), "byTenant": group(0, "tenant"),
            "byQueue": group(1, "queue")}


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/queue")
    @deps.limiter.limit("60/minute")
    async def get_queue(request: Request, queue: Optional[str] = Query(default=None, max_length=63),
                        tenant: Optional[str] = Query(default=None, max_length=63),
                        limit: int = Query(default=200, ge=1, le=1000),
                        _=Depends(deps.verify_auth)):
        rows = waiting_order(await list_items(deps, PLURAL), datetime.now(timezone.utc))  # cluster-wide positions
        mine = {(ns_of(j), name_of(j)) for j in await load_jobs(request, deps)}
        rows = [r for r in rows if (r["namespace"], r["name"]) in mine]
        if queue:
            rows = [r for r in rows if r["queue"] == queue]
        if tenant:
            rows = [r for r in rows if r["tenant"] == tenant]
        waits = [r["waitingSeconds"] for r in rows if r["waitingSeconds"] is not None]
        return {"waiting": len(rows), "items": rows[:limit], "truncated": len(rows) > limit,
                "gpusWaiting": sum(r["gpus"] for r in rows),
                "oldestWaitSeconds": max(waits) if waits else None}

    @router.get("/api/queue/stats")
    @deps.limiter.limit("30/minute")
    async def get_queue_stats(request: Request, days: int = Query(default=7, ge=1, le=90),
                              _=Depends(deps.verify_auth)):
        hi = datetime.now(timezone.utc)
        out = wait_stats(await load_jobs(request, deps), hi - timedelta(days=days), hi)
        out["days"] = days
        return out

    return router
