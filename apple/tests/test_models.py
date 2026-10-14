# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import json, tempfile, threading, unittest, urllib.error, urllib.request
from server.app import make_server
from server.core import Store, Problem


class ModelHubTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.t = [1000.0]
        self.store = Store(self.temp.name + "/db", clock=lambda: self.t[0])
        self.tokens = {"a" * 32: {"role": "admin"}, "t" * 32: {"role": "tenant", "tenant": "default"}, "n" * 32: {"role": "agent", "node": "mini"}}
        self.server = make_server(self.store, self.tokens, port=0)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.base = f"http://127.0.0.1:{self.server.server_port}"

    def tearDown(self):
        self.server.shutdown(); self.server.server_close(); self.store.db.close(); self.temp.cleanup()

    def req(self, path, token="a" * 32, body=None):
        r = urllib.request.Request(self.base + path, headers={"Authorization": "Bearer " + token}, data=json.dumps(body).encode() if body is not None else None)
        try:
            with urllib.request.urlopen(r) as res:
                return res.status, json.load(res)
        except urllib.error.HTTPError as e:
            return e.code, json.load(e)

    def run_job(self, job_id, logs, success=True):
        self.req("/api/agent/heartbeat", "n" * 32, dict(budgetGiB=16, runtimes=["download", "bench", "evaluate", "llama", "demo"]))
        _, c = self.req("/api/agent/claim", "n" * 32, {})
        self.assertEqual(c["job"]["id"], job_id)
        lease = c["job"]["lease"]
        self.req("/api/agent/logs", "n" * 32, dict(id=job_id, lease=lease, lines=logs))
        self.req("/api/agent/complete", "n" * 32, dict(id=job_id, lease=lease, success=success, result={"exitCode": 0 if success else 1}))

    def test_memory_estimate_scales_with_quant(self):
        q4 = Store.estimate_memory_gib(8, "Q4_K_M")
        f16 = Store.estimate_memory_gib(8, "F16")
        self.assertTrue(4 < q4 < 8 < f16)

    def test_onboard_creates_model_and_download_job_then_ready_on_result(self):
        status, r = self.req("/api/models/onboard", body=dict(id="tiny", paramsB=1.1, quant="Q4_K_M", source=dict(kind="hf", repo="TheBloke/tiny", file="tiny.Q4_K_M.gguf", maxGiB=2)))
        self.assertEqual(status, 201)
        state = self.req("/api/state")[1]
        m = next(x for x in state["models"] if x["id"] == "tiny")
        self.assertEqual(m["status"], "onboarding")
        self.assertGreater(m["memoryGiB"], 0.7)
        job = next(j for j in state["jobs"] if j["id"] == r["job"])
        self.assertEqual((job["spec"]["kind"], job["spec"]["modelId"], job["spec"]["runtime"]), ("download", "tiny", "download"))
        self.assertIn("--repo", job["spec"]["args"])
        self.run_job(r["job"], [json.dumps(dict(kind="progress", pct=50)), json.dumps(dict(kind="result", path="/m/tiny.gguf", sha256="ab" * 32, bytes=123))])
        m = next(x for x in self.req("/api/state")[1]["models"] if x["id"] == "tiny")
        self.assertEqual((m["status"], m["path"], m["sizeBytes"], m["node"]), ("ready", "/m/tiny.gguf", 123, "mini"))
        self.assertEqual(self.req("/api/results?model=tiny")[1][0]["kind"], "download")

    def test_failed_download_marks_model_failed(self):
        _, r = self.req("/api/models/onboard", body=dict(id="bad", source=dict(kind="hf", repo="a/b", file="x.gguf")))
        self.run_job(r["job"], ["oops"], success=False)
        m = next(x for x in self.req("/api/state")[1]["models"] if x["id"] == "bad")
        self.assertEqual(m["status"], "failed")

    def test_onboard_rejects_unsafe_sources(self):
        bad = [dict(kind="hf", repo="a/b", file="../../etc/passwd"), dict(kind="hf", repo="../x", file="m.gguf"),
               dict(kind="url", url="http://huggingface.co/x"), dict(kind="url", url="https://evil.example/x.gguf"), dict(kind="ftp")]
        for src in bad:
            with self.subTest(src=src):
                self.assertEqual(self.req("/api/models/onboard", body=dict(id="m", source=src))[0], 400)
        self.assertEqual(self.req("/api/models/onboard", "t" * 32, dict(id="m", source=dict(kind="hf", repo="a/b", file="m.gguf")))[0], 403)

    def test_benchmark_and_eval_results_recorded(self):
        self.store.model("m", dict(memoryGiB=2, runtime="llama", status="ready", path="/m.gguf"))
        sid = self.store.submit("default", dict(runtime="bench", kind="benchmark", modelId="m", memoryGiB=2, args=["--model", "/m.gguf"], mode="exclusive"))
        self.run_job(sid, [json.dumps(dict(kind="result", promptTokensPerSecond=420.5, generationTokensPerSecond=61.2))])
        res = self.req("/api/results?kind=benchmark")[1]
        self.assertEqual(res[0]["body"]["generationTokensPerSecond"], 61.2)
        self.assertTrue(res[0]["body"]["success"])

    def test_suites_validated(self):
        ok = self.req("/api/suites", body=dict(id="smoke", cases=[dict(prompt="2+2=", contains="4")]))
        self.assertEqual(ok[0], 200)
        self.assertEqual(self.req("/api/suites")[1]["smoke"][0]["contains"], "4")
        self.assertEqual(self.req("/api/suites", body=dict(id="x", cases=[dict(prompt="p")]))[0], 400)
        self.assertEqual(self.req("/api/suites", body=dict(id="x", cases=[]))[0], 400)


if __name__ == "__main__":
    unittest.main()
