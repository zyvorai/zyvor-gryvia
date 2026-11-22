# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Network usage records, invoice network lines, reconcile verdicts and scoping."""
import csv
import io

import pytest

GB = 1_000_000_000


def nrec(name, tenant, peer="external", zone="internet", egress=GB, ingress=0, hour="2026-03-10T10:00:00Z",
         node="n1", final=True, source=None):
    spec = {"tenant": tenant, "node": node, "hour": hour, "peerClass": peer, "zoneClass": zone,
            "egressBytes": egress, "ingressBytes": ingress, "final": final}
    if source:
        spec["source"] = source
    return {"metadata": {"name": name}, "spec": spec}


def add(fake_k8s, r, tenant):
    fake_k8s.add("gryvianetworkusagerecords", r, f"tenant-{tenant}")


def rate(fake_k8s, same=0.01, cross=0.02, net=0.09, cur="USD", name="default"):
    fake_k8s.add("gryvianetworkrates", {"metadata": {"name": name},
                                        "spec": {"sameZone": same, "crossZone": cross, "internetEgress": net,
                                                 "currency": cur}})


@pytest.fixture
def seeded(fake_k8s):
    add(fake_k8s, nrec("a1", "alpha", egress=2 * GB, ingress=5 * GB), "alpha")
    add(fake_k8s, nrec("a2", "alpha", peer="same-tenant", zone="cross-zone", egress=10 * GB, node="n2"), "alpha")
    add(fake_k8s, nrec("a3", "alpha", peer="cluster", zone="same-zone", egress=4 * GB,
                       hour="2026-03-11T00:00:00Z", final=False), "alpha")
    add(fake_k8s, nrec("a4", "alpha", peer="unknown", zone="unknown-zone", egress=7 * GB), "alpha")
    add(fake_k8s, nrec("b1", "beta", egress=GB), "beta")
    add(fake_k8s, nrec("b2", "beta", egress=GB, hour="2026-04-01T00:00:00Z"), "beta")


def test_usage_groups_and_prices_egress_only(make_client, fake_k8s, seeded):
    rate(fake_k8s)
    body = make_client("netusage").get("/api/network/usage?tenant=alpha&groupBy=zoneClass").json()
    assert body["billing"] == "egress-only" and body["source"] == "collector"
    items = {i["key"]: i for i in body["items"]}
    assert items["internet"]["egressBytes"] == 2 * GB and items["internet"]["ingressBytes"] == 5 * GB
    assert items["internet"]["cost"] == pytest.approx(0.18)
    assert items["cross-zone"]["cost"] == pytest.approx(0.2)
    assert items["unknown-zone"]["cost"] == 0  # unpriced class
    assert body["totals"]["egressBytes"] == 23 * GB and body["totals"]["open"] is True
    assert body["totals"]["cost"] == pytest.approx(0.18 + 0.2 + 0.04)


def test_usage_without_rates_has_bytes_only(make_client, seeded):
    body = make_client("netusage").get("/api/network/usage").json()
    assert body["rates"] is None and "cost" not in body["totals"] and "cost" not in body["items"][0]


def test_usage_date_range_and_day_grouping(make_client, seeded):
    c = make_client("netusage")
    d = c.get("/api/network/usage?groupBy=day&from=2026-03-01&to=2026-03-31").json()
    assert [i["key"] for i in d["items"]] == ["2026-03-10", "2026-03-11"]
    assert c.get("/api/network/usage?from=nope").status_code == 400
    assert c.get("/api/network/usage?groupBy=sku").status_code == 422


def test_one_source_per_request_never_summed(make_client, fake_k8s):
    add(fake_k8s, nrec("c", "alpha", egress=GB), "alpha")
    add(fake_k8s, nrec("n", "alpha", egress=GB, source="netra", node="netra"), "alpha")
    c = make_client("netusage")
    assert c.get("/api/network/usage").json()["totals"]["egressBytes"] == GB
    assert c.get("/api/network/usage?source=netra").json()["totals"]["egressBytes"] == GB


def test_duplicate_fragment_counted_once(make_client, fake_k8s):
    add(fake_k8s, nrec("x1", "alpha", egress=GB), "alpha")
    add(fake_k8s, nrec("x2", "alpha", egress=3 * GB), "alpha")  # same (node, hour, peer, zone) key
    add(fake_k8s, nrec("x3", "alpha", egress=GB, node="n2"), "alpha")  # another node: a real fragment
    assert make_client("netusage").get("/api/network/usage").json()["totals"]["egressBytes"] == 4 * GB


def test_tenant_user_scoped_to_own_namespace(make_client, seeded):
    body = make_client("netusage", role="tenant", tenants=["beta"]).get("/api/network/usage?tenant=alpha").json()
    assert {i["key"] for i in body["items"]} == {"beta"}
    assert body["totals"]["egressBytes"] == 2 * GB


# ---- invoices -------------------------------------------------------------------------------------------

def test_invoice_network_lines_math(make_client, fake_k8s, seeded):
    rate(fake_k8s)
    inv = make_client("invoices").get("/api/invoices/alpha/2026-03").json()
    assert inv["status"] == "estimate" and inv["subtotal"] == 0  # GPU part untouched: no GPU usage
    lines = {(ln["peerClass"], ln["zoneClass"]): ln for ln in inv["networkLines"]}
    assert lines[("same-tenant", "cross-zone")]["egressGB"] == 10 and lines[("same-tenant", "cross-zone")]["amount"] == 0.2
    assert lines[("external", "internet")]["amount"] == 0.18
    assert lines[("cluster", "same-zone")]["amount"] == 0.04
    unpriced = lines[("unknown", "unknown-zone")]
    assert unpriced["priced"] is False and unpriced["amount"] == 0 and unpriced["rate"] is None
    assert inv["networkSubtotal"] == 0.42 and inv["open"] is True and inv["currency"] == "USD"
    assert "not a tax invoice" in inv["networkNote"]


def test_invoice_without_rates_or_records_has_no_network_section(make_client, fake_k8s, seeded):
    r = make_client("invoices").get("/api/invoices/alpha/2026-03")
    assert r.status_code == 404  # no GPU records and no rates: nothing to invoice
    rate(fake_k8s)
    fake_k8s.add("gryviausagerecords", {"metadata": {"name": "j"}, "spec": {
        "tenant": "gamma", "job": "j", "sku": "a100", "gpuType": "A100", "gpus": 1, "start": "2026-03-10T10:00:00Z",
        "gpuHours": 1, "rate": 4, "cost": 4, "currency": "USD", "final": True}}, "tenant-gamma")
    inv = make_client("invoices").get("/api/invoices/gamma/2026-03").json()
    assert "networkLines" not in inv and inv["subtotal"] == 4.0


def test_invoice_gpu_and_network_are_separate(make_client, fake_k8s):
    rate(fake_k8s)
    add(fake_k8s, nrec("a1", "alpha", egress=GB), "alpha")
    fake_k8s.add("gryviausagerecords", {"metadata": {"name": "j"}, "spec": {
        "tenant": "alpha", "job": "j", "sku": "a100", "gpuType": "A100", "gpus": 1, "start": "2026-03-10T10:00:00Z",
        "gpuHours": 2, "rate": 4, "cost": 8, "currency": "USD", "final": True}}, "tenant-alpha")
    c = make_client("invoices")
    inv = c.get("/api/invoices/alpha/2026-03").json()
    assert inv["subtotal"] == 8.0 and inv["networkSubtotal"] == 0.09 and len(inv["lines"]) == 1
    rows = list(csv.reader(io.StringIO(c.get("/api/invoices/alpha/2026-03?format=csv").text)))
    assert rows[-3][4] == "TOTAL" and rows[-3][9] == "8.0"
    assert rows[-2][4] == "network:external/internet" and rows[-2][9] == "0.09"
    assert rows[-1][4] == "NETWORK-SUBTOTAL"


def test_invoice_scoping_hides_other_tenants_network(make_client, fake_k8s, seeded):
    rate(fake_k8s)
    c = make_client("invoices", role="tenant", tenants=["beta"])
    assert c.get("/api/invoices/alpha/2026-03").status_code == 404
    inv = c.get("/api/invoices/beta/2026-03").json()
    assert inv["networkSubtotal"] == 0.09


# ---- reconcile ------------------------------------------------------------------------------------------

def netra(bytes_, ns="tenant-alpha", direction="egress", at="2026-03-10T11:00:00Z"):
    return {"namespace": ns, "pod": "p", "peer": "1.1.1.1", "bytes": bytes_, "direction": direction, "observedAt": at}


def make(make_client, records, **kw):
    async def fetch():
        return records
    return make_client("netusage", netra_fetch=fetch, **kw)


def test_reconcile_within_tolerance(make_client, fake_k8s):
    add(fake_k8s, nrec("a", "alpha", egress=100 * GB), "alpha")
    body = make(make_client, [netra(95 * GB), netra(GB, ns="tenant-beta"), netra(50 * GB, direction="ingress"),
                              netra(9 * GB, at="2026-04-02T00:00:00Z")]).get(
        "/api/network/reconcile?tenant=alpha&month=2026-03").json()
    assert body["verdict"] == "within-tolerance" and body["deltaBytes"] == 5 * GB
    assert body["percent"] == 5.0 and body["tolerancePercent"] == 10.0 and body["netraRecords"] == 1


def test_reconcile_investigate_and_tolerance_override(make_client, fake_k8s):
    add(fake_k8s, nrec("a", "alpha", egress=100 * GB), "alpha")
    c = make(make_client, [netra(50 * GB)])
    assert c.get("/api/network/reconcile?tenant=alpha&month=2026-03").json()["verdict"] == "investigate"
    assert c.get("/api/network/reconcile?tenant=alpha&month=2026-03&tolerancePct=60").json()["verdict"] == "within-tolerance"
    assert c.get("/api/network/reconcile?tenant=alpha&month=2026-03&tolerancePct=500").status_code == 422


def test_reconcile_insufficient_data(make_client, fake_k8s):
    add(fake_k8s, nrec("a", "alpha", egress=GB), "alpha")
    # Netra not configured
    b = make_client("netusage").get("/api/network/reconcile?tenant=alpha&month=2026-03").json()
    assert b["verdict"] == "insufficient-data" and b["deltaBytes"] is None and b["percent"] is None
    # no collector records in the month
    b = make(make_client, [netra(GB)]).get("/api/network/reconcile?tenant=alpha&month=2026-02").json()
    assert b["verdict"] == "insufficient-data" and "collector" in b["reason"]
    # no Netra egress in the period
    b = make(make_client, [netra(GB, direction="ingress")]).get("/api/network/reconcile?tenant=alpha&month=2026-03").json()
    assert b["verdict"] == "insufficient-data" and "Netra" in b["reason"]


def test_reconcile_scoping(make_client, fake_k8s):
    add(fake_k8s, nrec("a", "alpha", egress=GB), "alpha")
    add(fake_k8s, nrec("b", "beta", egress=GB), "beta")
    recs = [netra(GB), netra(GB, ns="tenant-beta")]
    assert make(make_client, recs).get("/api/network/reconcile?month=2026-03").status_code == 400  # admin needs tenant
    t = make(make_client, recs, role="tenant", tenants=["beta"])
    assert t.get("/api/network/reconcile?tenant=alpha&month=2026-03").status_code == 404
    b = t.get("/api/network/reconcile?month=2026-03").json()
    assert b["tenant"] == "beta" and b["collectorEgressBytes"] == GB and b["netraEgressBytes"] == GB
    assert b["verdict"] == "within-tolerance" and b["percent"] == 0.0
    assert make(make_client, recs).get("/api/network/reconcile?tenant=alpha&month=bad").status_code == 400


def test_reconcile_function_edges():
    from routers.netusage import reconcile
    assert reconcile(10, 0, 10, 1, 1)["verdict"] == "investigate"
    assert reconcile(0, 0, 10, 1, 1)["percent"] == 0.0
    assert reconcile(10, 10, 10, 1, 5000, netra_truncated=True)["verdict"] == "insufficient-data"
