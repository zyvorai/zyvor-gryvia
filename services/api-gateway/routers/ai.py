# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""AI / GPU communication routes used by the web dashboard's GPU Communication page.

Sources:
  * /api/ai/training/insight -> status of the most recently analysed GryviaTrainingInsight
    (namespaced; see operators/ai-operator/api/v1).
  * /api/ai/training/nccl    -> per-operation NCCL stats from the eBPF collectors (routers/collector.py);
    empty when no collector is reachable.
  * /api/gpu/memory          -> host/device transfer counters from the same collectors; zeros when none
    is reachable.
Nothing is gryviaated: no source object -> zeros / empty lists in the shape the UI expects.
"""
from typing import Any, Dict

from fastapi import APIRouter, Depends, Request

from .collector import fetch_all_with_stats, merge_gpu_memory, merge_nccl
from .common import Deps, list_items, require_admin


def _num(value: Any, default: float = 0) -> Any:
    return value if isinstance(value, (int, float)) and not isinstance(value, bool) else default


def _empty_insight() -> Dict[str, Any]:
    return {
        "rankStats": [],
        "commPattern": "",
        "commComputeRatio": 0,
        "stragglers": [],
        "bottleneck": "",
    }


def _analysis_key(obj: Dict[str, Any]) -> str:
    status = obj.get("status") or {}
    return str(status.get("lastAnalysis") or "")


def _insight_from(obj: Dict[str, Any]) -> Dict[str, Any]:
    status = obj.get("status") or {}
    out = _empty_insight()
    out["rankStats"] = [
        {
            "rank": int(_num(r.get("rank"))),
            "avgLatencyNs": _num(r.get("avgLatencyNs")),
            "totalBytes": _num(r.get("totalBytes")),
            "isStraggler": bool(r.get("isStraggler", False)),
        }
        for r in (status.get("rankStats") or []) if isinstance(r, dict)
    ]
    out["stragglers"] = [
        {
            "rank": int(_num(s.get("rank"))),
            "slowdownFactor": _num(s.get("slowdownFactor")),
            "reason": s.get("reason") or "",
        }
        for s in (status.get("stragglers") or []) if isinstance(s, dict)
    ]
    out["commPattern"] = status.get("commPattern") or ""
    out["commComputeRatio"] = _num(status.get("commComputeRatio"))
    out["bottleneck"] = status.get("bottleneck") or ""
    if status.get("lastAnalysis"):
        out["lastAnalysis"] = status["lastAnalysis"]
    return out


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/ai/training/insight")
    @deps.limiter.limit("30/minute")
    async def training_insight(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        """Latest training communication analysis (empty shape when none exists)."""
        items = await list_items(deps, "gryviatraininginsights")
        analysed = [o for o in items if o.get("status")]
        if not analysed:
            return _empty_insight()
        latest = max(analysed, key=lambda o: (_analysis_key(o), o.get("metadata", {}).get("creationTimestamp", "")))
        out = _insight_from(latest)
        m = latest.get("metadata") or {}
        out["source"] = {"name": m.get("name", ""), "namespace": m.get("namespace", "")}
        return out

    @router.get("/api/ai/training/nccl")
    @deps.limiter.limit("30/minute")
    async def training_nccl(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        """NCCL per-operation stats merged from every collector; empty when none is reachable."""
        bodies, stats = await fetch_all_with_stats(deps, "/api/v1/gpu/nccl")
        return {**merge_nccl(bodies), "collectors": stats}

    @router.get("/api/gpu/memory")
    @deps.limiter.limit("30/minute")
    async def gpu_memory(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        """Host/device transfer counters summed across collectors; zeros when none is reachable."""
        bodies, stats = await fetch_all_with_stats(deps, "/api/v1/gpu/memory")
        return {**merge_gpu_memory(bodies), "collectors": stats}

    return router
