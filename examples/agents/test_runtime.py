# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Tests of the reference agent runtime (pytest examples/agents)."""

import json

import httpx
import pytest
from fastapi.testclient import TestClient

import runtime

GATEWAY = "http://gw:8080"


def config(**over):
    conf = {
        "name": "helper",
        "namespace": "tenant-a",
        "model": "chat",
        "systemPrompt": "Be brief.",
        "maxSteps": 3,
        "tools": [
            {"name": "search", "type": "retrieval", "index": "handbook", "topK": 2},
            {"name": "status", "type": "http", "urls": ["https://status.example.com/api/"], "method": "GET"},
        ],
    }
    conf.update(over)
    return conf


def tool_call(name, args, call_id="call-1"):
    return {"id": call_id, "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}


def completion(content=None, calls=None, prompt=10, completion_tokens=5):
    msg = {"role": "assistant", "content": content}
    if calls:
        msg["tool_calls"] = calls
    return {
        "choices": [{"index": 0, "message": msg, "finish_reason": "tool_calls" if calls else "stop"}],
        "usage": {
            "prompt_tokens": prompt,
            "completion_tokens": completion_tokens,
            "total_tokens": prompt + completion_tokens,
        },
    }


class Stream:
    """A streamed model reply: the data events of one SSE response."""

    def __init__(self, *events):
        self.events = events


def delta(content=None, calls=None, finish=None):
    d = {}
    if content is not None:
        d["content"] = content
    if calls is not None:
        d["tool_calls"] = calls
    return {"choices": [{"index": 0, "delta": d, "finish_reason": finish}]}


def usage_event(prompt=10, completion_tokens=5):
    total = prompt + completion_tokens
    return {"choices": [], "usage": {"prompt_tokens": prompt, "completion_tokens": completion_tokens,
                                     "total_tokens": total}}


def events(resp):
    lines = [line[6:] for line in resp.text.split("\n\n") if line.startswith("data: ")]
    assert lines[-1] == "[DONE]", resp.text
    return [json.loads(line) for line in lines[:-1]]


class Fake:
    """A scripted gateway and tool host behind httpx.MockTransport."""

    def __init__(self, replies, retrieve=None, http=None):
        self.replies = list(replies)
        self.retrieve = retrieve or {"data": [{"text": "Quotas reset monthly.", "source": "kb.md", "score": 0.9}]}
        self.http = http or (200, "all green")
        self.model_bodies = []
        self.requests = []

    def __call__(self, request):
        self.requests.append(request)
        url = str(request.url)
        if url == GATEWAY + "/v1/chat/completions":
            assert request.headers["authorization"] == "Bearer gk-test"
            self.model_bodies.append(json.loads(request.content))
            reply = self.replies.pop(0)
            if isinstance(reply, httpx.Response):
                return reply
            if isinstance(reply, Stream):
                body = "".join(f"data: {json.dumps(e)}\n\n" for e in reply.events) + "data: [DONE]\n\n"
                return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=body.encode())
            return httpx.Response(200, json=reply)
        if url == GATEWAY + "/v1/retrieve":
            self.retrieve_body = json.loads(request.content)
            if isinstance(self.retrieve, httpx.Response):
                return self.retrieve
            return httpx.Response(200, json=self.retrieve)
        status, text = self.http
        return httpx.Response(status, text=text)


def make(fake, **over):
    agent = runtime.Agent(config(**over), GATEWAY, "gk-test", httpx.Client(transport=httpx.MockTransport(fake)))
    return TestClient(runtime.create_app(agent))


def chat(client, text="How do quotas work?"):
    return client.post("/v1/chat/completions", json={"messages": [{"role": "user", "content": text}]})


def test_answer_without_tools():
    fake = Fake([completion("Hi.")])
    resp = chat(make(fake))
    assert resp.status_code == 200
    body = resp.json()
    assert body["object"] == "chat.completion" and body["model"] == "helper"
    assert body["choices"][0]["message"]["content"] == "Hi."
    assert body["gryvia"] == {"steps": 1, "toolCalls": []}
    sent = fake.model_bodies[0]
    assert sent["model"] == "chat" and sent["messages"][0] == {"role": "system", "content": "Be brief."}
    assert [t["function"]["name"] for t in sent["tools"]] == ["search", "status"]


def test_retrieval_loop_sums_usage():
    fake = Fake([completion(calls=[tool_call("search", {"query": "quotas"})]), completion("Monthly.")])
    body = chat(make(fake)).json()
    assert body["choices"][0]["message"]["content"] == "Monthly."
    assert body["usage"] == {"prompt_tokens": 20, "completion_tokens": 10, "total_tokens": 30}
    assert body["gryvia"]["toolCalls"] == [{"step": 1, "tool": "search", "arguments": {"query": "quotas"}, "ok": True}]
    assert fake.retrieve_body == {"index": "handbook", "query": "quotas", "topK": 2}
    second = fake.model_bodies[1]["messages"]
    assert second[-2]["tool_calls"][0]["id"] == "call-1"
    assert second[-1] == {"role": "tool", "tool_call_id": "call-1", "content": "[1] (kb.md) Quotas reset monthly."}


def test_last_step_offers_no_tools():
    calls = [tool_call("search", {"query": "q"})]
    fake = Fake([completion(calls=calls), completion(calls=calls), completion(calls=calls)])
    body = chat(make(fake)).json()
    assert len(fake.model_bodies) == 3
    assert "tools" in fake.model_bodies[1] and "tools" not in fake.model_bodies[2]
    assert body["choices"][0]["finish_reason"] == "length" and body["gryvia"]["steps"] == 3


def test_http_tool_allowed_and_refused():
    calls = [
        tool_call("status", {"url": "https://status.example.com/api/health"}, "a"),
        tool_call("status", {"url": "https://status.example.com.evil.io/api/x"}, "b"),
        tool_call("status", {"url": "https://status.example.com/api/../admin"}, "c"),
    ]
    fake = Fake([completion(calls=calls), completion("ok")])
    body = chat(make(fake)).json()
    tools = [m for m in fake.model_bodies[1]["messages"] if m["role"] == "tool"]
    assert tools[0]["content"] == "HTTP 200\nall green"
    assert tools[1]["content"].startswith("error: url is not allowed")
    assert tools[2]["content"].startswith("error: url is not allowed")
    hosts = [r.url.host for r in fake.requests if r.url.host != "gw"]
    assert hosts == ["status.example.com"]
    assert [c["ok"] for c in body["gryvia"]["toolCalls"]] == [True, False, False]


def test_tool_errors_go_back_to_the_model():
    calls = [
        tool_call("nope", {}, "a"),
        {"id": "b", "type": "function", "function": {"name": "search", "arguments": "{not json"}},
        tool_call("search", {"query": ""}, "c"),
    ]
    fake = Fake([completion(calls=calls), completion("sorry")])
    chat(make(fake))
    tools = [m["content"] for m in fake.model_bodies[1]["messages"] if m["role"] == "tool"]
    assert tools == ["error: unknown tool nope", "error: arguments are not valid JSON", "error: query is required"]


def test_retrieval_error_is_reported():
    fake = Fake(
        [completion(calls=[tool_call("search", {"query": "q"})]), completion("x")],
        retrieve=httpx.Response(404, json={"error": {"message": "index not found"}}),
    )
    chat(make(fake))
    tool = fake.model_bodies[1]["messages"][-1]["content"]
    assert tool == "error: retrieval answered 404: index not found"


@pytest.mark.parametrize("status,want", [(429, 429), (401, 401), (500, 502)])
def test_gateway_errors(status, want):
    fake = Fake([httpx.Response(status, json={"error": {"message": "nope"}})])
    resp = chat(make(fake))
    assert resp.status_code == want
    assert resp.json()["error"]["type"] == "gateway_error"


@pytest.mark.parametrize(
    "body,msg",
    [
        ({"messages": []}, "non-empty"),
        ({"messages": [{"role": "robot", "content": "x"}]}, "role"),
        ({"messages": [], "stream": True}, "non-empty"),
    ],
)
def test_bad_requests(body, msg):
    resp = make(Fake([])).post("/v1/chat/completions", json=body)
    assert resp.status_code == 400 and msg in resp.json()["error"]["message"]


def stream_chat(client, text="How do quotas work?"):
    return client.post("/v1/chat/completions", json={"messages": [{"role": "user", "content": text}], "stream": True})


def test_stream_relays_content_and_runs_tools_between_steps():
    first = Stream(
        delta("Let me check. "),
        delta(calls=[{"index": 0, "id": "call-9", "type": "function",
                      "function": {"name": "search", "arguments": ""}}]),
        delta(calls=[{"index": 0, "function": {"arguments": '{"query": '}}]),
        delta(calls=[{"index": 0, "function": {"arguments": '"quotas"}'}}], finish="tool_calls"),
        usage_event(),
    )
    second = Stream(delta("Quotas "), delta("reset monthly.", finish="stop"), usage_event(12, 4))
    fake = Fake([first, second])
    resp = stream_chat(make(fake))
    assert resp.status_code == 200 and resp.headers["content-type"].startswith("text/event-stream")
    evs = events(resp)
    assert all(e["object"] == "chat.completion.chunk" and e["model"] == "helper" for e in evs)
    assert len({e["id"] for e in evs}) == 1
    texts = [e["choices"][0]["delta"].get("content", "") for e in evs if e["choices"]]
    assert "".join(texts) == "Let me check. Quotas reset monthly."
    assert evs[0]["choices"][0]["delta"] == {"role": "assistant", "content": "Let me check. "}
    tool = [e["gryvia"]["toolCall"] for e in evs if "toolCall" in e.get("gryvia", {})]
    assert tool == [{"step": 1, "tool": "search", "arguments": {"query": "quotas"}, "ok": True}]
    assert evs[-2]["choices"][0]["finish_reason"] == "stop"
    assert evs[-1]["choices"] == []
    assert evs[-1]["usage"] == {"prompt_tokens": 22, "completion_tokens": 9, "total_tokens": 31}
    assert evs[-1]["gryvia"] == {"steps": 2, "toolCalls": tool}
    assert all(b["stream"] and b["stream_options"] == {"include_usage": True} for b in fake.model_bodies)
    assistant = fake.model_bodies[1]["messages"][-2]
    assert assistant["content"] == "Let me check. "
    assert assistant["tool_calls"] == [{"id": "call-9", "type": "function",
                                        "function": {"name": "search", "arguments": '{"query": "quotas"}'}}]
    assert fake.model_bodies[1]["messages"][-1]["tool_call_id"] == "call-9"


def test_stream_last_step_offers_no_tools():
    call = [{"index": 0, "id": "c", "function": {"name": "search", "arguments": '{"query": "q"}'}}]
    fake = Fake([Stream(delta(calls=call)), Stream(delta(calls=call)), Stream(delta(calls=call))])
    evs = events(stream_chat(make(fake)))
    assert "tools" not in fake.model_bodies[2]
    assert evs[-2]["choices"][0] == {"index": 0, "delta": {"role": "assistant", "content": ""},
                                     "finish_reason": "length"}
    assert evs[-1]["gryvia"]["steps"] == 3


@pytest.mark.parametrize("status,want", [(429, 429), (500, 502)])
def test_stream_error_before_the_first_event_is_an_http_status(status, want):
    fake = Fake([httpx.Response(status, json={"error": {"message": "quota exceeded"}})])
    resp = stream_chat(make(fake))
    assert resp.status_code == want
    assert "quota exceeded" in resp.json()["error"]["message"]


def test_stream_error_after_the_first_event_is_an_error_event():
    call = [{"index": 0, "id": "c", "function": {"name": "search", "arguments": '{"query": "q"}'}}]
    fake = Fake([Stream(delta(calls=call), usage_event()), httpx.Response(503, json={"error": {"message": "busy"}})])
    resp = stream_chat(make(fake))
    assert resp.status_code == 200
    evs = events(resp)
    assert "toolCall" in evs[0]["gryvia"]
    assert evs[-1]["error"]["code"] == 503 and "busy" in evs[-1]["error"]["message"]


def test_stream_accepts_a_non_streamed_reply():
    fake = Fake([completion("Plain answer.")])
    evs = events(stream_chat(make(fake, tools=[])))
    assert evs[0]["choices"][0]["delta"] == {"role": "assistant", "content": "Plain answer."}
    assert evs[-1]["usage"]["total_tokens"] == 15


def test_health_and_models():
    client = make(Fake([]))
    assert client.get("/healthz").json() == {"status": "ok"}
    assert client.get("/v1/models").json()["data"][0]["id"] == "helper"


def test_no_tools_configured():
    fake = Fake([completion("plain")])
    chat(make(fake, tools=[], systemPrompt=""))
    sent = fake.model_bodies[0]
    assert "tools" not in sent and sent["messages"][0]["role"] == "user"


@pytest.mark.parametrize(
    "url,ok",
    [
        ("https://status.example.com/api/", True),
        ("https://status.example.com/api", True),
        ("https://STATUS.example.com:443/api/x?y=1", True),
        ("http://status.example.com/api/x", False),
        ("https://status.example.com:8443/api/x", False),
        ("https://status.example.com/apix", False),
        ("https://user:pw@status.example.com/api/x", False),
        ("ftp://status.example.com/api/x", False),
        ("https://status.example.com:bad/api/", False),
    ],
)
def test_url_allowed(url, ok):
    assert runtime.url_allowed(url, ["https://status.example.com/api/"]) is ok


def test_url_allowed_host_only_prefix():
    assert runtime.url_allowed("http://inv.shop.svc:9000/items/3", ["http://inv.shop.svc:9000"])
    assert not runtime.url_allowed("http://inv.shop.svc:9001/items/3", ["http://inv.shop.svc:9000"])


def test_post_tool_schema_has_body(tmp_path):
    conf = config(tools=[{"name": "hook", "type": "http", "urls": ["http://x/"], "method": "POST"}], maxSteps=0)
    path = tmp_path / "c.json"
    path.write_text(json.dumps(conf))
    loaded = runtime.load_config(str(path))
    assert loaded["maxSteps"] == 5
    params = runtime.tool_schemas(loaded["tools"])[0]["function"]["parameters"]
    assert set(params["properties"]) == {"url", "body"}


ZYNTRA = "http://zyntra:8080"
ZYNTRA_TOOL = {"name": "ops", "type": "zyntra", "url": ZYNTRA + "/", "tokenEnv": "ZYNTRA_TOKEN_OPS",
               "propose": True, "actions": ["raise-inference-priority"]}
NODE = {"id": "GpuNode:gryvia:gpu-1", "type": "GpuNode",
        "props": {"name": {"v": "gpu-1", "prov": {"source": "gryvia"}}, "gpuCount": {"v": 8, "prov": {}}}}


class FakeZyntra(Fake):
    """The gateway plus a Zyntra that wants the service token."""

    def __init__(self, replies, propose=None):
        super().__init__(replies)
        self.propose = propose or httpx.Response(201, json={"id": "p-1", "action": "raise-inference-priority",
                                                            "action_name": "Raise priority", "status": "pending",
                                                            "required_approvals": 1})
        self.zyntra = []

    def __call__(self, request):
        if request.url.host != "zyntra":
            return super().__call__(request)
        self.zyntra.append(request)
        if request.headers.get("authorization") != "Bearer zst_test":
            return httpx.Response(401, json={"error": "unauthorized"})
        path = request.url.path
        if path == "/api/v1/ontology/objects":
            return httpx.Response(200, json={"objects": [NODE], "next": ""})
        if path == "/api/v1/ontology/objects/GpuNode:gryvia:gpu-1":
            link = {"link": {"type": "owned_by", "from": NODE["id"], "to": "Tenant:gryvia:a"},
                    "other": {"id": "Tenant:gryvia:a", "type": "Tenant", "props": {}}, "out": True}
            return httpx.Response(200, json={"object": NODE, "links": [link], "failing_kpis": ["gpu_utilization"]})
        if path == "/api/v1/proposals" and request.method == "POST":
            self.propose_body = json.loads(request.content)
            return self.propose
        return httpx.Response(404, json={"error": "no such object"})


def zyntra_tools(fake, **over):
    body = chat(make(fake, tools=[dict(ZYNTRA_TOOL, **over)])).json()
    msgs = [m["content"] for m in fake.model_bodies[1]["messages"] if m["role"] == "tool"]
    return body, msgs


def test_zyntra_schemas():
    names = [s["function"]["name"] for s in runtime.tool_schemas([ZYNTRA_TOOL])]
    assert names == ["ops_search", "ops_object", "ops_propose"]
    assert "raise-inference-priority" in runtime.tool_schemas([ZYNTRA_TOOL])[2]["function"]["description"]
    read_only = runtime.tool_schemas([dict(ZYNTRA_TOOL, propose=False, actions=[])])
    assert [s["function"]["name"] for s in read_only] == ["ops_search", "ops_object"]


def test_zyntra_search_and_object(monkeypatch):
    monkeypatch.setenv("ZYNTRA_TOKEN_OPS", "zst_test")
    calls = [tool_call("ops_search", {"query": "gpu", "type": "GpuNode"}, "a"),
             tool_call("ops_object", {"id": "GpuNode:gryvia:gpu-1"}, "b")]
    fake = FakeZyntra([completion(calls=calls), completion("ok")])
    body, msgs = zyntra_tools(fake)
    assert msgs[0] == "GpuNode:gryvia:gpu-1 (GpuNode): gpuCount=8, name=gpu-1"
    obj = json.loads(msgs[1])
    assert obj["props"] == {"gpuCount": 8, "name": "gpu-1"} and obj["failing_kpis"] == ["gpu_utilization"]
    assert obj["links"] == [{"type": "owned_by", "direction": "out", "object": "Tenant:gryvia:a"}]
    q = fake.zyntra[0].url.params
    assert q["q"] == "gpu" and q["type"] == "GpuNode" and q["limit"] == "20"
    assert [c["ok"] for c in body["gryvia"]["toolCalls"]] == [True, True]


def test_zyntra_propose(monkeypatch):
    monkeypatch.setenv("ZYNTRA_TOKEN_OPS", "zst_test")
    calls = [tool_call("ops_propose", {"action": "raise-inference-priority", "inputs": {"service": "svc-1"}}, "a"),
             tool_call("ops_propose", {"action": "enable-mig-sharing"}, "b"),
             tool_call("ops_propose", {"action": "raise-inference-priority", "inputs": {"service": 3}}, "c")]
    fake = FakeZyntra([completion(calls=calls), completion("ok")])
    _, msgs = zyntra_tools(fake)
    assert msgs[0].startswith("Proposal p-1 (Raise priority) is pending. It needs 1 approval(s) in Zyntra")
    assert msgs[1].startswith("error: action is not allowed")
    assert msgs[2] == "error: inputs must map input names to object ids"
    assert fake.propose_body == {"action": "raise-inference-priority", "inputs": {"service": "svc-1"}}
    assert len(fake.zyntra) == 1


def test_zyntra_refusals(monkeypatch):
    monkeypatch.setenv("ZYNTRA_TOKEN_OPS", "zst_test")
    blocked = httpx.Response(422, json={"error": "action contract not met", "blocked_reasons": ["needs approver"]})
    calls = [tool_call("ops_propose", {"action": "raise-inference-priority"}, "a"),
             tool_call("ops_object", {"id": "nope"}, "b")]
    fake = FakeZyntra([completion(calls=calls), completion("ok")], propose=blocked)
    _, msgs = zyntra_tools(fake)
    assert msgs[0] == "error: Zyntra refused the proposal: action contract not met (needs approver)"
    assert msgs[1] == "error: Zyntra answered 404: no such object"


def test_zyntra_token(monkeypatch):
    calls = [tool_call("ops_search", {}, "a")]
    monkeypatch.delenv("ZYNTRA_TOKEN_OPS", raising=False)
    fake = FakeZyntra([completion(calls=calls), completion("ok")])
    _, msgs = zyntra_tools(fake)
    assert msgs == ["error: the Zyntra token is not configured"] and not fake.zyntra
    monkeypatch.setenv("ZYNTRA_TOKEN_OPS", "zst_wrong")
    fake = FakeZyntra([completion(calls=calls), completion("ok")])
    _, msgs = zyntra_tools(fake)
    assert msgs == ["error: Zyntra answered 401: unauthorized"]
