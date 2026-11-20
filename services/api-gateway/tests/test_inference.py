# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import pytest

NS = "default"
BODY = {"name": "svc1", "modelRef": "bert", "backend": "vLLM", "replicas": 2,
        "minReplicas": 1, "maxReplicas": 4, "targetUtilization": 80}


def seed_model(fake_k8s, name="bert"):
    fake_k8s.add("gryviamodelregistries", {"metadata": {"name": name}, "spec": {"version": "v1"}}, namespace=NS)


def seed(fake_k8s, name="svc1", canary=None):
    spec = {"modelRef": "bert", "backend": "triton", "replicas": 2,
            "autoscaling": {"enabled": True, "minReplicas": 1, "maxReplicas": 4, "targetGPUUtilization": 70}}
    if canary:
        spec["canary"] = canary
    fake_k8s.add("gryviainferenceservices", {
        "metadata": {"name": name}, "spec": spec,
        "status": {"phase": "Running", "readyReplicas": 2, "endpoint": "http://e"}}, namespace=NS)


def test_list_empty(make_client):
    r = make_client("inference").get("/api/inference")
    assert r.status_code == 200 and r.json() == {"items": []}


def test_list_get_shape(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("inference")
    item = c.get("/api/inference/svc1").json()
    assert c.get("/api/inference").json()["items"] == [item]
    assert item["spec"] == {"modelRef": "bert", "backend": "triton", "replicas": 2,
                            "autoscaling": {"minReplicas": 1, "maxReplicas": 4, "targetUtilization": 70}}
    assert item["status"] == {"phase": "Running", "readyReplicas": 2, "endpoint": "http://e"}
    assert "latencyMs" not in item["status"]


def test_canary_only_when_enabled(make_client, fake_k8s):
    seed(fake_k8s, "a", {"enabled": True, "weight": 20, "modelVersion": "v2"})
    seed(fake_k8s, "b", {"enabled": False, "weight": 20, "modelVersion": "v2"})
    c = make_client("inference")
    assert c.get("/api/inference/a").json()["spec"]["canary"] == {"trafficPercent": 20}
    assert "canary" not in c.get("/api/inference/b").json()["spec"]


def test_get_404(make_client):
    assert make_client("inference").get("/api/inference/nope").status_code == 404


def test_create(make_client, fake_k8s):
    seed_model(fake_k8s)
    r = make_client("inference").post("/api/inference", json=BODY)
    assert r.status_code == 201
    stored = fake_k8s.store[("gryviainferenceservices", NS, "svc1")]
    assert stored["kind"] == "GryviaInferenceService"
    assert stored["spec"] == {"modelRef": "bert", "backend": "vllm", "replicas": 2,
                              "autoscaling": {"enabled": True, "minReplicas": 1, "maxReplicas": 4,
                                              "targetGPUUtilization": 80}}


@pytest.mark.parametrize("ui,crd", [("Triton", "triton"), ("TensorRT-LLM", "tensorrt-llm"),
                                    ("TorchServe", "torchserve")])
def test_backend_mapping(make_client, fake_k8s, ui, crd):
    seed_model(fake_k8s)
    make_client("inference").post("/api/inference", json={**BODY, "backend": ui})
    assert fake_k8s.store[("gryviainferenceservices", NS, "svc1")]["spec"]["backend"] == crd


def test_create_duplicate(make_client, fake_k8s):
    seed_model(fake_k8s)
    seed(fake_k8s)
    assert make_client("inference").post("/api/inference", json=BODY).status_code == 409


def test_create_unknown_model(make_client, fake_k8s):
    r = make_client("inference").post("/api/inference", json=BODY)
    assert r.status_code == 422 and not fake_k8s.store


@pytest.mark.parametrize("patch", [
    {"name": "Bad Name"}, {"modelRef": "Bad_Ref"}, {"backend": "onnx"}, {"replicas": 0},
    {"replicas": 101}, {"minReplicas": 5, "maxReplicas": 2}, {"targetUtilization": 0},
    {"targetUtilization": 101}, {"extra": True},
])
def test_create_validation(make_client, fake_k8s, patch):
    seed_model(fake_k8s)
    assert make_client("inference").post("/api/inference", json={**BODY, **patch}).status_code == 422


def test_delete(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("inference")
    assert c.delete("/api/inference/svc1").json() == {"status": "deleted", "name": "svc1"}
    assert c.delete("/api/inference/svc1").status_code == 404
