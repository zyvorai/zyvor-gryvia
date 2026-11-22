# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

NS = "default"
TEMPLATE = {"steps": [{"name": "finetune", "type": "job",
                       "jobTemplate": {"type": "training", "image": "ft:1", "gpus": 1}}]}
BODY = {"name": "open-llms", "sources": [{"author": "Qwen", "nameRegex": "^Qwen3-"}],
        "workflowTemplate": TEMPLATE, "maxParamsB": 14, "tokenSecretName": "hf-token",
        "pollInterval": "1h", "sizing": {"autoSizeSteps": ["finetune"]}}

STATUS = {"phase": "Watching", "lastPollTime": "2026-01-01T00:00:00Z", "activeRuns": 1,
          "candidates": [
              {"id": "Qwen/Qwen3-0.5B", "revision": "a" * 40, "phase": "Baseline", "firstSeen": "2026-01-01T00:00:00Z"},
              {"id": "Qwen/Qwen3-8B", "revision": "b" * 40, "paramsB": "8", "license": "apache-2.0",
               "gpus": 1, "phase": "Running", "workflow": "open-llms-qwen3-8b-bbbbbbb",
               "firstSeen": "2026-01-02T00:00:00Z"}]}


def seed(fake_k8s, status=None):
    obj = {"metadata": {"name": "open-llms"},
           "spec": {"sources": [{"provider": "huggingface", "author": "Qwen"}], "maxParamsB": 14,
                    "workflowTemplate": TEMPLATE}}
    if status is not None:
        obj["status"] = status
    fake_k8s.add("gryviamodelwatches", obj, namespace=NS)


def test_list_empty(make_client):
    r = make_client("model_watches").get("/api/model-watches")
    assert r.status_code == 200 and r.json() == {"items": []}


def test_get_shape(make_client, fake_k8s):
    seed(fake_k8s, STATUS)
    c = make_client("model_watches")
    item = c.get("/api/model-watches/open-llms").json()
    assert c.get("/api/model-watches").json()["items"] == [item]
    assert item["spec"] == {"sources": [{"provider": "huggingface", "author": "Qwen"}], "maxParamsB": 14,
                            "suspend": False, "steps": ["finetune"]}
    assert item["status"] == {"phase": "Watching", "lastPollTime": "2026-01-01T00:00:00Z", "activeRuns": 1,
                              "candidates": {"Baseline": 1, "Running": 1}}


def test_runs_newest_first(make_client, fake_k8s):
    seed(fake_k8s, STATUS)
    items = make_client("model_watches").get("/api/model-watches/open-llms/runs").json()["items"]
    assert [i["model"] for i in items] == ["Qwen/Qwen3-8B", "Qwen/Qwen3-0.5B"]
    assert items[0]["workflow"] == "open-llms-qwen3-8b-bbbbbbb" and items[0]["gpus"] == 1


def test_404(make_client):
    c = make_client("model_watches")
    assert c.get("/api/model-watches/nope").status_code == 404
    assert c.get("/api/model-watches/nope/runs").status_code == 404
    assert c.post("/api/model-watches/nope/suspend").status_code == 404


def test_create(make_client, fake_k8s):
    r = make_client("model_watches").post("/api/model-watches", json=BODY)
    assert r.status_code == 201, r.text
    stored = fake_k8s.store[("gryviamodelwatches", NS, "open-llms")]
    assert stored["kind"] == "GryviaModelWatch"
    assert stored["spec"] == {
        "sources": [{"provider": "huggingface", "author": "Qwen", "nameRegex": "^Qwen3-"}],
        "workflowTemplate": TEMPLATE, "maxParamsB": 14, "pollInterval": "1h",
        "tokenSecretRef": {"name": "hf-token"}, "sizing": {"autoSizeSteps": ["finetune"]}}


def test_create_rejects_bad_input(make_client):
    c = make_client("model_watches")
    for patch in ({"sources": []}, {"workflowTemplate": {}}, {"workflowTemplate": {"steps": []}},
                  {"pollInterval": "hourly"}, {"sources": [{"nameRegex": "("}]},
                  {"sources": [{"provider": "civitai"}]}, {"name": "Bad_Name"}, {"extra": 1}):
        assert c.post("/api/model-watches", json={**BODY, **patch}).status_code == 422, patch


def test_suspend_resume_delete(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("model_watches")
    assert c.post("/api/model-watches/open-llms/suspend").json() == {"status": "suspended", "name": "open-llms"}
    assert fake_k8s.store[("gryviamodelwatches", NS, "open-llms")]["spec"]["suspend"] is True
    assert c.post("/api/model-watches/open-llms/resume").status_code == 200
    assert fake_k8s.store[("gryviamodelwatches", NS, "open-llms")]["spec"]["suspend"] is False
    assert c.delete("/api/model-watches/open-llms").status_code == 200
    assert ("gryviamodelwatches", NS, "open-llms") not in fake_k8s.store
