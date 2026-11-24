# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Failure report: why jobs failed, grouped by cause.

Jobs that ended in Failed, Preempted or Rejected are classified from the phase, the status message and the condition
reasons into a small taxonomy (out of memory, preempted, node failure, image pull, deadline, quota or admission,
scheduling, application error). Each category is marked ``transient`` (a retry can succeed: preemption, node loss,
image pull) or deterministic (it will fail again until something changes: out of memory, quota, application error),
the split GPU-cluster failure studies use to decide whether to retry. A rule-based classifier on what the operator
already records; it reads no logs.
"""
import csv
import io
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Literal, Optional

from fastapi import APIRouter, Depends, Query, Request
from fastapi.responses import Response

from .common import Deps
from .jobview import load_jobs, name_of, ns_of, num, queue_of, status_of, tenant_of_ns, total_gpus
from .uiutil import parse_ts

FAILED_PHASES = ("Failed", "Preempted", "Rejected")

# (category, transient, markers) in priority order; the first category with a marker in the text wins.
RULES = [
    ("oom", False, ("oomkilled", "out of memory", "cuda oom", "memory limit")),
    ("image-pull", True, ("imagepullbackoff", "errimagepull", "image pull", "pull access denied", "invalidimagename")),
    ("node-failure", True, ("nodelost", "node lost", "node failure", "notready", "node not ready", "node is gone",
                            "xid", "gpu fell off")),
    ("preempted", True, ("preempt", "evicted")),
    ("deadline", False, ("deadlineexceeded", "deadline", "timed out", "timeout")),
    ("quota", False, ("quota", "admission", "denied", "forbidden", "budget")),
    ("scheduling", False, ("unschedulable", "no nodes", "insufficient", "no available", "failedscheduling")),
    ("app-error", False, ("backofflimitexceeded", "exit code", "exited with", "error", "crashloop")),
]
CATEGORIES = [r[0] for r in RULES] + ["unknown"]
TRANSIENT = {c: t for c, t, _ in RULES}


def _text(job: Dict[str, Any]) -> str:
    st = status_of(job)
    parts = [st.get("message") or ""]
    for c in st.get("conditions") or []:
        if c.get("status") != "True" or c.get("type") in ("Failed", "Ready", "Scheduled"):
            parts.append(f"{c.get('reason') or ''} {c.get('message') or ''}")
    return " ".join(parts).lower()


def classify(job: Dict[str, Any]) -> Dict[str, Any]:
    """Category of a failed job. A Preempted phase is preempted whatever the text says."""
    st = status_of(job)
    phase = st.get("phase") or ""
    text = _text(job)
    if phase == "Preempted":
        cat = "preempted"
    elif phase == "Rejected" and not any(m in text for _, _, ms in RULES[:3] for m in ms):
        cat = "quota"
    else:
        cat = next((c for c, _, ms in RULES if any(m in text for m in ms)), "unknown")
    return {"category": cat, "transient": TRANSIENT.get(cat, False)}


def _ended(job: Dict[str, Any]) -> Optional[datetime]:
    st = status_of(job)
    return parse_ts(st.get("completionTime")) or parse_ts(st.get("startTime")) or parse_ts(
        (job.get("metadata") or {}).get("creationTimestamp"))


def failure_report(jobs: List[Dict[str, Any]], lo: datetime, hi: datetime) -> Dict[str, Any]:
    rows: List[Dict[str, Any]] = []
    for j in jobs:
        st = status_of(j)
        if st.get("phase") not in FAILED_PHASES:
            continue
        ended = _ended(j)
        if ended is None or not lo <= ended <= hi:
            continue
        c = classify(j)
        rows.append({"name": name_of(j), "namespace": ns_of(j), "tenant": tenant_of_ns(ns_of(j)),
                     "queue": queue_of(j), "phase": st.get("phase"), "gpus": total_gpus(j),
                     "retries": int(num(st.get("retries"))), "ended": ended.strftime("%Y-%m-%dT%H:%M:%SZ"),
                     "message": (st.get("message") or "")[:300], **c})
    rows.sort(key=lambda r: (r["ended"], r["name"]), reverse=True)
    by_cat: Dict[str, Dict[str, Any]] = {}
    for r in rows:
        b = by_cat.setdefault(r["category"], {"category": r["category"], "transient": r["transient"], "jobs": 0,
                                              "gpus": 0, "retries": 0, "tenants": {}})
        b["jobs"] += 1
        b["gpus"] += r["gpus"]
        b["retries"] += r["retries"]
        b["tenants"][r["tenant"]] = b["tenants"].get(r["tenant"], 0) + 1
    cats = sorted(by_cat.values(), key=lambda b: (-b["jobs"], b["category"]))
    n = len(rows)
    for b in cats:
        b["share"] = round(b["jobs"] / n, 4)
    return {"total": n, "transient": sum(1 for r in rows if r["transient"]),
            "categories": cats, "items": rows}


CSV_COLUMNS = ["name", "namespace", "tenant", "queue", "phase", "category", "transient", "gpus", "retries",
               "ended", "message"]


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/failures")
    @deps.limiter.limit("30/minute")
    async def get_failures(request: Request, days: int = Query(default=7, ge=1, le=90),
                           category: Optional[Literal[tuple(CATEGORIES)]] = None,  # type: ignore[valid-type]
                           tenant: Optional[str] = Query(default=None, max_length=63),
                           limit: int = Query(default=100, ge=1, le=1000),
                           format: Literal["json", "csv"] = "json",
                           _=Depends(deps.verify_auth)):
        hi = datetime.now(timezone.utc)
        jobs = await load_jobs(request, deps)
        if tenant:
            jobs = [j for j in jobs if tenant_of_ns(ns_of(j)) == tenant]
        body = failure_report(jobs, hi - timedelta(days=days), hi)
        if category:
            body["items"] = [r for r in body["items"] if r["category"] == category]
        body["days"] = days
        body["truncated"] = len(body["items"]) > limit
        body["items"] = body["items"][:limit]
        if format == "csv":
            buf = io.StringIO()
            w = csv.DictWriter(buf, fieldnames=CSV_COLUMNS, lineterminator="\n")
            w.writeheader()
            for r in body["items"]:
                w.writerow({k: _cell(r[k]) for k in CSV_COLUMNS})
            return Response(buf.getvalue(), media_type="text/csv", headers={
                "Content-Disposition": f'attachment; filename="gryvia-failures-{hi.strftime("%Y%m%d")}.csv"'})
        return body

    return router


def _cell(v: Any) -> Any:
    """Spreadsheet formula injection guard: a text cell that starts with = + - @ is prefixed with a quote."""
    if isinstance(v, str) and v[:1] in ("=", "+", "-", "@", "\t", "\r"):
        return "'" + v
    return v
