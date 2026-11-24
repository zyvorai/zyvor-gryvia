# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""GPU waste report: allocated vs measured utilization."""
import csv
import io

import pytest

from routers.waste import compute_waste


def rec(name, tenant, hours, cost, start="2026-03-01T10:00:00Z"):
    return {"metadata": {"name": name},
            "spec": {"tenant": tenant, "job": name, "sku": "a100", "gpus": 1, "start": start, "end": start,
                     "gpuHours": hours, "cost": cost, "currency": "USD", "final": True}}


def _tenant_rec(name, tenant, hours, cost):
    r = rec(name, tenant, hours, cost)
    r["metadata"]["namespace"] = f"tenant-{tenant}"
    return r


def test_waste_math_and_ordering():
    recs = [_tenant_rec("a1", "alpha", 10, 40), _tenant_rec("b1", "beta", 10, 40)]
    out = compute_waste(recs, {"tenant-alpha": 80, "tenant-beta": 20})
    by = {i["tenant"]: i for i in out["items"]}
    assert by["alpha"]["utilizationPct"] == 80.0 and by["alpha"]["wastedGpuHours"] == 2.0
    assert by["alpha"]["wastedCost"] == 8.0 and by["alpha"]["verdict"] == "ok"
    assert by["beta"]["wastedCost"] == 32.0 and by["beta"]["verdict"] == "underused"
    assert [i["tenant"] for i in out["items"]] == ["beta", "alpha"]  # most waste first
    t = out["totals"]
    assert t["utilizationPct"] == 50.0 and t["wastedGpuHours"] == 10.0 and t["wastedCost"] == 40.0


def test_no_telemetry_is_not_counted_as_waste():
    out = compute_waste([_tenant_rec("a1", "alpha", 4, 16), _tenant_rec("b1", "beta", 4, 16)], {"tenant-alpha": 50})
    by = {i["tenant"]: i for i in out["items"]}
    assert by["beta"]["verdict"] == "noTelemetry" and by["beta"]["wastedCost"] is None
    assert out["totals"]["gpuHours"] == 8 and out["totals"]["measuredGpuHours"] == 4
    assert out["totals"]["wastedCost"] == 8.0


def test_hours_weighted_mean_and_clamping():
    recs = [_tenant_rec("a1", "alpha", 1, 4), _tenant_rec("a2", "alpha", 3, 12)]
    assert compute_waste(recs, {"tenant-alpha": 100})["items"][0]["utilizationPct"] == 100.0
    # out-of-range and junk values: clamped or ignored, never crash
    assert compute_waste(recs, {"tenant-alpha": 250})["items"][0]["utilizationPct"] == 100.0
    assert compute_waste(recs, {"tenant-alpha": -5})["items"][0]["utilizationPct"] == 0.0
    assert compute_waste(recs, {"tenant-alpha": float("nan")})["items"][0]["verdict"] == "noTelemetry"
    assert compute_waste(recs, {"tenant-alpha": True})["items"][0]["verdict"] == "noTelemetry"


def test_empty():
    out = compute_waste([], None)
    assert out["items"] == [] and out["telemetry"] is False and out["totals"]["wastedCost"] is None


@pytest.fixture
def seeded(fake_k8s):
    from datetime import datetime, timezone
    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    fake_k8s.add("gryviausagerecords", rec("a1", "alpha", 10, 40, now), "tenant-alpha")
    fake_k8s.add("gryviausagerecords", rec("b1", "beta", 10, 40, now), "tenant-beta")
    fake_k8s.add("gryviausagerecords", rec("old", "beta", 100, 400, "2020-01-01T00:00:00Z"), "tenant-beta")


def test_endpoint_admin_with_telemetry(make_client, seeded):
    async def fetch(days):
        assert days == 7
        return {"tenant-alpha": 90, "tenant-beta": 10}
    body = make_client("waste", gpu_util_fetch=fetch).get("/api/waste").json()
    assert body["telemetry"] and body["days"] == 7
    by = {i["tenant"]: i for i in body["items"]}
    assert by["beta"]["gpuHours"] == 10 and by["beta"]["verdict"] == "underused"  # old record outside the window


def test_endpoint_without_or_failing_telemetry(make_client, seeded):
    assert make_client("waste").get("/api/waste").json()["telemetry"] is False

    async def boom(days):
        raise RuntimeError("prometheus down")
    r = make_client("waste", gpu_util_fetch=boom).get("/api/waste")
    assert r.status_code == 200 and r.json()["telemetry"] is False


def test_endpoint_tenant_scoped(make_client, seeded):
    async def fetch(days):
        return {"tenant-alpha": 50, "tenant-beta": 50}
    body = make_client("waste", role="tenant", tenants=["alpha"], gpu_util_fetch=fetch).get(
        "/api/waste?tenant=beta").json()
    assert [i["tenant"] for i in body["items"]] == ["alpha"]


def test_endpoint_csv_and_validation(make_client, seeded):
    async def fetch(days):
        return {"tenant-alpha": 50}
    c = make_client("waste", gpu_util_fetch=fetch)
    r = c.get("/api/waste?format=csv")
    assert r.headers["content-type"].startswith("text/csv")
    rows = list(csv.DictReader(io.StringIO(r.text)))
    assert {x["tenant"]: x["verdict"] for x in rows} == {"alpha": "ok", "beta": "noTelemetry"}
    assert c.get("/api/waste?days=0").status_code == 422
    assert c.get("/api/waste?lowUtilPct=101").status_code == 422
    assert c.get("/api/waste?format=xml").status_code == 422
