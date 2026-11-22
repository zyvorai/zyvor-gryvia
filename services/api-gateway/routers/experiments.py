# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Experiment routes (GryviaLiveExperiment, namespaced): the leaderboard view for the dashboard and CLI.

Read-only. The ai-operator's controller owns the status; this module only projects it and adds, per leaderboard
entry, how far it is from the best run, so two runs can be compared without client-side arithmetic.
Tenant isolation is the same as every other namespaced kind (uiutil.list_all / find_one).
"""
from typing import Any, Dict, List

from fastapi import APIRouter, Depends, Request

from .common import Deps
from .uiutil import find_one, list_all, meta, prune

PLURAL = "gryvialiveexperiments"


def _num(v: Any):
    return float(v) if isinstance(v, (int, float)) and not isinstance(v, bool) else None


def leaderboard(entries: List[Dict[str, Any]], direction: str) -> List[Dict[str, Any]]:
    """Entries sorted best first with ``deltaToBest`` (metric - best) and ``deltaPct`` (None when best is 0).

    Direction is "minimize" or "maximize"; anything else keeps the operator's ranks. Entries whose metric is
    not a number are kept, unranked by this function, and sorted last.
    """
    rows = [dict(e) for e in entries or []]
    vals = [_num(r.get("primaryMetricValue")) for r in rows]
    scored = [v for v in vals if v is not None]
    if direction in ("minimize", "maximize") and scored:
        best = min(scored) if direction == "minimize" else max(scored)
        sign = 1 if direction == "minimize" else -1
        order = sorted(range(len(rows)), key=lambda i: (vals[i] is None, sign * (vals[i] or 0)))
        rows = [rows[i] for i in order]
        vals = [vals[i] for i in order]
        for i, r in enumerate(rows):
            r["rank"] = i + 1
            if vals[i] is not None:
                r["deltaToBest"] = round(vals[i] - best, 12)
                r["deltaPct"] = round((vals[i] - best) / abs(best) * 100, 4) if best != 0 else None
    else:
        rows.sort(key=lambda r: (r.get("rank") is None, r.get("rank") or 0))
    return rows


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    cmp_ = spec.get("comparison") or {}
    direction = cmp_.get("direction") or ""
    return {
        "metadata": meta(obj),
        "spec": prune({
            "description": spec.get("description"),
            "primaryMetric": cmp_.get("primaryMetric"),
            "direction": direction or None,
            "jobs": [prune({"name": j.get("name"), "jobRef": j.get("jobRef")}) for j in spec.get("jobs") or []],
        }),
        "status": prune({
            "phase": st.get("phase"),
            "startedAt": st.get("startTime"),
            "leaderboard": [prune(r) for r in leaderboard(st.get("leaderboard") or [], direction)] or None,
            "gpuHoursSaved": st.get("gpuHoursSaved"),
            "costSaved": st.get("costSaved"),
            "anomaliesDetected": st.get("anomaliesDetected"),
        }),
    }


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/experiments")
    @deps.limiter.limit("30/minute")
    async def list_experiments(request: Request, _=Depends(deps.verify_auth)):
        return {"items": [to_ui(o) for o in await list_all(request, deps, PLURAL)]}

    @router.get("/api/experiments/{name}")
    @deps.limiter.limit("30/minute")
    async def get_experiment(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, _ns = await find_one(request, deps, PLURAL, name)
        return to_ui(obj)

    return router
