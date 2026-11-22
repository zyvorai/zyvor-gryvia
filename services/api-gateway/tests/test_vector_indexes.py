# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

NS = "default"
BODY = {"name": "handbook", "datasetRef": "docs", "embedding": {"model": "embed", "batchSize": 16},
        "chunking": {"size": 800, "overlap": 100}, "schedule": "0 3 * * *"}
STATUS = {"phase": "Ready", "message": "Serving 12 chunks",
          "storeURL": "http://handbook-qdrant.default.svc.cluster.local:6333", "collection": "handbook",
          "documents": 3, "chunks": 12, "dimensions": 384, "datasetVersion": "v1",
          "lastIngested": "2026-01-01T00:00:00Z", "keySecret": "handbook-llm-key"}


def seed(fake_k8s, status=None, **spec):
    obj = {"metadata": {"name": "handbook"},
           "spec": {"datasetRef": "docs", "embedding": {"model": "embed"}, "store": {"type": "managed"}, **spec}}
    if status is not None:
        obj["status"] = status
    fake_k8s.add("gryviavectorindexes", obj, namespace=NS)


def stored(fake_k8s):
    return fake_k8s.store[("gryviavectorindexes", NS, "handbook")]


def test_list_and_get(make_client, fake_k8s):
    seed(fake_k8s, STATUS)
    c = make_client("vector_indexes")
    body = c.get("/api/vector-indexes").json()
    item = c.get("/api/vector-indexes/handbook").json()
    assert body["items"] == [item]
    assert item["spec"] == {"datasetRef": "docs", "embeddingModel": "embed", "store": "managed", "suspend": False}
    assert item["status"]["chunks"] == 12 and item["status"]["phase"] == "Ready"
    assert item["status"]["keySecret"] == "handbook-llm-key"


def test_404(make_client):
    c = make_client("vector_indexes")
    assert c.get("/api/vector-indexes/nope").status_code == 404
    assert c.post("/api/vector-indexes/nope/reingest").status_code == 404
    assert c.delete("/api/vector-indexes/nope").status_code == 404


def test_create_managed(make_client, fake_k8s):
    r = make_client("vector_indexes").post("/api/vector-indexes", json={**BODY, "store": {"storageSize": "5Gi"}})
    assert r.status_code == 201, r.text
    s = stored(fake_k8s)
    assert s["kind"] == "GryviaVectorIndex"
    assert s["spec"] == {"datasetRef": "docs", "embedding": {"model": "embed", "batchSize": 16},
                         "chunking": {"size": 800, "overlap": 100},
                         "store": {"type": "managed", "managed": {"storageSize": "5Gi"}},
                         "schedule": "0 3 * * *"}


def test_create_external(make_client, fake_k8s):
    store = {"type": "external", "url": "https://qdrant.example.com",
             "apiKeySecretName": "qdrant", "apiKeySecretKey": "apiKey"}
    r = make_client("vector_indexes").post("/api/vector-indexes", json={**BODY, "store": store, "collection": "hb"})
    assert r.status_code == 201, r.text
    spec = stored(fake_k8s)["spec"]
    assert spec["store"] == {"type": "external", "external": {"url": "https://qdrant.example.com",
                                                              "apiKeySecretRef": {"name": "qdrant", "key": "apiKey"}}}
    assert spec["collection"] == "hb"


def test_create_rejects_bad_input(make_client):
    c = make_client("vector_indexes")
    bad = (
        {"name": "Bad_Name"}, {"datasetRef": ""}, {"embedding": {"model": "bad model"}},
        {"chunking": {"size": 100, "overlap": 100}}, {"chunking": {"size": 10}}, {"chunking": {"overlap": 1000}},
        {"store": {"type": "external"}}, {"store": {"type": "external", "url": "ftp://x"}},
        {"store": {"url": "https://q"}}, {"store": {"type": "external", "url": "https://q", "storageSize": "1Gi"}},
        {"store": {"storageSize": "lots"}}, {"collection": "a/b"}, {"extra": 1},
    )
    for patch in bad:
        assert c.post("/api/vector-indexes", json={**BODY, **patch}).status_code == 422, patch


def test_reingest_sets_annotation(make_client, fake_k8s):
    seed(fake_k8s, STATUS)
    c = make_client("vector_indexes")
    r = c.post("/api/vector-indexes/handbook/reingest")
    assert r.status_code == 200 and r.json()["status"] == "reingesting"
    first = stored(fake_k8s)["metadata"]["annotations"]["gryvia.io/reingest"]
    assert first == r.json()["reingest"]
    second = c.post("/api/vector-indexes/handbook/reingest").json()["reingest"]
    assert second != first


def test_reingest_suspended_is_409(make_client, fake_k8s):
    seed(fake_k8s, STATUS, suspend=True)
    assert make_client("vector_indexes").post("/api/vector-indexes/handbook/reingest").status_code == 409


def test_suspend_resume_delete(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("vector_indexes")
    assert c.post("/api/vector-indexes/handbook/suspend").json() == {"status": "suspended", "name": "handbook"}
    assert stored(fake_k8s)["spec"]["suspend"] is True
    assert c.post("/api/vector-indexes/handbook/resume").status_code == 200
    assert stored(fake_k8s)["spec"]["suspend"] is False
    assert c.delete("/api/vector-indexes/handbook").status_code == 200
    assert ("gryviavectorindexes", NS, "handbook") not in fake_k8s.store
