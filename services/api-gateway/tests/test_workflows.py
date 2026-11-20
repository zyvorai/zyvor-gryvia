# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import pytest

NS = "default"
JOB = {"image": "img:1", "gpus": 1}


def body(steps=None, name="wf1", **spec):
    return {"metadata": {"name": name},
            "spec": {"steps": steps or [{"name": "a", "jobTemplate": JOB},
                                        {"name": "b", "dependsOn": ["a"], "jobTemplate": JOB}], **spec}}


def seed(fake_k8s, name="wf1", status=None):
    obj = {"metadata": {"name": name},
           "spec": {"steps": [{"name": "a", "type": "job"}, {"name": "b", "type": "job", "dependsOn": ["a"]}]}}
    if status is not None:
        obj["status"] = status
    fake_k8s.add("gryviaworkflows", obj, namespace=NS)


def test_list_empty(make_client):
    r = make_client("workflows").get("/api/workflows")
    assert r.status_code == 200 and r.json() == {"items": []}


def test_pending_workflow_falls_back_to_spec_steps(make_client, fake_k8s):
    seed(fake_k8s)
    item = make_client("workflows").get("/api/workflows/wf1").json()
    assert item["spec"]["steps"] == [{"name": "a", "type": "job"},
                                     {"name": "b", "type": "job", "dependsOn": ["a"]}]
    assert "steps" not in item["status"] and "phase" not in item["status"]


def test_status_projection(make_client, fake_k8s):
    seed(fake_k8s, status={
        "phase": "Running", "startTime": "2026-01-01T00:00:00Z",
        "completionTime": "2026-01-01T00:10:00Z",
        "stepStatuses": [
            {"name": "a", "phase": "Succeeded", "jobName": "wf1-a", "startTime": "2026-01-01T00:00:00Z",
             "completionTime": "2026-01-01T00:02:30Z"},
            {"name": "b", "phase": "Running"}]})
    c = make_client("workflows")
    st = c.get("/api/workflows/wf1").json()["status"]
    assert st["phase"] == "Running" and st["duration"] == "10m 0s" and st["startedAt"] == "2026-01-01T00:00:00Z"
    assert st["steps"][0] == {"name": "a", "type": "job", "status": "Succeeded", "duration": "2m 30s",
                              "jobRef": "wf1-a"}
    assert st["steps"][1] == {"name": "b", "type": "job", "status": "Running", "dependsOn": ["a"]}
    assert c.get("/api/workflows").json()["items"][0]["metadata"]["name"] == "wf1"


def test_get_404(make_client):
    assert make_client("workflows").get("/api/workflows/nope").status_code == 404


def test_create(make_client, fake_k8s):
    r = make_client("workflows").post("/api/workflows", json=body(parameters={"lr": "0.1"}))
    assert r.status_code == 201
    stored = fake_k8s.store[("gryviaworkflows", NS, "wf1")]
    assert stored["kind"] == "GryviaWorkflow" and stored["spec"]["parameters"] == {"lr": "0.1"}
    assert stored["spec"]["steps"][0] == {"name": "a", "type": "job",
                                          "jobTemplate": {"type": "training", "image": "img:1", "gpus": 1}}
    assert stored["spec"]["steps"][1]["dependsOn"] == ["a"]


def test_create_script_and_webhook(make_client, fake_k8s):
    steps = [{"name": "s", "type": "script", "script": {"image": "i", "command": ["sh"]}},
             {"name": "w", "type": "webhook", "dependsOn": ["s"], "webhook": {"url": "https://h/x"}}]
    r = make_client("workflows").post("/api/workflows", json=body(steps))
    assert r.status_code == 201
    st = fake_k8s.store[("gryviaworkflows", NS, "wf1")]["spec"]["steps"]
    assert st[0]["script"] == {"image": "i", "command": ["sh"], "args": []}
    assert st[1]["webhook"] == {"url": "https://h/x", "method": "POST", "body": ""}


def test_create_duplicate(make_client, fake_k8s):
    seed(fake_k8s)
    assert make_client("workflows").post("/api/workflows", json=body()).status_code == 409


@pytest.mark.parametrize("payload", [
    {},
    {"metadata": {"name": "Bad_Name"}, "spec": {"steps": [{"name": "a", "jobTemplate": JOB}]}},
    {"metadata": {"name": "w"}, "spec": {"steps": []}},
    body([{"name": "a", "jobTemplate": JOB}, {"name": "a", "jobTemplate": JOB}]),
    body([{"name": "a", "dependsOn": ["zzz"], "jobTemplate": JOB}]),
    body([{"name": "a", "dependsOn": ["a"], "jobTemplate": JOB}]),
    body([{"name": "a", "dependsOn": ["b"], "jobTemplate": JOB}, {"name": "b", "dependsOn": ["a"], "jobTemplate": JOB}]),
    body([{"name": "a"}]),
    body([{"name": "a", "type": "script", "jobTemplate": JOB}]),
    body([{"name": "a", "type": "webhook", "webhook": {"url": "ftp://x"}}]),
    body([{"name": "a", "type": "bogus", "jobTemplate": JOB}]),
    body([{"name": "a", "jobTemplate": {"image": "i", "gpus": -1}}]),
])
def test_create_validation(make_client, fake_k8s, payload):
    assert make_client("workflows").post("/api/workflows", json=payload).status_code == 422
    assert not fake_k8s.store


def test_delete(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("workflows")
    assert c.delete("/api/workflows/wf1").json() == {"status": "deleted", "name": "wf1"}
    assert not fake_k8s.store
    assert c.delete("/api/workflows/wf1").status_code == 404
