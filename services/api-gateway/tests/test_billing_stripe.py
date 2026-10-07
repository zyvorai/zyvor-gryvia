"""Stripe test-mode integration with Stripe mocked (httpx.MockTransport); no network calls."""
import hashlib
import hmac
import json
import time
from urllib.parse import parse_qs

import httpx
import pytest
from fastapi import HTTPException

from routers import billing_stripe as bs
from routers import ledger as lg

WHSEC = "whsec_test_secret"
NAME = "inv-acme-202609-1"


def finalized(payment=None, state="Finalized"):
    spec = {"tenant": "acme", "period": "2026-09", "number": 1, "state": state, "currency": "USD",
            "subtotal": "15.00", "jobs": 2, "finalizedAt": "2026-10-01T00:00:00Z", "finalizedBy": "admin",
            "lines": [{"sku": "h100", "gpuType": "H100", "jobs": 2, "gpuHours": "6.000000", "rate": "2.500000",
                       "amount": "15.00", "currency": "USD"},
                      {"sku": "l4", "gpuType": "L4", "jobs": 1, "gpuHours": "1.000000", "rate": "0.333333",
                       "amount": "0.33", "currency": "USD"}],
            "ledger": {"entries": 3, "firstSequence": 1, "lastSequence": 3, "headHash": "h"}}
    if payment:
        spec["payment"] = payment
    return {"metadata": {"name": NAME}, "spec": spec}


class FakeStripe:
    def __init__(self, livemode=False, existing_customer=None):
        self.calls = []
        self.livemode = livemode
        self.existing_customer = existing_customer

    def __call__(self, request: httpx.Request) -> httpx.Response:
        form = {k: v[0] for k, v in parse_qs(request.content.decode()).items()}
        self.calls.append((request.method, request.url.path, form, request.headers.get("idempotency-key"),
                           request.headers.get("authorization")))
        path = request.url.path
        if path == "/v1/customers/search":
            data = [{"id": self.existing_customer}] if self.existing_customer else []
            return httpx.Response(200, json={"data": data, "livemode": self.livemode})
        if path == "/v1/customers":
            return httpx.Response(200, json={"id": "cus_new", "livemode": self.livemode})
        if path == "/v1/invoices":
            return httpx.Response(200, json={"id": "in_test_1", "livemode": self.livemode})
        if path == "/v1/invoiceitems":
            return httpx.Response(200, json={"id": "ii_1", "livemode": self.livemode})
        if path == "/v1/invoices/in_test_1/finalize":
            return httpx.Response(200, json={"id": "in_test_1", "hosted_invoice_url": "https://invoice.stripe.com/i/x",
                                             "livemode": self.livemode})
        return httpx.Response(404, json={"error": {"message": "no such route"}})


@pytest.fixture
def stripe_env(monkeypatch):
    monkeypatch.setenv("GRYVIA_BILLING_LEDGER", "1")
    monkeypatch.setenv("GRYVIA_STRIPE_SECRET_KEY", "sk_test_abc")
    monkeypatch.setenv("GRYVIA_STRIPE_WEBHOOK_SECRET", WHSEC)


@pytest.fixture
def fake_stripe(monkeypatch):
    fake = FakeStripe()
    monkeypatch.setattr(bs, "TRANSPORT", httpx.MockTransport(fake))
    return fake


def signed(event, secret=WHSEC, ts=None):
    body = json.dumps(event).encode()
    ts = int(time.time()) if ts is None else ts
    sig = hmac.new(secret.encode(), f"{ts}.".encode() + body, hashlib.sha256).hexdigest()
    return body, {"Stripe-Signature": f"t={ts},v1={sig}", "Content-Type": "application/json"}


def paid_event(invoice_id="in_test_1", name=NAME, livemode=False, type_="invoice.paid"):
    return {"id": "evt_1", "type": type_, "livemode": livemode,
            "data": {"object": {"id": invoice_id, "metadata": {"gryvia_invoice": name}}}}


def test_only_test_keys_enable_stripe(monkeypatch, make_client, fake_k8s):
    monkeypatch.setenv("GRYVIA_BILLING_LEDGER", "1")
    for key in ("", "sk_live_abc", "rk_live_abc", "pk_test_abc"):
        monkeypatch.setenv("GRYVIA_STRIPE_SECRET_KEY", key)
        assert bs.secret_key() is None
        assert make_client("billing_stripe").post("/api/invoices/acme/2026-09/stripe").status_code == 503
    monkeypatch.setenv("GRYVIA_STRIPE_SECRET_KEY", "sk_test_abc")
    assert bs.secret_key() == "sk_test_abc"


def test_minor_units():
    assert bs.minor_units("15.00", "USD") == 1500
    assert bs.minor_units("0.33", "eur") == 33
    assert bs.minor_units("1200", "JPY") == 1200


def test_send_creates_customer_invoice_items_and_records_payment(make_client, fake_k8s, stripe_env, fake_stripe):
    fake_k8s.add(lg.INVOICES, finalized())
    r = make_client("billing_stripe").post("/api/invoices/acme/2026-09/stripe")
    assert r.status_code == 200, r.text
    assert r.json()["payment"] == {"provider": "stripe", "invoiceID": "in_test_1",
                                   "hostedURL": "https://invoice.stripe.com/i/x", "liveMode": False}
    paths = [(m, p) for m, p, *_ in fake_stripe.calls]
    assert paths == [("GET", "/v1/customers/search"), ("POST", "/v1/customers"), ("POST", "/v1/invoices"),
                     ("POST", "/v1/invoiceitems"), ("POST", "/v1/invoiceitems"),
                     ("POST", "/v1/invoices/in_test_1/finalize")]
    _, _, inv_form, inv_key, auth = fake_stripe.calls[2]
    assert inv_form["metadata[gryvia_invoice]"] == NAME and inv_form["customer"] == "cus_new"
    assert inv_form["collection_method"] == "send_invoice" and inv_form["currency"] == "usd"
    assert inv_key == f"gryvia-{NAME}-invoice" and auth.startswith("Basic ")
    items = [c[2] for c in fake_stripe.calls if c[1] == "/v1/invoiceitems"]
    assert [i["amount"] for i in items] == ["1500", "33"] and items[0]["invoice"] == "in_test_1"
    assert [c[3] for c in fake_stripe.calls if c[1] == "/v1/invoiceitems"] == [f"gryvia-{NAME}-item-0",
                                                                               f"gryvia-{NAME}-item-1"]
    assert fake_k8s.store[(lg.INVOICES, None, NAME)]["spec"]["payment"]["invoiceID"] == "in_test_1"

    again = make_client("billing_stripe").post("/api/invoices/acme/2026-09/stripe")
    assert again.status_code == 409


def test_send_reuses_existing_customer(make_client, fake_k8s, stripe_env, monkeypatch):
    fake = FakeStripe(existing_customer="cus_old")
    monkeypatch.setattr(bs, "TRANSPORT", httpx.MockTransport(fake))
    fake_k8s.add(lg.INVOICES, finalized())
    assert make_client("billing_stripe").post("/api/invoices/acme/2026-09/stripe").status_code == 200
    assert "/v1/customers" not in [c[1] for c in fake.calls]
    assert fake.calls[1][2]["customer"] == "cus_old"


def test_send_refuses_live_objects_and_needs_finalized(make_client, fake_k8s, stripe_env, monkeypatch):
    c = make_client("billing_stripe")
    assert c.post("/api/invoices/acme/2026-09/stripe").status_code == 404
    monkeypatch.setattr(bs, "TRANSPORT", httpx.MockTransport(FakeStripe(livemode=True)))
    fake_k8s.add(lg.INVOICES, finalized())
    r = c.post("/api/invoices/acme/2026-09/stripe")
    assert r.status_code == 502 and "live-mode" in r.json()["detail"]
    assert "payment" not in fake_k8s.store[(lg.INVOICES, None, NAME)]["spec"]


def test_send_admin_only(make_client, fake_k8s, stripe_env, fake_stripe):
    fake_k8s.add(lg.INVOICES, finalized())
    tenant = make_client("billing_stripe", role="tenant", tenants=["acme"])
    assert tenant.post("/api/invoices/acme/2026-09/stripe").status_code == 403
    assert fake_stripe.calls == []


def test_webhook_marks_paid_idempotently(make_client, fake_k8s, stripe_env):
    fake_k8s.add(lg.INVOICES, finalized(payment={"provider": "stripe", "invoiceID": "in_test_1", "liveMode": False}))
    c = make_client("billing_stripe")
    body, headers = signed(paid_event())
    r = c.post("/api/billing/stripe/webhook", content=body, headers=headers)
    assert r.status_code == 200 and r.json() == {"received": True, "handled": True, "state": "Paid"}
    spec = fake_k8s.store[(lg.INVOICES, None, NAME)]["spec"]
    assert spec["state"] == "Paid" and spec["paidAt"].endswith("Z")
    assert c.post("/api/billing/stripe/webhook", content=body, headers=headers).status_code == 200


def test_webhook_rejects_bad_signatures_and_live_events(make_client, fake_k8s, stripe_env):
    fake_k8s.add(lg.INVOICES, finalized(payment={"provider": "stripe", "invoiceID": "in_test_1", "liveMode": False}))
    c = make_client("billing_stripe")
    body, headers = signed(paid_event(), secret="whsec_wrong")
    assert c.post("/api/billing/stripe/webhook", content=body, headers=headers).status_code == 400
    body, headers = signed(paid_event(), ts=int(time.time()) - 301)
    assert c.post("/api/billing/stripe/webhook", content=body, headers=headers).status_code == 400
    assert c.post("/api/billing/stripe/webhook", content=body, headers={"Stripe-Signature": "v1=x"}).status_code == 400
    body, headers = signed(paid_event(livemode=True))
    assert c.post("/api/billing/stripe/webhook", content=body, headers=headers).status_code == 400
    body, headers = signed(paid_event(invoice_id="in_other"))
    assert c.post("/api/billing/stripe/webhook", content=body, headers=headers).status_code == 409
    assert fake_k8s.store[(lg.INVOICES, None, NAME)]["spec"]["state"] == "Finalized"


def test_webhook_ignores_other_events_and_void_invoices(make_client, fake_k8s, stripe_env):
    c = make_client("billing_stripe")
    body, headers = signed(paid_event(type_="invoice.created"))
    assert c.post("/api/billing/stripe/webhook", content=body, headers=headers).json()["handled"] is False
    fake_k8s.add(lg.INVOICES, finalized(payment={"provider": "stripe", "invoiceID": "in_test_1", "liveMode": False},
                                        state="Void"))
    body, headers = signed(paid_event())
    assert c.post("/api/billing/stripe/webhook", content=body, headers=headers).status_code == 409


def test_verify_signature_accepts_any_v1():
    payload, ts = b"{}", int(time.time())
    good = hmac.new(WHSEC.encode(), f"{ts}.".encode() + payload, hashlib.sha256).hexdigest()
    bs.verify_signature(WHSEC, f"t={ts},v1=deadbeef,v1={good}", payload)
    with pytest.raises(HTTPException):
        bs.verify_signature(WHSEC, f"t={ts},v0={good}", payload)
