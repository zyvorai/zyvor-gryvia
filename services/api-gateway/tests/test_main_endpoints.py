# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Tests for the aggregate/list endpoints defined in main.py, run against the fake Kubernetes client.

main.py loads a kubeconfig at import time, so the loaders are stubbed before importing it.
"""
import os
import sys
from datetime import datetime, timedelta, timezone

import pytest
from fastapi import Request
from fastapi.testclient import TestClient

sys.path.insert(0, os.path.dirname(os.path.dirname(__file__)))


@pytest.fixture
def gw(fake_k8s, monkeypatch):
    from kubernetes import config
    monkeypatch.setattr(config, "load_incluster_config", lambda: None)
    monkeypatch.setattr(config, "load_kube_config", lambda: None)
    import main

    async def allow(request: Request):
        request.state.role = "admin"
        request.state.tenant_namespaces = None

    monkeypatch.setattr(main, "k8s_custom", fake_k8s)
    main.limiter.enabled = False
    main.app.dependency_overrides[main.verify_auth] = allow
    yield TestClient(main.app), fake_k8s, main
    main.app.dependency_overrides.clear()
    main.limiter.enabled = True


def job(name, phase, ns="default", gpus=1, hours=2, labels=None, gpu_type="T4"):
    start = datetime.now(timezone.utc) - timedelta(hours=hours)
    md = {"name": name}
    if labels:
        md["labels"] = labels
    return {"metadata": md, "spec": {"gpus": gpus, "gpuType": gpu_type},
            "status": {"phase": phase, "startTime": start.isoformat(),
                       "completionTime": datetime.now(timezone.utc).isoformat()}}


def test_cluster_stats_phases_case_insensitive(gw):
    c, k, _ = gw
    for i, ph in enumerate(["Running", "running", "Succeeded", "Completed", "Scheduling", "Queued",
                            "Pending", "failed", "Weird"]):
        k.add("gryviaaijobs", job(f"j{i}", ph), "default")
    s = c.get("/api/cluster/stats").json()
    assert (s["runningJobs"], s["completedJobs"], s["pendingJobs"], s["failedJobs"]) == (2, 2, 3, 1)
    assert s["totalJobs"] == 9
    assert set(s) == {"totalGPUs", "availableGPUs", "allocatedGPUs", "utilizationPercent", "totalJobs",
                      "runningJobs", "pendingJobs", "completedJobs", "failedJobs", "totalNodes", "timestamp"}


def test_costs_scope_and_team_grouping(gw):
    c, k, _ = gw
    k.add("gryviaquotas", {"metadata": {"name": "q"}, "spec": {"team": "research", "namespaces": ["ml", "default"]}})
    k.add("gryviaaijobs", job("a", "Succeeded", labels={"gryvia.io/team": "ignored"}), "default")
    r = c.get("/api/metrics/costs").json()
    assert r["scope"] == "all-time"
    assert [t["team"] for t in r["byTeam"]] == ["research"]
    assert r["byTeam"][0]["cost"] > 0
    assert {"monthly", "hasHistoricalData", "byTeam", "byGPUType", "totalCost", "timestamp"} <= set(r)


def test_costs_label_fallback_then_unassigned(gw):
    c, k, _ = gw
    k.add("gryviaaijobs", job("a", "Running", labels={"gryvia.io/team": "vision"}), "default")
    k.add("gryviaaijobs", job("b", "completed"), "default")
    k.add("gryviaaijobs", job("c", "Pending"), "default")  # not billable
    teams = {t["team"] for t in c.get("/api/metrics/costs").json()["byTeam"]}
    assert teams == {"vision", "unassigned"}


@pytest.mark.parametrize("path,plural,cluster", [
    ("/api/jobs", "gryviaaijobs", False), ("/api/quotas", "gryviaquotas", True),
    ("/api/nodes", "gryviagpunodes", True)])
def test_list_defaults_and_pagination(gw, path, plural, cluster):
    c, k, _ = gw
    for i in range(150):
        k.add(plural, {"metadata": {"name": f"o{i:03d}"}}, None if cluster else "default")
    body = c.get(path).json()
    assert (body["total"], body["limit"], body["offset"], len(body["items"])) == (150, 500, 0, 150)
    body = c.get(path, params={"limit": 10, "offset": 145}).json()
    assert (body["total"], body["limit"], body["offset"], len(body["items"])) == (150, 10, 145, 5)
    assert c.get(path, params={"limit": 1000}).status_code == 200
    assert c.get(path, params={"limit": 1001}).status_code == 422


def test_node_health_is_not_shadowed_by_the_node_name_route(gw):
    # /api/nodes/health must reach the health handler; registered after /api/nodes/{name} it answered 404
    # "Node 'health' not found".
    c, k, _ = gw
    k.add("gryviagpunodes", {"metadata": {"name": "n1"}, "spec": {}, "status": {"phase": "Ready"}}, None)
    r = c.get("/api/nodes/health")
    assert r.status_code == 200, r.text
    assert c.get("/api/nodes/n1").status_code == 200
    assert c.get("/api/nodes/missing").status_code == 404
