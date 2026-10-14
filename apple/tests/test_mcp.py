# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import io, json, os, sys, tempfile, threading, unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "mcp"))
import gryvia_mcp as m  # noqa: E402
from server.app import make_server  # noqa: E402
from server.core import Store  # noqa: E402

TOKEN = "a" * 32


class McpTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.store = Store(self.tmp.name + "/db")
        self.store.register("mac", dict(budgetGiB=8, runtimes=["demo"], gpuModel="Apple M4", gpuCores=8, gpuUtilPct=12.0,
                                        presence=dict(idleSeconds=5, onBattery=False)))
        self.server = make_server(self.store, {TOKEN: {"role": "admin"}, "t" * 32: {"role": "tenant", "tenant": "default"}}, port=0)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base = f"http://127.0.0.1:{self.server.server_port}"

    def tearDown(self):
        self.server.shutdown(); self.server.server_close(); self.thread.join(); self.store.db.close(); self.tmp.cleanup()

    def rpc(self, srv, method, params=None, ident=1):
        return srv.handle({"jsonrpc": "2.0", "id": ident, "method": method, "params": params or {}})

    def test_read_only_by_default_and_every_listed_tool_is_get(self):
        srv = m.Server(self.base, TOKEN)
        tools = self.rpc(srv, "tools/list")["result"]["tools"]
        self.assertEqual({t["name"] for t in tools}, set(m.READ_TOOLS))
        self.assertTrue(all(t["annotations"]["readOnlyHint"] for t in tools))
        denied = self.rpc(srv, "tools/call", {"name": "gryvia_cancel_job", "arguments": {"id": "x"}})
        self.assertEqual(denied["error"]["code"], -32001)

    def test_write_tools_only_with_flag_and_marked_not_read_only(self):
        srv = m.Server(self.base, TOKEN, allow_write=True)
        tools = {t["name"]: t for t in self.rpc(srv, "tools/list")["result"]["tools"]}
        self.assertEqual(set(tools), set(m.READ_TOOLS) | set(m.WRITE_TOOLS))
        for name in m.WRITE_TOOLS:
            self.assertFalse(tools[name]["annotations"]["readOnlyHint"])

    def test_fleet_view_includes_gpu_and_gate(self):
        srv = m.Server(self.base, TOKEN)
        text = self.rpc(srv, "tools/call", {"name": "gryvia_fleet", "arguments": {}})["result"]["content"][0]["text"]
        fleet = json.loads(text)
        n = fleet["nodes"][0]
        self.assertEqual((n["gpuCores"], n["gpuModel"], n["gate"]["allowed"]), (8, "Apple M4", True))
        self.assertNotIn(TOKEN, text)

    def test_demo_job_then_jobs_listing_and_policy_change(self):
        srv = m.Server(self.base, TOKEN, allow_write=True)
        self.rpc(srv, "tools/call", {"name": "gryvia_run_demo_job", "arguments": {}})
        jobs = json.loads(self.rpc(srv, "tools/call", {"name": "gryvia_jobs", "arguments": {"state": "queued"}})["result"]["content"][0]["text"])
        self.assertEqual(jobs[0]["model"], "mcp demo")
        pol = self.rpc(srv, "tools/call", {"name": "gryvia_set_node_policy", "arguments": {"id": "mac", "mode": "paused"}})
        self.assertIn("paused", pol["result"]["content"][0]["text"])
        self.assertEqual(self.store.node_policy("mac")["mode"], "paused")

    def test_argument_validation(self):
        srv = m.Server(self.base, TOKEN, allow_write=True)
        for name, args in [("gryvia_jobs", {"state": "nope"}), ("gryvia_jobs", {"limit": True}), ("gryvia_cancel_job", {}),
                           ("gryvia_submit_job", {"runtime": "rm -rf", "memoryGiB": 1}), ("gryvia_submit_job", {"runtime": "demo", "memoryGiB": 1, "args": "--x"})]:
            with self.subTest(name=name, args=args):
                r = self.rpc(srv, "tools/call", {"name": name, "arguments": args})
                self.assertEqual(r["error"]["code"], -32602)

    def test_tenant_token_scopes_the_agent(self):
        srv = m.Server(self.base, "t" * 32, allow_write=True)
        r = self.rpc(srv, "tools/call", {"name": "gryvia_set_node_policy", "arguments": {"id": "mac", "mode": "paused"}})
        self.assertTrue(r["result"]["isError"])         # server enforces the role: 403

    def test_plain_http_only_to_loopback(self):
        with self.assertRaises(ValueError):
            m.gryvia_url("http://example.com")
        self.assertEqual(m.gryvia_url("https://fleet.example.com/"), "https://fleet.example.com")

    def test_stdio_loop(self):
        out = io.StringIO()
        old_in, old_out = sys.stdin, sys.stdout
        env = os.environ.copy()
        os.environ.update(GRYVIA_TOKEN=TOKEN, GRYVIA_URL=self.base)
        sys.stdin = io.StringIO(json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize"}) + "\n" + "not json\n")
        sys.stdout = out
        try:
            code = m.main([])
        finally:
            sys.stdin, sys.stdout = old_in, old_out
            os.environ.clear(); os.environ.update(env)
        self.assertEqual(code, 0)
        lines = [json.loads(x) for x in out.getvalue().splitlines()]
        self.assertEqual(lines[0]["result"]["serverInfo"]["name"], "gryvia")
        self.assertEqual(lines[1]["error"]["code"], -32700)


if __name__ == "__main__":
    unittest.main()
