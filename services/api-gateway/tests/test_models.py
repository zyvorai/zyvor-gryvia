# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import pytest

NS = "default"


def seed(fake_k8s, name="bert", stage="dev", **spec):
    fake_k8s.add("gryviamodelregistries", {
        "metadata": {"name": name},
        "spec": {"modelName": "bert", "version": "v1", "stage": stage,
                 "source": {"jobRef": "train-1"},
                 "artifacts": {"s3Path": "s3://b/m", "pvcName": "pvc1", "subPath": "out"}, **spec},
        "status": {"servingEndpoint": "http://svc"},
    }, namespace=NS)


def test_list_empty(make_client):
    r = make_client("models").get("/api/models")
    assert r.status_code == 200 and r.json() == {"items": []}


def test_list_and_get(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("models")
    item = c.get("/api/models").json()["items"][0]
    assert item["spec"] == {"version": "v1", "stage": "dev", "sourceJob": "train-1",
                            "artifacts": ["s3://b/m", "pvc://pvc1/out"]}
    assert item["status"] == {"servingEndpoint": "http://svc"}
    assert c.get("/api/models/bert").json()["metadata"]["name"] == "bert"


def test_no_source_omits_field(make_client, fake_k8s):
    fake_k8s.add("gryviamodelregistries", {"metadata": {"name": "m"}, "spec": {"version": "v2"}}, namespace=NS)
    spec = make_client("models").get("/api/models/m").json()["spec"]
    assert "sourceJob" not in spec and spec["artifacts"] == []


def test_get_404(make_client):
    assert make_client("models").get("/api/models/nope").status_code == 404


@pytest.mark.parametrize("cur,target", [("dev", "staging"), ("staging", "production"),
                                        ("production", "archived")])
def test_promote(make_client, fake_k8s, cur, target):
    seed(fake_k8s, stage=cur)
    r = make_client("models").post("/api/models/bert/promote", json={"targetStage": target})
    assert r.status_code == 200 and r.json()["spec"]["stage"] == target
    assert fake_k8s.store[("gryviamodelregistries", NS, "bert")]["spec"]["stage"] == target


@pytest.mark.parametrize("cur,target", [("dev", "production"), ("staging", "dev"), ("archived", "dev"),
                                        ("dev", "dev")])
def test_promote_invalid_transition(make_client, fake_k8s, cur, target):
    seed(fake_k8s, stage=cur)
    r = make_client("models").post("/api/models/bert/promote", json={"targetStage": target})
    assert r.status_code == 409
    assert fake_k8s.store[("gryviamodelregistries", NS, "bert")]["spec"]["stage"] == cur


def test_promote_validation(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("models")
    assert c.post("/api/models/bert/promote", json={"targetStage": "bogus"}).status_code == 422
    assert c.post("/api/models/bert/promote", json={}).status_code == 422


def test_promote_404(make_client):
    r = make_client("models").post("/api/models/nope/promote", json={"targetStage": "staging"})
    assert r.status_code == 404


CREATE = {"name": "bert-v2", "version": "v2", "artifacts": {"s3Path": "s3://bucket/bert/v2"}}


def test_create_minimal(make_client, fake_k8s):
    r = make_client("models").post("/api/models", json=CREATE)
    assert r.status_code == 201
    stored = fake_k8s.store[("gryviamodelregistries", NS, "bert-v2")]
    assert stored["kind"] == "GryviaModelRegistry"
    # modelName defaults to the object name; the stage to dev; nothing else is invented.
    assert stored["spec"] == {"modelName": "bert-v2", "version": "v2", "stage": "dev",
                              "artifacts": {"s3Path": "s3://bucket/bert/v2"}}
    body = r.json()
    assert body["metadata"]["name"] == "bert-v2"
    assert body["spec"]["stage"] == "dev" and body["spec"]["artifacts"] == ["s3://bucket/bert/v2"]


def test_create_full(make_client, fake_k8s):
    r = make_client("models").post("/api/models", json={
        "name": "llama", "modelName": "Llama-3.1", "version": "1.0.0+rc1", "stage": "production",
        "description": "chat model", "sourceJob": "train-7", "autoServe": True,
        "artifacts": {"pvcName": "models", "subPath": "llama/1", "format": "safetensors", "s3Path": "s3://b/l"},
        "servingConfig": {"backend": "triton", "replicas": 2, "gpuCount": 1, "gpuType": "A100-80G"},
    })
    assert r.status_code == 201
    spec = fake_k8s.store[("gryviamodelregistries", NS, "llama")]["spec"]
    assert spec == {
        "modelName": "Llama-3.1", "version": "1.0.0+rc1", "stage": "production", "description": "chat model",
        "source": {"jobRef": "train-7"}, "autoServe": True,
        "artifacts": {"s3Path": "s3://b/l", "pvcName": "models", "subPath": "llama/1", "format": "safetensors"},
        "servingConfig": {"backend": "triton", "replicas": 2, "gpuCount": 1, "gpuType": "A100-80G"},
    }
    assert r.json()["spec"]["sourceJob"] == "train-7"


def test_create_serving_config_omits_zero_gpu(make_client, fake_k8s):
    make_client("models").post("/api/models", json={**CREATE, "autoServe": True, "servingConfig": {}})
    sc = fake_k8s.store[("gryviamodelregistries", NS, "bert-v2")]["spec"]["servingConfig"]
    # gpuCount 0 is the CRD default: it is left out so the operator's --autoserve-default-gpu-count applies.
    assert sc == {"backend": "vllm", "replicas": 1}


def test_create_uses_the_tenant_namespace(make_client, fake_k8s):
    make_client("models").post("/api/models", json=CREATE)
    assert [k for k in fake_k8s.store] == [("gryviamodelregistries", NS, "bert-v2")]
    # And the new object is visible only through the same scoping as the other routes.
    c = make_client("models")
    assert c.get("/api/models/bert-v2").status_code == 200


def test_create_duplicate(make_client, fake_k8s):
    seed(fake_k8s, name="bert-v2")
    assert make_client("models").post("/api/models", json=CREATE).status_code == 409


@pytest.mark.parametrize("patch", [
    {"name": "Bad_Name"}, {"name": ""}, {"name": "a" * 64}, {"version": ""}, {"version": "v 1"},
    {"modelName": "bad name"}, {"stage": "archived"}, {"stage": "bogus"}, {"sourceJob": "Bad Job"},
    {"description": "x" * 1025}, {"extra": 1}, {"artifacts": {}}, {"artifacts": {"s3Path": "not a url"}},
    {"artifacts": {"pvcName": "Bad_PVC"}}, {"artifacts": {"pvcName": "p", "subPath": "../etc"}},
    {"artifacts": {"pvcName": "p", "subPath": "a/../b"}}, {"artifacts": {"s3Path": "s3://b/x", "extra": 1}},
    {"servingConfig": {"backend": "onnx"}}, {"servingConfig": {"replicas": 0}}, {"servingConfig": {"gpuCount": 65}},
    {"servingConfig": {"gpuType": "a b"}}, {"servingConfig": {"extra": 1}},
])
def test_create_validation(make_client, fake_k8s, patch):
    r = make_client("models").post("/api/models", json={**CREATE, **patch})
    assert r.status_code == 422, r.text
    assert not fake_k8s.store


def test_create_missing_fields(make_client, fake_k8s):
    c = make_client("models")
    assert c.post("/api/models", json={}).status_code == 422
    assert c.post("/api/models", json={"name": "m", "version": "v1"}).status_code == 422
    assert not fake_k8s.store


def test_rollback_sets_the_request_annotation(make_client, fake_k8s):
    seed(fake_k8s, stage="production", servingConfig={"serviceName": "chat"})
    fake_k8s.store[("gryviamodelregistries", NS, "bert")]["status"]["previousVersion"] = "bert-v0"
    r = make_client("models").post("/api/models/bert/rollback")
    assert r.status_code == 202
    assert r.json()["status"]["previousVersion"] == "bert-v0"
    ann = fake_k8s.store[("gryviamodelregistries", NS, "bert")]["metadata"]["annotations"]
    assert ann["gryvia.io/rollback-requested"].startswith("api at ")


@pytest.mark.parametrize("stage,serving,previous", [("staging", {"serviceName": "chat"}, "v0"),
                                                    ("production", {}, "v0"),
                                                    ("production", {"serviceName": "chat"}, "")])
def test_rollback_refused(make_client, fake_k8s, stage, serving, previous):
    seed(fake_k8s, stage=stage, servingConfig=serving)
    fake_k8s.store[("gryviamodelregistries", NS, "bert")]["status"]["previousVersion"] = previous
    assert make_client("models").post("/api/models/bert/rollback").status_code == 409
    assert "annotations" not in fake_k8s.store[("gryviamodelregistries", NS, "bert")]["metadata"]


def test_rollback_404(make_client):
    assert make_client("models").post("/api/models/nope/rollback").status_code == 404


# -- promotion approval (GRYVIA_REQUIRE_PROD_APPROVAL) ----------------------------------------------

def _stage(fake_k8s, ns="tenant-alpha"):
    return fake_k8s.store[("gryviamodelregistries", ns, "bert")]["spec"]["stage"]


def _note(fake_k8s, ns="tenant-alpha"):
    return fake_k8s.store[("gryviamodelregistries", ns, "bert")]["metadata"].get("annotations", {}).get("gryvia.io/promotion-request")


def test_tenant_promotion_to_production_is_requested_not_applied(make_client, fake_k8s):
    fake_k8s.add("gryviamodelregistries", {"metadata": {"name": "bert"}, "spec": {"version": "v1", "stage": "staging"}},
                 namespace="tenant-alpha")
    c = make_client("models", role="tenant", tenants=["alpha"], require_prod_approval=True)
    r = c.post("/api/models/bert/promote", json={"targetStage": "production"})
    assert r.status_code == 202 and r.json()["pendingPromotion"]["by"] == "alpha"
    assert _stage(fake_k8s) == "staging" and _note(fake_k8s)
    # a tenant cannot approve its own request
    assert c.post("/api/models/bert/approve").status_code == 403
    assert _stage(fake_k8s) == "staging"


def test_tenant_other_stages_are_not_gated(make_client, fake_k8s):
    fake_k8s.add("gryviamodelregistries", {"metadata": {"name": "bert"}, "spec": {"version": "v1", "stage": "dev"}},
                 namespace="tenant-alpha")
    c = make_client("models", role="tenant", tenants=["alpha"], require_prod_approval=True)
    assert c.post("/api/models/bert/promote", json={"targetStage": "staging"}).status_code == 200


def test_admin_approves_and_request_clears(make_client, fake_k8s):
    seed(fake_k8s, stage="staging")
    fake_k8s.store[("gryviamodelregistries", NS, "bert")]["metadata"]["annotations"] = {
        "gryvia.io/promotion-request": '{"target":"production","by":"alpha","at":"2026-01-01T00:00:00Z"}'}
    c = make_client("models", require_prod_approval=True)
    r = c.post("/api/models/bert/approve")
    assert r.status_code == 200 and r.json()["spec"]["stage"] == "production" and not r.json()["pendingPromotion"]
    assert c.post("/api/models/bert/approve").status_code == 409


def test_admin_rejects(make_client, fake_k8s):
    seed(fake_k8s, stage="staging")
    fake_k8s.store[("gryviamodelregistries", NS, "bert")]["metadata"]["annotations"] = {
        "gryvia.io/promotion-request": '{"target":"production","by":"alpha","at":"x"}'}
    c = make_client("models", require_prod_approval=True)
    r = c.post("/api/models/bert/reject")
    assert r.status_code == 200 and r.json()["pendingPromotion"] is None
    assert fake_k8s.store[("gryviamodelregistries", NS, "bert")]["spec"]["stage"] == "staging"
    assert c.post("/api/models/bert/reject").status_code == 409


def test_flag_off_keeps_tenant_promotion_direct(make_client, fake_k8s):
    fake_k8s.add("gryviamodelregistries", {"metadata": {"name": "bert"}, "spec": {"version": "v1", "stage": "staging"}},
                 namespace="tenant-alpha")
    c = make_client("models", role="tenant", tenants=["alpha"])
    assert c.post("/api/models/bert/promote", json={"targetStage": "production"}).status_code == 200
    assert _stage(fake_k8s) == "production"
