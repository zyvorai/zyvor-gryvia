# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""GPU right-sizing recommendations."""
from datetime import datetime, timedelta, timezone

import pytest

from routers.rightsizing import recommend, rightsizing


def ago(hours):
    return (datetime.now(timezone.utc) - timedelta(hours=hours)).strftime("%Y-%m-%dT%H:%M:%SZ")


def job(name, gpus, util, ns="tenant-alpha", phase="Succeeded", start_h=10, end_h=0, nodes=None):
    spec = {"gpus": gpus, "image": "x"}
    if nodes:
        spec["distributed"] = {"nodes": nodes}
    st = {"phase": phase, "startTime": ago(start_h)}
    if end_h is not None and phase == "Succeeded":
        st["completionTime"] = ago(end_h)
    if util is not None:
        st["metrics"] = {"gpuUtilization": util}
    return {"metadata": {"name": name, "namespace": ns, "creationTimestamp": ago(start_h + 1)}, "spec": spec,
            "status": st}


def test_recommend_rules():
    assert recommend(1, 10, 70) == {"gpus": 1, "gpuShare": 0.25, "freedGpus": 0.75}
    assert recommend(1, 30, 70)["gpuShare"] == 0.5  # need 0.43 of a GPU
    assert recommend(1, 50, 70)["gpuShare"] == 0.75
    assert recommend(1, 60, 70) is None  # needs 0.86: stays whole
    assert recommend(8, 35, 70) == {"gpus": 4, "gpuShare": None, "freedGpus": 4}
    assert recommend(4, 69, 70) is None  # ceil(3.94) = 4: no change
    assert recommend(4, 90, 70) is None and recommend(4, 0, 70) is None and recommend(0, 10, 70) is None


def test_rightsizing_report():
    now = datetime.now(timezone.utc)
    jobs = [job("small", 1, 15), job("big", 8, 25, nodes=2, start_h=4), job("fine", 2, 95),
            job("nodata", 4, None), job("short", 1, 10, start_h=0.1, end_h=0), job("running", 4, 20, phase="Running"),
            job("failed", 4, 10, phase="Failed"), job("old", 1, 10, start_h=24 * 40, end_h=24 * 39)]
    rep = rightsizing(jobs, now - timedelta(days=14), now, target_pct=70, min_hours=0.5)
    by = {i["name"]: i for i in rep["items"]}
    assert set(by) == {"small", "big", "running"}
    assert by["small"]["recommendation"]["gpuShare"] == 0.25 and by["small"]["savedGpuHours"] == 7.5  # 0.75 * 10h
    # 8 GPUs x 2 nodes at 25%: ceil(8*25/70)=3 per node, frees 5 per node over 4h
    assert by["big"]["recommendation"]["gpus"] == 3 and by["big"]["savedGpuHours"] == 40.0
    assert rep["jobsWithTelemetry"] == 5 and rep["oversized"] == 3  # nodata, failed and old are not sampled
    assert [i["name"] for i in rep["items"]] == ["big", "running", "small"]  # most saved GPU-hours first
    assert rep["byTenant"][0]["tenant"] == "alpha"


@pytest.fixture
def seeded(fake_k8s):
    fake_k8s.add("gryviaaijobs", job("a1", 1, 20), "tenant-alpha")
    fake_k8s.add("gryviaaijobs", job("b1", 4, 20, ns="tenant-beta"), "tenant-beta")


def test_endpoint_admin(make_client, seeded):
    c = make_client("rightsizing")
    body = c.get("/api/rightsizing").json()
    assert {i["name"] for i in body["items"]} == {"a1", "b1"} and body["days"] == 14
    assert body["savedGpuHours"] > 0
    assert [i["name"] for i in c.get("/api/rightsizing?tenant=beta").json()["items"]] == ["b1"]
    # a stricter target flags the 25%-ish job less often: at 15% nothing is oversized
    assert c.get("/api/rightsizing?targetUtilPct=15").json()["oversized"] == 0
    assert c.get("/api/rightsizing?limit=1").json()["truncated"] is True


def test_endpoint_tenant_scoped(make_client, seeded):
    c = make_client("rightsizing", role="tenant", tenants=["alpha"])
    assert [i["name"] for i in c.get("/api/rightsizing").json()["items"]] == ["a1"]
    assert c.get("/api/rightsizing?tenant=beta").json()["items"] == []


def test_endpoint_validation(make_client, seeded):
    c = make_client("rightsizing")
    assert c.get("/api/rightsizing?targetUtilPct=0").status_code == 422
    assert c.get("/api/rightsizing?targetUtilPct=101").status_code == 422
    assert c.get("/api/rightsizing?days=0").status_code == 422
    assert c.get("/api/rightsizing?minHours=-1").status_code == 422
