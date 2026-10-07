import importlib

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from kubernetes.client.exceptions import ApiException

from routers.common import Deps
from tests.conftest import FakeCore, NoopLimiter, _allow

JOB = {"metadata": {"name": "train"}, "spec": {}}


@pytest.fixture
def core():
    return FakeCore()


@pytest.fixture
def client(fake_k8s, core):
    fake_k8s.add("gryviaaijobs", JOB, namespace="default")
    deps = Deps(verify_auth=_allow, k8s_custom=fake_k8s, k8s_core=core, limiter=NoopLimiter(),
                job_namespace="default")
    app = FastAPI()
    app.include_router(importlib.import_module("routers.jobs").build_router(deps))
    return TestClient(app)


def test_pods_shape(client, core):
    core.add_pod("train-0", job="train", containers=[("main", "waiting", 3, "CrashLoopBackOff", False),
                                                    ("side", "running", 1, None, True)])
    core.add_pod("other-0", job="other")
    r = client.get("/api/jobs/train/pods")
    assert r.status_code == 200
    items = r.json()["items"]
    assert len(items) == 1
    p = items[0]
    assert p["name"] == "train-0" and p["phase"] == "Running" and p["node"] == "node-1"
    assert p["podIP"] == "10.0.0.1" and p["startTime"] == "2026-01-01T00:00:00Z"
    assert p["restarts"] == 4 and "message" not in p
    assert p["containers"][0] == {"name": "main", "ready": False, "state": "waiting",
                                  "restartCount": 3, "reason": "CrashLoopBackOff"}
    assert "reason" not in p["containers"][1]


def test_no_pods_yet(client):
    r = client.get("/api/jobs/train/pods")
    assert r.status_code == 200 and r.json() == {"items": []}


def test_unknown_job_404(client):
    for path in ("pods", "logs", "events"):
        assert client.get(f"/api/jobs/nope/{path}").status_code == 404


def test_invalid_name(client):
    assert client.get("/api/jobs/Bad_Name/pods").status_code == 422


def test_logs_default_pod_prefers_running_then_newest(client, core):
    core.add_pod("a", job="train", phase="Succeeded", start_time="2026-01-03T00:00:00Z")
    core.add_pod("b", job="train", phase="Running", start_time="2026-01-01T00:00:00Z")
    core.add_pod("c", job="train", phase="Running", start_time="2026-01-02T00:00:00Z")
    core.set_log("c", "l1\nl2")
    body = client.get("/api/jobs/train/logs").json()
    assert body == {"pod": "c", "container": "main", "lines": ["l1", "l2"], "truncated": False}
    call = core.log_calls[-1]
    assert call["tail_lines"] == 201 and call["timestamps"] is True and call["namespace"] == "default"


def test_logs_no_pods(client):
    body = client.get("/api/jobs/train/logs").json()
    assert body["pod"] is None and body["lines"] == [] and body["truncated"] is False


def test_logs_explicit_pod_and_foreign_pod(client, core):
    core.add_pod("a", job="train")
    core.add_pod("b", job="train")
    core.add_pod("x", job="other")
    core.set_log("a", "from-a")
    core.set_log("x", "secret")
    assert client.get("/api/jobs/train/logs?pod=a").json()["lines"] == ["from-a"]
    assert client.get("/api/jobs/train/logs?pod=x").status_code == 404
    assert client.get("/api/jobs/train/logs?pod=missing").status_code == 404


@pytest.mark.parametrize("tail", [0, -1, 2001, "abc"])
def test_tail_bounds(client, tail):
    assert client.get(f"/api/jobs/train/logs?tail={tail}").status_code == 422


def test_tail_edges_ok(client, core):
    core.add_pod("a", job="train")
    assert client.get("/api/jobs/train/logs?tail=1").status_code == 200
    assert client.get("/api/jobs/train/logs?tail=2000").status_code == 200


def test_truncated(client, core):
    core.add_pod("a", job="train")
    core.set_log("a", "\n".join(f"line{i}" for i in range(10)))
    body = client.get("/api/jobs/train/logs?tail=3").json()
    assert body["truncated"] is True and body["lines"] == ["line7", "line8", "line9"]
    body = client.get("/api/jobs/train/logs?tail=10").json()
    assert body["truncated"] is False and len(body["lines"]) == 10


def test_container_not_started(client, core):
    core.add_pod("a", job="train", phase="Pending",
                 containers=[("main", "waiting", 0, "ContainerCreating", False)])
    core.set_log("a", ApiException(status=400, reason="Bad Request"))
    body = client.get("/api/jobs/train/logs").json()
    assert body["lines"] == [] and body["truncated"] is False
    assert body["message"] == "container is not running yet" and body["pod"] == "a"


def test_log_error_does_not_leak(client, core):
    core.add_pod("a", job="train")
    core.set_log("a", RuntimeError("secret internal detail"))
    r = client.get("/api/jobs/train/logs")
    assert r.status_code == 500 and "secret" not in r.text


def test_control_chars_and_line_cap(client, core):
    core.add_pod("a", job="train")
    core.set_log("a", "ok\x1b[31mred\x00\ttab\rend\n" + "x" * 5000)
    lines = client.get("/api/jobs/train/logs").json()["lines"]
    assert lines[0] == "ok[31mred\ttabend"
    assert len(lines[1]) == 4096


def test_events_sorted_and_limited(client, core):
    core.add_pod("train-0", job="train")
    core.add_event("GryviaAIJob", "train", reason="Created", last="2026-01-01T00:00:00Z")
    core.add_event("Pod", "train-0", reason="Started", last="2026-01-03T00:00:00Z", count=2,
                   first="2026-01-02T00:00:00Z")
    core.add_event("Pod", "other-0", reason="Nope", last="2026-01-09T00:00:00Z")
    items = client_get(client)
    assert [e["reason"] for e in items] == ["Started", "Created"]
    assert items[0] == {"type": "Normal", "reason": "Started", "message": "m", "count": 2,
                        "firstSeen": "2026-01-02T00:00:00Z", "lastSeen": "2026-01-03T00:00:00Z",
                        "object": "Pod/train-0"}
    assert items[1]["object"] == "GryviaAIJob/train"


def client_get(client):
    r = client.get("/api/jobs/train/events")
    assert r.status_code == 200
    return r.json()["items"]


def test_events_limit_100(client, core):
    for i in range(150):
        core.add_event("GryviaAIJob", "train", reason=f"r{i}", last=f"2026-01-01T00:{i // 60:02d}:{i % 60:02d}Z")
    items = client_get(client)
    assert len(items) == 100 and items[0]["reason"] == "r149"


def test_auth_required(fake_k8s, core):
    from fastapi import HTTPException

    async def deny():
        raise HTTPException(status_code=401, detail="no")

    fake_k8s.add("gryviaaijobs", JOB, namespace="default")
    deps = Deps(verify_auth=deny, k8s_custom=fake_k8s, k8s_core=core, limiter=NoopLimiter())
    app = FastAPI()
    app.include_router(importlib.import_module("routers.jobs").build_router(deps))
    c = TestClient(app)
    for path in ("pods", "logs", "events"):
        assert c.get(f"/api/jobs/train/{path}").status_code == 401


def _elastic(name, nodes=4, min_nodes=1, phase="Running", labels=None):
    md = {"name": name}
    if labels:
        md["labels"] = labels
    return {"metadata": md, "status": {"phase": phase},
            "spec": {"distributed": {"enabled": True, "nodes": nodes, "elastic": {"minNodes": min_nodes}}}}


def test_resize_sets_desired_nodes(client, fake_k8s):
    fake_k8s.add("gryviaaijobs", _elastic("el", min_nodes=2), namespace="default")
    r = client.post("/api/jobs/el/resize", json={"nodes": 3})
    assert r.status_code == 200, r.text
    assert r.json() == {"name": "el", "namespace": "default", "desiredNodes": 3, "minNodes": 2, "maxNodes": 4,
                        "currentNodes": None}
    stored = fake_k8s.get_namespaced_custom_object("gryvia.io", "v1alpha1", "default", "gryviaaijobs", "el")
    assert stored["spec"]["distributed"]["elastic"] == {"minNodes": 2, "desiredNodes": 3}
    assert stored["spec"]["distributed"]["nodes"] == 4


@pytest.mark.parametrize("nodes", [1, 5, 0, -1])
def test_resize_out_of_bounds(client, fake_k8s, nodes):
    fake_k8s.add("gryviaaijobs", _elastic("el", min_nodes=2), namespace="default")
    r = client.post("/api/jobs/el/resize", json={"nodes": nodes})
    assert r.status_code == 400 and "between minNodes (2) and distributed.nodes (4)" in r.json()["detail"]


@pytest.mark.parametrize("body", [{}, {"nodes": "3"}, {"nodes": True}, {"nodes": 2.5}, [3]])
def test_resize_bad_body(client, fake_k8s, body):
    fake_k8s.add("gryviaaijobs", _elastic("el"), namespace="default")
    assert client.post("/api/jobs/el/resize", json=body).status_code == 400


def test_resize_refuses_non_elastic_finished_and_kueue(client, fake_k8s):
    fake_k8s.add("gryviaaijobs", _elastic("done", phase="Succeeded"), namespace="default")
    fake_k8s.add("gryviaaijobs", _elastic("kq", labels={"kueue.x-k8s.io/queue-name": "q"}), namespace="default")
    assert client.post("/api/jobs/train/resize", json={"nodes": 2}).status_code == 409  # JOB is not elastic
    assert client.post("/api/jobs/done/resize", json={"nodes": 2}).status_code == 409
    r = client.post("/api/jobs/kq/resize", json={"nodes": 2})
    assert r.status_code == 409 and "Kueue" in r.json()["detail"]
    assert client.post("/api/jobs/nope/resize", json={"nodes": 2}).status_code == 404
    assert client.post("/api/jobs/Bad_Name/resize", json={"nodes": 2}).status_code == 422
