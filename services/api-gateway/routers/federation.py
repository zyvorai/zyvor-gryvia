# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Federation routes (GryviaFederation, cluster-scoped): a read-only view for the provider administrator.

It shows what the ai-operator's opt-in federation controller (``--federation-servers``) records: per-cluster
health and utilisation and the aggregate GPU totals. It does not place jobs, fail over or move data, and it
never returns credentials: the cluster API server address and the credentials secret name are left out, only
``hasCredentials`` is reported.
"""
from typing import Any, Dict

from fastapi import APIRouter, Depends, HTTPException, Request

from .common import Deps, list_items, require_admin
from .uiutil import meta, prune

PLURAL = "gryviafederations"


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    health = {c.get("name"): c for c in st.get("clusterStatus") or []}
    clusters = []
    for c in spec.get("clusters") or []:
        h = health.get(c.get("name")) or {}
        util = h.get("utilization") or {}
        clusters.append(prune({
            "name": c.get("name"),
            "region": c.get("region"),
            "hasCredentials": bool((c.get("credentials") or {}).get("secretRef")),
            "state": h.get("state"),
            "lastHealthCheck": h.get("lastHealthCheck"),
            "gpus": util.get("gpus"),
            "utilizationPercent": util.get("percentage"),
        }))
    agg = st.get("aggregateStats") or {}
    return {
        "metadata": meta(obj),
        "spec": {"clusters": clusters},
        "status": prune({
            "state": st.get("state"),
            "totalGPUs": agg.get("totalGPUs"),
            "availableGPUs": agg.get("availableGPUs"),
            "jobsRunning": agg.get("jobsRunning"),
            "totalCostPerHour": agg.get("totalCostPerHour"),
        }),
    }


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/federations")
    @deps.limiter.limit("30/minute")
    async def list_federations(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        items = await list_items(deps, PLURAL)
        return {"items": [to_ui(o) for o in sorted(items, key=lambda o: (o.get("metadata") or {}).get("name", ""))]}

    @router.get("/api/federations/{name}")
    @deps.limiter.limit("30/minute")
    async def get_federation(request: Request, name: str, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        for o in await list_items(deps, PLURAL):
            if (o.get("metadata") or {}).get("name") == name:
                return to_ui(o)
        raise HTTPException(status_code=404, detail=f"{PLURAL}/{name}: Not Found")

    return router
