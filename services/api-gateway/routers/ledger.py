"""Immutable billing ledger (opt-in, chart ``billing.ledger.enabled`` -> GRYVIA_BILLING_LEDGER=1).

The quota-operator appends one cluster-scoped GryviaLedgerEntry per sealed GryviaUsageRecord, chained per tenant:
entry N stores the SHA-256 of entry N-1 (``prevHash``) and its own ``hash`` over a fixed line layout (``hash_input``,
identical to LedgerHashInput in operators/quota-operator/api/v1/gryvialedgerentry_types.go). Every hashed field is a
string or an integer, so this module recomputes the hash byte for byte. The admission webhook rejects every update
and delete of an entry; ``GET /api/ledger/{tenant}/verify`` proves the stored chain was not rewritten around it.
"""
import hashlib
import os
from typing import Any, Dict, List

from fastapi import APIRouter, Depends, HTTPException, Request

from .common import Deps, list_items
from .tenancy import caller_tenant_names, is_admin

LEDGER = "gryvialedgerentries"
INVOICES = "gryviainvoices"
HASH_VERSION = "v1"
LABEL_TENANT = "gryvia.io/tenant"
LABEL_PERIOD = "gryvia.io/period"
ANNOTATION_LEDGER_ENTRY = "gryvia.io/ledger-entry"


def enabled() -> bool:
    return os.environ.get("GRYVIA_BILLING_LEDGER", "") == "1"


def require_enabled() -> None:
    if not enabled():
        raise HTTPException(status_code=503, detail="The billing ledger is disabled; enable billing.ledger.enabled")


def hash_input(spec: Dict[str, Any]) -> str:
    rec = spec.get("usageRecord") or {}
    fields = [HASH_VERSION, spec.get("tenant", ""), str(int(spec.get("sequence", 0))), spec.get("prevHash", ""),
              rec.get("namespace", ""), rec.get("name", ""), rec.get("uid", ""),
              spec.get("kind", ""), spec.get("job", ""), spec.get("sku", ""), spec.get("start", ""),
              spec.get("end", ""), spec.get("gpuHours", ""), spec.get("rate", ""), spec.get("cost", ""),
              spec.get("currency", ""), spec.get("recordedAt", "")]
    return "\n".join("" if f is None else str(f) for f in fields)


def ledger_hash(spec: Dict[str, Any]) -> str:
    return hashlib.sha256(hash_input(spec).encode()).hexdigest()


def verify_chain(tenant: str, entries: List[Dict[str, Any]]) -> Dict[str, Any]:
    """Check a tenant's entries: sequences 1..N without gaps or duplicates, each prevHash equal to the previous
    entry's hash and each hash equal to the recomputed one. Problems are listed, not raised."""
    specs = sorted((e.get("spec") or {} for e in entries), key=lambda s: int(s.get("sequence", 0)))
    problems: List[Dict[str, Any]] = []
    prev = ""
    for want, s in enumerate(specs, start=1):
        seq = int(s.get("sequence", 0))
        if seq != want:
            problems.append({"sequence": seq, "problem": f"expected sequence {want}"})
        if (s.get("prevHash") or "") != prev:
            problems.append({"sequence": seq, "problem": "prevHash does not match the previous entry's hash"})
        if s.get("hash") != ledger_hash(s):
            problems.append({"sequence": seq, "problem": "hash does not match the entry's contents"})
        prev = s.get("hash") or ""
    head = specs[-1] if specs else {}
    return {"tenant": tenant, "entries": len(specs), "valid": not problems, "problems": problems,
            "headSequence": int(head.get("sequence", 0)), "headHash": head.get("hash", ""),
            "hashVersion": HASH_VERSION}


async def tenant_entries(deps: Deps, tenant: str) -> List[Dict[str, Any]]:
    return [e for e in await list_items(deps, LEDGER) if (e.get("spec") or {}).get("tenant") == tenant]


async def tenant_invoices(deps: Deps, tenant: str) -> List[Dict[str, Any]]:
    try:
        items = await list_items(deps, INVOICES)
    except HTTPException:
        return []  # CRD not installed: no persisted invoices
    return [i for i in items if (i.get("spec") or {}).get("tenant") == tenant]


def period_invoices(invoices: List[Dict[str, Any]], period: str, *states: str) -> List[Dict[str, Any]]:
    return [i for i in invoices if (i.get("spec") or {}).get("period") == period
            and (not states or (i.get("spec") or {}).get("state") in states)]


def caller_may_read(request: Request, tenant: str) -> bool:
    return is_admin(request) or tenant in caller_tenant_names(request)


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/ledger/{tenant}/verify")
    @deps.limiter.limit("10/minute")
    async def verify(request: Request, tenant: str, _=Depends(deps.verify_auth)):
        """Recompute the tenant's hash chain. Admins may verify any tenant, a tenant user only its own."""
        require_enabled()
        if not caller_may_read(request, tenant):
            raise HTTPException(status_code=404, detail="no ledger for that tenant")
        return verify_chain(tenant, await tenant_entries(deps, tenant))

    return router
