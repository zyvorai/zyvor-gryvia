# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Usage aggregation, tenant scoping and export."""
import csv
import io

import pytest


def rec(name, tenant, sku="a100", hours=2.0, cost=8.0, start="2026-03-01T10:00:00Z", currency="USD", job=None,
        gpu_type="A100-80G"):
    return {"metadata": {"name": name},
            "spec": {"tenant": tenant, "job": job or name, "sku": sku, "gpuType": gpu_type, "gpus": 1,
                     "start": start, "end": start, "gpuHours": hours, "rate": cost / hours if hours else 0,
                     "cost": cost, "currency": currency, "final": True}}


@pytest.fixture
def seeded(fake_k8s):
    add = fake_k8s.add
    add("gryviausagerecords", rec("j1", "alpha", hours=2, cost=8), "tenant-alpha")
    add("gryviausagerecords", rec("j2", "alpha", sku="h100", gpu_type="H100", hours=1, cost=8,
                                  start="2026-03-02T10:00:00Z"), "tenant-alpha")
    add("gryviausagerecords", rec("j3", "beta", hours=10, cost=40, start="2026-03-02T23:00:00Z"), "tenant-beta")
    add("gryviausagerecords", rec("j4", "beta", sku="h100", gpu_type="H100", hours=1, cost=8,
                                  start="2026-04-01T00:00:00Z"), "tenant-beta")


def by_key(resp):
    return {i["key"]: i for i in resp.json()["items"]}


def test_admin_groups_by_tenant_by_default(make_client, seeded):
    body = make_client("usage").get("/api/usage").json()
    assert body["groupBy"] == "tenant"
    items = by_key(make_client("usage").get("/api/usage"))
    assert items["alpha"] == {"key": "alpha", "gpuHours": 3.0, "cost": 16.0, "currency": "USD", "jobs": 2}
    assert items["beta"]["gpuHours"] == 11.0 and items["beta"]["cost"] == 48.0
    assert body["totals"] == {"gpuHours": 14.0, "cost": 64.0, "currency": "USD", "jobs": 4}
    assert [i["key"] for i in body["items"]] == ["beta", "alpha"]  # highest cost first


def test_group_by_sku_and_day(make_client, seeded):
    c = make_client("usage")
    sku = by_key(c.get("/api/usage?groupBy=sku"))
    assert sku["a100"]["cost"] == 48.0 and sku["h100"]["cost"] == 16.0 and sku["h100"]["jobs"] == 2
    day = c.get("/api/usage?groupBy=day").json()["items"]
    assert [d["key"] for d in day] == ["2026-03-01", "2026-03-02", "2026-04-01"]
    assert day[1]["cost"] == 48.0


def test_bad_group_by_and_dates(make_client, seeded):
    c = make_client("usage")
    assert c.get("/api/usage?groupBy=nope").status_code == 422
    assert c.get("/api/usage?from=yesterday").status_code == 400
    assert c.get("/api/usage?to=2026-13-45").status_code == 400


def test_date_range_is_inclusive_of_the_to_day(make_client, seeded):
    c = make_client("usage")
    r = c.get("/api/usage?from=2026-03-02&to=2026-03-02").json()
    assert r["totals"]["jobs"] == 2 and r["totals"]["cost"] == 48.0  # j2 and j3 (23:00 that day)
    assert c.get("/api/usage?to=2026-03-01").json()["totals"]["jobs"] == 1
    assert c.get("/api/usage?from=2026-04-01T00:00:00Z").json()["totals"]["jobs"] == 1


def test_admin_tenant_filter(make_client, seeded):
    r = make_client("usage").get("/api/usage?tenant=beta&groupBy=sku").json()
    assert r["totals"]["jobs"] == 2 and set(by_key_items(r)) == {"a100", "h100"}


def by_key_items(body):
    return {i["key"]: i for i in body["items"]}


def test_tenant_is_forced_to_its_own_tenant(make_client, seeded):
    c = make_client("usage", role="tenant", tenants=["alpha"])
    for q in ("", "?tenant=beta", "?tenant=alpha", "?tenant=*"):
        body = c.get("/api/usage" + q).json()
        assert [i["key"] for i in body["items"]] == ["alpha"], q
        assert body["totals"]["cost"] == 16.0
    exported = c.get("/api/usage/export?format=json&tenant=beta").json()
    assert {r["tenant"] for r in exported["items"]} == {"alpha"}


def test_tenant_without_records_gets_empty(make_client, seeded):
    body = make_client("usage", role="tenant", tenants=["gamma"]).get("/api/usage").json()
    assert body["items"] == [] and body["totals"] == {"gpuHours": 0.0, "cost": 0.0, "currency": "USD", "jobs": 0}


def test_mixed_currency_is_flagged(make_client, fake_k8s):
    fake_k8s.add("gryviausagerecords", rec("a", "t", currency="USD"), "tenant-t")
    fake_k8s.add("gryviausagerecords", rec("b", "t", currency="EUR"), "tenant-t")
    body = make_client("usage").get("/api/usage").json()
    assert body["totals"]["currency"] == "MIXED"


def test_export_csv(make_client, seeded):
    r = make_client("usage").get("/api/usage/export?format=csv")
    assert r.status_code == 200 and r.headers["content-type"].startswith("text/csv")
    assert r.headers["content-disposition"].startswith("attachment; filename=")
    rows = list(csv.DictReader(io.StringIO(r.text)))
    assert len(rows) == 4
    assert list(rows[0]) == ["tenant", "namespace", "job", "sku", "gpuType", "gpus", "start", "end", "gpuHours",
                             "rate", "cost", "currency", "final"]
    assert sum(float(x["cost"]) for x in rows) == 64.0
    assert rows[0]["namespace"] == "tenant-alpha" and rows[0]["job"] == "j1"


def test_export_csv_is_default_and_filtered(make_client, seeded):
    r = make_client("usage").get("/api/usage/export?tenant=alpha&to=2026-03-01")
    rows = list(csv.DictReader(io.StringIO(r.text)))
    assert [x["job"] for x in rows] == ["j1"]


def test_export_json(make_client, seeded):
    r = make_client("usage").get("/api/usage/export?format=json&from=2026-03-02&to=2026-03-31")
    assert r.headers["content-disposition"].startswith("attachment; filename=")
    assert r.headers["content-disposition"].endswith('.json"')
    body = r.json()
    assert [x["job"] for x in body["items"]] == ["j2", "j3"] and body["totals"]["cost"] == 48.0
    assert make_client("usage").get("/api/usage/export?format=xml").status_code == 422


def test_csv_formula_injection_is_neutralised(make_client, fake_k8s):
    fake_k8s.add("gryviausagerecords", rec("a", "=cmd", job="=1+1"), "tenant-t")
    text = make_client("usage").get("/api/usage/export").text
    assert "'=cmd" in text and "'=1+1" in text
