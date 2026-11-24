# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Failure classification and the /api/failures report."""
import csv
import io
from datetime import datetime, timedelta, timezone

import pytest

from routers.failures import classify, failure_report


def ago(hours):
    return (datetime.now(timezone.utc) - timedelta(hours=hours)).strftime("%Y-%m-%dT%H:%M:%SZ")


def failed(name, msg="", phase="Failed", ns="tenant-alpha", hours=1, conditions=None, retries=0, gpus=1):
    return {"metadata": {"name": name, "namespace": ns, "creationTimestamp": ago(hours + 1)},
            "spec": {"gpus": gpus, "image": "x"},
            "status": {"phase": phase, "message": msg, "completionTime": ago(hours), "retries": retries,
                       "conditions": conditions or []}}


@pytest.mark.parametrize("msg,phase,cat,transient", [
    ("container trainer terminated: OOMKilled", "Failed", "oom", False),
    ("CUDA out of memory. Tried to allocate 2 GiB", "Failed", "oom", False),
    ("pod stuck in ImagePullBackOff", "Failed", "image-pull", True),
    ("NodeLost: node gpu-3 is NotReady", "Failed", "node-failure", True),
    ("Evicted by Kueue (Preempted), requeued", "Failed", "preempted", True),
    ("anything", "Preempted", "preempted", True),
    ("Job was active longer than specified deadline: DeadlineExceeded", "Failed", "deadline", False),
    ("exceeds tenant quota", "Failed", "quota", False),
    ("something odd", "Rejected", "quota", False),
    ("0/4 nodes are available: Unschedulable", "Failed", "scheduling", False),
    ("BackoffLimitExceeded", "Failed", "app-error", False),
    ("", "Failed", "unknown", False),
])
def test_classify(msg, phase, cat, transient):
    assert classify(failed("j", msg, phase)) == {"category": cat, "transient": transient}


def test_condition_reason_is_used():
    j = failed("j", "", conditions=[{"type": "Ready", "status": "False", "reason": "OOMKilled", "message": ""}])
    assert classify(j)["category"] == "oom"


def test_report_groups_and_window():
    now = datetime.now(timezone.utc)
    jobs = [failed("a", "OOMKilled", retries=2, gpus=4), failed("b", "OOMKilled", ns="tenant-beta"),
            failed("c", "NodeLost"), failed("old", "OOMKilled", hours=24 * 30),
            {"metadata": {"name": "ok", "namespace": "tenant-alpha"}, "status": {"phase": "Succeeded"}}]
    rep = failure_report(jobs, now - timedelta(days=7), now)
    assert rep["total"] == 3 and rep["transient"] == 1
    oom = rep["categories"][0]
    assert oom["category"] == "oom" and oom["jobs"] == 2 and oom["gpus"] == 5 and oom["retries"] == 2
    assert oom["tenants"] == {"alpha": 1, "beta": 1} and oom["share"] == 0.6667


@pytest.fixture
def seeded(fake_k8s):
    fake_k8s.add("gryviaaijobs", failed("a1", "OOMKilled"), "tenant-alpha")
    fake_k8s.add("gryviaaijobs", failed("a2", "=cmd|calc"), "tenant-alpha")
    fake_k8s.add("gryviaaijobs", failed("b1", "NodeLost", ns="tenant-beta"), "tenant-beta")


def test_endpoint_admin_and_filters(make_client, seeded):
    c = make_client("failures")
    body = c.get("/api/failures").json()
    assert body["total"] == 3 and body["days"] == 7
    assert {x["category"] for x in body["categories"]} == {"oom", "node-failure", "unknown"}
    assert [i["name"] for i in c.get("/api/failures?category=oom").json()["items"]] == ["a1"]
    assert [i["name"] for i in c.get("/api/failures?tenant=beta").json()["items"]] == ["b1"]
    assert c.get("/api/failures?limit=1").json()["truncated"] is True


def test_endpoint_tenant_scoped(make_client, seeded):
    c = make_client("failures", role="tenant", tenants=["alpha"])
    assert {i["tenant"] for i in c.get("/api/failures").json()["items"]} == {"alpha"}
    assert c.get("/api/failures?tenant=beta").json()["items"] == []  # another tenant is simply not visible


def test_endpoint_csv_guards_formulas_and_validates(make_client, seeded):
    c = make_client("failures")
    r = c.get("/api/failures?format=csv")
    assert r.headers["content-type"].startswith("text/csv")
    rows = {x["name"]: x for x in csv.DictReader(io.StringIO(r.text))}
    assert rows["a2"]["message"] == "'=cmd|calc" and rows["a1"]["category"] == "oom"
    assert c.get("/api/failures?category=bogus").status_code == 422
    assert c.get("/api/failures?days=0").status_code == 422
    assert c.get("/api/failures?format=xml").status_code == 422
