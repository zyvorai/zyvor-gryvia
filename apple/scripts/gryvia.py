#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Dependency-free CLI for the Gryvia Apple control plane.

Output is a coloured table on a terminal and JSON when piped (or with --json),
so existing scripts keep working.
"""
import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

TERMINAL_STATES = {"succeeded", "failed", "cancelled", "canceled"}
COLORS = {"running": "34", "queued": "33", "succeeded": "32", "failed": "31", "cancelled": "90", "online": "32", "offline": "90", "draining": "33"}


class CliError(Exception):
    pass


def color_on() -> bool:
    return sys.stdout.isatty() and "NO_COLOR" not in os.environ


def paint(text: str, code: str) -> str:
    return f"\033[{code}m{text}\033[0m" if color_on() else text


def table(headers, rows, state_col=None) -> str:
    cells = [[str(c) for c in r] for r in rows]
    widths = [max(len(h), *(len(r[i]) for r in cells)) if cells else len(h) for i, h in enumerate(headers)]
    out = [paint("  ".join(h.ljust(w) for h, w in zip(headers, widths)), "1")]
    for r in cells:
        line = []
        for i, (c, w) in enumerate(zip(r, widths)):
            padded = c.ljust(w)
            if i == state_col:
                padded = paint(padded, COLORS.get(c, "0"))
            line.append(padded)
        out.append("  ".join(line))
    return "\n".join(out)


def keychain_token() -> str:
    if sys.platform != "darwin":
        return ""
    r = subprocess.run(["security", "find-generic-password", "-s", "dev.zyvor.gryvia.admin", "-w"],
                       capture_output=True, text=True, check=False)
    return r.stdout.strip() if r.returncode == 0 else ""


def resolve_token(args) -> str:
    if args.token_file:
        return open(args.token_file, encoding="utf-8").read().strip()
    return os.getenv(args.token_env, "") or keychain_token()


class Client:
    def __init__(self, server: str, token: str):
        u = urllib.parse.urlsplit(server)
        if u.scheme not in ("http", "https") or not u.hostname or (u.scheme == "http" and u.hostname not in ("localhost", "127.0.0.1", "::1")):
            raise CliError("remote server requires HTTPS (or use http://127.0.0.1 with an ssh tunnel)")
        self.base, self.token = server.rstrip("/"), token

    def call(self, path: str, body=None):
        req = urllib.request.Request(
            f"{self.base}/api/{path}", data=json.dumps(body).encode() if body is not None else None,
            headers={"Authorization": "Bearer " + self.token, "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            detail = e.read().decode(errors="replace")
            hint = {401: "Token rejected. Check GRYVIA_ADMIN_TOKEN (see local/credentials.env).",
                    403: "This token's role is not allowed to do that."}.get(e.code, "")
            raise CliError(f"HTTP {e.code}: {detail}" + (f"\n  hint: {hint}" if hint else "")) from None
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            raise CliError(f"cannot reach {self.base}: {getattr(e, 'reason', e)}\n  hint: is the server running? try `make run-server`.") from None

    def raw(self, path: str) -> str:
        req = urllib.request.Request(self.base + path, headers={"Authorization": "Bearer " + self.token})
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                return r.read().decode()
        except urllib.error.HTTPError as e:
            raise CliError(f"HTTP {e.code} for {path}") from None
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            raise CliError(f"cannot reach {self.base}: {getattr(e, 'reason', e)}") from None

    def health(self):
        with urllib.request.urlopen(self.base + "/healthz", timeout=5) as r:
            return json.load(r)


def render_status(state) -> str:
    nodes = [(n["id"], "draining" if n.get("drain") else ("online" if n.get("online") else "offline"),
              f'{n["budgetGiB"]:.1f} GiB', ",".join(n.get("runtimes", [])), n.get("thermal") or "-") for n in state["nodes"]]
    jobs = [(j["id"][:10], j["state"], j["tenant"], j["spec"].get("model") or j["spec"]["runtime"],
             f'{j["spec"]["memoryGiB"]:.1f} GiB', j.get("node") or "-", j.get("reason", "")) for j in state["jobs"][:30]]
    counts = {s: sum(1 for j in state["jobs"] if j["state"] == s) for s in ("running", "queued", "succeeded", "failed")}
    head = "  ".join(paint(f"{v} {k}", COLORS[k]) for k, v in counts.items())
    parts = [head, "", paint("Nodes", "1"), table(["NODE", "STATE", "BUDGET", "RUNTIMES", "THERMAL"], nodes, 1) if nodes else "  none registered",
             "", paint("Workloads", "1"), table(["JOB", "STATE", "TENANT", "MODEL", "MEM", "NODE", "REASON"], jobs, 1) if jobs else "  none yet"]
    return "\n".join(parts)


def find_job(client: Client, ident: str):
    return next((j for j in client.call("state")["jobs"] if j["id"] == ident or j["id"].startswith(ident)), None)


def follow_logs(client: Client, ident: str, follow: bool):
    seen = 0
    while True:
        rows = client.call("logs/" + urllib.parse.quote(ident, safe=""))
        for r in rows:
            if r["seq"] > seen:
                print(r["text"])
                seen = r["seq"]
        if not follow:
            return
        job = find_job(client, ident)
        if job and job["state"] in TERMINAL_STATES:
            return
        time.sleep(1)


def wait_job(client: Client, ident: str) -> int:
    last = None
    while True:
        job = find_job(client, ident)
        if job is None:
            raise CliError("job not found")
        if job["state"] != last:
            print(f'{ident[:10]}  {paint(job["state"], COLORS.get(job["state"], "0"))}', file=sys.stderr)
            last = job["state"]
        if job["state"] in TERMINAL_STATES:
            return 0 if job["state"] == "succeeded" else 1
        time.sleep(1)


def doctor(client: Client) -> int:
    ok = True

    def line(good: bool, msg: str):
        nonlocal ok
        ok = ok and good
        print(("  " + paint("✔", "32") if good else "  " + paint("✘", "31")) + " " + msg)

    try:
        h = client.health()
        line(h.get("status") == "ok", f"server reachable at {client.base}")
    except Exception as e:  # noqa: BLE001 - diagnostic output
        line(False, f"server not reachable: {e}")
        return 1
    try:
        s = client.call("state")
        line(True, "token accepted")
        online = sum(1 for n in s["nodes"] if n.get("online"))
        line(online > 0, f"{online} agent(s) online of {len(s['nodes'])} registered" + ("" if online else " — start `make run-agent`"))
    except CliError as e:
        line(False, str(e).splitlines()[0])
    return 0 if ok else 1


VERSION = "0.2.0"
LICENSE_LINE = "Gryvia Apple " + VERSION + " · BUSL-1.1 · Copyright © 2026 Zyvor AI Labs Private Limited"
ZSH_COMPLETION = """#compdef gryvia.py gryvia
_arguments '--server=[server origin]' '--json[raw JSON output]' '--output=[table|json|csv]' '1:command:(status top nodes jobs models fuse datasets dataset-add train metrics-run deploy undeploy onboard bench eval results submit cancel priority logs drain tenant metrics quickstart doctor completion)'
"""


def jobs_csv(jobs) -> str:
    def cell(v):
        s = str(v if v is not None else "")
        return '"' + s.replace('"', '""') + '"' if any(c in s for c in ',"\n') else s
    head = ["id", "tenant", "state", "runtime", "model", "memoryGiB", "priority", "node", "attempt", "labels"]
    rows = [[j["id"], j["tenant"], j["state"], j["spec"]["runtime"], j["spec"].get("model", ""), j["spec"]["memoryGiB"],
             j["spec"].get("priority", ""), j.get("node") or "", j.get("attempt", 0), "|".join(j["spec"].get("labels", []))] for j in jobs]
    return "\n".join(",".join(cell(c) for c in r) for r in [head] + rows)


def render_nodes(state) -> str:
    rows = []
    for n in state["nodes"]:
        used = sum(j["spec"]["memoryGiB"] for j in state["jobs"] if j.get("node") == n["id"] and j["state"] in ("running", "cancelling"))
        rows.append((n["id"], "draining" if n.get("drain") else ("online" if n.get("online") else "offline"),
                     f'{used:.1f}/{n["budgetGiB"]:.1f} GiB', ",".join(n.get("runtimes", [])), n.get("thermal") or "-", n.get("gpu") or n.get("host") or "-"))
    return table(["NODE", "STATE", "RESERVED", "RUNTIMES", "THERMAL", "HARDWARE"], rows, 1) if rows else "  none registered"


def quickstart(client: Client) -> int:
    state = client.call("state")
    online = [n for n in state["nodes"] if n.get("online")]
    steps = [
        (bool(online), "Connect a Mac", "run `make run-agent` here, or `make deploy` for another Mac"),
        (bool(state["jobs"]), "Run a job", "gryvia.py submit examples/demo-job.json --wait"),
        (any(j["state"] == "succeeded" for j in state["jobs"]), "Watch it finish", "gryvia.py top   (or open the dashboard)"),
    ]
    for ok, title, how in steps:
        print(("  " + paint("✔", "32") if ok else "  " + paint("○", "33")) + f" {title}: {how}")
    nxt = next((t for ok, t, _ in steps if not ok), None)
    print("\n" + (f"Next: {nxt}." if nxt else "All set. `gryvia.py top` for a live view, `gryvia.py --help` for everything."))
    return 0


def build_parser():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--server", default=os.getenv("GRYVIA_SERVER", "http://127.0.0.1:8443"))
    p.add_argument("--token-env", default="GRYVIA_ADMIN_TOKEN")
    p.add_argument("--token-file", help="read the token from a file (default: env, then login Keychain)")
    p.add_argument("--json", action="store_true", help="always print JSON")
    p.add_argument("--output", choices=["table", "json", "csv"], help="output format (default: table on a terminal, JSON when piped)")
    p.add_argument("--version", action="version", version=LICENSE_LINE)
    sub = p.add_subparsers(dest="op", required=True)
    s = sub.add_parser("status"); s.add_argument("-w", "--watch", action="store_true")
    s = sub.add_parser("submit"); s.add_argument("file"); s.add_argument("--wait", action="store_true", help="block until the job finishes")
    s = sub.add_parser("cancel"); s.add_argument("id")
    s = sub.add_parser("logs"); s.add_argument("id"); s.add_argument("-f", "--follow", action="store_true")
    s = sub.add_parser("drain"); s.add_argument("id"); s.add_argument("--resume", action="store_true")
    s = sub.add_parser("tenant"); s.add_argument("id"); s.add_argument("--quota", type=float, required=True); s.add_argument("--weight", type=float, default=1)
    s = sub.add_parser("nodes", help="list Macs and their reservations")
    s = sub.add_parser("jobs", help="list workloads"); s.add_argument("--state"); s.add_argument("--tenant"); s.add_argument("--label"); s.add_argument("--limit", type=int, default=50)
    s = sub.add_parser("top", help="live fleet view (refreshes every 2s)")
    s = sub.add_parser("priority", help="set a queued job's priority"); s.add_argument("id"); s.add_argument("value", type=int)
    sub.add_parser("metrics", help="print Prometheus metrics (admin token)")
    sub.add_parser("models", help="list models in the hub")
    s = sub.add_parser("onboard", help="download a model onto a Mac"); s.add_argument("id")
    s.add_argument("--repo"); s.add_argument("--file"); s.add_argument("--url"); s.add_argument("--revision", default="main")
    s.add_argument("--params-b", type=float); s.add_argument("--quant", default=""); s.add_argument("--runtime", default="llama"); s.add_argument("--node"); s.add_argument("--max-gib", type=float, default=8)
    s = sub.add_parser("bench", help="benchmark a ready model (tokens/s)"); s.add_argument("model")
    s = sub.add_parser("eval", help="run a prompt suite against a ready model"); s.add_argument("model"); s.add_argument("cases", nargs="?", help="local JSON file with a list of cases (4 KB inline limit)")
    s.add_argument("--remote-cases", help="path to a cases file already on the Mac (under its managed folders); no size limit beyond 200 cases")
    s.add_argument("--base", action="store_true", help="MLX adapter models: evaluate the base model instead (for before/after comparison)")
    s = sub.add_parser("fuse", help="fuse a LoRA adapter model into a standalone MLX model"); s.add_argument("model", help="adapter model id")
    sub.add_parser("datasets", help="list datasets")
    s = sub.add_parser("dataset-add", help="download a dataset file from Hugging Face"); s.add_argument("id"); s.add_argument("--repo", required=True); s.add_argument("--file", required=True)
    s.add_argument("--revision", default="main"); s.add_argument("--max-gib", type=float, default=1); s.add_argument("--node")
    s = sub.add_parser("train", help="start a LoRA fine-tune (MLX)"); s.add_argument("base", help="MLX model repo id or local path"); s.add_argument("--dataset", required=True)
    s.add_argument("--iters", type=int, default=100); s.add_argument("--batch-size", type=int, default=2); s.add_argument("--layers", type=int, default=8); s.add_argument("--lr", type=float, default=1e-5); s.add_argument("--memory-gib", type=float, default=6)
    s = sub.add_parser("metrics-run", help="print loss history of a training run"); s.add_argument("job")
    s = sub.add_parser("deploy", help="serve a ready GGUF model with llama-server"); s.add_argument("model")
    s = sub.add_parser("undeploy", help="stop a deployment"); s.add_argument("model")
    s = sub.add_parser("results", help="show benchmark/eval/download history"); s.add_argument("--model"); s.add_argument("--kind")
    sub.add_parser("quickstart", help="what to do next, based on your fleet's state")
    sub.add_parser("doctor", help="check server, token and agents")
    sub.add_parser("completion", help="print zsh completion")
    return p


def emit(data, args, pretty=None):
    if args.output == "json" or args.json or (args.output is None and not sys.stdout.isatty()) or pretty is None:
        print(json.dumps(data, indent=2))
    else:
        print(pretty(data))


def run(args) -> int:
    if args.op == "completion":
        print(ZSH_COMPLETION, end="")
        return 0
    token = resolve_token(args)
    if not token:
        raise CliError(f"missing token: set {args.token_env}, pass --token-file, or store it in the Keychain (service dev.zyvor.gryvia.admin)")
    c = Client(args.server, token)
    if args.op == "doctor":
        return doctor(c)
    if args.op == "quickstart":
        return quickstart(c)
    if args.op == "models":
        models = c.call("state")["models"]
        rows = [(m["id"], m.get("status", "ready"), m["runtime"], m.get("quant") or "-", f'{m["memoryGiB"]:.1f} GiB', m.get("node") or "-", m.get("source") or "-") for m in models]
        emit(models, args, lambda _: table(["MODEL", "STATUS", "RUNTIME", "QUANT", "MEMORY", "MAC", "SOURCE"], rows, 1) if rows else "  no models yet: gryvia.py onboard ...")
        return 0
    if args.op == "onboard":
        src = {"kind": "url", "url": args.url} if args.url else {"kind": "hf", "repo": args.repo, "file": args.file, "revision": args.revision}
        src["maxGiB"] = args.max_gib
        body = {"id": args.id, "source": src, "runtime": args.runtime, "quant": args.quant}
        if args.params_b:
            body["paramsB"] = args.params_b
        if args.node:
            body["node"] = args.node
        job = c.call("models/onboard", body)
        emit(job, args, lambda j: f'download started: job {j["job"][:10]}. Follow with: gryvia.py logs {j["job"]} -f')
        return 0
    if args.op in ("bench", "eval"):
        models = c.call("state")["models"]
        m = next((m for m in models if m["id"] == args.model), None)
        if not m or m.get("status", "ready") != "ready" or not m.get("path"):
            raise CliError(f"model '{args.model}' is not ready (status: {m.get('status') if m else 'unknown'})")
        kind, runtime = ("benchmark", "bench") if args.op == "bench" else ("evaluate", "evaluate")
        jobargs = ["--model", m["path"]]
        if args.op == "eval" and m.get("kind") == "lora-adapter":
            base = next((b for b in models if b["id"] == m.get("parent")), {})
            jobargs = ["--engine", "mlx", "--model", base.get("source") or base.get("path") or m["parent"]]
            if not args.base:
                jobargs += ["--adapter", m["path"]]
        elif args.op == "eval" and m.get("runtime") == "mlx":
            jobargs = ["--engine", "mlx", "--model", m["path"]]
        if args.op == "eval" and args.remote_cases:
            jobargs += ["--cases-file", args.remote_cases]
        elif args.op == "eval":
            if not args.cases:
                raise CliError("give a cases file, or --remote-cases PATH")
            with open(args.cases, encoding="utf-8") as f:
                inline = json.dumps(json.load(f))
            if len(inline) > 3900:
                raise CliError("suite is over the 4 KB inline limit; copy it to the Mac and use --remote-cases")
            jobargs += ["--cases", inline]
        spec = {"runtime": runtime, "kind": kind, "modelId": m["id"], "model": f"{kind} {m['id']}", "memoryGiB": max(1.0, m["memoryGiB"]),
                "priority": 55, "mode": "exclusive", "timeoutSeconds": 3600, "retries": 0, "args": jobargs, "labels": [kind]}
        if m.get("node"):
            spec["node"] = m["node"]
        job = c.call("jobs", spec)
        emit(job, args, lambda j: f'{kind} queued: job {j["id"][:10]}')
        return 0
    if args.op == "datasets":
        ds = c.call("state").get("datasets", [])
        rows = [(d["id"], d.get("status", "?"), d.get("rows") if d.get("rows") is not None else "-", f'{d.get("bytes", 0) / 1e6:.1f} MB', d.get("node") or "-") for d in ds]
        emit(ds, args, lambda _: table(["DATASET", "STATUS", "ROWS", "SIZE", "MAC"], rows, 1) if rows else "  none: gryvia.py dataset-add ...")
        return 0
    if args.op == "dataset-add":
        body = {"id": args.id, "source": {"repo": args.repo, "file": args.file, "revision": args.revision, "maxGiB": args.max_gib}}
        if args.node:
            body["node"] = args.node
        emit(c.call("datasets/onboard", body), args, lambda j: f'dataset download started: job {j["job"][:10]}')
        return 0
    if args.op == "train":
        ds = next((d for d in c.call("state").get("datasets", []) if d["id"] == args.dataset and d.get("status") == "ready"), None)
        if not ds:
            raise CliError(f"dataset '{args.dataset}' is not ready (see: gryvia.py datasets)")
        mid = "".join(ch if ch.isalnum() or ch in "._-" else "__" for ch in args.base)
        if not any(m["id"] == mid for m in c.call("state")["models"]):
            c.call("models", {"id": mid, "runtime": "mlx", "memoryGiB": args.memory_gib, "source": args.base, "status": "ready"})
        spec = {"runtime": "train", "kind": "train", "modelId": mid, "datasetId": args.dataset, "model": f"train {mid}", "memoryGiB": args.memory_gib, "priority": 60,
                "mode": "exclusive", "retries": 0, "timeoutSeconds": 21600, "labels": ["train"],
                "args": ["--model", args.base, "--data", "{{dataset.path}}", "--iters", str(args.iters), "--batch-size", str(args.batch_size), "--num-layers", str(args.layers), "--learning-rate", str(args.lr)]}
        if ds.get("node"):
            spec["node"] = ds["node"]
        emit(c.call("jobs", spec), args, lambda j: f'training queued: job {j["id"][:10]}. Loss: gryvia.py metrics-run {j["id"]}')
        return 0
    if args.op == "metrics-run":
        m = c.call("runs/" + args.job + "/metrics")
        steps = sorted({s for k in ("trainLoss", "valLoss") for s, _ in m.get(k, [])})
        tl, vl = dict(m.get("trainLoss", [])), dict(m.get("valLoss", []))
        emit(m, args, lambda _: table(["STEP", "TRAIN LOSS", "VAL LOSS"], [(s, f"{tl[s]:.4f}" if s in tl else "-", f"{vl[s]:.4f}" if s in vl else "-") for s in steps]) if steps else "  no metrics yet")
        return 0
    if args.op == "fuse":
        models = c.call("state")["models"]
        m = next((m for m in models if m["id"] == args.model), None)
        if not m or m.get("kind") != "lora-adapter" or not m.get("path"):
            raise CliError("fuse needs a ready LoRA adapter model (see: gryvia.py models)")
        base = next((b for b in models if b["id"] == m.get("parent")), {})
        spec = {"runtime": "fuse", "kind": "fuse", "modelId": m["id"], "model": f"fuse {m['id']}", "memoryGiB": max(2.0, m["memoryGiB"]), "priority": 55,
                "mode": "exclusive", "retries": 0, "timeoutSeconds": 3600, "labels": ["fuse"],
                "args": ["--model", base.get("source") or m["parent"], "--adapter", m["path"], "--name", m["id"]]}
        if m.get("node"):
            spec["node"] = m["node"]
        emit(c.call("jobs", spec), args, lambda j: f'fusing: job {j["id"][:10]}; a `{m["id"]}-fused` model appears when done')
        return 0
    if args.op in ("deploy", "undeploy"):
        m = next((m for m in c.call("state")["models"] if m["id"] == args.model), None)
        if not m:
            raise CliError("unknown model")
        if args.op == "undeploy":
            if not m.get("deployJob"):
                raise CliError("model is not deployed")
            emit(c.call("cancel", {"id": m["deployJob"]}), args)
            return 0
        if not m.get("path"):
            raise CliError("model has no downloaded file yet")
        if m.get("endpoints"):
            raise CliError(f"already deployed at {m['endpoints'][-1]}; `gryvia.py undeploy {m['id']}` first")
        spec = {"runtime": "serve", "kind": "serve", "modelId": m["id"], "model": f"serve {m['id']}", "memoryGiB": max(1.0, m["memoryGiB"]), "priority": 70,
                "mode": "shared", "retries": 0, "timeoutSeconds": 604800, "args": ["--model", m["path"]] + (["--engine", "mlx"] if m.get("runtime") == "mlx" else []), "labels": ["serve"]}
        if m.get("node"):
            spec["node"] = m["node"]
        emit(c.call("jobs", spec), args, lambda j: f'deploying: job {j["id"][:10]}; endpoint appears in `gryvia.py models`')
        return 0
    if args.op == "results":
        q = "&".join(f"{k}={v}" for k, v in (("model", args.model), ("kind", args.kind)) if v)
        res = json.loads(c.raw("/api/results" + ("?" + q if q else "")))
        def pretty(rows):
            def detail(r):
                b = r["body"]
                if r["kind"] == "benchmark":
                    return f'{b.get("generationTokensPerSecond", 0):.1f} tok/s gen, {b.get("promptTokensPerSecond", 0):.0f} tok/s prompt'
                if r["kind"] == "evaluate":
                    return f'{b.get("passed")}/{b.get("total")} passed'
                return f'{b.get("bytes", 0) / 1024**3:.2f} GiB'
            return table(["WHEN", "MODEL", "KIND", "OK", "DETAIL", "MAC"], [(time.strftime("%m-%d %H:%M", time.localtime(r["at"])), r["model"], r["kind"], "yes" if r["body"].get("success") else "no", detail(r), r["node"]) for r in rows])
        emit(res, args, pretty)
        return 0
    if args.op == "metrics":
        print(c.raw("/metrics"), end="")
        return 0
    if args.op == "nodes":
        state = c.call("state")
        emit(state["nodes"], args, lambda _: render_nodes(state))
        return 0
    if args.op == "jobs":
        jobs = [j for j in c.call("state")["jobs"]
                if (not args.state or j["state"] == args.state) and (not args.tenant or j["tenant"] == args.tenant)
                and (not args.label or args.label in j["spec"].get("labels", []))][: args.limit]
        if args.output == "csv":
            print(jobs_csv(jobs))
        else:
            rows = [(j["id"][:10], j["state"], j["tenant"], j["spec"].get("model") or j["spec"]["runtime"], f'{j["spec"]["memoryGiB"]:.1f} GiB', j.get("node") or "-") for j in jobs]
            emit(jobs, args, lambda _: table(["JOB", "STATE", "TENANT", "MODEL", "MEM", "NODE"], rows, 1) if rows else "  no matching workloads")
        return 0
    if args.op == "priority":
        emit(c.call("priority", {"id": args.id, "priority": args.value}), args)
        return 0
    if args.op == "top":
        args.op, args.watch = "status", True
    if args.op == "status":
        while True:
            state = c.call("state")
            if args.watch and sys.stdout.isatty():
                print("\033[2J\033[H", end="")
            emit(state, args, render_status)
            if not args.watch:
                return 0
            time.sleep(2)
    if args.op == "submit":
        with open(args.file, encoding="utf-8") as f:
            job = c.call("jobs", json.load(f))
        emit(job, args, lambda j: f'submitted {paint(j.get("id", "?"), "1")}')
        return wait_job(c, job["id"]) if args.wait else 0
    if args.op == "cancel":
        emit(c.call("cancel", {"id": args.id}), args)
    elif args.op == "logs":
        follow_logs(c, args.id, args.follow)
    elif args.op == "drain":
        emit(c.call("drain", {"id": args.id, "drain": not args.resume}), args)
    elif args.op == "tenant":
        emit(c.call("tenants", {"id": args.id, "quota": args.quota, "weight": args.weight}), args)
    return 0


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return run(args)
    except CliError as e:
        print(paint("error: ", "31") + str(e), file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    sys.exit(main())
