"""Monthly invoices: on-demand estimates from GryviaUsageRecord objects, or a persisted GryviaInvoice.

Records are bucketed by the UTC month of ``spec.start``. Admins see every tenant; a tenant user is forced to its own
tenant (asking for another tenant is a 404). Without the billing ledger nothing is persisted. With it
(GRYVIA_BILLING_LEDGER=1) an admin can finalize a closed month: the lines are built from the tenant's hash-chained
GryviaLedgerEntry objects and frozen in a GryviaInvoice with a per-tenant sequential number; a Finalized or Paid
invoice then replaces the estimate. Voiding keeps the invoice (the webhook forbids deleting it) and lets the month be
finalized again under a new number. No payment is processed here (see billing_stripe.py for test mode).
"""
import calendar
import csv
import hashlib
import io
import re
from collections import OrderedDict
from datetime import datetime, timezone
from decimal import ROUND_HALF_UP, Decimal
from typing import Any, Dict, List, Literal, Optional

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from fastapi.responses import Response
from pydantic import BaseModel, Field

from . import ledger as lg
from .common import create_item, delete_item, list_items, patch_item, require_admin
from .invoice_webhook import deliver_invoice
from .tenancy import caller_tenant_names, is_admin
from .uiutil import namespaces, parse_ts
from .netusage import (clean, fetch_network_records, hour_of, in_month as hour_in_month, load_rates,
                       network_lines, tenant_of as net_tenant_of)
from .usage import _csv_cell, _currency, _num, _start_of, _tenant_of, fetch_records
from .common import Deps

CSV_COLUMNS = ["invoice", "tenant", "period_from", "period_to", "sku", "gpuType", "jobs", "gpuHours", "rate",
               "amount", "currency"]
NOTE = "Estimate from job run time; not a tax invoice."
FINAL_NOTE = "Finalized from the billing ledger; lines are frozen. Not a tax invoice."
CENT = Decimal("0.01")
MICRO = Decimal("0.000001")
_DNS_LABEL = re.compile(r"^[a-z0-9]([-a-z0-9]{0,40}[a-z0-9])?$")
_MONTH = re.compile(r"^(\d{4})-(0[1-9]|1[0-2])$")


def parse_month(value: Optional[str]) -> "tuple[int, int]":
    if not value:
        now = datetime.now(timezone.utc)
        return now.year, now.month
    m = _MONTH.match(value)
    if not m or int(m.group(1)) < 1970:
        raise HTTPException(status_code=400, detail="month must be YYYY-MM")
    return int(m.group(1)), int(m.group(2))


def in_month(rec: Dict[str, Any], year: int, month: int) -> bool:
    s = (rec.get("spec") or {}).get("start")
    start = parse_ts(s) if s else _start_of(rec)
    if start is None:
        return False
    u = start.astimezone(timezone.utc)
    return u.year == year and u.month == month


def build_invoice(tenant: str, year: int, month: int, records: List[Dict[str, Any]],
                  network: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
    groups: "OrderedDict[str, Dict[str, Any]]" = OrderedDict()
    all_cur: set = set()
    jobs_all: set = set()
    open_ = False
    for r in records:
        spec, meta = r.get("spec") or {}, r.get("metadata") or {}
        key = spec.get("sku") or spec.get("gpuType") or "unknown"
        g = groups.setdefault(key, {"gpuType": spec.get("gpuType") or "", "hours": 0.0, "amount": 0.0,
                                    "rates": set(), "cur": set(), "jobs": set()})
        job = (meta.get("namespace"), spec.get("job") or meta.get("name"))
        cur = spec.get("currency") or "USD"
        g["hours"] += _num(spec.get("gpuHours"))
        g["amount"] += _num(spec.get("cost"))
        g["rates"].add(_num(spec.get("rate")))
        g["cur"].add(cur)
        g["jobs"].add(job)
        if not g["gpuType"]:
            g["gpuType"] = spec.get("gpuType") or ""
        all_cur.add(cur)
        jobs_all.add(job)
        if not spec.get("final", False):
            open_ = True
    lines = []
    for key in sorted(groups, key=lambda k: (-groups[k]["amount"], k)):
        g = groups[key]
        if len(g["rates"]) == 1:
            rate = next(iter(g["rates"]))
        else:
            rate = g["amount"] / g["hours"] if g["hours"] else 0.0
        lines.append({"sku": key, "gpuType": g["gpuType"], "gpuHours": round(g["hours"], 4),
                      "rate": round(rate, 4), "amount": round(g["amount"], 2), "jobs": len(g["jobs"]),
                      "currency": _currency(g["cur"])})
    last = calendar.monthrange(year, month)[1]
    currency = _currency(all_cur)
    inv: Dict[str, Any] = {
        "number": f"INV-{tenant}-{year:04d}{month:02d}", "tenant": tenant,
        "period": {"from": f"{year:04d}-{month:02d}-01", "to": f"{year:04d}-{month:02d}-{last:02d}"},
        "currency": currency, "status": "estimate",
        "generatedAt": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "lines": lines, "subtotal": round(sum(g["amount"] for g in groups.values()), 2),
        "jobs": len(jobs_all), "note": NOTE}
    if currency == "MIXED":
        inv["mixedCurrency"] = True
    if open_:
        inv["open"] = True
    if network:
        # Kept apart from the GPU lines: subtotal above is GPU only.
        inv["networkLines"] = network["lines"]
        inv["networkSubtotal"] = network["subtotal"]
        inv["networkNote"] = network["note"]
        if network.get("open"):
            inv["open"] = True
        if not records:
            inv["currency"] = network["currency"]
        elif network["currency"] != currency:
            inv["mixedCurrency"] = True
    return inv


def ledger_lines(entries: List[Dict[str, Any]], gpu_types: Dict[str, str]) -> Dict[str, Any]:
    """Frozen invoice lines from ledger entries, grouped by SKU. Amounts are decimal strings; the subtotal is the
    sum of the rounded line amounts so the lines always add up."""
    groups: "OrderedDict[str, Dict[str, Any]]" = OrderedDict()
    jobs_all: set = set()
    currencies: set = set()
    for e in entries:
        s = e.get("spec") or {}
        ref = s.get("usageRecord") or {}
        key = s.get("sku") or "unknown"
        g = groups.setdefault(key, {"gpuType": "", "hours": Decimal(0), "amount": Decimal(0), "rates": set(),
                                    "jobs": set(), "cur": set()})
        job = (ref.get("namespace"), s.get("job") or ref.get("name"))
        g["hours"] += Decimal(s.get("gpuHours") or "0")
        g["amount"] += Decimal(s.get("cost") or "0")
        g["rates"].add(Decimal(s.get("rate") or "0"))
        g["jobs"].add(job)
        g["cur"].add(s.get("currency") or "USD")
        g["gpuType"] = g["gpuType"] or gpu_types.get(ref.get("uid", ""), "")
        jobs_all.add(job)
        currencies.add(s.get("currency") or "USD")
    lines = []
    for key in sorted(groups, key=lambda k: (-groups[k]["amount"], k)):
        g = groups[key]
        if len(g["rates"]) == 1:
            rate = next(iter(g["rates"]))
        else:
            rate = g["amount"] / g["hours"] if g["hours"] else Decimal(0)
        lines.append({"sku": key, "gpuType": g["gpuType"], "jobs": len(g["jobs"]),
                      "gpuHours": str(g["hours"].quantize(MICRO)), "rate": str(rate.quantize(MICRO)),
                      "amount": str(g["amount"].quantize(CENT, ROUND_HALF_UP)), "currency": _currency(g["cur"])})
    subtotal = sum((Decimal(ln["amount"]) for ln in lines), Decimal(0))
    return {"lines": lines, "subtotal": str(subtotal.quantize(CENT)), "jobs": len(jobs_all),
            "currency": _currency(currencies)}


def invoice_view(obj: Dict[str, Any]) -> Dict[str, Any]:
    """A persisted GryviaInvoice in the same shape as an estimate (floats for display, CSV export works)."""
    s = obj.get("spec") or {}
    y, m = (int(x) for x in s["period"].split("-"))
    last = calendar.monthrange(y, m)[1]
    view: Dict[str, Any] = {
        "number": f"INV-{s['tenant']}-{int(s['number']):06d}", "tenant": s["tenant"],
        "invoice": obj["metadata"]["name"],
        "period": {"from": f"{y:04d}-{m:02d}-01", "to": f"{y:04d}-{m:02d}-{last:02d}"},
        "currency": s.get("currency", "USD"), "status": (s.get("state") or "").lower(),
        "generatedAt": s.get("finalizedAt") or "",
        "lines": [{"sku": ln.get("sku", ""), "gpuType": ln.get("gpuType", ""), "jobs": int(ln.get("jobs", 0)),
                   "gpuHours": float(ln.get("gpuHours") or 0), "rate": float(ln.get("rate") or 0),
                   "amount": float(ln.get("amount") or 0), "currency": ln.get("currency", "")}
                  for ln in s.get("lines") or []],
        "subtotal": float(s.get("subtotal") or 0), "jobs": int(s.get("jobs", 0)), "note": FINAL_NOTE,
        "finalizedAt": s.get("finalizedAt"), "finalizedBy": s.get("finalizedBy"), "ledger": s.get("ledger") or {}}
    for k in ("paidAt", "voidedAt", "voidedBy", "voidReason"):
        if s.get(k):
            view[k] = s[k]
    if s.get("payment"):
        view["payment"] = s["payment"]
    return view


def invoice_name(tenant: str, year: int, month: int, revision: int) -> str:
    """inv-<tenant>-<yyyymm>-<rev>; a tenant that is not a short DNS label is replaced by its hash."""
    slug = tenant if _DNS_LABEL.match(tenant) else "t" + hashlib.sha256(tenant.encode()).hexdigest()[:12]
    return f"inv-{slug}-{year:04d}{month:02d}-{revision}"


def now_rfc3339() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def actor_of(request: Request) -> str:
    claims = getattr(request.state, "user_claims", None) or {}
    who = claims.get("email") or claims.get("sub") or getattr(request.state, "auth_method", None) or "admin"
    return str(who)[:253]


class VoidRequest(BaseModel):
    reason: str = Field(min_length=3, max_length=500)


def invoice_csv(inv: Dict[str, Any]) -> str:
    buf = io.StringIO()
    w = csv.writer(buf, lineterminator="\n")
    w.writerow(CSV_COLUMNS)
    p = inv["period"]

    def row(sku, gpu, jobs, hours, rate, amount, cur):
        w.writerow([_csv_cell(v) for v in (inv["number"], inv["tenant"], p["from"], p["to"], sku, gpu, jobs,
                                            hours, rate, amount, cur)])
    for ln in inv["lines"]:
        row(ln["sku"], ln["gpuType"], ln["jobs"], ln["gpuHours"], ln["rate"], ln["amount"], ln["currency"])
    row("TOTAL", "", inv["jobs"], round(sum(ln["gpuHours"] for ln in inv["lines"]), 4), "", inv["subtotal"],
        inv["currency"])  # GPU only
    if "networkLines" in inv:  # network egress estimate, after and apart from the GPU total; gpuHours holds GB
        for ln in inv["networkLines"]:
            row(f"network:{ln['peerClass']}/{ln['zoneClass']}", "egress-GB", "", ln["egressGB"],
                "" if ln["rate"] is None else ln["rate"], ln["amount"], ln["currency"])
        row("NETWORK-SUBTOTAL", "egress-GB", "", round(sum(ln["egressGB"] for ln in inv["networkLines"]), 6), "",
            inv["networkSubtotal"], inv["currency"])
    return buf.getvalue()


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    async def _month_invoices(request: Request, year: int, month: int,
                              tenant: Optional[str]) -> List[Dict[str, Any]]:
        if is_admin(request):
            records = await fetch_records(deps.k8s_custom, None)
        else:
            tenant = None  # forced: only the caller's own namespaces are ever loaded
            records = await fetch_records(deps.k8s_custom, namespaces(request, deps))
        by_tenant: "OrderedDict[str, List[Dict[str, Any]]]" = OrderedDict()
        for r in records:
            if in_month(r, year, month):
                t = _tenant_of(r)
                if tenant and t != tenant:
                    continue
                by_tenant.setdefault(t, []).append(r)
        # Network usage (opt-in feature): only tenants with records AND a rate table get networkLines.
        rates = await load_rates(deps)
        net_by_tenant: Dict[str, List[Dict[str, Any]]] = {}
        if rates is not None:
            net = await _network_records(request, tenant)
            for r in clean(net, "collector", tenant):
                h = hour_of(r)
                if h is not None and hour_in_month(h, year, month):
                    net_by_tenant.setdefault(net_tenant_of(r), []).append(r)
        out = {t: build_invoice(t, year, month, by_tenant.get(t, []), network_lines(net_by_tenant.get(t, []), rates))
               for t in set(by_tenant) | set(net_by_tenant)}
        if lg.enabled():
            visible = None if is_admin(request) else set(caller_tenant_names(request))
            for obj in await _persisted(f"{year:04d}-{month:02d}"):
                t = obj["spec"]["tenant"]
                if (tenant and t != tenant) or (visible is not None and t not in visible):
                    continue
                out[t] = invoice_view(obj)
        return [out[t] for t in sorted(out)]

    async def _persisted(period: str) -> List[Dict[str, Any]]:
        try:
            items = await list_items(deps, lg.INVOICES)
        except HTTPException:
            return []  # CRD not installed yet: estimates only
        return lg.period_invoices(items, period, "Finalized", "Paid")

    async def _network_records(request: Request, tenant: Optional[str]) -> List[Dict[str, Any]]:
        try:
            if is_admin(request):
                return await fetch_network_records(deps.k8s_custom, None)
            return await fetch_network_records(deps.k8s_custom, namespaces(request, deps))
        except Exception:  # noqa: BLE001 - the CRD may not be installed: invoices must still work
            return []

    @router.get("/api/invoices")
    @deps.limiter.limit("30/minute")
    async def list_invoices(request: Request, month: Optional[str] = Query(default=None, max_length=10),
                            tenant: Optional[str] = Query(default=None, max_length=63),
                            _=Depends(deps.verify_auth)):
        y, m = parse_month(month)
        return {"month": f"{y:04d}-{m:02d}", "items": await _month_invoices(request, y, m, tenant),
                "billingLedger": lg.enabled()}

    @router.get("/api/invoices/{tenant}/{month}")
    @deps.limiter.limit("30/minute")
    async def get_invoice(request: Request, tenant: str, month: str,
                          format: Literal["json", "csv"] = "json", _=Depends(deps.verify_auth)):
        y, m = parse_month(month)
        found = await _month_invoices(request, y, m, tenant)
        inv = next((i for i in found if i["tenant"] == tenant), None)
        if inv is None:
            raise HTTPException(status_code=404, detail="no usage for that tenant in that month")
        if format == "csv":
            return Response(invoice_csv(inv), media_type="text/csv", headers={
                "Content-Disposition": f'attachment; filename="{inv["number"]}.csv"'})
        return inv

    @router.post("/api/invoices/{tenant}/{month}/send")
    @deps.limiter.limit("10/minute")
    async def send_invoice(request: Request, tenant: str, month: str, _=Depends(deps.verify_auth),
                           __=Depends(require_admin)):
        """POST the invoice JSON to the operator-configured webhook (GRYVIA_INVOICE_WEBHOOK_URL).
        Delivery only: no payment is processed."""
        y, m = parse_month(month)
        found = await _month_invoices(request, y, m, tenant)
        inv = next((i for i in found if i["tenant"] == tenant), None)
        if inv is None:
            raise HTTPException(status_code=404, detail="no usage for that tenant in that month")
        return await deliver_invoice(inv)

    @router.post("/api/invoices/{tenant}/{month}/finalize")
    @deps.limiter.limit("10/minute")
    async def finalize_invoice(request: Request, tenant: str, month: str, _=Depends(deps.verify_auth),
                               __=Depends(require_admin)):
        """Freeze a closed month into a GryviaInvoice built from the tenant's ledger entries."""
        lg.require_enabled()
        y, m = parse_month(month)
        period = f"{y:04d}-{m:02d}"
        records = [r for r in await fetch_records(deps.k8s_custom, None)
                   if in_month(r, y, m) and _tenant_of(r) == tenant]
        if not records:
            raise HTTPException(status_code=404, detail="no usage for that tenant in that month")
        if any(not (r.get("spec") or {}).get("final") for r in records):
            raise HTTPException(status_code=409, detail=f"{period} still has open usage records; finalize after "
                                                        "the jobs finish")
        pending = [r for r in records if not ((r.get("metadata") or {}).get("annotations") or {}).get(
            lg.ANNOTATION_LEDGER_ENTRY)]
        if pending:
            raise HTTPException(status_code=409, detail=f"{len(pending)} sealed usage records are not in the ledger "
                                                        "yet; retry shortly")
        invoices = await lg.tenant_invoices(deps, tenant)
        if lg.period_invoices(invoices, period, "Finalized", "Paid"):
            raise HTTPException(status_code=409, detail=f"{period} is already finalized for {tenant}; void it first")
        for draft in lg.period_invoices(invoices, period, "Draft"):  # left over by an interrupted finalize
            await delete_item(deps, lg.INVOICES, draft["metadata"]["name"])
        invoices = [i for i in invoices if i["spec"].get("state") != "Draft"]

        entries = await lg.tenant_entries(deps, tenant)
        chain = lg.verify_chain(tenant, entries)
        if not chain["valid"]:
            raise HTTPException(status_code=409, detail=f"the ledger chain of {tenant} fails verification; "
                                                        f"see /api/ledger/{tenant}/verify")
        uids = {(r.get("metadata") or {}).get("uid") for r in records}
        mine = [e for e in entries if ((e.get("spec") or {}).get("usageRecord") or {}).get("uid") in uids]
        if len(mine) != len(records):
            raise HTTPException(status_code=409, detail="some sealed usage records have no ledger entry; retry shortly")
        built = ledger_lines(mine, {(r.get("metadata") or {}).get("uid", ""): (r.get("spec") or {}).get(
            "gpuType", "") for r in records})
        if built["currency"] == "MIXED":
            raise HTTPException(status_code=409, detail="an invoice must have one currency; this month mixes several")
        seqs = sorted(int(e["spec"]["sequence"]) for e in mine)
        head = max(mine, key=lambda e: int(e["spec"]["sequence"]))
        number = max((int(i["spec"].get("number", 0)) for i in invoices), default=0) + 1
        name = invoice_name(tenant, y, m, len(lg.period_invoices(invoices, period)) + 1)
        spec = {"tenant": tenant, "period": period, "number": number, "state": "Draft",
                "currency": built["currency"], "lines": built["lines"], "subtotal": built["subtotal"],
                "jobs": built["jobs"],
                "ledger": {"entries": len(mine), "firstSequence": seqs[0], "lastSequence": seqs[-1],
                           "headHash": head["spec"]["hash"]}}
        await create_item(deps, lg.INVOICES, "GryviaInvoice", name, spec,
                          labels={lg.LABEL_TENANT: tenant, lg.LABEL_PERIOD: period})
        clash = [i for i in await lg.tenant_invoices(deps, tenant)
                 if i["metadata"]["name"] != name and int(i["spec"].get("number", 0)) == number]
        if clash:
            await delete_item(deps, lg.INVOICES, name)
            raise HTTPException(status_code=409, detail="another invoice took that number concurrently; retry")
        done = await patch_item(deps, lg.INVOICES, name, {"spec": {
            "state": "Finalized", "finalizedAt": now_rfc3339(), "finalizedBy": actor_of(request)}})
        return invoice_view(done)

    @router.post("/api/invoices/{tenant}/{month}/void")
    @deps.limiter.limit("10/minute")
    async def void_invoice(request: Request, tenant: str, month: str, body: VoidRequest,
                           _=Depends(deps.verify_auth), __=Depends(require_admin)):
        """Void the month's Finalized invoice (kept for audit); the month can then be finalized again."""
        lg.require_enabled()
        y, m = parse_month(month)
        period = f"{y:04d}-{m:02d}"
        invoices = await lg.tenant_invoices(deps, tenant)
        if lg.period_invoices(invoices, period, "Paid"):
            raise HTTPException(status_code=409, detail="a paid invoice cannot be voided")
        current = lg.period_invoices(invoices, period, "Finalized")
        if not current:
            raise HTTPException(status_code=404, detail="no finalized invoice for that tenant in that month")
        done = await patch_item(deps, lg.INVOICES, current[0]["metadata"]["name"], {"spec": {
            "state": "Void", "voidedAt": now_rfc3339(), "voidedBy": actor_of(request), "voidReason": body.reason}})
        return invoice_view(done)

    return router
