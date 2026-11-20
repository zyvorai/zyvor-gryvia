# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import pytest

NS = "default"


@pytest.fixture
def client(make_client):
    return make_client("security")


def valid(**over):
    return {"name": "gpu-guard", "targetNamespaces": ["ml"],
            "detectionRules": [{"type": "mining", "enabled": True, "sensitivity": "high"}],
            "autoBlock": True, **over}


def test_empty(client):
    assert client.get("/api/security/alerts").json() == {"items": [], "eventSource": False}
    assert client.get("/api/security/policies").json() == {"items": []}


def test_alerts_never_gryviaated(client, fake_k8s):
    fake_k8s.add("gryviasecuritypolicies", {"metadata": {"name": "p"}, "spec": {},
                                            "status": {"alertsTriggered": 5, "detectionCounts": {"mining": 5}}}, NS)
    assert client.get("/api/security/alerts").json() == {"items": [], "eventSource": False}


def test_alerts_survive_unreadable_policies_crd(make_client, fake_k8s):
    from kubernetes.client.exceptions import ApiException

    def boom(*a, **kw):
        raise ApiException(status=403, reason="Forbidden")

    fake_k8s.list_cluster_custom_object = boom
    r = make_client("security").get("/api/security/alerts")
    assert r.status_code == 200
    assert r.json() == {"items": [], "eventSource": False}


def test_list_shape(client, fake_k8s):
    fake_k8s.add("gryviasecuritypolicies", {"metadata": {"name": "p"}, "spec": {
        "targetNamespaces": ["ml"], "detectionRules": [{"type": "escape", "enabled": True, "sensitivity": "low"}],
        "autoBlock": False}, "status": {"phase": "Active", "activeDetections": 1, "alertsTriggered": 2,
                                        "detectionCounts": {"escape": 2}}}, NS)
    p = client.get("/api/security/policies").json()["items"][0]
    assert p["spec"]["detectionRules"][0]["type"] == "escape" and p["spec"]["autoBlock"] is False
    assert p["status"]["alertsTriggered"] == 2 and p["status"]["detectionCounts"] == {"escape": 2}


def test_create(client, fake_k8s):
    r = client.post("/api/security/policies", json=valid(alertWebhook="https://hooks.example.com/x"))
    assert r.status_code == 201
    assert r.json()["metadata"]["name"] == "gpu-guard"
    stored = fake_k8s.store[("gryviasecuritypolicies", NS, "gpu-guard")]
    assert stored["spec"]["alertWebhook"] == "https://hooks.example.com/x"
    assert stored["spec"]["detectionRules"][0]["sensitivity"] == "high"
    assert client.post("/api/security/policies", json=valid()).status_code == 409


@pytest.mark.parametrize("over", [
    {"name": "Bad_Name"}, {"name": ""}, {"name": "a" * 64}, {"targetNamespaces": []},
    {"targetNamespaces": ["Bad NS"]}, {"detectionRules": []},
    {"detectionRules": [{"type": "nope", "enabled": True, "sensitivity": "low"}]},
    {"detectionRules": [{"type": "mining", "enabled": True, "sensitivity": "extreme"}]},
    {"alertWebhook": "ftp://x"}, {"alertWebhook": "javascript:alert(1)"},
])
def test_create_validation(client, over):
    assert client.post("/api/security/policies", json=valid(**over)).status_code == 422
