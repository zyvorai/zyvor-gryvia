# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Tenant routes: admin manages, a tenant user sees only its own tenant."""
import pytest


@pytest.fixture
def seeded(fake_k8s):
    fake_k8s.add("gryviatenants", {"metadata": {"name": "alpha"}, "spec": {"displayName": "Alpha", "allowedSkus": ["a100"]}})
    fake_k8s.add("gryviatenants", {"metadata": {"name": "beta"}, "spec": {"displayName": "Beta"}})


def test_admin_lists_all(make_client, seeded):
    items = make_client("tenants").get("/api/tenants").json()["items"]
    assert [i["metadata"]["name"] for i in items] == ["alpha", "beta"]
    assert items[0]["spec"]["namespace"] == "tenant-alpha" and items[0]["spec"]["allowedSkus"] == ["a100"]


def test_tenant_sees_only_itself(make_client, seeded):
    c = make_client("tenants", role="tenant", tenants=["alpha"])
    assert [i["metadata"]["name"] for i in c.get("/api/tenants").json()["items"]] == ["alpha"]
    assert c.get("/api/tenants/alpha").status_code == 200
    assert c.get("/api/tenants/beta").status_code == 404


def test_admin_get_and_missing(make_client, seeded):
    c = make_client("tenants")
    assert c.get("/api/tenants/beta").json()["spec"]["displayName"] == "Beta"
    assert c.get("/api/tenants/nope").status_code == 404


def test_tenant_cannot_write(make_client, seeded, fake_k8s):
    c = make_client("tenants", role="tenant", tenants=["alpha"])
    assert c.post("/api/tenants", json={"name": "evil"}).status_code == 403
    assert c.delete("/api/tenants/beta").status_code == 403
    assert ("gryviatenants", None, "beta") in fake_k8s.store
    assert ("gryviatenants", None, "evil") not in fake_k8s.store


def test_admin_create_maps_to_spec(make_client, fake_k8s):
    c = make_client("tenants")
    r = c.post("/api/tenants", json={"name": "acme", "displayName": "Acme", "allowedSkus": ["h100"], "maxGPUs": 8})
    assert r.status_code == 201
    obj = fake_k8s.store[("gryviatenants", None, "acme")]
    assert obj["kind"] == "GryviaTenant"
    assert obj["spec"] == {"displayName": "Acme", "allowedSkus": ["h100"], "quotas": {"concurrentGPUs": 8},
                           "networkPolicy": {"isolated": True}}
    assert c.post("/api/tenants", json={"name": "acme"}).status_code == 409


def test_create_defaults_and_isolation_flag(make_client, fake_k8s):
    c = make_client("tenants")
    assert c.post("/api/tenants", json={"name": "open", "isolated": False}).status_code == 201
    assert fake_k8s.store[("gryviatenants", None, "open")]["spec"] == {"networkPolicy": {"isolated": False}}


@pytest.mark.parametrize("body", [{"name": "Bad"}, {"name": "-x"}, {"name": "a" * 60}, {"name": "x", "maxGPUs": -1},
                                  {"name": "x", "junk": 1}, {}])
def test_create_validation(make_client, body):
    assert make_client("tenants").post("/api/tenants", json=body).status_code == 422


def test_admin_delete(make_client, seeded, fake_k8s):
    c = make_client("tenants")
    assert c.delete("/api/tenants/alpha").json()["status"] == "deleted"
    assert ("gryviatenants", None, "alpha") not in fake_k8s.store
    assert c.delete("/api/tenants/alpha").status_code == 404
