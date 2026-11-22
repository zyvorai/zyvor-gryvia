# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

NS = "default"
BODY = {
    "name": "notify", "events": ["Failed", "Succeeded"], "kinds": ["GryviaAIJob"], "matchLabels": {"team": "nlp"},
    "url": "https://hooks.example.com/gryvia", "headers": {"X-Team": "nlp"}, "secretName": "hook-secret",
    "timeoutSeconds": 5, "attempts": 4, "backoffSeconds": 30,
}
STATUS = {
    "conditions": [{"type": "Ready", "status": "True", "reason": "Valid", "message": "Delivering"}],
    "deliveries": 3, "failures": 1,
    "lastDelivery": {"kind": "GryviaAIJob", "name": "train", "event": "Failed", "attempt": 1, "succeeded": True},
}


def stored(fake_k8s):
    return fake_k8s.store[("gryviajobhooks", NS, "notify")]


def test_create_builds_spec(make_client, fake_k8s):
    r = make_client("jobhooks").post("/api/job-hooks", json=BODY)
    assert r.status_code == 201, r.text
    s = stored(fake_k8s)
    assert s["kind"] == "GryviaJobHook"
    assert s["spec"] == {
        "events": ["Failed", "Succeeded"], "kinds": ["GryviaAIJob"], "selector": {"matchLabels": {"team": "nlp"}},
        "webhook": {"url": "https://hooks.example.com/gryvia", "format": "gryvia", "headers": {"X-Team": "nlp"},
                    "secretRef": {"name": "hook-secret"}, "timeoutSeconds": 5},
        "retry": {"attempts": 4, "backoffSeconds": 30},
    }


def test_create_minimal_slack(make_client, fake_k8s):
    r = make_client("jobhooks").post("/api/job-hooks", json={
        "name": "notify", "events": ["Failed"], "url": "https://hooks.slack.com/services/T/B/X", "format": "slack"})
    assert r.status_code == 201, r.text
    assert stored(fake_k8s)["spec"] == {
        "events": ["Failed"], "webhook": {"url": "https://hooks.slack.com/services/T/B/X", "format": "slack"}}


def test_create_validation(make_client):
    c = make_client("jobhooks")
    base = {"name": "h", "events": ["Failed"], "url": "https://x.example/h"}
    for change in (
        {"events": []}, {"events": ["Exploded"]}, {"events": ["Failed", "Failed"]}, {"kinds": ["Pod"]},
        {"url": "ftp://x/"}, {"url": "https://user:pw@x.example/"}, {"url": "https://x.example/#frag"},
        {"headers": {"X-Gryvia-Signature": "forged"}}, {"headers": {"Host": "evil"}}, {"headers": {"X-A": "a\nb"}},
        {"matchLabels": {"bad key!": "v"}}, {"secretName": "Bad_Name"}, {"timeoutSeconds": 31}, {"attempts": 11},
        {"format": "xml"}, {"extra": 1},
    ):
        assert c.post("/api/job-hooks", json={**base, **change}).status_code == 422, change


def test_list_get_suspend_delete(make_client, fake_k8s):
    fake_k8s.add("gryviajobhooks", {"metadata": {"name": "notify"}, "spec": {
        "events": ["Failed"], "webhook": {"url": "https://x.example/h", "headers": {"Authorization": "Bearer t"},
                                          "secretRef": {"name": "s"}}}, "status": STATUS}, namespace=NS)
    c = make_client("jobhooks")
    item = c.get("/api/job-hooks/notify").json()
    assert c.get("/api/job-hooks").json()["items"] == [item]
    assert item["spec"]["headers"] == ["Authorization"], "header values must not be returned"
    assert item["spec"]["secretName"] == "s"
    assert item["status"]["ready"] == "True" and item["status"]["deliveries"] == 3 and item["status"]["failures"] == 1
    assert item["status"]["lastDelivery"]["name"] == "train"

    r = c.post("/api/job-hooks/notify/suspend", json={"suspend": True})
    assert r.status_code == 200 and r.json()["status"] == "suspended"
    assert stored(fake_k8s)["spec"]["suspend"] is True
    assert c.post("/api/job-hooks/notify/suspend", json={"suspend": False}).json()["status"] == "resumed"
    assert c.delete("/api/job-hooks/notify").status_code == 200
    assert ("gryviajobhooks", NS, "notify") not in fake_k8s.store


def test_404(make_client):
    c = make_client("jobhooks")
    assert c.get("/api/job-hooks/nope").status_code == 404
    assert c.post("/api/job-hooks/nope/suspend", json={"suspend": True}).status_code == 404
    assert c.delete("/api/job-hooks/nope").status_code == 404


def test_tenant_sees_only_own_namespace(make_client, fake_k8s):
    spec = {"events": ["Failed"], "webhook": {"url": "https://x.example/h"}}
    fake_k8s.add("gryviajobhooks", {"metadata": {"name": "other"}, "spec": spec}, namespace="tenant-beta")
    fake_k8s.add("gryviajobhooks", {"metadata": {"name": "mine"}, "spec": spec}, namespace="tenant-alpha")
    c = make_client("jobhooks", role="tenant", tenants=["alpha"])
    assert [i["metadata"]["name"] for i in c.get("/api/job-hooks").json()["items"]] == ["mine"]
    assert c.get("/api/job-hooks/other").status_code == 404
    r = c.post("/api/job-hooks", json={"name": "new", "events": ["Failed"], "url": "https://x.example/h"})
    assert r.status_code == 201
    assert ("gryviajobhooks", "tenant-alpha", "new") in fake_k8s.store
