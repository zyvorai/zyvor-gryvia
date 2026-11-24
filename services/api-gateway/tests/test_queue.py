# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Queue positions and wait-time statistics."""
from datetime import datetime, timedelta, timezone

import pytest

from routers.queue import waiting_order, wait_stats

NOW = datetime.now(timezone.utc).replace(microsecond=0)


def ts(minutes_ago):
    return (NOW - timedelta(minutes=minutes_ago)).strftime("%Y-%m-%dT%H:%M:%SZ")


def job(name, ns="tenant-alpha", *, created=0, phase="Queued", gpus=1, prio=0, queue=None, started=None,
        nodes=None, message=None, category=None):
    spec = {"type": "training", "image": "x", "gpus": gpus, "priority": prio}
    if queue:
        spec["queueName"] = queue
    if nodes:
        spec["distributed"] = {"nodes": nodes}
    status = {"phase": phase}
    if started is not None:
        status["startTime"] = ts(started)
    if message:
        status["message"] = message
    if category:
        status["queueExplanation"] = {"category": category}
    return {"metadata": {"name": name, "namespace": ns, "creationTimestamp": ts(created)}, "spec": spec,
            "status": status}


def test_order_priority_then_age_and_positions():
    jobs = [job("old-low", created=30), job("new-high", created=5, prio=10), job("mid", created=20, queue="gpu-b"),
            job("beta", "tenant-beta", created=25, gpus=4, nodes=2), job("run", phase="Running", started=10)]
    rows = waiting_order(jobs, NOW)
    assert [r["name"] for r in rows] == ["new-high", "old-low", "beta", "mid"]  # 'run' is not waiting
    by = {r["name"]: r for r in rows}
    assert by["new-high"]["position"] == 0 and by["old-low"]["position"] == 1
    assert by["old-low"]["positionInQueue"] == 1 and by["mid"]["positionInQueue"] == 0  # queue gpu-b is its own line
    assert by["old-low"]["positionInTenant"] == 1 and by["beta"]["positionInTenant"] == 0
    assert by["beta"]["gpus"] == 8 and by["mid"]["gpusAhead"] == 10  # 1 + 1 + 8
    assert by["old-low"]["waitingSeconds"] == 30 * 60


def test_reason_prefers_queue_explanation():
    rows = waiting_order([job("a", category="QuotaExhausted", message="Waiting for Kueue"),
                          job("b", message="No nodes")], NOW)
    assert {r["name"]: r["reason"] for r in rows} == {"a": "QuotaExhausted", "b": "No nodes"}


def test_wait_stats_percentiles_and_window():
    jobs = [job(f"j{i}", phase="Succeeded", created=100, started=100 - w) for i, w in enumerate([1, 2, 3, 4, 10])]
    jobs.append(job("other", "tenant-beta", phase="Running", created=60, started=30, queue="gpu-b"))
    jobs.append(job("outside", phase="Succeeded", created=9000, started=8990))  # started long before the window
    jobs.append(job("waiting"))  # no start time: not counted
    s = wait_stats(jobs, NOW - timedelta(hours=3), NOW)
    assert s["overall"]["started"] == 6
    alpha = next(r for r in s["byTenant"] if r["tenant"] == "alpha")
    assert alpha["started"] == 5 and alpha["p50Seconds"] == 180 and alpha["p90Seconds"] == 600
    assert alpha["maxSeconds"] == 600
    assert [r["tenant"] for r in s["byTenant"]] == ["beta", "alpha"]  # slowest p90 first (30 min)
    assert {r["queue"] for r in s["byQueue"]} == {"default", "gpu-b"}


@pytest.fixture
def seeded(fake_k8s):
    fake_k8s.add("gryviaaijobs", job("a-low", created=30), "tenant-alpha")
    fake_k8s.add("gryviaaijobs", job("b-high", "tenant-beta", created=10, prio=5, gpus=2), "tenant-beta")
    fake_k8s.add("gryviaaijobs", job("a-run", phase="Running", created=40, started=35), "tenant-alpha")
    fake_k8s.add("gryviaaijobs", job("b-done", "tenant-beta", phase="Succeeded", created=60, started=50), "tenant-beta")


def test_endpoint_admin(make_client, seeded):
    body = make_client("queue").get("/api/queue").json()
    assert body["waiting"] == 2 and body["gpusWaiting"] == 3
    assert [i["name"] for i in body["items"]] == ["b-high", "a-low"]
    assert body["oldestWaitSeconds"] >= 30 * 60
    assert make_client("queue").get("/api/queue?queue=nope").json()["items"] == []
    assert [i["name"] for i in make_client("queue").get("/api/queue?tenant=alpha").json()["items"]] == ["a-low"]
    assert make_client("queue").get("/api/queue?limit=1").json()["truncated"] is True


def test_endpoint_tenant_sees_own_jobs_with_cluster_position(make_client, seeded):
    body = make_client("queue", role="tenant", tenants=["alpha"]).get("/api/queue").json()
    assert [i["name"] for i in body["items"]] == ["a-low"]
    assert body["items"][0]["position"] == 1  # behind beta's higher-priority job, which is not listed
    assert body["items"][0]["gpusAhead"] == 2


def test_endpoint_validation(make_client, seeded):
    c = make_client("queue")
    assert c.get("/api/queue?limit=0").status_code == 422
    assert c.get("/api/queue/stats?days=0").status_code == 422
    assert c.get("/api/queue/stats?days=91").status_code == 422


def test_stats_endpoint_scoped(make_client, seeded):
    admin = make_client("queue").get("/api/queue/stats?days=1").json()
    assert admin["days"] == 1 and {r["tenant"] for r in admin["byTenant"]} == {"alpha", "beta"}
    mine = make_client("queue", role="tenant", tenants=["alpha"]).get("/api/queue/stats").json()
    assert [r["tenant"] for r in mine["byTenant"]] == ["alpha"]
