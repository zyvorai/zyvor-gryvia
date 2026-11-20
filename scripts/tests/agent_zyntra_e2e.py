# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""The agent runtime's zyntra tool against a running Zyntra (scripts/tests/sovereign-aios-agent-zyntra.sh).

The model is scripted; every Zyntra call goes over the network with the service token the chart generated. Checks
that the agent finds and reads Gryvia objects, proposes a typed action, is refused an approver-only action, cannot
approve its own proposal, and that a person can.

Environment: ZYNTRA_URL, ZYNTRA_TOKEN_OPS (the agent token), ZYNTRA_ADMIN_KEY.
"""
import json
import os
import re
import sys

import httpx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "examples", "agents"))
import runtime  # noqa: E402

GATEWAY = "http://llm-gateway.test"
ZYNTRA = os.environ["ZYNTRA_URL"]
ADMIN = {"Authorization": "Bearer " + os.environ["ZYNTRA_ADMIN_KEY"]}
AGENT = {"Authorization": "Bearer " + os.environ["ZYNTRA_TOKEN_OPS"]}

failed = 0


def ok(cond, msg):
    global failed
    print(("  ok   " if cond else "  FAIL ") + msg)
    failed |= not cond


def first_id(text):
    m = re.match(r"(\S+) \(", text)
    return m.group(1) if m else ""


class ScriptedModel(httpx.BaseTransport):
    """Answers the LLM gateway with a fixed plan of tool calls; sends everything else to the network."""

    def __init__(self):
        self.net = httpx.HTTPTransport()
        self.results = []

    def handle_request(self, request):
        if request.url.host != "llm-gateway.test":
            return self.net.handle_request(request)
        body = json.loads(request.content)
        self.results = [m["content"] for m in body["messages"] if m["role"] == "tool"]
        r = self.results
        plan = [
            ("ops_search", {"type": "InferenceService"}),
            ("ops_search", {"query": "gpu-0", "type": "GpuNode"}),
            ("ops_object", {"id": first_id(r[0]) if r else ""}),
            ("ops_propose", {"action": "raise-inference-priority", "inputs": {"service": first_id(r[0]) if r else ""}}),
            ("ops_propose", {"action": "enable-mig-sharing", "inputs": {"node": first_id(r[1]) if len(r) > 1 else ""}}),
        ]
        if len(r) < len(plan):
            name, args = plan[len(r)]
            call = {"id": f"c{len(r)}", "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}
            msg = {"role": "assistant", "content": None, "tool_calls": [call]}
        else:
            msg = {"role": "assistant", "content": "Proposed; waiting for approval in Zyntra."}
        return httpx.Response(200, json={"choices": [{"index": 0, "message": msg, "finish_reason": "stop"}],
                                         "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})


def main():
    tool = {"name": "ops", "type": "zyntra", "url": ZYNTRA, "tokenEnv": "ZYNTRA_TOKEN_OPS", "propose": True}
    conf = {"name": "ops-agent", "model": "chat", "maxSteps": 8, "tools": [tool]}
    model = ScriptedModel()
    agent = runtime.Agent(conf, GATEWAY, "gk-test", httpx.Client(transport=model))
    reply = agent.chat([{"role": "user", "content": "Inference is slow; do something."}])
    r = model.results
    print("tool results:\n    " + "\n    ".join(x[:160] for x in r))

    ok(len(r) == 5, "the agent ran five Zyntra calls")
    ok(first_id(r[0]).startswith("InferenceService:") and "phase=Running" in r[0], "search finds the inference service")
    ok(first_id(r[1]).startswith("GpuNode:") and "gpuType=B300" in r[1], "search finds the GPU node")
    obj = json.loads(r[2]) if r[2].startswith("{") else {}
    ok(obj.get("props", {}).get("backend") == "vllm", "object read returns Gryvia's properties")
    ok(r[3].startswith("Proposal ") and "is pending" in r[3], "the agent proposes raise-inference-priority")
    ok(r[4].startswith("error: Zyntra refused the proposal"), "an approver-only action is refused to the agent")
    ok(reply["choices"][0]["message"]["content"].startswith("Proposed"), "the model answers after the tools")

    pid = (re.match(r"Proposal (\S+) ", r[3]) or [None, ""])[1]
    p = httpx.get(f"{ZYNTRA}/api/v1/proposals/{pid}", headers=ADMIN).json()
    ok(p.get("created_by") == "service:gryvia-agents", f"Zyntra records the agent as the proposer ({p.get('created_by')})")
    ok(p.get("status") == "pending", "the proposal waits for approval")
    resp = httpx.post(f"{ZYNTRA}/api/v1/proposals/{pid}/approve", headers=AGENT, json={})
    ok(resp.status_code == 403, f"the agent's token cannot approve ({resp.status_code})")
    resp = httpx.post(f"{ZYNTRA}/api/v1/proposals/{pid}/approve", headers=ADMIN, json={})
    ok(resp.status_code == 200, f"a person approves it ({resp.status_code})")

    os.environ["ZYNTRA_TOKEN_OPS"] = "zst_" + "0" * 64
    bad = runtime.Agent(conf, GATEWAY, "gk-test", httpx.Client()).run_tool("ops_search", {})
    ok(bad.startswith("error: Zyntra answered 401"), f"an unknown token is refused ({bad[:60]})")
    sys.exit(failed)


if __name__ == "__main__":
    main()
