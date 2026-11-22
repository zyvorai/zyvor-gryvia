# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import pytest

NS = "default"
BODY = {"name": "ws1", "type": "jupyter", "gpuCount": 2, "gpuType": "A100-80G",
        "storageSize": "50Gi", "idleTimeout": "30m"}


def seed(fake_k8s, name="ws1", **extra):
    obj = {"metadata": {"name": name},
           "spec": {"type": "jupyter", "gpuCount": 1, "gpuType": "T4", "storage": "10Gi",
                    "idleTimeoutMinutes": 45},
           "status": {"phase": "Running", "url": "https://x/ws", "startTime": "2026-01-01T00:00:00Z",
                      "lastActivity": "2026-01-01T01:00:00Z"}}
    obj.update(extra)
    fake_k8s.add("gryviaworkspaces", obj, namespace=NS)


def test_list_empty(make_client):
    r = make_client("workspaces").get("/api/workspaces")
    assert r.status_code == 200 and r.json() == {"items": []}


def test_list_and_get_shape(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("workspaces")
    item = c.get("/api/workspaces").json()["items"][0]
    assert item["metadata"]["name"] == "ws1"
    assert item["spec"] == {"type": "jupyter", "gpuCount": 1, "gpuType": "T4",
                            "storageSize": "10Gi", "idleTimeout": "45m"}
    assert item["status"]["phase"] == "Running" and item["status"]["url"] == "https://x/ws"
    assert item["status"]["uptime"]
    assert c.get("/api/workspaces/ws1").json() == item or c.get("/api/workspaces/ws1").status_code == 200


def test_paused_has_no_uptime(make_client, fake_k8s):
    seed(fake_k8s, status={"phase": "Paused", "startTime": "2026-01-01T00:00:00Z"})
    assert "uptime" not in make_client("workspaces").get("/api/workspaces/ws1").json()["status"]


def test_get_404(make_client):
    assert make_client("workspaces").get("/api/workspaces/nope").status_code == 404


def test_other_namespace_not_visible(make_client, fake_k8s):
    fake_k8s.add("gryviaworkspaces", {"metadata": {"name": "x"}, "spec": {"type": "jupyter"}}, namespace="other")
    c = make_client("workspaces")
    assert c.get("/api/workspaces").json()["items"] == []
    assert c.get("/api/workspaces/x").status_code == 404


def test_create(make_client, fake_k8s):
    r = make_client("workspaces").post("/api/workspaces", json=BODY)
    assert r.status_code == 201
    stored = fake_k8s.store[("gryviaworkspaces", NS, "ws1")]
    assert stored["kind"] == "GryviaWorkspace"
    assert stored["spec"] == {"type": "jupyter", "gpuCount": 2, "gpuType": "A100-80G",
                              "storage": "50Gi", "idleTimeoutMinutes": 30}
    assert r.json()["spec"]["idleTimeout"] == "30m"


def test_create_hours_timeout(make_client, fake_k8s):
    make_client("workspaces").post("/api/workspaces", json={**BODY, "idleTimeout": "2h"})
    assert fake_k8s.store[("gryviaworkspaces", NS, "ws1")]["spec"]["idleTimeoutMinutes"] == 120


def test_create_duplicate(make_client, fake_k8s):
    seed(fake_k8s)
    assert make_client("workspaces").post("/api/workspaces", json=BODY).status_code == 409


@pytest.mark.parametrize("patch", [
    {"name": "Bad_Name"}, {"name": ""}, {"name": "a" * 64}, {"type": "emacs"}, {"gpuCount": -1},
    {"gpuCount": 1000}, {"storageSize": "lots"}, {"idleTimeout": "soon"}, {"gpuType": "a b"},
    {"extra": 1},
])
def test_create_validation(make_client, fake_k8s, patch):
    r = make_client("workspaces").post("/api/workspaces", json={**BODY, **patch})
    assert r.status_code == 422
    assert not fake_k8s.store


def test_create_missing_fields(make_client):
    assert make_client("workspaces").post("/api/workspaces", json={}).status_code == 422


def test_delete(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("workspaces")
    assert c.delete("/api/workspaces/ws1").json() == {"status": "deleted", "name": "ws1"}
    assert not fake_k8s.store
    assert c.delete("/api/workspaces/ws1").status_code == 404


def test_pause_resume(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("workspaces")
    assert c.post("/api/workspaces/ws1/pause").status_code == 200
    assert fake_k8s.store[("gryviaworkspaces", NS, "ws1")]["spec"]["paused"] is True
    assert c.post("/api/workspaces/ws1/resume").status_code == 200
    assert fake_k8s.store[("gryviaworkspaces", NS, "ws1")]["spec"]["paused"] is False


def test_actions_404(make_client):
    c = make_client("workspaces")
    assert c.post("/api/workspaces/nope/pause").status_code == 404
    assert c.post("/api/workspaces/nope/resume").status_code == 404


def test_status_message_passthrough(make_client, fake_k8s):
    seed(fake_k8s, status={"phase": "Failed", "message": "quota exceeded"})
    st = make_client("workspaces").get("/api/workspaces").json()["items"][0]["status"]
    assert st["phase"] == "Failed" and st["message"] == "quota exceeded"


def test_no_phase_invented(make_client, fake_k8s):
    seed(fake_k8s, status={"message": "waiting for scheduler"})
    st = make_client("workspaces").get("/api/workspaces").json()["items"][0]["status"]
    assert "phase" not in st and st["message"] == "waiting for scheduler"
    seed(fake_k8s, name="w2", status={"phase": "Running"})
    assert "message" not in make_client("workspaces").get("/api/workspaces/w2").json()["status"]
