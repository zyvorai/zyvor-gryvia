# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Measured network usage per tenant (GryviaNetworkUsageRecord), its price estimate and a reconciliation.

Records are fragments written by the collectors (one per node, tenant, UTC hour, peerClass, zoneClass) and carry
a ``source``. Exactly ONE source is used per request (``source=collector`` by default): fragments of different
sources are never summed, so the same traffic measured by the collector and by Netra cannot be counted twice.
Only egress is priced. Admins read every namespace; a tenant user is forced to its own tenant's namespaces.
Everything is an estimate from byte counters; nothing is invoiced or paid.
"""
import os
from collections import OrderedDict
from datetime import datetime, timezone
from typing import Any, Dict, Iterable, List, Literal, Optional

from fastapi import APIRouter, Depends, HTTPException, Query, Request

from .common import GROUP, VERSION, Deps, list_items, run
from .netra import HISTORY_MAX, fetch_history
from .tenancy import caller_tenant_names, is_admin, namespace_of
from .uiutil import namespaces, parse_ts
from .usage import _num, parse_bound

PLURAL = "gryvianetworkusagerecords"
RATES = "gryvianetworkrates"
GB = 1_000_000_000  # price unit: 1 GB = 10^9 bytes
DEFAULT_TOLERANCE_PCT = 10.0
RATE_OF_ZONE = {"same-zone": "sameZone", "cross-zone": "crossZone", "internet": "internetEgress"}
NOTE = "Estimate from measured byte counters; not a tax invoice. Unpriced zone classes are listed but not charged."


def _tolerance_default() -> float:
    try:
        v = float(os.environ.get("GRYVIA_NETWORK_RECONCILE_TOLERANCE_PCT", DEFAULT_TOLERANCE_PCT))
    except ValueError:
        return DEFAULT_TOLERANCE_PCT
    return v if 0 <= v <= 100 else DEFAULT_TOLERANCE_PCT


async def fetch_network_records(custom: Any, nss: Optional[List[str]]) -> List[Dict[str, Any]]:
    """Network usage records in the given namespaces, or in every namespace when ``nss`` is None."""
    out: List[Dict[str, Any]] = []
    if nss is None:
        res = await run(custom.list_cluster_custom_object, group=GROUP, version=VERSION, plural=PLURAL)
        return list(res.get("items", []))
    for ns in nss:
        res = await run(custom.list_namespaced_custom_object, group=GROUP, version=VERSION, namespace=ns,
                        plural=PLURAL)
        out.extend(res.get("items", []))
    return out


async def load_rates(deps: Deps) -> Optional[Dict[str, Any]]:
    """The provider's GryviaNetworkRate (first by name), or None when none is defined."""
    try:
        items = await list_items(deps, RATES)
    except HTTPException:
        return None
    items = sorted(items, key=lambda o: (o.get("metadata") or {}).get("name", ""))
    if not items:
        return None
    spec = items[0].get("spec") or {}
    return {"sameZone": _num(spec.get("sameZone")), "crossZone": _num(spec.get("crossZone")),
            "internetEgress": _num(spec.get("internetEgress")), "currency": spec.get("currency") or "USD"}


def tenant_of(rec: Dict[str, Any]) -> str:
    spec, ns = rec.get("spec") or {}, (rec.get("metadata") or {}).get("namespace", "")
    if spec.get("tenant"):
        return spec["tenant"]
    return ns[len("tenant-"):] if ns.startswith("tenant-") else ns


def hour_of(rec: Dict[str, Any]) -> Optional[datetime]:
    return parse_ts((rec.get("spec") or {}).get("hour"))


def clean(records: Iterable[Dict[str, Any]], source: str = "collector", tenant: Optional[str] = None,
          lo: Optional[datetime] = None, hi: Optional[datetime] = None) -> List[Dict[str, Any]]:
    """Records of ONE source in [lo, hi), de-duplicated on (node, tenant, hour, peerClass, zoneClass).

    A duplicate fragment (same key written twice, e.g. a re-created object) keeps the larger egress instead of
    being added: bytes are counted once."""
    best: "OrderedDict[tuple, Dict[str, Any]]" = OrderedDict()
    for r in records:
        spec = r.get("spec") or {}
        if (spec.get("source") or "collector") != source:
            continue
        t = tenant_of(r)
        if tenant and t != tenant:
            continue
        h = hour_of(r)
        if h is None or (lo and h < lo) or (hi and h >= hi):
            continue
        key = (spec.get("node", ""), t, h, spec.get("peerClass", ""), spec.get("zoneClass", ""))
        if key not in best or _num(spec.get("egressBytes")) > _num(best[key]["spec"].get("egressBytes")):
            best[key] = r
    return list(best.values())


def price(zone: str, egress_bytes: float, rates: Optional[Dict[str, Any]]) -> Optional[float]:
    """Amount for egress bytes of a zone class, None when the class is not priced."""
    field = RATE_OF_ZONE.get(zone)
    if rates is None or field is None:
        return None
    return egress_bytes / GB * rates[field]


def _key(rec: Dict[str, Any], group_by: str) -> str:
    spec = rec.get("spec") or {}
    if group_by == "tenant":
        return tenant_of(rec)
    if group_by == "peerClass":
        return spec.get("peerClass") or "unknown"
    if group_by == "zoneClass":
        return spec.get("zoneClass") or "unknown-zone"
    h = hour_of(rec)
    return h.astimezone(timezone.utc).date().isoformat() if h else "unknown"


def aggregate(records: List[Dict[str, Any]], group_by: str, rates: Optional[Dict[str, Any]]) -> Dict[str, Any]:
    groups: "OrderedDict[str, Dict[str, float]]" = OrderedDict()
    tot = {"egressBytes": 0.0, "ingressBytes": 0.0, "cost": 0.0}
    open_ = False
    for r in records:
        spec = r.get("spec") or {}
        e, i = _num(spec.get("egressBytes")), _num(spec.get("ingressBytes"))
        c = price(spec.get("zoneClass", ""), e, rates) or 0.0
        g = groups.setdefault(_key(r, group_by), {"egressBytes": 0.0, "ingressBytes": 0.0, "cost": 0.0, "records": 0})
        g["egressBytes"] += e
        g["ingressBytes"] += i
        g["cost"] += c
        g["records"] += 1
        tot["egressBytes"] += e
        tot["ingressBytes"] += i
        tot["cost"] += c
        open_ = open_ or not spec.get("final", False)
    keys = sorted(groups) if group_by == "day" else sorted(groups, key=lambda k: (-groups[k]["egressBytes"], k))
    items = [{"key": k, "egressBytes": int(groups[k]["egressBytes"]), "ingressBytes": int(groups[k]["ingressBytes"]),
              "records": int(groups[k]["records"]),
              **({"cost": round(groups[k]["cost"], 4)} if rates else {})} for k in keys]
    totals: Dict[str, Any] = {"egressBytes": int(tot["egressBytes"]), "ingressBytes": int(tot["ingressBytes"]),
                              "records": len(records)}
    if rates:
        totals["cost"] = round(tot["cost"], 4)
        totals["currency"] = rates["currency"]
    if open_:
        totals["open"] = True
    return {"items": items, "totals": totals}


def network_lines(records: List[Dict[str, Any]], rates: Optional[Dict[str, Any]]) -> Optional[Dict[str, Any]]:
    """Invoice section: egress GB x rate per (peerClass, zoneClass). None unless records AND rates exist."""
    if not records or rates is None:
        return None
    groups: "OrderedDict[tuple, float]" = OrderedDict()
    open_ = False
    for r in records:
        spec = r.get("spec") or {}
        k = (spec.get("peerClass") or "unknown", spec.get("zoneClass") or "unknown-zone")
        groups[k] = groups.get(k, 0.0) + _num(spec.get("egressBytes"))
        open_ = open_ or not spec.get("final", False)
    lines, total = [], 0.0
    for (peer, zone) in sorted(groups, key=lambda k: (-groups[k], k)):
        b = groups[(peer, zone)]
        amount = price(zone, b, rates)
        rate = rates[RATE_OF_ZONE[zone]] if zone in RATE_OF_ZONE else None
        total += amount or 0.0
        lines.append({"peerClass": peer, "zoneClass": zone, "egressBytes": int(b), "egressGB": round(b / GB, 6),
                      "rate": rate, "amount": round(amount, 2) if amount is not None else 0.0,
                      "priced": amount is not None, "currency": rates["currency"]})
    out: Dict[str, Any] = {"lines": lines, "subtotal": round(total, 2), "currency": rates["currency"],
                           "note": NOTE}
    if open_:
        out["open"] = True
    return out


def in_month(dt: datetime, year: int, month: int) -> bool:
    u = dt.astimezone(timezone.utc)
    return u.year == year and u.month == month


def reconcile(collector_egress: float, netra_egress: Optional[float], tolerance_pct: float,
              n_collector: int, n_netra: int, netra_truncated: bool = False) -> Dict[str, Any]:
    """Verdict comparing the collector's egress with Netra's for the same tenant and period.

    The two are measured independently and are never added together. ``percent`` is relative to the larger of
    the two so it stays within 0..100."""
    out: Dict[str, Any] = {"collectorEgressBytes": int(collector_egress), "netraEgressBytes": None,
                           "deltaBytes": None, "percent": None, "tolerancePercent": tolerance_pct,
                           "collectorRecords": n_collector, "netraRecords": n_netra}
    if netra_egress is None:
        return {**out, "verdict": "insufficient-data", "reason": "Netra is not configured or not reachable"}
    out["netraEgressBytes"] = int(netra_egress)
    if n_collector == 0:
        return {**out, "verdict": "insufficient-data", "reason": "no collector records for the period"}
    if n_netra == 0:
        return {**out, "verdict": "insufficient-data", "reason": "no Netra egress records for the period"}
    if netra_truncated:
        return {**out, "verdict": "insufficient-data",
                "reason": "Netra history was truncated at its limit; the period is not fully covered"}
    delta = collector_egress - netra_egress
    denom = max(collector_egress, netra_egress)
    pct = abs(delta) / denom * 100 if denom else 0.0
    out.update({"deltaBytes": int(delta), "percent": round(pct, 2)})
    out["verdict"] = "within-tolerance" if pct <= tolerance_pct else "investigate"
    return out


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    async def _records(request: Request, tenant: Optional[str]):
        if is_admin(request):
            return await fetch_network_records(deps.k8s_custom, None), tenant
        return await fetch_network_records(deps.k8s_custom, namespaces(request, deps)), None

    @router.get("/api/network/usage")
    @deps.limiter.limit("30/minute")
    async def get_network_usage(request: Request, tenant: Optional[str] = Query(default=None, max_length=63),
                                frm: Optional[str] = Query(default=None, alias="from", max_length=40),
                                to: Optional[str] = Query(default=None, max_length=40),
                                groupBy: Literal["tenant", "peerClass", "zoneClass", "day"] = "tenant",
                                source: Literal["collector", "netra"] = "collector",
                                _=Depends(deps.verify_auth)):
        lo, hi = parse_bound(frm, "from", False), parse_bound(to, "to", True)
        records, tenant = await _records(request, tenant)
        rates = await load_rates(deps)
        used = clean(records, source, tenant, lo, hi)
        return {"groupBy": groupBy, "source": source, "billing": "egress-only", "rates": rates,
                **aggregate(used, groupBy, rates)}

    @router.get("/api/network/reconcile")
    @deps.limiter.limit("10/minute")
    async def get_reconcile(request: Request, month: Optional[str] = Query(default=None, max_length=10),
                            tenant: Optional[str] = Query(default=None, max_length=63),
                            tolerancePct: Optional[float] = Query(default=None, ge=0, le=100),
                            _=Depends(deps.verify_auth)):
        from .invoices import parse_month  # local: invoices imports this module

        y, m = parse_month(month)
        if is_admin(request):
            if not tenant:
                raise HTTPException(status_code=400, detail="tenant is required")
        else:
            mine = caller_tenant_names(request)
            if tenant and tenant not in mine:
                raise HTTPException(status_code=404, detail="no usage for that tenant")
            tenant = tenant or (mine[0] if len(mine) == 1 else None)
            if not tenant:
                raise HTTPException(status_code=400, detail="tenant is required")
        records, _t = await _records(request, tenant)
        mine_records = [r for r in clean(records, "collector", tenant) if (h := hour_of(r)) and in_month(h, y, m)]
        collector = sum(_num((r.get("spec") or {}).get("egressBytes")) for r in mine_records)

        start = datetime(y, m, 1, tzinfo=timezone.utc)
        end = datetime(y + (m == 12), m % 12 + 1, 1, tzinfo=timezone.utc)
        hours = int((datetime.now(timezone.utc) - start).total_seconds() // 3600) + 1
        history = await fetch_history(deps, hours)
        netra: Optional[float] = None
        n_netra = 0
        truncated = False
        if history is not None:
            ns = namespace_of(tenant)
            netra = 0.0
            for rec in history:
                ts = parse_ts(rec.get("observedAt"))
                if (rec.get("namespace") != ns or str(rec.get("direction", "")).lower() != "egress"
                        or ts is None or not (start <= ts < end)):
                    continue
                netra += _num(rec.get("bytes"))
                n_netra += 1
            truncated = len(history) >= HISTORY_MAX
        tol = tolerancePct if tolerancePct is not None else _tolerance_default()
        res = reconcile(collector, netra, tol, len(mine_records), n_netra, truncated)
        return {"tenant": tenant, "month": f"{y:04d}-{m:02d}", "source": "collector", "compared": "netra",
                "billing": "egress-only",
                "note": "Collector and Netra are independent measurements of the same traffic and are compared, "
                        "never summed. Same-node and unattributed traffic is not in the collector figure.",
                **res}

    return router
