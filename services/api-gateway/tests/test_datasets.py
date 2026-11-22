# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

BODY = {"name": "corpus", "type": "text", "version": "v1",
        "source": {"type": "http", "http": {"url": "https://example.com/c.jsonl", "checksumURL": "https://example.com/c.sha256"}},
        "size": "20Gi", "retention": {"keepLast": 3}}


def seed(fake_k8s, name="corpus", namespace="tenant-alpha", status=None):
    obj = {"metadata": {"name": name},
           "spec": {"type": "text", "namespace": namespace,
                    "source": {"type": "s3", "s3": {"bucket": "data", "prefix": "corpus/"}}}}
    if status:
        obj["status"] = status
    fake_k8s.add("gryviadatasets", obj)


def test_list_and_get_shape(make_client, fake_k8s):
    seed(fake_k8s, status={"state": "ready", "pvcName": "dataset-corpus", "subPath": "latest", "currentVersion": "latest",
                           "fileCount": 2, "totalSizeBytes": 10, "namespace": "tenant-alpha",
                           "versions": [{"version": "latest", "size": 10, "checksum": "abc"}]})
    c = make_client("datasets")
    item = c.get("/api/datasets/corpus").json()
    assert c.get("/api/datasets").json()["items"] == [item]
    assert item["spec"] == {"type": "text", "namespace": "tenant-alpha", "source": {"type": "s3", "location": "s3://data/corpus/"}}
    assert item["status"]["state"] == "ready" and item["status"]["pvcName"] == "dataset-corpus"
    assert item["status"]["versions"] == [{"version": "latest", "size": 10, "checksum": "abc"}]


def test_tenant_sees_only_own_namespaces(make_client, fake_k8s):
    seed(fake_k8s, "mine", "tenant-alpha")
    seed(fake_k8s, "theirs", "tenant-beta")
    c = make_client("datasets", role="tenant", tenants=["alpha"])
    assert [i["metadata"]["name"] for i in c.get("/api/datasets").json()["items"]] == ["mine"]
    assert c.get("/api/datasets/theirs").status_code == 404
    assert c.delete("/api/datasets/theirs").status_code == 404
    assert ("gryviadatasets", None, "theirs") in fake_k8s.store
    assert c.delete("/api/datasets/mine").json() == {"status": "deleted", "name": "mine"}
    assert ("gryviadatasets", None, "mine") not in fake_k8s.store


def test_create_admin(make_client, fake_k8s):
    r = make_client("datasets").post("/api/datasets", json={**BODY, "namespace": "ml"})
    assert r.status_code == 201, r.text
    stored = fake_k8s.store[("gryviadatasets", None, "corpus")]
    assert stored["kind"] == "GryviaDataset"
    assert stored["spec"] == {
        "source": {"type": "http", "http": {"url": "https://example.com/c.jsonl", "checksumURL": "https://example.com/c.sha256"}},
        "type": "text", "version": "v1", "namespace": "ml", "cache": {"size": "20Gi"},
        "versioning": {"enabled": True, "retentionPolicy": {"keepLast": 3}}}


def test_create_s3_compatible_endpoint(make_client, fake_k8s):
    s3 = {"bucket": "data", "prefix": "corpus/", "region": "us-east-1", "endpoint": "http://minio.storage.svc:9000",
          "credentialsSecret": "minio"}
    r = make_client("datasets").post("/api/datasets", json={**BODY, "source": {"type": "s3", "s3": s3}})
    assert r.status_code == 201, r.text
    stored = fake_k8s.store[("gryviadatasets", None, "corpus")]["spec"]["source"]["s3"]
    assert stored["endpoint"] == "http://minio.storage.svc:9000"
    bad = {"type": "s3", "s3": {**s3, "endpoint": "minio:9000"}}
    assert make_client("datasets").post("/api/datasets", json={**BODY, "name": "bad", "source": bad}).status_code == 422


def test_create_tenant_is_pinned_to_own_namespace(make_client, fake_k8s):
    c = make_client("datasets", role="tenant", tenants=["alpha"])
    assert c.post("/api/datasets", json=BODY).status_code == 201
    assert fake_k8s.store[("gryviadatasets", None, "corpus")]["spec"]["namespace"] == "tenant-alpha"
    assert c.post("/api/datasets", json={**BODY, "name": "other", "namespace": "tenant-beta"}).status_code == 403


def test_create_rejects_bad_input(make_client):
    c = make_client("datasets")
    for patch in ({"source": {"type": "http"}}, {"source": {"type": "gcs", "gcs": {}}},
                  {"source": {"type": "http", "http": {"url": "ftp://x"}}},
                  {"source": {"type": "nfs", "nfs": {"server": "s", "path": "rel"}}},
                  {"version": "../x"}, {"size": "lots"}, {"name": "Bad_Name"}, {"extra": 1}):
        assert c.post("/api/datasets", json={**BODY, **patch}).status_code == 422, patch


def test_404(make_client):
    c = make_client("datasets")
    assert c.get("/api/datasets/nope").status_code == 404
    assert c.delete("/api/datasets/nope").status_code == 404
