# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import json

NS = "tenant-alpha"


def ds(fake_k8s, name="sales", ns=NS, annotations=None, versions=None, desc="", pvc="sales-pvc"):
    meta = {"name": name}
    if annotations:
        meta["annotations"] = annotations
    fake_k8s.add("gryviadatasets", {"metadata": meta, "spec": {"namespace": ns, "description": desc},
                                    "status": {"state": "Ready", "pvcName": pvc, "currentVersion": "v2",
                                               "versions": versions or []}})


def chain(fake_k8s):
    fake_k8s.add("gryviaaijobs", {"metadata": {"name": "train"}, "spec": {"volumes": [
        {"name": "d", "persistentVolumeClaim": {"claimName": "sales-pvc"}}]}}, namespace=NS)
    fake_k8s.add("gryviamodelregistries", {"metadata": {"name": "m"}, "spec": {"source": {"jobRef": "train"}}}, namespace=NS)
    fake_k8s.add("gryviainferenceservices", {"metadata": {"name": "svc"}, "spec": {"modelRef": "m"}}, namespace=NS)


def test_lists_with_metadata_freshness_and_used_by(make_client, fake_k8s):
    ds(fake_k8s, annotations={"gryvia.io/owner": "data-team", "gryvia.io/tags": "pii,sales",
                              "gryvia.io/schema": json.dumps([{"name": "id", "type": "int"}])},
       versions=[{"version": "v1", "createdAt": "2026-01-01T00:00:00Z"}, {"version": "v2", "createdAt": "2026-02-01T00:00:00Z"}],
       desc="Orders")
    chain(fake_k8s)
    r = make_client("data_catalog").get("/api/data-catalog").json()
    e = r["items"][0]
    assert (e["owner"], e["tags"], e["description"], e["updated"]) == ("data-team", ["pii", "sales"], "Orders", "2026-02-01T00:00:00Z")
    assert e["columns"] == [{"name": "id", "type": "int"}]
    assert e["usedBy"] == ["job/train", "model/m", "service/svc"]
    assert r["facets"] == {"tags": {"pii": 1, "sales": 1}, "owners": {"data-team": 1}}


def test_search_and_filters(make_client, fake_k8s):
    ds(fake_k8s, "sales", annotations={"gryvia.io/tags": "finance", "gryvia.io/schema": json.dumps([{"name": "revenue"}])})
    ds(fake_k8s, "logs", annotations={"gryvia.io/owner": "ops"}, pvc="l")
    c = make_client("data_catalog")
    names = lambda **p: [i["name"] for i in c.get("/api/data-catalog", params=p).json()["items"]]
    assert names() == ["logs", "sales"]
    assert names(q="REVENUE") == ["sales"]  # matches column names, case-insensitively
    assert names(tag="finance") == ["sales"] and names(owner="ops") == ["logs"] and names(q="zzz") == []


def test_tenant_sees_own_namespace_and_not_marked(make_client, fake_k8s):
    ds(fake_k8s, "mine")
    ds(fake_k8s, "theirs", ns="tenant-beta", pvc="x")
    ds(fake_k8s, "secret", annotations={"gryvia.io/marking": "pii"}, pvc="y")
    t = make_client("data_catalog", role="tenant", tenants=["alpha"])
    assert [i["name"] for i in t.get("/api/data-catalog").json()["items"]] == ["mine"]


def test_edit_sets_and_clears(make_client, fake_k8s):
    ds(fake_k8s)
    c = make_client("data_catalog", role="tenant", tenants=["alpha"])
    r = c.put("/api/data-catalog/sales", json={"owner": " me ", "description": "D", "tags": ["B", "a", "a"],
                                              "columns": [{"name": "id", "type": "int"}, {"name": "x"}]})
    assert r.status_code == 200, r.text
    e = r.json()
    assert (e["owner"], e["tags"], e["description"]) == ("me", ["a", "b"], "D")
    assert e["columns"] == [{"name": "id", "type": "int"}, {"name": "x"}]
    cleared = c.put("/api/data-catalog/sales", json={}).json()
    assert cleared["owner"] == "" and cleared["tags"] == [] and cleared["columns"] == []


def test_edit_rules(make_client, fake_k8s):
    ds(fake_k8s)
    ds(fake_k8s, "theirs", ns="tenant-beta", pvc="x")
    t = make_client("data_catalog", role="tenant", tenants=["alpha"])
    assert t.put("/api/data-catalog/theirs", json={}).status_code == 404
    assert t.put("/api/data-catalog/nope", json={}).status_code == 404
    assert t.put("/api/data-catalog/sales", json={"tags": ["Bad Tag!"]}).status_code == 422
    assert t.put("/api/data-catalog/sales", json={"columns": [{"name": "a"}, {"name": "a"}]}).status_code == 422
    assert t.put("/api/data-catalog/sales", json={"tags": [str(i) for i in range(11)]}).status_code == 422
