"""Lookback fairshare: GPU-hours vs tenant fairShareWeight over a window.

Recommend-only by default. When GRYVIA_FAIRSHARE_APPLY=1 (or true), POST /api/fairshare/apply
patches GryviaTenant.spec.quotas.fairShareWeight from the recommendation. Requires Kueue
fair-sharing (--kueue-fair-sharing) for the weight to take effect on ClusterQueues.
"""
from __future__ import annotations

import os
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Optional

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from pydantic import BaseModel, ConfigDict, Field

from . import tenancy
from .common import GROUP, VERSION, Deps, require_admin, run
from .tenancy import is_admin
from .usage import fetch_records, filter_records, _num, _tenant_of
from .uiutil import prune

TENANT_PLURAL = "gryviatenants"


def _env_apply() -> bool:
    return os.environ.get("GRYVIA_FAIRSHARE_APPLY", "").strip().lower() in ("1", "true", "yes")


def _weight_of(tenant: Dict[str, Any]) -> int:
    quotas = (tenant.get("spec") or {}).get("quotas") or {}
    w = quotas.get("fairShareWeight")
    try:
        n = int(w)
    except (TypeError, ValueError):
        n = 0
    return n if n > 0 else 1


def _gpu_hours(records: List[Dict[str, Any]]) -> Dict[str, float]:
    out: Dict[str, float] = {}
    for r in records:
        t = _tenant_of(r)
        out[t] = out.get(t, 0.0) + _num((r.get("spec") or {}).get("gpuHours"))
    return out


def compute_fairshare(
    tenants: List[Dict[str, Any]],
    records: List[Dict[str, Any]],
    *,
    lookback_days: int,
) -> Dict[str, Any]:
    hours_by = _gpu_hours(records)
    rows: List[Dict[str, Any]] = []
    total_w = 0.0
    total_h = 0.0
    for t in tenants:
        name = tenancy.tenant_name(t)
        w = float(_weight_of(t))
        h = hours_by.get(name, 0.0)
        total_w += w
        total_h += h
        rows.append({"tenant": name, "weight": int(w), "gpuHours": h})

    for row in rows:
        share_target = (row["weight"] / total_w) if total_w > 0 else 0.0
        share_actual = (row["gpuHours"] / total_h) if total_h > 0 else 0.0
        # Boost under-consumers, trim over-consumers (Run:ai-style correction signal).
        ratio = 1.0
        if share_actual > 0 and share_target > 0:
            ratio = share_target / share_actual
        elif share_actual == 0 and share_target > 0:
            ratio = 1.5
        recommended = max(1, int(round(row["weight"] * min(max(ratio, 0.5), 2.0))))
        row["shareTarget"] = round(share_target, 4)
        row["shareActual"] = round(share_actual, 4)
        row["recommendedWeight"] = recommended
        row["delta"] = recommended - row["weight"]

    rows.sort(key=lambda r: r["tenant"])
    return {
        "lookbackDays": lookback_days,
        "totalGpuHours": round(total_h, 4),
        "applyEnabled": _env_apply(),
        "items": rows,
    }


class ApplyFairshare(BaseModel):
    model_config = ConfigDict(extra="forbid")

    tenant: Optional[str] = Field(default=None, min_length=1, max_length=56)
    dryRun: bool = True


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/fairshare")
    @deps.limiter.limit("30/minute")
    async def get_fairshare(
        request: Request,
        days: int = Query(default=7, ge=1, le=90),
        _=Depends(deps.verify_auth),
    ):
        if not is_admin(request):
            raise HTTPException(status_code=403, detail="admin only")
        hi = datetime.now(timezone.utc)
        lo = hi - timedelta(days=days)
        tenants = await tenancy.list_tenants(deps.k8s_custom, force=True)
        records = filter_records(await fetch_records(deps.k8s_custom, None), None, lo, hi)
        return compute_fairshare(tenants, records, lookback_days=days)

    @router.post("/api/fairshare/apply")
    @deps.limiter.limit("10/minute")
    async def apply_fairshare(
        request: Request,
        body: ApplyFairshare,
        days: int = Query(default=7, ge=1, le=90),
        _=Depends(deps.verify_auth),
        __=Depends(require_admin),
    ):
        if not _env_apply() and not body.dryRun:
            raise HTTPException(
                status_code=403,
                detail="fairshare apply disabled; set GRYVIA_FAIRSHARE_APPLY=1 or use dryRun",
            )
        hi = datetime.now(timezone.utc)
        lo = hi - timedelta(days=days)
        tenants = await tenancy.list_tenants(deps.k8s_custom, force=True)
        records = filter_records(await fetch_records(deps.k8s_custom, None), None, lo, hi)
        report = compute_fairshare(tenants, records, lookback_days=days)

        applied: List[Dict[str, Any]] = []
        for row in report["items"]:
            if body.tenant and row["tenant"] != body.tenant:
                continue
            if row["delta"] == 0:
                continue
            patch = {
                "spec": {
                    "quotas": {
                        "fairShareWeight": row["recommendedWeight"],
                    }
                }
            }
            entry = {
                "tenant": row["tenant"],
                "from": row["weight"],
                "to": row["recommendedWeight"],
                "dryRun": body.dryRun,
            }
            if not body.dryRun:
                await run(
                    deps.k8s_custom.patch_cluster_custom_object,
                    group=GROUP,
                    version=VERSION,
                    plural=TENANT_PLURAL,
                    name=row["tenant"],
                    body=patch,
                )
                entry["status"] = "patched"
            else:
                entry["status"] = "dry-run"
            applied.append(entry)
        return {"applyEnabled": _env_apply(), "dryRun": body.dryRun, "applied": applied, "report": prune(report)}

    return router
