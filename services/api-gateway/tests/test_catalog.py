# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""GPU SKU catalog: visibility per tenant and admin-only writes."""
import pytest


def sku(name, gpu_type, rate=2.0, enabled=True):
    return {"metadata": {"name": name}, "spec": {"gpuType": gpu_type, "gpusPerUnit": 1, "hourlyRate": rate,
                                                  "currency": "USD", "enabled": enabled}}


@pytest.fixture
def seeded(fake_k8s):
    fake_k8s.add("gryviagpuskus", sku("h100", "H100", 8))
    fake_k8s.add("gryviagpuskus", sku("a100", "A100-80G", 4))
    fake_k8s.add("gryviagpuskus", sku("t4", "T4", 1, enabled=False))
    fake_k8s.add("gryviatenants", {"metadata": {"name": "alpha"}, "spec": {"allowedSkus": ["a100"]}})
    fake_k8s.add("gryviatenants", {"metadata": {"name": "beta"}, "spec": {}})


def names(resp):
    return [i["metadata"]["name"] for i in resp.json()["items"]]


def test_admin_sees_all_skus_including_disabled(make_client, seeded):
    r = make_client("catalog").get("/api/skus")
    assert r.status_code == 200
    assert names(r) == ["a100", "h100", "t4"]


def test_tenant_with_allowed_skus_sees_only_those(make_client, seeded):
    assert names(make_client("catalog", role="tenant", tenants=["alpha"]).get("/api/skus")) == ["a100"]


def test_tenant_without_allowed_skus_sees_enabled_only(make_client, seeded):
    assert names(make_client("catalog", role="tenant", tenants=["beta"]).get("/api/skus")) == ["a100", "h100"]


def test_allowed_sku_may_name_the_gpu_type(make_client, seeded, fake_k8s):
    fake_k8s.add("gryviatenants", {"metadata": {"name": "gamma"}, "spec": {"allowedSkus": ["H100"]}})
    assert names(make_client("catalog", role="tenant", tenants=["gamma"]).get("/api/skus")) == ["h100"]


def test_tenant_cannot_get_a_hidden_sku(make_client, seeded):
    c = make_client("catalog", role="tenant", tenants=["alpha"])
    assert c.get("/api/skus/a100").status_code == 200
    assert c.get("/api/skus/h100").status_code == 404
    assert c.get("/api/skus/t4").status_code == 404


def test_writes_are_admin_only(make_client, seeded, fake_k8s):
    c = make_client("catalog", role="tenant", tenants=["beta"])
    body = {"name": "x", "gpuType": "L40", "hourlyRate": 1}
    assert c.post("/api/skus", json=body).status_code == 403
    assert c.put("/api/skus/h100", json={"gpuType": "H100", "hourlyRate": 0}).status_code == 403
    assert c.delete("/api/skus/h100").status_code == 403
    assert fake_k8s.store[("gryviagpuskus", None, "h100")]["spec"]["hourlyRate"] == 8


def test_admin_create_update_delete(make_client, fake_k8s):
    c = make_client("catalog")
    r = c.post("/api/skus", json={"name": "l40-1x", "gpuType": "L40", "hourlyRate": 2.5, "description": "d"})
    assert r.status_code == 201
    assert r.json()["spec"]["hourlyRate"] == 2.5 and r.json()["spec"]["currency"] == "USD"
    assert fake_k8s.store[("gryviagpuskus", None, "l40-1x")]["kind"] == "GryviaGpuSku"
    assert c.post("/api/skus", json={"name": "l40-1x", "gpuType": "L40", "hourlyRate": 1}).status_code == 409
    r = c.put("/api/skus/l40-1x", json={"gpuType": "L40", "hourlyRate": 3, "enabled": False})
    assert r.status_code == 200 and r.json()["spec"]["hourlyRate"] == 3 and r.json()["spec"]["enabled"] is False
    assert c.put("/api/skus/nope", json={"gpuType": "L40", "hourlyRate": 3}).status_code == 404
    assert c.delete("/api/skus/l40-1x").json() == {"status": "deleted", "name": "l40-1x"}
    assert c.delete("/api/skus/l40-1x").status_code == 404


@pytest.mark.parametrize("body", [
    {"name": "x", "gpuType": "L40", "hourlyRate": -1},
    {"name": "Bad_Name", "gpuType": "L40", "hourlyRate": 1},
    {"name": "x", "gpuType": "", "hourlyRate": 1},
    {"name": "x", "gpuType": "L40"},
    {"name": "x", "gpuType": "L40", "hourlyRate": 1, "currency": "usd"},
    {"name": "x", "gpuType": "L40", "hourlyRate": 1, "spotDiscount": 101},
    {"name": "x", "gpuType": "L40", "hourlyRate": 1, "gpusPerUnit": 0},
    {"name": "x", "gpuType": "L40", "hourlyRate": 1, "extra": 1},
])
def test_validation(make_client, body):
    assert make_client("catalog").post("/api/skus", json=body).status_code == 422
