# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

NS = "default"


def model(fake_k8s, name="bert", marking=None):
    meta = {"name": name}
    if marking:
        meta["annotations"] = {"gryvia.io/marking": marking}
    fake_k8s.add("gryviamodelregistries", {"metadata": meta, "spec": {"version": "v1", "stage": "dev"}}, namespace=NS)


def dataset(fake_k8s, name="ds", marking=None):
    meta = {"name": name}
    if marking:
        meta["annotations"] = {"gryvia.io/marking": marking}
    fake_k8s.add("gryviadatasets", {"metadata": meta, "spec": {"namespace": NS}, "status": {"state": "Ready"}})


def with_groups(c, groups):
    """The stub auth does not set claims; add them the way verify_auth does for OIDC users."""
    from starlette.middleware.base import BaseHTTPMiddleware

    async def mw(request, call_next):
        request.state.user_claims = {"groups": list(groups)}
        return await call_next(request)

    # runs before the route dependency (which only sets role/tenants), so claims survive
    c.app.add_middleware(BaseHTTPMiddleware, dispatch=mw)
    return c


def test_unmarked_resources_are_unchanged(make_client, fake_k8s):
    model(fake_k8s)
    assert make_client("models").get("/api/models/bert").status_code == 200


def test_marked_model_hidden_from_uncleared_tenant_but_not_admin(make_client, fake_k8s):
    fake_k8s.add("gryviamodelregistries", {"metadata": {"name": "bert", "annotations": {"gryvia.io/marking": "pii"}},
                                           "spec": {"version": "v1", "stage": "dev"}}, namespace="tenant-alpha")
    t = make_client("models", role="tenant", tenants=["alpha"])
    assert t.get("/api/models").json() == {"items": []}
    assert t.get("/api/models/bert").status_code == 404
    assert t.post("/api/models/bert/promote", json={"targetStage": "staging"}).status_code == 404


def test_cleared_tenant_sees_it(make_client, fake_k8s):
    fake_k8s.add("gryviamodelregistries", {"metadata": {"name": "bert", "annotations": {"gryvia.io/marking": "pii"}},
                                           "spec": {"version": "v1", "stage": "dev"}}, namespace="tenant-alpha")
    t = with_groups(make_client("models", role="tenant", tenants=["alpha"]), ["gryvia-marking-pii"])
    item = t.get("/api/models/bert").json()
    assert item["metadata"]["marking"] == "pii"
    other = with_groups(make_client("models", role="tenant", tenants=["alpha"]), ["gryvia-marking-other"])
    assert other.get("/api/models/bert").status_code == 404


def test_marked_dataset_hidden_and_not_deletable(make_client, fake_k8s):
    fake_k8s.add("gryviadatasets", {"metadata": {"name": "ds", "annotations": {"gryvia.io/marking": "pii"}},
                                    "spec": {"namespace": "tenant-alpha"}})
    t = make_client("datasets", role="tenant", tenants=["alpha"])
    assert t.get("/api/datasets").json()["items"] == []
    assert t.delete("/api/datasets/ds").status_code == 404
    assert ("gryviadatasets", None, "ds") in fake_k8s.store


def test_lineage_omits_marked_nodes(make_client, fake_k8s):
    dataset(fake_k8s, marking="pii")
    fake_k8s.store[("gryviadatasets", None, "ds")]["spec"]["namespace"] = "tenant-alpha"
    g = make_client("lineage", role="tenant", tenants=["alpha"]).get("/api/lineage").json()
    assert g["nodes"] == []
    assert len(make_client("lineage").get("/api/lineage").json()["nodes"]) == 1


def test_set_and_clear_marking(make_client, fake_k8s):
    model(fake_k8s)
    c = make_client("markings")
    r = c.put("/api/markings/models/bert", json={"marking": "pii"})
    assert r.status_code == 200 and r.json()["group"] == "gryvia-marking-pii"
    assert fake_k8s.store[("gryviamodelregistries", NS, "bert")]["metadata"]["annotations"]["gryvia.io/marking"] == "pii"
    assert c.put("/api/markings/models/bert", json={"marking": ""}).json()["marking"] is None
    dataset(fake_k8s)
    assert c.put("/api/markings/datasets/ds", json={"marking": "secret"}).status_code == 200


def test_set_marking_rules(make_client, fake_k8s):
    model(fake_k8s)
    assert make_client("markings", role="tenant", tenants=["alpha"]).put(
        "/api/markings/models/bert", json={"marking": "pii"}).status_code == 403
    c = make_client("markings")
    assert c.put("/api/markings/models/bert", json={"marking": "Not Valid"}).status_code == 422
    assert c.put("/api/markings/pods/bert", json={"marking": "pii"}).status_code == 404
    assert c.put("/api/markings/models/nope", json={"marking": "pii"}).status_code == 404
