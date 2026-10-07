"""Exercise submission gates through the actual authenticated gateway endpoints."""
import copy
import json

import pytest
from fastapi import Request
from kubernetes.client.exceptions import ApiException

from tests.test_main_endpoints import gw  # noqa: F401


MANIFEST = {"apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaAIJob",
            "metadata": {"name": "train", "namespace": "victim"},
            "spec": {"image": "trainer:latest", "gpus": 2}}


@pytest.fixture
def enforce(gw, monkeypatch):
    monkeypatch.setattr(gw[2].deps, "submission_admission", True)
    return gw


def install(k, monkeypatch, reject=None):
    original = k.create_namespaced_custom_object
    calls = []

    def create(**kwargs):
        calls.append(copy.deepcopy(kwargs))
        if reject:
            raise reject
        if kwargs.get("dry_run") == "All":
            kwargs["body"]["spec"]["admissionMutation"] = True
            return kwargs["body"]
        return original(**kwargs)

    monkeypatch.setattr(k, "create_namespaced_custom_object", create)
    return calls


def test_preview_is_namespaced_strict_and_never_persists(gw, monkeypatch):
    c, k, main = gw

    async def tenant(request: Request):
        request.state.role = "tenant"
        request.state.tenant_namespaces = ["tenant-one"]

    main.app.dependency_overrides[main.verify_auth] = tenant
    calls = install(k, monkeypatch)
    r = c.post("/api/jobs/preflight", json=MANIFEST)
    assert r.status_code == 200
    assert r.json()["namespace"] == "tenant-one"
    assert r.json()["persisted"] is False
    assert not k.store
    assert len(calls) == 1
    assert calls[0]["dry_run"] == "All" and calls[0]["field_validation"] == "Strict"
    assert calls[0]["body"]["metadata"]["namespace"] == "tenant-one"


def test_create_rechecks_and_does_not_reuse_admission_mutations(enforce, monkeypatch):
    c, k, _ = enforce
    calls = install(k, monkeypatch)
    assert c.post("/api/jobs/preflight", json=MANIFEST).status_code == 200
    assert c.post("/api/jobs", json=MANIFEST).status_code == 200
    assert [call.get("dry_run") for call in calls] == ["All", "All", None]
    stored = next(iter(k.store.values()))
    assert stored["metadata"]["namespace"] == "default"
    assert "admissionMutation" not in stored["spec"]


@pytest.mark.parametrize("status", [400, 403, 409, 422, 429, 500, 0])
@pytest.mark.parametrize("path", ["/api/jobs", "/api/jobs/preflight"])
def test_rejections_fail_closed_without_echoing_env(enforce, monkeypatch, status, path):
    c, k, _ = enforce
    calls = install(k, monkeypatch, ApiException(status=status, reason="secret-token"))
    response = c.post(path, json=MANIFEST)
    assert response.status_code == (status if status in (400, 403, 409, 422, 429) else 503)
    assert "secret-token" not in response.text
    assert len(calls) == 1 and not k.store


def test_transport_failure_never_attempts_write(enforce, monkeypatch):
    c, k, _ = enforce
    calls = install(k, monkeypatch, TimeoutError("private transport info"))
    r = c.post("/api/jobs", json=MANIFEST)
    assert r.status_code == 503 and "private" not in r.text
    assert len(calls) == 1 and not k.store


@pytest.mark.parametrize("change", [
    {"metadata": {"name": "Bad_Name"}}, {"metadata": {"name": "x", "finalizers": ["x"]}},
    {"status": {}}, {"spec": {"image": "x", "gpus": True}},
    {"spec": {"image": "x", "gpus": -1}}, {"spec": {"image": "x", "gpus": 1.5}},
    {"spec": {"image": "x", "gpuCount": 1}}, {"spec": {"image": "", "gpus": 1}},
    {"metadata": {"name": "x", "labels": {"x": 1}}},
])
def test_invalid_gateway_inputs_never_reach_cluster(enforce, monkeypatch, change):
    c, k, _ = enforce
    calls = install(k, monkeypatch)
    assert c.post("/api/jobs", json={**MANIFEST, **change}).status_code == 400
    assert not calls and not k.store


def test_body_limit_and_invalid_json(enforce, monkeypatch):
    c, k, _ = enforce
    calls = install(k, monkeypatch)
    assert c.post("/api/jobs", content="x" * (512 * 1024 + 1)).status_code == 413
    assert c.post("/api/jobs/preflight", content="{").status_code == 400
    assert not calls


def test_default_create_is_unchanged_and_preview_says_so(gw, monkeypatch):
    c, k, _ = gw
    calls = install(k, monkeypatch)
    legacy = {**MANIFEST, "spec": {"image": "trainer:latest", "gpuCount": 2}}
    assert c.post("/api/jobs", json=legacy).status_code == 200
    assert [call.get("dry_run") for call in calls] == [None]
    assert "field_validation" not in calls[0]
    r = c.post("/api/jobs/preflight", json=MANIFEST)
    assert r.json()["enforcedOnCreate"] is False
    assert any("does not repeat" in limit for limit in r.json()["limitations"])


def test_rejection_reports_field_paths_but_not_values(gw, monkeypatch):
    c, k, _ = gw
    status = {"kind": "Status", "reason": "Invalid",
              "message": 'GryviaAIJob "train" is invalid: spec.env[0].value: Invalid value: "hunter2"; '
                         'strict decoding error: unknown field "spec.gpuz"',
              "details": {"causes": [{"field": "spec.env[0].value", "reason": "FieldValueInvalid",
                                      "message": "Invalid value: \"hunter2\""}]}}
    exc = ApiException(status=422, reason="Unprocessable")
    exc.body = json.dumps(status)
    install(k, monkeypatch, exc)
    r = c.post("/api/jobs/preflight", json=MANIFEST)
    assert r.status_code == 422
    detail = r.json()["detail"]
    assert "Invalid" in detail and "spec.env[0].value: FieldValueInvalid" in detail
    assert "spec.gpuz: unknown field" in detail
    assert "hunter2" not in r.text


def test_rejection_with_unparseable_body_stays_generic(gw, monkeypatch):
    c, k, _ = gw
    exc = ApiException(status=400, reason="Bad")
    exc.body = "<html>proxy said hunter2</html>"
    install(k, monkeypatch, exc)
    r = c.post("/api/jobs/preflight", json=MANIFEST)
    assert r.status_code == 400 and "hunter2" not in r.text
    assert r.json()["detail"] == "Job admission preflight failed; no job was created"


def test_preview_returns_webhook_warnings(gw, monkeypatch):
    c, k, _ = gw
    from urllib3 import HTTPHeaderDict

    calls = []

    def create(**kwargs):
        calls.append(kwargs)
        headers = HTTPHeaderDict()
        headers.add("Warning", '299 - "no node currently carries GPU type \\"H100\\" (label gryvia.io/gpu)"')
        headers.add("Warning", '299 - "preflight: Blocked: no pool fits 8 GPUs"')
        return kwargs["body"], 201, headers

    monkeypatch.setattr(k, "create_namespaced_custom_object_with_http_info", create, raising=False)
    r = c.post("/api/jobs/preflight", json=MANIFEST)
    assert r.status_code == 200 and not k.store
    assert calls[0]["dry_run"] == "All" and calls[0]["field_validation"] == "Strict"
    assert r.json()["warnings"] == ['no node currently carries GPU type "H100" (label gryvia.io/gpu)',
                                    "preflight: Blocked: no pool fits 8 GPUs"]
