# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import httpx

NS = "default"
BODY = {
    "name": "helper", "model": "chat", "systemPrompt": "Be brief.", "maxSteps": 4, "replicas": 2,
    "cpu": "100m", "memory": "256Mi",
    "tools": [
        {"name": "search", "type": "retrieval", "vectorIndexRef": "handbook", "topK": 3},
        {"name": "status", "type": "http", "urls": ["https://status.example.com/api/"], "method": "GET"},
    ],
}
STATUS = {"phase": "Ready", "message": "Serving", "endpoint": "http://helper-agent.default.svc.cluster.local:8080",
          "readyReplicas": 1, "keySecret": "helper-llm-key"}
REPLY = {"object": "chat.completion", "choices": [{"index": 0, "message": {"role": "assistant", "content": "Hi."}}],
         "gryvia": {"steps": 1, "toolCalls": []}}


def seed(fake_k8s, status=None, **spec):
    obj = {"metadata": {"name": "helper"}, "spec": {"model": "chat", **spec}}
    if status is not None:
        obj["status"] = status
    fake_k8s.add("gryviaagents", obj, namespace=NS)


def stored(fake_k8s):
    return fake_k8s.store[("gryviaagents", NS, "helper")]


def recorder(status=200, body=None, exc=None):
    calls = []

    async def chat(url, payload):
        calls.append((url, payload))
        if exc:
            raise exc
        return status, REPLY if body is None else body

    return chat, calls


def test_create_builds_spec(make_client, fake_k8s):
    r = make_client("agents").post("/api/agents", json=BODY)
    assert r.status_code == 201, r.text
    s = stored(fake_k8s)
    assert s["kind"] == "GryviaAgent"
    assert s["spec"] == {
        "model": "chat", "systemPrompt": "Be brief.", "maxSteps": 4, "replicas": 2,
        "resources": {"requests": {"cpu": "100m", "memory": "256Mi"}},
        "tools": [
            {"name": "search", "type": "retrieval", "retrieval": {"vectorIndexRef": "handbook", "topK": 3}},
            {"name": "status", "type": "http", "http": {"urls": ["https://status.example.com/api/"], "method": "GET"}},
        ],
    }


def test_create_minimal(make_client, fake_k8s):
    r = make_client("agents").post("/api/agents", json={"name": "helper", "model": "chat"})
    assert r.status_code == 201, r.text
    assert stored(fake_k8s)["spec"] == {"model": "chat"}


def test_create_validation(make_client):
    c = make_client("agents")
    bad_tools = [
        [{"name": "s", "type": "retrieval"}],
        [{"name": "s", "type": "http"}],
        [{"name": "s", "type": "http", "urls": ["ftp://x/"]}],
        [{"name": "s", "type": "http", "urls": ["http://x/"], "vectorIndexRef": "h"}],
        [{"name": "s", "type": "retrieval", "vectorIndexRef": "h", "urls": ["http://x/"]}],
        [{"name": "s", "type": "retrieval", "vectorIndexRef": "h"},
         {"name": "s", "type": "retrieval", "vectorIndexRef": "g"}],
        [{"name": "has space", "type": "retrieval", "vectorIndexRef": "h"}],
        [{"name": "z", "type": "zyntra", "url": "http://zyntra:8080"}],
        [{"name": "z", "type": "zyntra", "url": "zyntra:8080", "tokenSecret": "t"}],
        [{"name": "z", "type": "zyntra", "url": "http://zyntra:8080", "tokenSecret": "t", "actions": ["a"]}],
        [{"name": "z", "type": "http", "urls": ["http://x/"], "tokenSecret": "t"}],
        [{"name": "z" * 57, "type": "zyntra", "url": "http://zyntra:8080", "tokenSecret": "t"}],
    ]
    for tools in bad_tools:
        assert c.post("/api/agents", json={"name": "a", "model": "m", "tools": tools}).status_code == 422, tools
    assert c.post("/api/agents", json={"name": "a", "model": "m", "maxSteps": 21}).status_code == 422
    assert c.post("/api/agents", json={"name": "a", "model": "m", "cpu": "lots"}).status_code == 422
    assert c.post("/api/agents", json={"name": "a", "model": "m", "extra": 1}).status_code == 422


def test_create_zyntra_tool(make_client, fake_k8s):
    tool = {"name": "ops", "type": "zyntra", "url": "http://sa-zyntra:8080", "tokenSecret": "zyntra-token",
            "propose": True, "actions": ["raise-inference-priority"]}
    c = make_client("agents")
    r = c.post("/api/agents", json={"name": "helper", "model": "chat", "tools": [tool]})
    assert r.status_code == 201, r.text
    assert stored(fake_k8s)["spec"]["tools"] == [{"name": "ops", "type": "zyntra", "zyntra": {
        "url": "http://sa-zyntra:8080", "tokenSecretRef": {"name": "zyntra-token", "key": "token"},
        "propose": True, "actions": ["raise-inference-priority"]}}]
    assert c.get("/api/agents/helper").json()["spec"]["tools"] == [dict(tool, tokenKey="token")]


def test_list_and_get(make_client, fake_k8s):
    seed(fake_k8s, STATUS, tools=[{"name": "search", "type": "retrieval", "retrieval": {"vectorIndexRef": "handbook"}}])
    c = make_client("agents")
    body = c.get("/api/agents").json()
    item = c.get("/api/agents/helper").json()
    assert body["items"] == [item]
    assert item["spec"]["tools"] == [{"name": "search", "type": "retrieval", "vectorIndexRef": "handbook"}]
    assert item["status"]["phase"] == "Ready" and item["status"]["keySecret"] == "helper-llm-key"


def test_404(make_client):
    c = make_client("agents")
    assert c.get("/api/agents/nope").status_code == 404
    assert c.post("/api/agents/nope/chat", json={"messages": [{"role": "user", "content": "x"}]}).status_code == 404
    assert c.delete("/api/agents/nope").status_code == 404


def test_chat_proxies_to_runtime(make_client, fake_k8s):
    seed(fake_k8s, STATUS)
    chat, calls = recorder()
    r = make_client("agents", agent_chat=chat).post(
        "/api/agents/helper/chat", json={"messages": [{"role": "user", "content": "Hello"}]})
    assert r.status_code == 200, r.text
    assert r.json() == REPLY
    assert calls == [("http://helper-agent.default.svc.cluster.local:8080/v1/chat/completions",
                      {"messages": [{"role": "user", "content": "Hello"}]})]


def test_chat_ignores_foreign_endpoint(make_client, fake_k8s):
    seed(fake_k8s, {**STATUS, "endpoint": "http://evil.example.com:8080"})
    chat, calls = recorder()
    c = make_client("agents", agent_chat=chat)
    c.post("/api/agents/helper/chat", json={"messages": [{"role": "user", "content": "x"}]})
    assert calls[0][0] == "http://helper-agent.default.svc.cluster.local:8080/v1/chat/completions"


def test_chat_url_uses_only_the_stored_object():
    import pytest
    from fastapi import HTTPException
    from routers.agents import chat_url

    def url(endpoint, name="helper", namespace=NS):
        return chat_url({"metadata": {"name": name, "namespace": namespace}, "status": {"endpoint": endpoint}})

    derived = "http://helper-agent.default.svc.cluster.local:8080/v1/chat/completions"
    assert url("http://custom.default.svc.cluster.local:8080") == \
        "http://custom.default.svc.cluster.local:8080/v1/chat/completions"
    assert url("http://custom.other.svc.cluster.local:8080") == derived
    assert url("http://custom.default.svc.cluster.local:9999") == derived
    assert url("http://user@custom.default.svc.cluster.local:8080") == derived
    for name, namespace in (("x/../y", NS), ("Helper", NS), ("helper", "a.b"), ("a" * 60, NS)):
        with pytest.raises(HTTPException):
            url("", name, namespace)


def test_chat_not_ready(make_client, fake_k8s):
    seed(fake_k8s, {"phase": "Pending"})
    chat, calls = recorder()
    c = make_client("agents", agent_chat=chat)
    r = c.post("/api/agents/helper/chat", json={"messages": [{"role": "user", "content": "x"}]})
    assert r.status_code == 409 and "Pending" in r.json()["detail"]
    assert calls == []


def test_chat_errors(make_client, fake_k8s):
    seed(fake_k8s, STATUS)
    msg = {"messages": [{"role": "user", "content": "x"}]}
    chat, _ = recorder(429, {"error": {"message": "quota exceeded"}})
    r = make_client("agents", agent_chat=chat).post("/api/agents/helper/chat", json=msg)
    assert r.status_code == 429 and "quota exceeded" in r.json()["detail"]
    chat, _ = recorder(500, {"error": "boom"})
    assert make_client("agents", agent_chat=chat).post("/api/agents/helper/chat", json=msg).status_code == 502
    chat, _ = recorder(exc=httpx.ConnectError("refused"))
    r = make_client("agents", agent_chat=chat).post("/api/agents/helper/chat", json=msg)
    assert r.status_code == 502 and "unreachable" in r.json()["detail"]


SSE = b'data: {"choices": [{"index": 0, "delta": {"content": "Hi."}}]}\n\ndata: [DONE]\n\n'


def runtime(*responses):
    """A MockTransport answering streamed chats with the given responses in order."""
    calls = []

    def handle(request):
        calls.append((str(request.url), request.read()))
        return responses[len(calls) - 1]

    return httpx.MockTransport(handle), calls


def test_chat_stream_relays_events(make_client, fake_k8s):
    import json
    seed(fake_k8s, STATUS)
    transport, calls = runtime(httpx.Response(200, headers={"content-type": "text/event-stream"}, content=SSE))
    r = make_client("agents", agent_transport=transport).post(
        "/api/agents/helper/chat", json={"messages": [{"role": "user", "content": "Hello"}], "stream": True})
    assert r.status_code == 200, r.text
    assert r.headers["content-type"].startswith("text/event-stream")
    assert r.content == SSE
    url, body = calls[0]
    assert url == "http://helper-agent.default.svc.cluster.local:8080/v1/chat/completions"
    assert json.loads(body) == {"messages": [{"role": "user", "content": "Hello"}], "stream": True}


def test_chat_stream_errors(make_client, fake_k8s):
    seed(fake_k8s, STATUS)
    msg = {"messages": [{"role": "user", "content": "x"}], "stream": True}
    for status, body, want, text in ((429, {"error": {"message": "quota exceeded"}}, 429, "quota exceeded"),
                                     (500, {"error": "boom"}, 502, "boom")):
        transport, _ = runtime(httpx.Response(status, json=body))
        r = make_client("agents", agent_transport=transport).post("/api/agents/helper/chat", json=msg)
        assert r.status_code == want and text in r.json()["detail"]

    def refuse(request):
        raise httpx.ConnectError("refused")

    r = make_client("agents", agent_transport=httpx.MockTransport(refuse)).post("/api/agents/helper/chat", json=msg)
    assert r.status_code == 502 and "unreachable" in r.json()["detail"]


def test_chat_stream_passes_a_json_reply_through(make_client, fake_k8s):
    seed(fake_k8s, STATUS)
    transport, _ = runtime(httpx.Response(200, json=REPLY))
    r = make_client("agents", agent_transport=transport).post(
        "/api/agents/helper/chat", json={"messages": [{"role": "user", "content": "x"}], "stream": True})
    assert r.status_code == 200 and r.json() == REPLY


def test_chat_stream_not_ready(make_client, fake_k8s):
    seed(fake_k8s, {"phase": "Pending"})
    transport, calls = runtime()
    r = make_client("agents", agent_transport=transport).post(
        "/api/agents/helper/chat", json={"messages": [{"role": "user", "content": "x"}], "stream": True})
    assert r.status_code == 409 and calls == []


def test_chat_validation(make_client, fake_k8s):
    seed(fake_k8s, STATUS)
    c = make_client("agents", agent_chat=recorder()[0])
    assert c.post("/api/agents/helper/chat", json={"messages": []}).status_code == 422
    assert c.post("/api/agents/helper/chat", json={"messages": [{"role": "tool", "content": "x"}]}).status_code == 422


def test_scale_and_delete(make_client, fake_k8s):
    seed(fake_k8s, STATUS)
    c = make_client("agents")
    r = c.post("/api/agents/helper/scale", json={"replicas": 0})
    assert r.status_code == 200 and stored(fake_k8s)["spec"]["replicas"] == 0
    assert c.post("/api/agents/helper/scale", json={"replicas": 21}).status_code == 422
    assert c.delete("/api/agents/helper").status_code == 200
    assert ("gryviaagents", NS, "helper") not in fake_k8s.store


def test_tenant_sees_only_own_namespace(make_client, fake_k8s):
    fake_k8s.add("gryviaagents", {"metadata": {"name": "other"}, "spec": {"model": "m"}}, namespace="tenant-beta")
    fake_k8s.add("gryviaagents", {"metadata": {"name": "mine"}, "spec": {"model": "m"}}, namespace="tenant-alpha")
    c = make_client("agents", role="tenant", tenants=["alpha"])
    names = [i["metadata"]["name"] for i in c.get("/api/agents").json()["items"]]
    assert names == ["mine"]
    assert c.get("/api/agents/other").status_code == 404
