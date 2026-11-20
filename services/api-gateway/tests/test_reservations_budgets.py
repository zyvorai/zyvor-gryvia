# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Reservation and budget routes: scoping, validation, shapes."""


def res(name, owner, state="active", nodes=("n1",)):
    return {"metadata": {"name": name},
            "spec": {"owner": {"type": "tenant", "name": owner}, "resources": {"gpuType": "H100", "gpuCount": 8},
                     "schedule": {"type": "scheduled", "endTime": "2030-01-01T00:00:00Z"},
                     "guarantees": {"exclusive": True}},
            "status": {"state": state, "allocatedNodes": list(nodes), "allocatedGPUs": 8}}


def budget(name, scope, kind="team"):
    return {"metadata": {"name": name},
            "spec": {"scope": {"type": kind, "name": scope}, "period": {"type": "monthly"},
                     "limits": {"costUSD": 100}, "enforcement": {"enabled": True, "action": "block"}},
            "status": {"state": "warning", "usage": {"costUSD": 60, "gpuHours": 5},
                       "utilization": {"costPercent": 60}, "forecast": {
                           "projectedCostUSD": 90, "budgetSufficient": True},
                       "currentPeriod": {"daysRemaining": 3}}}


BODY = {"gpuType": "H100", "gpuCount": 8, "scheduleType": "scheduled",
        "startTime": "2030-01-01T00:00:00Z", "endTime": "2030-01-02T00:00:00Z"}


def test_list_scoping(make_client, fake_k8s):
    fake_k8s.add("gryviareservations", res("r-a", "alpha"))
    fake_k8s.add("gryviareservations", res("r-b", "tenant-beta"))
    assert {i["name"] for i in make_client("reservations").get("/api/reservations").json()["items"]} == {"r-a", "r-b"}
    mine = make_client("reservations", role="tenant", tenants=["alpha"]).get("/api/reservations").json()["items"]
    assert [i["name"] for i in mine] == ["r-a"]
    item = mine[0]
    assert item["allocatedNodes"] == ["n1"] and item["exclusive"] is True and item["state"] == "active"
    assert item["schedule"]["recurrence"] is None and item["gpuCount"] == 8


def test_tenant_create_forces_owner(make_client, fake_k8s):
    c = make_client("reservations", role="tenant", tenants=["alpha"])
    r = c.post("/api/reservations", json={**BODY, "name": "mine"})
    assert r.status_code == 201
    assert fake_k8s.store[("gryviareservations", None, "mine")]["spec"]["owner"] == {"type": "tenant", "name": "alpha"}
    assert c.post("/api/reservations", json={**BODY, "owner": {"type": "team", "name": "beta"}}).status_code == 403


def test_admin_needs_owner_and_valid_schedule(make_client, fake_k8s):
    c = make_client("reservations")
    assert c.post("/api/reservations", json=BODY).status_code == 400
    owner = {"owner": {"type": "team", "name": "alpha"}}
    r = c.post("/api/reservations", json={**BODY, **owner})
    assert r.status_code == 201 and r.json()["name"].startswith("res-")
    assert c.post("/api/reservations", json={**BODY, **owner, "endTime": "2029-01-01T00:00:00Z"}).status_code == 400
    assert c.post("/api/reservations", json={**BODY, **owner, "startTime": "nope"}).status_code == 400
    assert c.post("/api/reservations", json={**BODY, **owner, "scheduleType": "recurring"}).status_code == 400
    assert c.post("/api/reservations", json={**BODY, **owner, "name": "Bad_Name"}).status_code == 422
    assert c.post("/api/reservations", json={**BODY, **owner, "gpuCount": 0}).status_code == 422


def test_delete_scoping(make_client, fake_k8s):
    fake_k8s.add("gryviareservations", res("r-a", "alpha"))
    fake_k8s.add("gryviareservations", res("r-b", "beta"))
    t = make_client("reservations", role="tenant", tenants=["alpha"])
    assert t.delete("/api/reservations/r-b").status_code == 404
    assert t.delete("/api/reservations/r-a").status_code == 200
    assert make_client("reservations").delete("/api/reservations/r-b").status_code == 200
    assert not [k for k in fake_k8s.store if k[0] == "gryviareservations"]


def test_budgets_scoping_and_shape(make_client, fake_k8s):
    fake_k8s.add("gryviabudgets", budget("b-a", "alpha"))
    fake_k8s.add("gryviabudgets", budget("b-b", "tenant-beta", "namespace"))
    assert len(make_client("budgets").get("/api/budgets").json()["items"]) == 2
    items = make_client("budgets", role="tenant", tenants=["beta"]).get("/api/budgets").json()["items"]
    assert [i["name"] for i in items] == ["b-b"]
    b = items[0]
    assert b["usage"]["costUSD"] == 60 and b["utilization"]["costPercent"] == 60
    assert b["enforcement"] == {"enabled": True, "action": "block"} and b["state"] == "warning"
    assert b["forecast"]["budgetSufficient"] is True and b["currentPeriod"]["daysRemaining"] == 3
    assert make_client("budgets", role="tenant", tenants=["zed"]).get("/api/budgets").json()["items"] == []
