# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Budgets (GryviaBudget, cluster-scoped), read-only.

Admins see every budget; a tenant user sees only budgets whose scope name is one of its tenants or its
``tenant-<name>`` namespace. Status values are written by the quota operator.
"""
from typing import Any, Dict, List

from fastapi import APIRouter, Depends, Request

from .common import Deps, list_items
from .tenancy import caller_tenant_names, is_admin, namespace_of

PLURAL = "gryviabudgets"


def visible_to(obj: Dict[str, Any], tenants: List[str]) -> bool:
    name = ((obj.get("spec") or {}).get("scope") or {}).get("name", "")
    return bool(name) and any(name in (t, namespace_of(t)) for t in tenants)


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    s, st = obj.get("spec") or {}, obj.get("status") or {}
    scope, per, lim = s.get("scope") or {}, s.get("period") or {}, s.get("limits") or {}
    enf, use = s.get("enforcement"), st.get("usage") or {}
    cur, fc = st.get("currentPeriod") or {}, st.get("forecast") or {}
    return {
        "name": (obj.get("metadata") or {}).get("name", ""),
        "scope": {"type": scope.get("type", ""), "name": scope.get("name", "")},
        "period": {"type": per.get("type", ""), "startDate": per.get("startDate"), "endDate": per.get("endDate")},
        "limits": {"costUSD": lim.get("costUSD", 0), "gpuHours": lim.get("gpuHours", 0)},
        "enforcement": ({"enabled": bool(enf.get("enabled", False)), "action": enf.get("action", "")}
                        if enf else None),
        "state": st.get("state") or "active",
        "usage": {"costUSD": use.get("costUSD", 0), "gpuHours": use.get("gpuHours", 0)},
        "utilization": {"costPercent": (st.get("utilization") or {}).get("costPercent", 0)},
        "forecast": {"projectedCostUSD": fc.get("projectedCostUSD", 0),
                     "budgetSufficient": bool(fc.get("budgetSufficient", False))},
        "currentPeriod": {"startDate": cur.get("startDate"), "endDate": cur.get("endDate"),
                          "daysRemaining": cur.get("daysRemaining", 0)},
    }


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/budgets")
    @deps.limiter.limit("30/minute")
    async def list_budgets(request: Request, _=Depends(deps.verify_auth)):
        items = await list_items(deps, PLURAL)
        if not is_admin(request):
            mine = caller_tenant_names(request)
            items = [o for o in items if visible_to(o, mine)]
        items.sort(key=lambda o: (o.get("metadata") or {}).get("name", ""))
        return {"items": [to_ui(o) for o in items]}

    return router
