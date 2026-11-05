"""Metered usage (GryviaUsageRecord, namespaced in the job's namespace) with grouping and export.

Admins read every namespace; a tenant user is forced to its own tenant's namespaces regardless of the
``tenant`` parameter. Costs are metered estimates from job wall-clock time.
"""
import csv
import io
from collections import OrderedDict
from datetime import date, datetime, timedelta, timezone
from typing import Any, Dict, Iterable, List, Literal, Optional

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from fastapi.responses import JSONResponse, Response

from .common import GROUP, VERSION, Deps, run
from .tenancy import is_admin, namespace_of
from .uiutil import namespaces, parse_ts

PLURAL = "gryviausagerecords"
CSV_COLUMNS = ["tenant", "namespace", "job", "sku", "gpuType", "gpus", "kind", "start", "end", "gpuHours", "rate",
               "cost", "currency", "final"]


async def fetch_records(custom: Any, nss: Optional[List[str]]) -> List[Dict[str, Any]]:
    """Usage records in the given namespaces, or in every namespace when ``nss`` is None."""
    if nss is None:
        res = await run(custom.list_cluster_custom_object, group=GROUP, version=VERSION, plural=PLURAL)
        return list(res.get("items", []))
    out: List[Dict[str, Any]] = []
    for ns in nss:
        res = await run(custom.list_namespaced_custom_object, group=GROUP, version=VERSION,
                        namespace=ns, plural=PLURAL)
        out.extend(res.get("items", []))
    return out


def _num(v: Any) -> float:
    return float(v) if isinstance(v, (int, float)) and not isinstance(v, bool) else 0.0


def _tenant_of(rec: Dict[str, Any]) -> str:
    spec, ns = rec.get("spec") or {}, (rec.get("metadata") or {}).get("namespace", "")
    if spec.get("tenant"):
        return spec["tenant"]
    return ns[len("tenant-"):] if ns.startswith("tenant-") else ns


def _start_of(rec: Dict[str, Any]) -> Optional[datetime]:
    spec = rec.get("spec") or {}
    return (parse_ts(spec.get("start")) or parse_ts(spec.get("end"))
            or parse_ts((rec.get("metadata") or {}).get("creationTimestamp")))


def parse_bound(value: Optional[str], name: str, end: bool) -> Optional[datetime]:
    """ISO date or datetime. A date-only ``to`` covers that whole day."""
    if not value:
        return None
    try:
        if len(value) == 10:
            d = date.fromisoformat(value)
            dt = datetime(d.year, d.month, d.day, tzinfo=timezone.utc)
            return dt + timedelta(days=1) if end else dt
        dt = parse_ts(value)
    except ValueError:
        dt = None
    if dt is None:
        raise HTTPException(status_code=400, detail=f"{name} must be an ISO date (YYYY-MM-DD) or timestamp")
    return dt


def _kind_of(r: Dict[str, Any]) -> str:
    k = ((r.get("spec") or {}).get("kind") or "gpu").strip().lower()
    return k if k else "gpu"


def filter_records(records: Iterable[Dict[str, Any]], tenant: Optional[str], lo: Optional[datetime],
                   hi: Optional[datetime], kind: Optional[str] = None) -> List[Dict[str, Any]]:
    out = []
    for r in records:
        if tenant and _tenant_of(r) != tenant and (r.get("metadata") or {}).get("namespace") != namespace_of(tenant):
            continue
        if kind and kind != "all" and _kind_of(r) != kind:
            continue
        if lo or hi:
            s = _start_of(r)
            if s is None or (lo and s < lo) or (hi and s >= hi):
                continue
        out.append(r)
    return out


def _key(rec: Dict[str, Any], group_by: str) -> str:
    spec = rec.get("spec") or {}
    if group_by == "tenant":
        return _tenant_of(rec)
    if group_by == "sku":
        return spec.get("sku") or spec.get("gpuType") or "unknown"
    s = _start_of(rec)
    return s.astimezone(timezone.utc).date().isoformat() if s else "unknown"


def _currency(currencies: set) -> str:
    return next(iter(currencies)) if len(currencies) == 1 else ("MIXED" if currencies else "USD")


def aggregate(records: List[Dict[str, Any]], group_by: str) -> Dict[str, Any]:
    groups: "OrderedDict[str, Dict[str, Any]]" = OrderedDict()
    all_jobs: set = set()
    all_cur: set = set()
    hours = cost = 0.0
    for r in records:
        spec = r.get("spec") or {}
        g = groups.setdefault(_key(r, group_by), {"gpuHours": 0.0, "cost": 0.0, "cur": set(), "jobs": set()})
        job_id = ((r.get("metadata") or {}).get("namespace"), spec.get("job") or (r.get("metadata") or {}).get("name"))
        cur = spec.get("currency") or "USD"
        h, c = _num(spec.get("gpuHours")), _num(spec.get("cost"))
        g["gpuHours"] += h
        g["cost"] += c
        g["cur"].add(cur)
        g["jobs"].add(job_id)
        hours += h
        cost += c
        all_cur.add(cur)
        all_jobs.add(job_id)
    keys = sorted(groups) if group_by == "day" else sorted(groups, key=lambda k: (-groups[k]["cost"], k))
    items = [{"key": k, "gpuHours": round(groups[k]["gpuHours"], 4), "cost": round(groups[k]["cost"], 4),
              "currency": _currency(groups[k]["cur"]), "jobs": len(groups[k]["jobs"])} for k in keys]
    return {"items": items, "totals": {"gpuHours": round(hours, 4), "cost": round(cost, 4),
                                       "currency": _currency(all_cur), "jobs": len(all_jobs)}}


def _csv_cell(v: Any) -> Any:
    if isinstance(v, str) and v[:1] in ("=", "+", "-", "@", "\t", "\r"):
        return "'" + v  # spreadsheet formula injection guard
    return v


def record_row(r: Dict[str, Any]) -> Dict[str, Any]:
    spec, m = r.get("spec") or {}, r.get("metadata") or {}
    return {"tenant": _tenant_of(r), "namespace": m.get("namespace", ""), "job": spec.get("job", ""),
            "sku": spec.get("sku", ""), "gpuType": spec.get("gpuType", ""), "gpus": spec.get("gpus", 0),
            "kind": _kind_of(r),
            "start": spec.get("start", ""), "end": spec.get("end", ""), "gpuHours": _num(spec.get("gpuHours")),
            "rate": _num(spec.get("rate")), "cost": _num(spec.get("cost")),
            "currency": spec.get("currency") or "USD", "final": bool(spec.get("final", False))}


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    async def _load(request: Request, tenant: Optional[str], frm: Optional[str], to: Optional[str],
                    kind: Optional[str] = None):
        lo, hi = parse_bound(frm, "from", False), parse_bound(to, "to", True)
        if is_admin(request):
            records = await fetch_records(deps.k8s_custom, None)
        else:
            tenant = None  # forced: a tenant user only ever sees its own namespaces
            records = await fetch_records(deps.k8s_custom, namespaces(request, deps))
        return filter_records(records, tenant, lo, hi, kind)

    @router.get("/api/usage")
    @deps.limiter.limit("30/minute")
    async def get_usage(request: Request, tenant: Optional[str] = Query(default=None, max_length=63),
                        frm: Optional[str] = Query(default=None, alias="from", max_length=40),
                        to: Optional[str] = Query(default=None, max_length=40),
                        groupBy: Literal["tenant", "sku", "day"] = "tenant",
                        kind: Literal["all", "gpu", "tokens"] = "all",
                        _=Depends(deps.verify_auth)):
        records = await _load(request, tenant, frm, to, kind)
        return {"groupBy": groupBy, "kind": kind, **aggregate(records, groupBy)}

    @router.get("/api/usage/export")
    @deps.limiter.limit("10/minute")
    async def export_usage(request: Request, format: Literal["csv", "json"] = "csv",
                           tenant: Optional[str] = Query(default=None, max_length=63),
                           frm: Optional[str] = Query(default=None, alias="from", max_length=40),
                           to: Optional[str] = Query(default=None, max_length=40),
                           kind: Literal["all", "gpu", "tokens"] = "all",
                           _=Depends(deps.verify_auth)):
        records = await _load(request, tenant, frm, to, kind)
        rows = [record_row(r) for r in records]
        rows.sort(key=lambda r: (r["start"], r["tenant"], r["job"]))
        stamp = datetime.now(timezone.utc).strftime("%Y%m%d")
        if format == "json":
            body = {"items": rows, "totals": aggregate(records, "tenant")["totals"]}
            return JSONResponse(body, headers={
                "Content-Disposition": f'attachment; filename="gryvia-usage-{stamp}.json"'})
        buf = io.StringIO()
        w = csv.DictWriter(buf, fieldnames=CSV_COLUMNS, lineterminator="\n")
        w.writeheader()
        for row in rows:
            w.writerow({k: _csv_cell(v) for k, v in row.items()})
        return Response(buf.getvalue(), media_type="text/csv", headers={
            "Content-Disposition": f'attachment; filename="gryvia-usage-{stamp}.csv"'})

    return router
