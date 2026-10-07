"""Stripe, test mode only (opt-in, chart ``billing.stripe.secretName`` on top of ``billing.ledger.enabled``).

Enabled only when GRYVIA_STRIPE_SECRET_KEY starts with ``sk_test_``; a live key (``sk_live_``/``rk_live_``) keeps the
integration off, and any Stripe object that reports ``livemode: true`` is refused. Nothing here can move real money.

``POST /api/invoices/{tenant}/{month}/stripe`` (admin) sends a Finalized GryviaInvoice to Stripe: it finds or creates
the tenant's customer (metadata ``gryvia_tenant``), creates a draft invoice, one invoice item per frozen line and
finalizes it, every call with an idempotency key derived from the GryviaInvoice name, then records the Stripe invoice
id once in ``spec.payment`` (the admission webhook allows setting it once). ``POST /api/billing/stripe/webhook`` is
called by Stripe; it checks the ``Stripe-Signature`` header (HMAC-SHA256 of ``"<t>.<body>"`` with
GRYVIA_STRIPE_WEBHOOK_SECRET, 300 s tolerance) and moves the matching invoice to Paid on ``invoice.paid``.
"""
import hashlib
import hmac
import json
import logging
import os
import time
from decimal import Decimal
from typing import Any, Dict, Optional

import httpx
from fastapi import APIRouter, Depends, HTTPException, Request

from . import ledger as lg
from .common import Deps, get_item, patch_item, require_admin
from .invoices import invoice_view, now_rfc3339, parse_month

logger = logging.getLogger(__name__)

API = "https://api.stripe.com"
TOLERANCE_SECONDS = 300
TIMEOUT_SECONDS = 15.0
MAX_WEBHOOK_BYTES = 256 * 1024
# Tests replace this with an httpx.MockTransport; None uses the network.
TRANSPORT: Optional[httpx.AsyncBaseTransport] = None
# Currencies without minor units (amounts are sent as whole units).
ZERO_DECIMAL = frozenset({"bif", "clp", "djf", "gnf", "jpy", "kmf", "krw", "mga", "pyg", "rwf", "ugx", "vnd", "vuv",
                          "xaf", "xof", "xpf"})


def secret_key() -> Optional[str]:
    key = os.environ.get("GRYVIA_STRIPE_SECRET_KEY", "")
    return key if key.startswith("sk_test_") else None


def require_stripe() -> str:
    lg.require_enabled()
    key = secret_key()
    if not key:
        raise HTTPException(status_code=503, detail="Stripe is disabled: set billing.stripe.secretName to a Secret "
                                                    "holding a test-mode key (sk_test_...)")
    return key


def minor_units(amount: str, currency: str) -> int:
    value = Decimal(amount)
    return int(value if currency.lower() in ZERO_DECIMAL else (value * 100).quantize(Decimal(1)))


def verify_signature(secret: str, header: str, payload: bytes, now: Optional[float] = None) -> None:
    """Stripe's scheme: header ``t=<unix>,v1=<hex>[,v1=...]``; v1 = HMAC-SHA256(secret, "<t>.<payload>")."""
    parts: Dict[str, list] = {}
    for item in header.split(","):
        k, _, v = item.strip().partition("=")
        parts.setdefault(k, []).append(v)
    try:
        ts = int(parts["t"][0])
    except (KeyError, ValueError, IndexError):
        raise HTTPException(status_code=400, detail="malformed Stripe-Signature header")
    if abs((now if now is not None else time.time()) - ts) > TOLERANCE_SECONDS:
        raise HTTPException(status_code=400, detail="Stripe-Signature timestamp outside the tolerance")
    want = hmac.new(secret.encode(), f"{ts}.".encode() + payload, hashlib.sha256).hexdigest()
    if not any(hmac.compare_digest(want, sig) for sig in parts.get("v1", [])):
        raise HTTPException(status_code=400, detail="Stripe-Signature does not match")


class Stripe:
    def __init__(self, key: str) -> None:
        self.client = httpx.AsyncClient(base_url=API, auth=(key, ""), timeout=TIMEOUT_SECONDS, transport=TRANSPORT,
                                        follow_redirects=False)

    async def __aenter__(self) -> "Stripe":
        return self

    async def __aexit__(self, *exc: Any) -> None:
        await self.client.aclose()

    async def call(self, method: str, path: str, data: Optional[Dict[str, Any]] = None,
                   idempotency_key: Optional[str] = None, params: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
        headers = {"Idempotency-Key": idempotency_key} if idempotency_key else {}
        try:
            res = await self.client.request(method, path, data=data, params=params, headers=headers)
        except httpx.HTTPError as exc:
            raise HTTPException(status_code=502, detail=f"Stripe unreachable: {type(exc).__name__}") from exc
        if res.status_code >= 400:
            msg = ((res.json() if res.headers.get("content-type", "").startswith("application/json") else {})
                   .get("error") or {}).get("message") or res.reason_phrase
            raise HTTPException(status_code=502, detail=f"Stripe {path}: {msg}")
        body = res.json()
        if body.get("livemode") is True:
            raise HTTPException(status_code=502, detail="Stripe returned a live-mode object; refusing")
        return body


async def find_or_create_customer(stripe: Stripe, tenant: str) -> str:
    found = await stripe.call("GET", "/v1/customers/search",
                              params={"query": f"metadata['gryvia_tenant']:'{tenant}'", "limit": 1})
    if found.get("data"):
        return found["data"][0]["id"]
    created = await stripe.call("POST", "/v1/customers", {"name": tenant, "metadata[gryvia_tenant]": tenant},
                                idempotency_key=f"gryvia-customer-{tenant}")
    return created["id"]


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.post("/api/invoices/{tenant}/{month}/stripe")
    @deps.limiter.limit("5/minute")
    async def send_to_stripe(request: Request, tenant: str, month: str, _=Depends(deps.verify_auth),
                             __=Depends(require_admin)):
        key = require_stripe()
        y, m = parse_month(month)
        period = f"{y:04d}-{m:02d}"
        current = lg.period_invoices(await lg.tenant_invoices(deps, tenant), period, "Finalized")
        if not current:
            raise HTTPException(status_code=404, detail="no finalized invoice for that tenant in that month")
        inv = current[0]
        name, spec = inv["metadata"]["name"], inv["spec"]
        if spec.get("payment"):
            raise HTTPException(status_code=409, detail="this invoice was already sent to Stripe")
        currency = (spec.get("currency") or "USD").lower()
        async with Stripe(key) as stripe:
            customer = await find_or_create_customer(stripe, tenant)
            draft = await stripe.call("POST", "/v1/invoices", {
                "customer": customer, "collection_method": "send_invoice", "days_until_due": 30,
                "auto_advance": "false", "currency": currency, "description": f"Gryvia GPU usage {period}",
                "metadata[gryvia_invoice]": name, "metadata[gryvia_tenant]": tenant,
                "metadata[gryvia_period]": period}, idempotency_key=f"gryvia-{name}-invoice")
            for i, ln in enumerate(spec.get("lines") or []):
                await stripe.call("POST", "/v1/invoiceitems", {
                    "customer": customer, "invoice": draft["id"], "currency": currency,
                    "amount": minor_units(ln["amount"], currency),
                    "description": f"{ln['sku']}: {ln['gpuHours']} GPU-hours x {ln['rate']} ({ln['jobs']} jobs)"},
                    idempotency_key=f"gryvia-{name}-item-{i}")
            final = await stripe.call("POST", f"/v1/invoices/{draft['id']}/finalize", {"auto_advance": "false"},
                                      idempotency_key=f"gryvia-{name}-finalize")
        done = await patch_item(deps, lg.INVOICES, name, {"spec": {"payment": {
            "provider": "stripe", "invoiceID": final["id"], "hostedURL": final.get("hosted_invoice_url") or "",
            "liveMode": False}}})
        return invoice_view(done)

    @router.post("/api/billing/stripe/webhook")
    @deps.limiter.limit("60/minute")
    async def stripe_webhook(request: Request):
        """Called by Stripe, authenticated by the signature only. Unknown event types are acknowledged."""
        require_stripe()
        secret = os.environ.get("GRYVIA_STRIPE_WEBHOOK_SECRET", "")
        if not secret:
            raise HTTPException(status_code=503, detail="GRYVIA_STRIPE_WEBHOOK_SECRET is not set")
        payload = await request.body()
        if len(payload) > MAX_WEBHOOK_BYTES:
            raise HTTPException(status_code=413, detail="payload too large")
        verify_signature(secret, request.headers.get("stripe-signature", ""), payload)
        try:
            event = json.loads(payload)
        except ValueError:
            raise HTTPException(status_code=400, detail="payload is not JSON")
        if event.get("livemode") is not False:
            raise HTTPException(status_code=400, detail="only test-mode events are accepted")
        if event.get("type") != "invoice.paid":
            return {"received": True, "handled": False}
        obj = (event.get("data") or {}).get("object") or {}
        name = (obj.get("metadata") or {}).get("gryvia_invoice")
        if not name:
            return {"received": True, "handled": False}
        inv = await get_item(deps, lg.INVOICES, name)
        spec = inv.get("spec") or {}
        if (spec.get("payment") or {}).get("invoiceID") != obj.get("id"):
            raise HTTPException(status_code=409, detail="Stripe invoice does not match the recorded payment")
        if spec.get("state") == "Paid":
            return {"received": True, "handled": True, "state": "Paid"}
        if spec.get("state") != "Finalized":
            raise HTTPException(status_code=409, detail=f"invoice is {spec.get('state')}, not Finalized")
        await patch_item(deps, lg.INVOICES, name, {"spec": {"state": "Paid", "paidAt": now_rfc3339()}})
        logger.info("invoice %s paid through Stripe (test mode)", name)
        return {"received": True, "handled": True, "state": "Paid"}

    return router
