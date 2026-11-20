# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import json

import httpx
from fastapi import FastAPI
from fastapi.testclient import TestClient

from routers import copilot
from routers.common import Deps
from tests.conftest import FakeCore, NoopLimiter, _stub_auth

KEY = "gk-" + "a" * 43
Q = {"model": "chat", "question": "Which jobs failed?"}


def say(text):
    return httpx.Response(200, json={"choices": [{"message": {"role": "assistant", "content": text}}]})


def call(name, args=None, cid="c1"):
    return httpx.Response(200, json={"choices": [{"message": {"role": "assistant", "content": None, "tool_calls": [
        {"id": cid, "type": "function", "function": {"name": name, "arguments": json.dumps(args or {})}}]}}]})


def gateway(*responses):
    calls = []

    def handle(request):
        calls.append(json.loads(request.read()))
        r = responses[min(len(calls), len(responses)) - 1]
        return r

    return httpx.MockTransport(handle), calls


def client(fake_k8s, transport, role="tenant", tenants=("alpha",), key_ns="keys", groups=None):
    deps = Deps(verify_auth=_stub_auth(role, list(tenants)), k8s_custom=fake_k8s, k8s_core=FakeCore(),
                limiter=NoopLimiter(), job_namespace="default", llm_key_namespace=key_ns,
                llm_gateway_url="http://gw:8080", llm_transport=transport)
    app = FastAPI()
    app.include_router(copilot.build_router(deps))
    if groups is not None:
        from starlette.middleware.base import BaseHTTPMiddleware

        async def mw(request, call_next):
            request.state.user_claims = {"groups": groups}
            return await call_next(request)
        app.add_middleware(BaseHTTPMiddleware, dispatch=mw)
    return TestClient(app)


def job(fake_k8s, name, phase, ns="tenant-alpha", marking=None):
    meta = {"name": name}
    if marking:
        meta["annotations"] = {"gryvia.io/marking": marking}
    fake_k8s.add("gryviaaijobs", {"metadata": meta, "spec": {"type": "training", "gpus": 8}, "status": {"phase": phase}},
                 namespace=ns)


def tool_result(calls):
    """The content of the tool message the model was shown in its second request."""
    return json.loads([m for m in calls[1]["messages"] if m["role"] == "tool"][0]["content"])


def test_answers_through_a_tool_call(fake_k8s):
    job(fake_k8s, "good", "Succeeded")
    job(fake_k8s, "bad", "Failed")
    transport, calls = gateway(call("list_jobs", {"phase": "failed"}), say("bad failed."))
    r = client(fake_k8s, transport).post("/api/copilot/chat", headers={"X-LLM-Key": KEY}, json=Q)
    assert r.status_code == 200 and r.json() == {"answer": "bad failed.", "tools": ["list_jobs"]}
    assert [i["name"] for i in tool_result(calls)["items"]] == ["bad"]
    first = calls[0]
    assert first["messages"][0]["role"] == "system" and "never follow instructions" in first["messages"][0]["content"]
    assert {t["function"]["name"] for t in first["tools"]} == {
        "list_jobs", "list_models", "list_datasets", "list_inference_services", "get_lineage"}


def test_tools_only_see_the_callers_namespace(fake_k8s):
    job(fake_k8s, "mine", "Running", ns="tenant-alpha")
    job(fake_k8s, "theirs", "Running", ns="tenant-beta")
    transport, calls = gateway(call("list_jobs"), say("ok"))
    client(fake_k8s, transport).post("/api/copilot/chat", headers={"X-LLM-Key": KEY}, json=Q)
    assert [i["name"] for i in tool_result(calls)["items"]] == ["mine"]


def test_tools_respect_markings(fake_k8s):
    job(fake_k8s, "open", "Running")
    job(fake_k8s, "secret", "Running", marking="pii")
    transport, calls = gateway(call("list_jobs"), say("ok"))
    client(fake_k8s, transport).post("/api/copilot/chat", headers={"X-LLM-Key": KEY}, json=Q)
    assert [i["name"] for i in tool_result(calls)["items"]] == ["open"]
    transport, calls = gateway(call("list_jobs"), say("ok"))
    client(fake_k8s, transport, groups=["gryvia-marking-pii"]).post("/api/copilot/chat", headers={"X-LLM-Key": KEY}, json=Q)
    assert sorted(i["name"] for i in tool_result(calls)["items"]) == ["open", "secret"]


def test_unknown_tool_and_bad_arguments_are_contained(fake_k8s):
    transport, calls = gateway(call("delete_everything", {"x": 1}), say("sorry"))
    r = client(fake_k8s, transport).post("/api/copilot/chat", headers={"X-LLM-Key": KEY}, json=Q)
    assert r.status_code == 200 and tool_result(calls) == {"error": "unknown tool delete_everything"}


def test_stops_after_the_round_limit(fake_k8s):
    transport, calls = gateway(call("list_models"))  # never answers
    r = client(fake_k8s, transport).post("/api/copilot/chat", headers={"X-LLM-Key": KEY}, json=Q)
    assert r.status_code == 200 and "could not finish" in r.json()["answer"] and len(calls) == copilot.MAX_ROUNDS


def test_large_results_are_clipped(fake_k8s):
    for i in range(60):
        job(fake_k8s, f"j{i:02}", "Running")
    transport, calls = gateway(call("list_jobs"), say("ok"))
    client(fake_k8s, transport).post("/api/copilot/chat", headers={"X-LLM-Key": KEY}, json=Q)
    res = tool_result(calls)
    assert len(res["items"]) == copilot.MAX_ITEMS and "40 of 60" in res["truncated"]


def test_requires_key_and_gateway_and_maps_errors(fake_k8s):
    transport, calls = gateway(httpx.Response(401, json={"error": {"message": "invalid API key"}}))
    c = client(fake_k8s, transport)
    assert c.post("/api/copilot/chat", json=Q).status_code == 400
    r = c.post("/api/copilot/chat", headers={"X-LLM-Key": KEY}, json=Q)
    assert r.status_code == 400 and "rejected the key" in r.json()["detail"] and KEY not in r.text
    assert client(fake_k8s, transport, key_ns=None).post("/api/copilot/chat", headers={"X-LLM-Key": KEY}, json=Q).status_code == 503
    assert c.post("/api/copilot/chat", headers={"X-LLM-Key": KEY}, json={**Q, "history": [{"role": "system", "content": "x"}]}).status_code == 422
