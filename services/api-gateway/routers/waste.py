# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""GPU waste report: metered (allocated) GPU-hours against measured utilization.

Allocated cost is what the metering bills (reserved GPU time). Effective cost scales it by the average DCGM
GPU utilization of each namespace over the window; the difference is waste. Utilization comes from
Prometheus through ``Deps.gpu_util_fetch`` (days -> {namespace: percent}); without it the report still lists
allocated hours and cost and marks every tenant ``noTelemetry``. Tenant users see only their own tenant.
"""
import csv
import io
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Literal, Optional

from fastapi import APIRouter, Depends, Query, Request
from fastapi.responses import Response

from .common import Deps
from .tenancy import is_admin
from .uiutil import namespaces
from .usage import _csv_cell, _currency, _num, _tenant_of, fetch_records, filter_records

CSV_COLUMNS = ["tenant", "gpuHours", "cost", "currency", "utilizationPct", "effectiveGpuHours",
               "wastedGpuHours", "wastedCost", "verdict"]


def _clamp_pct(v: Any) -> Optional[float]:
    if isinstance(v, bool) or not isinstance(v, (int, float)) or v != v:  # NaN
        return None
    return min(100.0, max(0.0, float(v)))


def compute_waste(records: List[Dict[str, Any]], util_by_ns: Optional[Dict[str, float]], *,
                  low_util_pct: float = 30.0) -> Dict[str, Any]:
    """Per-tenant waste. A record's utilization is its namespace's; a tenant's is the GPU-hour weighted mean over
    the records that have telemetry. GPU-hours without telemetry stay allocated but are not counted as waste."""
    util_by_ns = util_by_ns or {}
    acc: Dict[str, Dict[str, Any]] = {}
    for r in records:
        spec, ns = r.get("spec") or {}, (r.get("metadata") or {}).get("namespace", "")
        t = acc.setdefault(_tenant_of(r), {"h": 0.0, "c": 0.0, "mh": 0.0, "eff": 0.0, "wc": 0.0, "cur": set()})
        h, c = _num(spec.get("gpuHours")), _num(spec.get("cost"))
        t["h"] += h
        t["c"] += c
        t["cur"].add(spec.get("currency") or "USD")
        u = _clamp_pct(util_by_ns.get(ns))
        if u is not None:
            t["mh"] += h
            t["eff"] += h * u / 100.0
            t["wc"] += c * (1 - u / 100.0)
    items: List[Dict[str, Any]] = []
    for name, t in acc.items():
        if t["mh"] > 0:
            util = 100.0 * t["eff"] / t["mh"]
            row = {"utilizationPct": round(util, 2), "effectiveGpuHours": round(t["eff"], 4),
                   "wastedGpuHours": round(t["mh"] - t["eff"], 4), "wastedCost": round(t["wc"], 4),
                   "verdict": "underused" if util < low_util_pct else "ok"}
        else:
            row = {"utilizationPct": None, "effectiveGpuHours": None, "wastedGpuHours": None,
                   "wastedCost": None, "verdict": "noTelemetry"}
        items.append({"tenant": name, "gpuHours": round(t["h"], 4), "cost": round(t["c"], 4),
                      "currency": _currency(t["cur"]), **row})
    items.sort(key=lambda i: (-(i["wastedCost"] or 0.0), i["tenant"]))
    mh = sum(t["mh"] for t in acc.values())
    eff = sum(t["eff"] for t in acc.values())
    totals = {"gpuHours": round(sum(t["h"] for t in acc.values()), 4),
              "cost": round(sum(t["c"] for t in acc.values()), 4),
              "currency": _currency({c for t in acc.values() for c in t["cur"]}),
              "measuredGpuHours": round(mh, 4),
              "utilizationPct": round(100.0 * eff / mh, 2) if mh > 0 else None,
              "wastedGpuHours": round(mh - eff, 4) if mh > 0 else None,
              "wastedCost": round(sum(t["wc"] for t in acc.values()), 4) if mh > 0 else None}
    return {"lowUtilizationPct": low_util_pct, "telemetry": mh > 0, "items": items, "totals": totals}


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/waste")
    @deps.limiter.limit("20/minute")
    async def get_waste(request: Request, days: int = Query(default=7, ge=1, le=90),
                        lowUtilPct: float = Query(default=30.0, ge=0, le=100),
                        tenant: Optional[str] = Query(default=None, max_length=63),
                        format: Literal["json", "csv"] = "json",
                        _=Depends(deps.verify_auth)):
        hi = datetime.now(timezone.utc)
        lo = hi - timedelta(days=days)
        if is_admin(request):
            records = await fetch_records(deps.k8s_custom, None)
        else:
            tenant = None  # forced: a tenant user only ever sees its own namespaces
            records = await fetch_records(deps.k8s_custom, namespaces(request, deps))
        records = filter_records(records, tenant, lo, hi)
        util: Dict[str, float] = {}
        if deps.gpu_util_fetch is not None:
            try:
                util = await deps.gpu_util_fetch(days) or {}
            except Exception:  # telemetry is optional; a Prometheus outage must not break the report
                util = {}
        body = compute_waste(records, util, low_util_pct=lowUtilPct)
        body["days"] = days
        if format == "csv":
            buf = io.StringIO()
            w = csv.DictWriter(buf, fieldnames=CSV_COLUMNS, lineterminator="\n")
            w.writeheader()
            for row in body["items"]:
                w.writerow({k: _csv_cell("" if row[k] is None else row[k]) for k in CSV_COLUMNS})
            return Response(buf.getvalue(), media_type="text/csv", headers={
                "Content-Disposition": f'attachment; filename="gryvia-waste-{hi.strftime("%Y%m%d")}.csv"'})
        return body

    return router
