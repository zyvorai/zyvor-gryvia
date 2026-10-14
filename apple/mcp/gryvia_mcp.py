#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Gryvia MCP server: lets an AI agent see and (optionally) operate your Mac fleet. stdio, newline-delimited JSON-RPC 2.0.

Python standard library only.

    GRYVIA_URL=http://127.0.0.1:8443 GRYVIA_TOKEN=... python3 mcp/gryvia_mcp.py              # read-only
    GRYVIA_URL=... GRYVIA_TOKEN=... python3 mcp/gryvia_mcp.py --allow-write                  # + submit / cancel / policy

Read tools are GETs. Write tools exist only with --allow-write (or GRYVIA_MCP_ALLOW_WRITE=1), are listed with
`readOnlyHint: false`, and still run with whatever role the token has (use a tenant token to scope an agent to one
tenant). The token comes from the environment only and is never printed. Tests assert both tables below.
"""
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

PROTOCOL = "2025-06-18"
MAX_TEXT = 200_000
GUARD = ("Report only what the data shows. GPU utilisation is the whole Mac, not one job. Reservations are admission "
         "bookkeeping, not measured memory. Energy figures are estimates. A missing field means it was not measured.")
RUNTIMES = ["demo", "mlx", "llama", "mps", "coreml", "download", "bench", "evaluate", "quantize", "train", "serve", "fuse"]


def num(kind="number", description="", **extra):
    return {"type": kind, "description": description, **extra}


def obj(properties=None, required=None):
    s = {"type": "object", "properties": properties or {}, "additionalProperties": False}
    if required:
        s["required"] = required
    return s


def _q(args, names):
    return urllib.parse.urlencode({k: args[k] for k in names if args.get(k) is not None})


# name -> (description, input schema, path builder). All GET. This table is the whole read surface.
READ_TOOLS = {
    "gryvia_fleet": ("Macs with GPU model, GPU cores, measured GPU busy %, memory, thermal state, owner presence, work policy and whether each Mac is accepting work; plus tenants.",
                     obj(), lambda a: "/api/state"),
    "gryvia_jobs": ("Recent workloads with state, tenant, model, node, reservation and the scheduler's reason when waiting. Filter client-side by state.",
                    obj({"state": {"type": "string", "enum": ["queued", "running", "succeeded", "failed", "cancelled"]},
                         "limit": num("integer", "Max rows, 1-100", minimum=1, maximum=100)}), lambda a: "/api/state"),
    "gryvia_job_logs": ("Worker log lines of one job (up to 2000, oldest first).", obj({"id": {"type": "string"}}, ["id"]),
                        lambda a: "/api/logs/" + urllib.parse.quote(str(a["id"]), safe="")),
    "gryvia_models": ("Models in the hub with status, runtime, quantisation, memory estimate, source and deployment endpoints.", obj(), lambda a: "/api/state"),
    "gryvia_results": ("Benchmark, evaluation, download and fine-tune results, newest first.",
                       obj({"model": {"type": "string"}, "kind": {"type": "string", "enum": ["benchmark", "evaluate", "download", "quantize", "train", "fuse", "serve"]}}),
                       lambda a: "/api/results?" + _q(a, ("model", "kind"))),
    "gryvia_run_metrics": ("Loss / throughput history of a training job: key -> [[step, value], ...].", obj({"job": {"type": "string"}}, ["job"]),
                           lambda a: "/api/runs/" + urllib.parse.quote(str(a["job"]), safe="") + "/metrics"),
    "gryvia_decisions": ("The scheduler's recent decisions (queued, admitted, waiting + reason, yielded, finished).", obj(), lambda a: "/api/state"),
}

# name -> description, schema, (method, path builder, body builder). Present only with --allow-write.
WRITE_TOOLS = {
    "gryvia_submit_job": ("Queue a workload. runtime must be one of the allowed runtimes; args is an array of strings (never a shell line).",
                          obj({"runtime": {"type": "string", "enum": RUNTIMES}, "model": {"type": "string"}, "memoryGiB": num(description="GiB to reserve"),
                               "priority": num("integer", "0-100", minimum=0, maximum=100), "args": {"type": "array", "items": {"type": "string"}, "maxItems": 64},
                               "node": {"type": "string"}, "labels": {"type": "array", "items": {"type": "string"}, "maxItems": 8}},
                              ["runtime", "memoryGiB"]),
                          ("POST", lambda a: "/api/jobs", lambda a: {**a, "mode": "exclusive"})),
    "gryvia_run_demo_job": ("Run the built-in 3-second CPU smoke workload to check the fleet end to end.", obj(),
                            ("POST", lambda a: "/api/jobs", lambda a: {"runtime": "demo", "model": "mcp demo", "memoryGiB": 1, "args": ["--seconds", "3"], "mode": "exclusive", "labels": ["mcp"]})),
    "gryvia_cancel_job": ("Cancel a queued or running job.", obj({"id": {"type": "string"}}, ["id"]),
                          ("POST", lambda a: "/api/cancel", lambda a: {"id": a["id"]})),
    "gryvia_set_node_policy": ("Set when a Mac may run fleet work. mode: always, idle (only when the owner is away) or paused.",
                               obj({"id": {"type": "string"}, "mode": {"type": "string", "enum": ["always", "idle", "paused"]},
                                    "idleMinutes": num(description="Minutes idle before work may start, 0-1440"),
                                    "allowOnBattery": {"type": "boolean"}, "minBatteryPct": num(description="0-100"),
                                    "pauseOnCall": {"type": "boolean"}, "preempt": {"type": "boolean"}}, ["id", "mode"]),
                               ("POST", lambda a: "/api/nodes/policy", lambda a: a)),
}

PROMPTS = {
    "gryvia_morning_check": ("Summarise fleet health: which Macs are up, accepting work, thermally fine; what is queued and why; what failed overnight.",
                             lambda: "Use gryvia_fleet, then gryvia_jobs (state=failed, then queued) and gryvia_decisions. " + GUARD),
}


def gryvia_url(raw):
    """Plain HTTP only to loopback, like the dashboard, CLI and native app."""
    u = urllib.parse.urlparse(raw)
    if u.scheme == "https" or (u.scheme == "http" and u.hostname in ("localhost", "127.0.0.1", "::1")):
        return raw.rstrip("/")
    raise ValueError("GRYVIA_URL must be https://, or http:// to localhost")


class Server:
    def __init__(self, base, token, allow_write=False, opener=None):
        self.base, self.token, self.allow_write = base, token, allow_write
        self.opener = opener or urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def tools(self):
        out = {n: (d, s, "GET") for n, (d, s, _) in READ_TOOLS.items()}
        if self.allow_write:
            out.update({n: (d, s, "POST") for n, (d, s, _) in WRITE_TOOLS.items()})
        return out

    def request(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method,
                                     headers={"Authorization": "Bearer " + self.token, "Content-Type": "application/json"})
        try:
            with self.opener.open(req, timeout=15) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            detail = ""
            try:
                detail = json.loads(e.read()).get("error", "")
            except (ValueError, OSError):
                pass
            raise RuntimeError(f"Gryvia answered HTTP {e.code}" + (f": {detail}" if detail else "") + ".") from None
        except (urllib.error.URLError, OSError, ValueError):
            raise RuntimeError("Gryvia is not reachable or returned invalid data.") from None

    @staticmethod
    def _validate(schema, args):
        unknown = set(args) - set(schema["properties"])
        missing = [k for k in schema.get("required", []) if k not in args]
        if unknown or missing:
            raise ValueError(f"Unexpected arguments: {sorted(unknown)}; missing: {missing}")
        for k, v in args.items():
            t = schema["properties"][k].get("type")
            if t in ("number", "integer") and (isinstance(v, bool) or not isinstance(v, (int, float))):
                raise ValueError(f"{k} must be a number")
            if t == "string" and not isinstance(v, str):
                raise ValueError(f"{k} must be a string")
            if t == "boolean" and not isinstance(v, bool):
                raise ValueError(f"{k} must be a boolean")
            if t == "array" and (not isinstance(v, list) or not all(isinstance(x, str) for x in v)):
                raise ValueError(f"{k} must be an array of strings")
            if "enum" in schema["properties"][k] and v not in schema["properties"][k]["enum"]:
                raise ValueError(f"{k} must be one of {schema['properties'][k]['enum']}")

    def call_tool(self, name, arguments):
        args = arguments or {}
        if name in READ_TOOLS:
            _, schema, build = READ_TOOLS[name]
            self._validate(schema, args)
            data = self.request("GET", build(args))
            if name == "gryvia_jobs":
                jobs = [j for j in data["jobs"] if not args.get("state") or j["state"] == args["state"]][: args.get("limit", 30)]
                data = [{"id": j["id"][:12], "state": j["state"], "tenant": j["tenant"], "model": j["spec"].get("model") or j["spec"]["runtime"],
                         "kind": j["spec"].get("kind", "inference"), "node": j.get("node"), "memoryGiB": j["spec"]["memoryGiB"], "reason": j.get("reason", "")} for j in jobs]
            elif name == "gryvia_fleet":
                data = {"nodes": [{k: n.get(k) for k in ("id", "online", "drain", "gpuModel", "gpuCores", "gpuUtilPct", "budgetGiB", "totalGiB", "thermal", "presence", "policy", "gate", "runtimes")} for n in data["nodes"]],
                        "tenants": data["tenants"]}
            elif name == "gryvia_models":
                data = data["models"]
            elif name == "gryvia_decisions":
                data = data.get("decisions", [])
        elif self.allow_write and name in WRITE_TOOLS:
            _, schema, (method, path, body) = WRITE_TOOLS[name]
            self._validate(schema, args)
            data = self.request(method, path(args), body(args))
        elif name in WRITE_TOOLS:
            raise PermissionError("Write tools are disabled. Start the server with --allow-write to enable them.")
        else:
            raise KeyError(name)
        text = json.dumps(data, indent=2)
        return text if len(text) <= MAX_TEXT else text[:MAX_TEXT] + "\n[truncated: ask for a smaller range or limit]"

    def handle(self, message):
        method, ident, params = message.get("method"), message.get("id"), message.get("params") or {}

        def result(v):
            return {"jsonrpc": "2.0", "id": ident, "result": v}

        def error(code, text):
            return {"jsonrpc": "2.0", "id": ident, "error": {"code": code, "message": text}}
        if ident is None:
            return None
        if method == "initialize":
            return result({"protocolVersion": PROTOCOL, "serverInfo": {"name": "gryvia", "version": _version()},
                           "capabilities": {"tools": {}, "prompts": {}},
                           "instructions": GUARD + (" Write tools are ENABLED." if self.allow_write else " Read-only.")})
        if method == "ping":
            return result({})
        if method == "tools/list":
            return result({"tools": [{"name": n, "description": d, "inputSchema": s, "annotations": {"readOnlyHint": m == "GET", "destructiveHint": False}}
                                     for n, (d, s, m) in self.tools().items()]})
        if method == "tools/call":
            name = params.get("name")
            if name not in READ_TOOLS and name not in WRITE_TOOLS:
                return error(-32602, f"Unknown tool: {name}")
            try:
                return result({"content": [{"type": "text", "text": self.call_tool(name, params.get("arguments"))}]})
            except PermissionError as e:
                return error(-32001, str(e))
            except ValueError as e:
                return error(-32602, str(e))
            except RuntimeError as e:
                return result({"content": [{"type": "text", "text": str(e)}], "isError": True})
        if method == "prompts/list":
            return result({"prompts": [{"name": n, "description": d} for n, (d, _) in PROMPTS.items()]})
        if method == "prompts/get":
            name = params.get("name")
            if name not in PROMPTS:
                return error(-32602, f"Unknown prompt: {name}")
            return result({"description": PROMPTS[name][0], "messages": [{"role": "user", "content": {"type": "text", "text": PROMPTS[name][1]()}}]})
        return error(-32601, f"Method not found: {method}")


def _version():
    try:
        from pathlib import Path
        return (Path(__file__).resolve().parents[1] / "VERSION").read_text().strip()
    except OSError:
        return "dev"


def main(argv=None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    allow = "--allow-write" in argv or os.getenv("GRYVIA_MCP_ALLOW_WRITE") == "1"
    token = os.getenv("GRYVIA_TOKEN", "")
    if not token:
        print("GRYVIA_TOKEN is required", file=sys.stderr)
        return 2
    try:
        base = gryvia_url(os.getenv("GRYVIA_URL", "http://127.0.0.1:8443"))
    except ValueError as e:
        print(str(e), file=sys.stderr)
        return 2
    server = Server(base, token, allow)
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except ValueError:
            print(json.dumps({"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": "Parse error"}}), flush=True)
            continue
        reply = server.handle(msg)
        if reply is not None:
            print(json.dumps(reply), flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
