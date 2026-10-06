"""Durable state, two identities, CAS conflicts, rollback and uncertain writes."""

import copy
import json
from types import SimpleNamespace as NS

from fastapi import FastAPI, Request
from fastapi.testclient import TestClient
from kubernetes.client.exceptions import ApiException

from conftest import FakeCore, FakeCustomObjects, NoopLimiter
from routers.common import Deps
from routers.intelligence_actions import build_router


class Store(FakeCore):
    def __init__(self):
        self.cms, self.version = {}, 0
        self.fail_replace = False
        self.node = NS(
            metadata=NS(name="gpu-node", uid="node-uid", resource_version="1", annotations={}),
            spec=NS(unschedulable=False),
        )

    def read_node(self, name):
        if name != self.node.metadata.name:
            raise ApiException(status=404)
        return copy.deepcopy(self.node)

    def patch_node(self, name, body):
        if (
            name != self.node.metadata.name
            or body["metadata"]["resourceVersion"] != self.node.metadata.resource_version
        ):
            raise ApiException(status=409)
        self.node.metadata.resource_version = str(int(self.node.metadata.resource_version) + 1)
        self.node.metadata.annotations.update(body["metadata"]["annotations"])
        self.node.spec.unschedulable = body["spec"]["unschedulable"]
        return copy.deepcopy(self.node)

    def create_namespaced_config_map(self, ns, body):
        self.version += 1
        body = copy.deepcopy(body)
        body["metadata"]["resourceVersion"] = str(self.version)
        self.cms[body["metadata"]["name"]] = body
        return self.read_namespaced_config_map(body["metadata"]["name"], ns)

    def read_namespaced_config_map(self, name, ns):
        if name not in self.cms:
            raise ApiException(status=404)
        b = copy.deepcopy(self.cms[name])
        return NS(
            metadata=NS(name=name, resource_version=b["metadata"]["resourceVersion"], labels=b["metadata"]["labels"]),
            data=b["data"],
        )

    def replace_namespaced_config_map(self, name, ns, body):
        if self.fail_replace:
            raise ApiException(status=503)
        if body["metadata"]["resourceVersion"] != self.cms[name]["metadata"]["resourceVersion"]:
            raise ApiException(status=409)
        return self.create_namespaced_config_map(ns, body)

    def list_namespaced_config_map(self, ns, **kwargs):
        return NS(items=[self.read_namespaced_config_map(n, ns) for n in self.cms], metadata=NS(_continue=None))


class Targets(FakeCustomObjects):
    def __init__(self):
        super().__init__()
        self.patches = 0
        self.fail_patch = False

    def _patch(self, plural, ns, name, body):
        obj = self.store[(plural, ns, name)]
        if self.fail_patch:
            raise ApiException(status=503)
        if body["metadata"]["resourceVersion"] != obj["metadata"]["resourceVersion"]:
            raise ApiException(status=409)
        self.patches += 1
        out = super()._patch(plural, ns, name, body)
        obj["metadata"]["resourceVersion"] = str(int(obj["metadata"]["resourceVersion"]) + 1)
        return copy.deepcopy(obj)


def setup(enabled=True, named=True):
    core, custom = Store(), Targets()
    custom.add(
        "gryviainferenceservices",
        {"metadata": {"name": "chat", "uid": "u", "resourceVersion": "1"}, "spec": {"replicas": 1}},
        "default",
    )

    async def auth(request: Request):
        request.state.role = "admin"
        request.state.auth_method = "oidc" if named else "api_key"
        request.state.user_claims = {"iss": "https://idp", "sub": request.headers.get("x-person", "alice")}

    deps = Deps(verify_auth=auth, k8s_core=core, k8s_custom=custom, limiter=NoopLimiter(), intelligence_actions=enabled)
    app = FastAPI()
    app.include_router(build_router(deps))
    return TestClient(app), core, custom, deps


BODY = {
    "kind": "inference-replicas",
    "name": "chat",
    "namespace": "default",
    "value": 2,
    "reason": "Observed queue pressure",
    "evidence": "Measured TTFT breached 1 second in a 60 second window",
}
BOB = {"x-person": "bob"}


def test_disabled_and_shared_key_refused():
    assert setup(enabled=False)[0].post("/api/intelligence/actions", json=BODY).status_code == 503
    assert setup(named=False)[0].post("/api/intelligence/actions", json=BODY).status_code == 403


def test_two_person_execution_rollback_and_replay():
    client, core, targets, deps = setup()
    result = client.post("/api/intelligence/actions", json=BODY)
    assert result.status_code == 201
    ident = result.json()["id"]
    path = f"/api/intelligence/actions/{ident}"
    assert client.post(path + "/approve").status_code == 403
    assert client.post(path + "/execute", headers=BOB).status_code == 409
    assert client.post(path + "/approve", headers=BOB).json()["state"] == "Approved"
    assert client.post(path + "/execute", headers=BOB).json()["state"] == "Applied"
    assert client.post(path + "/execute", headers=BOB).status_code == 409
    assert targets.patches == 1
    assert client.post(path + "/rollback", headers=BOB).json()["state"] == "RolledBack"
    assert targets.store[("gryviainferenceservices", "default", "chat")]["spec"]["replicas"] == 1
    # New application instance shares ConfigMaps: no in-memory state dependency.
    app = FastAPI()
    app.include_router(build_router(deps))
    assert TestClient(app).get(path).json()["state"] == "RolledBack"


def test_intervening_target_change_refused():
    client, _, targets, _ = setup()
    ident = client.post("/api/intelligence/actions", json=BODY).json()["id"]
    path = f"/api/intelligence/actions/{ident}"
    client.post(path + "/approve", headers=BOB)
    targets.store[("gryviainferenceservices", "default", "chat")]["metadata"]["resourceVersion"] = "7"
    assert client.post(path + "/execute", headers=BOB).status_code == 409
    assert targets.patches == 0


def test_uncertain_patch_never_replayed():
    client, _, targets, _ = setup()
    ident = client.post("/api/intelligence/actions", json=BODY).json()["id"]
    path = f"/api/intelligence/actions/{ident}"
    client.post(path + "/approve", headers=BOB)
    targets.fail_patch = True
    assert client.post(path + "/execute", headers=BOB).status_code == 503
    assert client.get(path).json()["state"] == "Executing"
    targets.fail_patch = False
    assert client.post(path + "/execute", headers=BOB).status_code == 409


def test_target_uid_and_expiry_and_hpa():
    client, core, targets, _ = setup()
    obj = targets.store[("gryviainferenceservices", "default", "chat")]
    obj["spec"]["autoscaling"] = {"enabled": True}
    assert client.post("/api/intelligence/actions", json=BODY).status_code == 409
    obj["spec"].pop("autoscaling")
    ident = client.post("/api/intelligence/actions", json=BODY).json()["id"]
    record = core.cms["gryvia-action-" + ident]
    data = json.loads(record["data"]["operation.json"])
    data["expiresAt"] = "2000-01-01T00:00:00+00:00"
    record["data"]["operation.json"] = json.dumps(data)
    assert client.post(f"/api/intelligence/actions/{ident}/approve", headers=BOB).status_code == 409


def test_state_lock_cas():
    from routers.intelligence_actions import save
    import asyncio
    import pytest
    from fastapi import HTTPException

    client, core, _, deps = setup()
    ident = client.post("/api/intelligence/actions", json=BODY).json()["id"]
    first = core.read_namespaced_config_map("gryvia-action-" + ident, "default")
    data = json.loads(first.data["operation.json"])
    asyncio.run(save(deps, first, data))
    with pytest.raises(HTTPException) as error:
        asyncio.run(save(deps, first, data))
    assert error.value.status_code == 409


def test_strict_action_value_and_unknown_fields():
    client = setup()[0]
    assert client.post("/api/intelligence/actions", json={**BODY, "value": True}).status_code == 422
    assert client.post("/api/intelligence/actions", json={**BODY, "command": "rm"}).status_code == 422


def test_copilot_creates_proposal_but_cannot_apply_it():
    import httpx
    from routers import copilot

    _, core, targets, deps = setup()
    deps.llm_key_namespace, deps.llm_gateway_url = "keys", "http://llm"
    seen = []

    def model(request):
        seen.append(json.loads(request.content))
        if len(seen) == 1:
            return httpx.Response(
                200,
                json={
                    "choices": [
                        {
                            "message": {
                                "tool_calls": [
                                    {
                                        "id": "one",
                                        "type": "function",
                                        "function": {"name": "propose_operation", "arguments": json.dumps(BODY)},
                                    }
                                ]
                            }
                        }
                    ]
                },
            )
        return httpx.Response(200, json={"choices": [{"message": {"content": "Proposal is ready for review"}}]})

    deps.llm_transport = httpx.MockTransport(model)
    app = FastAPI()
    app.include_router(copilot.build_router(deps))
    response = TestClient(app).post(
        "/api/copilot/chat",
        json={"model": "chat", "question": "Please propose scaling chat to 2"},
        headers={"X-LLM-Key": "gk-" + "a" * 43},
    )
    assert response.status_code == 200
    assert response.json()["tools"] == ["propose_operation"]
    assert len(core.cms) == 1 and targets.patches == 0
    record = json.loads(next(iter(core.cms.values()))["data"]["operation.json"])
    assert record["state"] == "Proposed"
    assert any(t["function"]["name"] == "propose_operation" for t in seen[0]["tools"])


def test_quota_and_node_operations_preserve_unrelated_fields():
    client, core, targets, _ = setup()
    targets.add(
        "gryviaquotas",
        {
            "metadata": {"name": "team", "uid": "quota-uid", "resourceVersion": "1"},
            "spec": {"gpuQuota": {"maxGPUs": 4, "maxGPUsPerJob": 2}, "team": "team"},
        },
    )
    for kind, name, value in [("quota-max-gpus", "team", 8), ("node-quarantine", "gpu-node", 1)]:
        created = client.post("/api/intelligence/actions", json={**BODY, "kind": kind, "name": name, "value": value})
        assert created.status_code == 201
        path = "/api/intelligence/actions/" + created.json()["id"]
        assert client.post(path + "/approve", headers=BOB).status_code == 200
        assert client.post(path + "/execute", headers=BOB).status_code == 200
        if kind == "quota-max-gpus":
            spec = targets.store[("gryviaquotas", None, "team")]["spec"]
            assert spec["gpuQuota"] == {"maxGPUs": 8, "maxGPUsPerJob": 2}
            assert spec["team"] == "team"
        else:
            assert core.node.spec.unschedulable is True
        assert client.post(path + "/rollback", headers=BOB).status_code == 200
    assert core.node.spec.unschedulable is False
