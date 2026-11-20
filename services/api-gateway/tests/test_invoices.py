# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""On-demand invoice estimates from usage records."""
import csv
import io

import pytest


def rec(name, tenant, sku="a100", hours=2.0, cost=8.0, start="2026-03-10T10:00:00Z", currency="USD",
        gpu_type="A100-80G", final=True, rate=None):
    return {"metadata": {"name": name},
            "spec": {"tenant": tenant, "job": name, "sku": sku, "gpuType": gpu_type, "gpus": 1, "start": start,
                     "end": start, "gpuHours": hours, "rate": cost / hours if rate is None else rate,
                     "cost": cost, "currency": currency, "final": final}}


def add(fake_k8s, r, tenant):
    fake_k8s.add("gryviausagerecords", r, f"tenant-{tenant}")


@pytest.fixture
def seeded(fake_k8s):
    add(fake_k8s, rec("a1", "alpha", hours=2, cost=8), "alpha")
    add(fake_k8s, rec("a2", "alpha", hours=1, cost=4, start="2026-03-11T10:00:00Z"), "alpha")
    add(fake_k8s, rec("a3", "alpha", sku="h100", gpu_type="H100", hours=1, cost=8), "alpha")
    add(fake_k8s, rec("b1", "beta", hours=10, cost=40), "beta")


def test_month_bucketing_boundaries(make_client, fake_k8s):
    add(fake_k8s, rec("before", "t", start="2026-02-28T23:59:59Z", cost=1), "t")
    add(fake_k8s, rec("first", "t", start="2026-03-01T00:00:00Z", cost=2), "t")
    add(fake_k8s, rec("last", "t", start="2026-03-31T23:59:59Z", cost=4), "t")
    add(fake_k8s, rec("after", "t", start="2026-04-01T00:00:00Z", cost=8), "t")
    inv = make_client("invoices").get("/api/invoices/t/2026-03").json()
    assert inv["subtotal"] == 6.0 and inv["jobs"] == 2
    assert inv["period"] == {"from": "2026-03-01", "to": "2026-03-31"}
    assert inv["number"] == "INV-t-202603"
    feb = make_client("invoices").get("/api/invoices/t/2026-02").json()
    assert feb["period"]["to"] == "2026-02-28" and feb["subtotal"] == 1.0


def test_shape_and_grouping_by_sku(make_client, seeded):
    inv = make_client("invoices").get("/api/invoices/alpha/2026-03").json()
    assert inv["status"] == "estimate" and inv["currency"] == "USD" and "open" not in inv
    assert inv["note"] == "Estimate from job run time; not a tax invoice." and inv["generatedAt"].endswith("Z")
    assert inv["lines"] == [
        {"sku": "a100", "gpuType": "A100-80G", "gpuHours": 3.0, "rate": 4.0, "amount": 12.0, "jobs": 2,
         "currency": "USD"},
        {"sku": "h100", "gpuType": "H100", "gpuHours": 1.0, "rate": 8.0, "amount": 8.0, "jobs": 1,
         "currency": "USD"}]
    assert inv["subtotal"] == 20.0 and inv["jobs"] == 3


def test_line_falls_back_to_gpu_type_and_blended_rate(make_client, fake_k8s):
    add(fake_k8s, rec("x", "t", sku="", gpu_type="L4", hours=1, cost=1, rate=1), "t")
    add(fake_k8s, rec("y", "t", sku="", gpu_type="L4", hours=2, cost=5, rate=2.5), "t")
    line = make_client("invoices").get("/api/invoices/t/2026-03").json()["lines"][0]
    assert line["sku"] == "L4" and line["rate"] == 2.0 and line["amount"] == 6.0


def test_rounding(make_client, fake_k8s):
    add(fake_k8s, rec("x", "t", hours=0.33333333, cost=1.005, rate=3.0), "t")
    add(fake_k8s, rec("y", "t", hours=0.33333333, cost=1.004, rate=3.5), "t")
    inv = make_client("invoices").get("/api/invoices/t/2026-03").json()
    ln = inv["lines"][0]
    assert ln["gpuHours"] == 0.6667 and ln["amount"] == 2.01 and ln["rate"] == 3.0135
    assert inv["subtotal"] == 2.01


def test_mixed_currency(make_client, fake_k8s):
    add(fake_k8s, rec("x", "t", currency="USD", cost=8), "t")
    add(fake_k8s, rec("y", "t", currency="EUR", cost=4, sku="h100"), "t")
    inv = make_client("invoices").get("/api/invoices/t/2026-03").json()
    assert inv["currency"] == "MIXED" and inv["mixedCurrency"] is True and inv["subtotal"] == 12.0
    assert {ln["currency"] for ln in inv["lines"]} == {"USD", "EUR"}


def test_open_flag(make_client, fake_k8s):
    add(fake_k8s, rec("x", "t"), "t")
    add(fake_k8s, rec("y", "t", final=False), "t")
    inv = make_client("invoices").get("/api/invoices/t/2026-03").json()
    assert inv["open"] is True and inv["status"] == "estimate" and inv["jobs"] == 2


def test_admin_list_and_filter(make_client, seeded):
    c = make_client("invoices")
    body = c.get("/api/invoices?month=2026-03").json()
    assert body["month"] == "2026-03" and [i["tenant"] for i in body["items"]] == ["alpha", "beta"]
    only = c.get("/api/invoices?month=2026-03&tenant=beta").json()["items"]
    assert [i["tenant"] for i in only] == ["beta"]
    assert c.get("/api/invoices?month=2025-01").json()["items"] == []


def test_missing_month_is_current_month(make_client, fake_k8s):
    from datetime import datetime, timezone
    now = datetime.now(timezone.utc)
    add(fake_k8s, rec("x", "t", start=now.strftime("%Y-%m-%dT00:00:00Z")), "t")
    body = make_client("invoices").get("/api/invoices").json()
    assert body["month"] == now.strftime("%Y-%m") and len(body["items"]) == 1


def test_tenant_isolation(make_client, seeded):
    c = make_client("invoices", role="tenant", tenants=["alpha"])
    for q in ("", "&tenant=beta", "&tenant=*"):
        items = c.get("/api/invoices?month=2026-03" + q).json()["items"]
        assert [i["tenant"] for i in items] == ["alpha"], q
    assert c.get("/api/invoices/alpha/2026-03").status_code == 200
    assert c.get("/api/invoices/beta/2026-03").status_code == 404
    assert c.get("/api/invoices/beta/2026-03?format=csv").status_code == 404


def test_bad_month_and_not_found(make_client, seeded):
    c = make_client("invoices")
    for bad in ("2026-13", "2026-3", "March", "2026-00"):
        assert c.get(f"/api/invoices?month={bad}").status_code == 400, bad
        assert c.get(f"/api/invoices/alpha/{bad}").status_code == 400, bad
    assert c.get("/api/invoices/alpha/2025-01").status_code == 404
    assert c.get("/api/invoices/nobody/2026-03").status_code == 404
    assert c.get("/api/invoices/alpha/2026-03?format=xml").status_code == 422


def test_csv(make_client, seeded):
    r = make_client("invoices").get("/api/invoices/alpha/2026-03?format=csv")
    assert r.headers["content-disposition"] == 'attachment; filename="INV-alpha-202603.csv"'
    assert r.headers["content-type"].startswith("text/csv")
    rows = list(csv.reader(io.StringIO(r.text)))
    assert rows[0] == ["invoice", "tenant", "period_from", "period_to", "sku", "gpuType", "jobs", "gpuHours",
                       "rate", "amount", "currency"]
    assert rows[1] == ["INV-alpha-202603", "alpha", "2026-03-01", "2026-03-31", "a100", "A100-80G", "2", "3.0",
                       "4.0", "12.0", "USD"]
    assert rows[-1][4] == "TOTAL" and rows[-1][6] == "3" and rows[-1][7] == "4.0" and rows[-1][9] == "20.0"
    assert len(rows) == 4


def test_csv_formula_hardening(make_client, fake_k8s):
    add(fake_k8s, rec("x", "t", sku="=HYPERLINK(1)", gpu_type="@cmd"), "t")
    rows = list(csv.reader(io.StringIO(make_client("invoices").get("/api/invoices/t/2026-03?format=csv").text)))
    assert rows[1][4] == "'=HYPERLINK(1)" and rows[1][5] == "'@cmd"
