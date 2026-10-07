"""Billing ledger: hash verification, finalize/void of GryviaInvoice and the persisted invoice view."""
import pytest

from routers import ledger as lg
from routers.invoices import invoice_name, ledger_lines

GOLDEN = "43516e7b3b191a079bd53edfb1138e394a588ee14a9571d316ada1c598da86ac"


def usage(name, tenant, cost, hours=5.0, final=True, sealed=True, start="2026-09-03T10:00:00Z", currency="USD",
          sku="h100"):
    meta = {"name": name, "uid": f"uid-{name}"}
    if sealed and final:
        meta["annotations"] = {lg.ANNOTATION_LEDGER_ENTRY: f"le-uid-{name}"}
    return {"metadata": meta,
            "spec": {"tenant": tenant, "job": f"train-{name}", "sku": sku, "gpuType": "H100", "gpus": 2,
                     "start": start, "end": start, "gpuHours": hours, "rate": cost / hours, "cost": cost,
                     "currency": currency, "final": final}}


def chain(tenant, records, start_seq=1, prev=""):
    out = []
    for i, r in enumerate(records):
        s = r["spec"]
        spec = {"tenant": tenant, "sequence": start_seq + i, "prevHash": prev,
                "usageRecord": {"namespace": f"tenant-{tenant}", "name": r["metadata"]["name"],
                                "uid": r["metadata"]["uid"]},
                "kind": "gpu", "job": s["job"], "sku": s["sku"], "start": s["start"], "end": s["end"],
                "gpuHours": f"{s['gpuHours']:.6f}", "rate": f"{s['rate']:.6f}", "cost": f"{s['cost']:.6f}",
                "currency": s["currency"], "recordedAt": "2026-10-01T00:00:00Z"}
        spec["hash"] = lg.ledger_hash(spec)
        prev = spec["hash"]
        out.append({"metadata": {"name": f"le-{r['metadata']['uid']}", "labels": {lg.LABEL_TENANT: tenant}},
                    "spec": spec})
    return out


def seed(fake_k8s, tenant, records, entries=True):
    for r in records:
        fake_k8s.add("gryviausagerecords", r, f"tenant-{tenant}")
    if entries:
        for e in chain(tenant, [r for r in records if r["spec"]["final"]]):
            fake_k8s.add(lg.LEDGER, e)


@pytest.fixture
def ledger_on(monkeypatch):
    monkeypatch.setenv("GRYVIA_BILLING_LEDGER", "1")


def test_hash_matches_go_golden():
    spec = {"tenant": "acme", "sequence": 2, "prevHash": "abc",
            "usageRecord": {"namespace": "tenant-acme", "name": "usage-1", "uid": "u1"},
            "kind": "gpu", "job": "train", "sku": "H100", "start": "2026-09-03T10:00:00Z",
            "end": "2026-09-03T12:30:00Z", "gpuHours": "5.000000", "rate": "2.500000", "cost": "12.500000",
            "currency": "USD", "recordedAt": "2026-10-01T00:00:00Z"}
    assert lg.ledger_hash(spec) == GOLDEN


def test_verify_detects_rewrite_gap_and_broken_link():
    recs = [usage(f"r{i}", "acme", 1.0 + i) for i in range(3)]
    good = chain("acme", recs)
    assert lg.verify_chain("acme", good)["valid"]
    assert lg.verify_chain("acme", good)["headSequence"] == 3

    edited = chain("acme", recs)
    edited[1]["spec"]["cost"] = "0.000000"
    problems = lg.verify_chain("acme", edited)["problems"]
    assert problems == [{"sequence": 2, "problem": "hash does not match the entry's contents"}]

    gap = [good[0], good[2]]
    assert {p["problem"] for p in lg.verify_chain("acme", gap)["problems"]} == {
        "expected sequence 2", "prevHash does not match the previous entry's hash"}

    relinked = chain("acme", recs)
    relinked[2]["spec"]["prevHash"] = "0" * 64
    relinked[2]["spec"]["hash"] = lg.ledger_hash(relinked[2]["spec"])
    assert not lg.verify_chain("acme", relinked)["valid"]


def test_verify_endpoint_scoping(make_client, fake_k8s, ledger_on):
    seed(fake_k8s, "acme", [usage("a", "acme", 12.5)])
    seed(fake_k8s, "beta", [usage("b", "beta", 3.0)])
    body = make_client("ledger").get("/api/ledger/acme/verify").json()
    assert body["valid"] and body["entries"] == 1 and body["hashVersion"] == "v1"
    tenant = make_client("ledger", role="tenant", tenants=["acme"])
    assert tenant.get("/api/ledger/acme/verify").status_code == 200
    assert tenant.get("/api/ledger/beta/verify").status_code == 404


def test_endpoints_off_without_flag(make_client, fake_k8s):
    assert make_client("ledger").get("/api/ledger/acme/verify").status_code == 503
    assert make_client("invoices").post("/api/invoices/acme/2026-09/finalize").status_code == 503
    assert make_client("invoices").get("/api/invoices?month=2026-09").json()["billingLedger"] is False


def test_ledger_lines_decimal_and_rounding():
    recs = [usage("a", "acme", 1.005, hours=0.333333), usage("b", "acme", 1.004, hours=0.333333),
            usage("c", "acme", 8.0, hours=1.0, sku="a100")]
    built = ledger_lines(chain("acme", recs), {"uid-a": "H100"})
    assert built["lines"][0] == {"sku": "a100", "gpuType": "", "jobs": 1, "gpuHours": "1.000000",
                                 "rate": "8.000000", "amount": "8.00", "currency": "USD"}
    h100 = built["lines"][1]
    assert h100["gpuType"] == "H100" and h100["amount"] == "2.01" and h100["gpuHours"] == "0.666666"
    assert built["subtotal"] == "10.01" and built["jobs"] == 3


def test_invoice_name_slug():
    assert invoice_name("acme", 2026, 9, 1) == "inv-acme-202609-1"
    odd = invoice_name("Team_A", 2026, 9, 2)
    assert odd.startswith("inv-t") and odd.endswith("-202609-2") and odd == odd.lower()


def test_finalize_freezes_lines_and_replaces_estimate(make_client, fake_k8s, ledger_on):
    seed(fake_k8s, "acme", [usage("a1", "acme", 12.5), usage("a2", "acme", 2.5, hours=1.0)])
    c = make_client("invoices")
    estimate = c.get("/api/invoices/acme/2026-09").json()
    assert estimate["status"] == "estimate"

    r = c.post("/api/invoices/acme/2026-09/finalize")
    assert r.status_code == 200, r.text
    inv = r.json()
    assert inv["status"] == "finalized" and inv["number"] == "INV-acme-000001"
    assert inv["invoice"] == "inv-acme-202609-1" and inv["subtotal"] == 15.0 and inv["jobs"] == 2
    assert inv["ledger"]["entries"] == 2 and inv["ledger"]["firstSequence"] == 1 and inv["ledger"]["lastSequence"] == 2
    stored = fake_k8s.store[(lg.INVOICES, None, "inv-acme-202609-1")]
    assert stored["spec"]["state"] == "Finalized" and stored["spec"]["finalizedBy"] == "admin"
    assert stored["spec"]["lines"][0]["amount"] == "15.00" and stored["spec"]["subtotal"] == "15.00"
    assert stored["metadata"]["labels"] == {lg.LABEL_TENANT: "acme", lg.LABEL_PERIOD: "2026-09"}

    # The persisted invoice now wins over the estimate, in the list, the single view and the CSV.
    assert c.get("/api/invoices/acme/2026-09").json()["status"] == "finalized"
    listed = c.get("/api/invoices?month=2026-09").json()
    assert listed["billingLedger"] is True and listed["items"][0]["number"] == "INV-acme-000001"
    assert "INV-acme-000001" in c.get("/api/invoices/acme/2026-09?format=csv").text
    assert c.post("/api/invoices/acme/2026-09/finalize").status_code == 409


def test_finalize_refuses_open_unsealed_mixed_and_tampered(make_client, fake_k8s, ledger_on):
    c = make_client("invoices")
    assert c.post("/api/invoices/none/2026-09/finalize").status_code == 404

    seed(fake_k8s, "open", [usage("o1", "open", 1.0), usage("o2", "open", 1.0, final=False)])
    r = c.post("/api/invoices/open/2026-09/finalize")
    assert r.status_code == 409 and "open usage records" in r.json()["detail"]

    seed(fake_k8s, "lag", [usage("l1", "lag", 1.0, sealed=False)], entries=False)
    r = c.post("/api/invoices/lag/2026-09/finalize")
    assert r.status_code == 409 and "not in the ledger yet" in r.json()["detail"]

    seed(fake_k8s, "mix", [usage("m1", "mix", 1.0), usage("m2", "mix", 1.0, currency="EUR")])
    r = c.post("/api/invoices/mix/2026-09/finalize")
    assert r.status_code == 409 and "one currency" in r.json()["detail"]

    seed(fake_k8s, "bad", [usage("b1", "bad", 1.0), usage("b2", "bad", 2.0)])
    fake_k8s.store[(lg.LEDGER, None, "le-uid-b1")]["spec"]["cost"] = "0.000000"
    r = c.post("/api/invoices/bad/2026-09/finalize")
    assert r.status_code == 409 and "fails verification" in r.json()["detail"]
    assert not [k for k in fake_k8s.store if k[0] == lg.INVOICES]


def test_finalize_admin_only(make_client, fake_k8s, ledger_on):
    seed(fake_k8s, "acme", [usage("a1", "acme", 1.0)])
    tenant = make_client("invoices", role="tenant", tenants=["acme"])
    assert tenant.post("/api/invoices/acme/2026-09/finalize").status_code == 403
    assert tenant.post("/api/invoices/acme/2026-09/void", json={"reason": "nope"}).status_code == 403


def test_void_then_refinalize_takes_next_number(make_client, fake_k8s, ledger_on):
    seed(fake_k8s, "acme", [usage("a1", "acme", 4.0)])
    c = make_client("invoices")
    assert c.post("/api/invoices/acme/2026-09/void", json={"reason": "nothing yet"}).status_code == 404
    c.post("/api/invoices/acme/2026-09/finalize")
    assert c.post("/api/invoices/acme/2026-09/void", json={"reason": ""}).status_code == 422
    v = c.post("/api/invoices/acme/2026-09/void", json={"reason": "wrong rate card"})
    assert v.status_code == 200 and v.json()["status"] == "void" and v.json()["voidReason"] == "wrong rate card"
    assert c.get("/api/invoices/acme/2026-09").json()["status"] == "estimate"

    again = c.post("/api/invoices/acme/2026-09/finalize").json()
    assert again["number"] == "INV-acme-000002" and again["invoice"] == "inv-acme-202609-2"
    assert fake_k8s.store[(lg.INVOICES, None, "inv-acme-202609-1")]["spec"]["state"] == "Void"


def test_paid_cannot_be_voided_and_stale_draft_is_replaced(make_client, fake_k8s, ledger_on):
    seed(fake_k8s, "acme", [usage("a1", "acme", 4.0)])
    fake_k8s.add(lg.INVOICES, {"metadata": {"name": "inv-acme-202609-1"},
                               "spec": {"tenant": "acme", "period": "2026-09", "number": 1, "state": "Draft"}})
    c = make_client("invoices")
    inv = c.post("/api/invoices/acme/2026-09/finalize").json()
    assert inv["invoice"] == "inv-acme-202609-1" and inv["number"] == "INV-acme-000001"
    fake_k8s.store[(lg.INVOICES, None, "inv-acme-202609-1")]["spec"]["state"] = "Paid"
    r = c.post("/api/invoices/acme/2026-09/void", json={"reason": "too late"})
    assert r.status_code == 409


def test_tenant_sees_only_own_persisted_invoice(make_client, fake_k8s, ledger_on):
    seed(fake_k8s, "acme", [usage("a1", "acme", 4.0)])
    seed(fake_k8s, "beta", [usage("b1", "beta", 9.0)])
    admin = make_client("invoices")
    admin.post("/api/invoices/acme/2026-09/finalize")
    admin.post("/api/invoices/beta/2026-09/finalize")
    items = make_client("invoices", role="tenant", tenants=["acme"]).get("/api/invoices?month=2026-09").json()["items"]
    assert [i["tenant"] for i in items] == ["acme"] and items[0]["status"] == "finalized"
