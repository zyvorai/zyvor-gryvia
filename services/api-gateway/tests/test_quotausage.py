# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Tenant quota utilization."""
import pytest

from routers.quotausage import quota_usage


def tenant(name, gpus=None):
    spec = {"displayName": name}
    if gpus is not None:
        spec["quotas"] = {"concurrentGPUs": gpus}
    return {"metadata": {"name": name}, "spec": spec}


def job(name, ns, gpus, phase, nodes=None, started=False):
    spec = {"gpus": gpus, "image": "x"}
    if nodes:
        spec["distributed"] = {"nodes": nodes}
    st = {"phase": phase}
    if started:
        st["startTime"] = "2026-01-01T00:00:00Z"
    return {"metadata": {"name": name, "namespace": ns}, "spec": spec, "status": st}


def test_usage_math_flags_and_order():
    tenants = [tenant("alpha", 8), tenant("beta", 4), tenant("gamma")]
    jobs = [job("a1", "tenant-alpha", 4, "Running"), job("a2", "tenant-alpha", 2, "Running", nodes=2),
            job("a3", "tenant-alpha", 1, "Queued"),
            job("b1", "tenant-beta", 2, "Running"), job("b2", "tenant-beta", 2, "Pending"),
            job("g1", "tenant-gamma", 1, "Running"), job("done", "tenant-gamma", 8, "Succeeded", started=True),
            job("x1", "tenant-ghost", 1, "Running")]
    out = quota_usage(tenants, jobs)
    by = {r["tenant"]: r for r in out["items"]}
    assert by["alpha"]["gpusInUse"] == 8 and by["alpha"]["utilizationPct"] == 100.0  # 4 + 2*2
    assert by["alpha"]["blocked"] is True and by["alpha"]["headroomGpus"] == 0 and by["alpha"]["gpusWaiting"] == 1
    assert by["beta"]["utilizationPct"] == 50.0 and by["beta"]["blocked"] is False
    assert by["gamma"]["quotaGpus"] is None and by["gamma"]["utilizationPct"] is None and by["gamma"]["gpusInUse"] == 1
    assert by["ghost"]["registered"] is False
    assert [r["tenant"] for r in out["items"]] == ["alpha", "beta", "gamma", "ghost"]
    assert out["totals"] == {"quotaGpus": 12, "gpusInUse": 12, "gpusWaiting": 3, "blockedTenants": 1}


def test_borrowing_over_quota():
    out = quota_usage([tenant("alpha", 2)], [job("a", "tenant-alpha", 3, "Running")])
    r = out["items"][0]
    assert r["borrowing"] is True and r["utilizationPct"] == 150.0 and r["headroomGpus"] == 0


@pytest.fixture
def seeded(fake_k8s):
    fake_k8s.add("gryviatenants", tenant("alpha", 2))
    fake_k8s.add("gryviatenants", tenant("beta", 4))
    fake_k8s.add("gryviaaijobs", job("a1", "tenant-alpha", 2, "Running"), "tenant-alpha")
    fake_k8s.add("gryviaaijobs", job("a2", "tenant-alpha", 1, "Queued"), "tenant-alpha")
    fake_k8s.add("gryviaaijobs", job("b1", "tenant-beta", 1, "Running"), "tenant-beta")


def test_endpoint_admin(make_client, seeded):
    body = make_client("quotausage").get("/api/quota-usage").json()
    assert [r["tenant"] for r in body["items"]] == ["alpha", "beta"]
    assert body["items"][0]["blocked"] is True and body["totals"]["blockedTenants"] == 1


def test_endpoint_tenant_sees_only_itself(make_client, seeded):
    body = make_client("quotausage", role="tenant", tenants=["beta"]).get("/api/quota-usage").json()
    assert [r["tenant"] for r in body["items"]] == ["beta"]
    assert body["items"][0]["gpusInUse"] == 1 and body["totals"]["quotaGpus"] == 4
