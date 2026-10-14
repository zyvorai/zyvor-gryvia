# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import json, tempfile, unittest
from server.core import Store, Problem


class TrainingTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.t = [1000.0]
        self.s = Store(self.tmp.name + "/db", clock=lambda: self.t[0])
        self.s.register("mini", dict(budgetGiB=16, runtimes=["demo", "download", "train", "serve", "bench", "mlx", "llama", "fuse"]))

    def tearDown(self):
        self.s.db.close(); self.tmp.cleanup()

    def spec(self, **kw):
        base = dict(runtime="demo", memoryGiB=1, mode="shared", args=[])
        base.update(kw)
        return base

    def finish(self, claim, logs=(), success=True, result=None):
        if logs:
            self.s.logs("mini", claim["id"], claim["lease"], list(logs))
        self.s.complete("mini", claim["id"], claim["lease"], success, result or {})

    def test_metrics_ingested_from_logs(self):
        j = self.s.submit("default", self.spec(runtime="train", kind="train", modelId="base", datasetId="d1"))
        c = self.s.claim("mini")
        self.s.logs("mini", j, c["lease"], [json.dumps(dict(kind="metric", step=10, trainLoss=2.5, lr=1e-5, tokensPerSec=900.0)), "plain text", json.dumps(dict(kind="metric", step=20, trainLoss=2.1, valLoss=2.3))])
        m = self.s.run_metrics(j)
        self.assertEqual(m["trainLoss"], [[10, 2.5], [20, 2.1]])
        self.assertEqual(m["valLoss"], [[20, 2.3]])

    def test_dependency_blocks_then_runs_and_cancels_on_failure(self):
        a = self.s.submit("default", self.spec())
        b = self.s.submit("default", self.spec(after=[a], priority=90))
        c1 = self.s.claim("mini")
        self.assertEqual(c1["id"], a)
        self.assertIsNone(self.s.claim("mini"))
        waiting = next(j for j in self.s.snapshot()["jobs"] if j["id"] == b)
        self.assertEqual(waiting["reason"], "waiting for dependency")
        self.finish(c1)
        c2 = self.s.claim("mini")
        self.assertEqual(c2["id"], b)
        self.finish(c2)
        x = self.s.submit("default", self.spec())
        y = self.s.submit("default", self.spec(after=[x]))
        cx = self.s.claim("mini")
        self.finish(cx, success=False)
        self.assertIsNone(self.s.claim("mini"))
        self.assertEqual(next(j for j in self.s.snapshot()["jobs"] if j["id"] == y)["state"], "cancelled")

    def test_unknown_dependency_rejected(self):
        with self.assertRaises(Problem):
            self.s.submit("default", self.spec(after=["nope"]))

    def test_workflow_chain_and_placeholders(self):
        self.s.model("m", dict(memoryGiB=2, runtime="llama", status="onboarding"))
        wid = self.s.workflow("default", dict(name="onboard-bench", steps=[
            dict(name="download", spec=self.spec(runtime="download", kind="download", modelId="m", args=["--repo", "a/b", "--file", "f.gguf"])),
            dict(name="bench", spec=self.spec(runtime="bench", kind="benchmark", modelId="m", args=["--model", "{{model.path}}"]))]))
        snap = self.s.snapshot()
        wf = next(w for w in snap["workflows"] if w["id"] == wid)
        self.assertEqual([s["name"] for s in wf["steps"]], ["download", "bench"])
        c = self.s.claim("mini")
        self.s.logs("mini", c["id"], c["lease"], [json.dumps(dict(kind="result", path="/models/f.gguf", sha256="x", bytes=5))])
        self.s.complete("mini", c["id"], c["lease"], True, {})
        c2 = self.s.claim("mini")
        self.assertEqual(c2["spec"]["args"], ["--model", "/models/f.gguf"])

    def test_train_registers_adapter_model_with_lineage(self):
        self.s.model("base", dict(memoryGiB=4, runtime="mlx", status="ready", path="/m"))
        j = self.s.submit("default", self.spec(runtime="train", kind="train", modelId="base", datasetId="d"))
        c = self.s.claim("mini")
        self.finish(c, [json.dumps(dict(kind="result", adapterPath="/runs/a", finalValLoss=1.7, finalTrainLoss=1.5))])
        m = next(x for x in self.s.snapshot()["models"] if x.get("parent") == "base")
        self.assertEqual((m["kind"], m["path"], m["finalValLoss"], m["status"]), ("lora-adapter", "/runs/a", 1.7, "ready"))

    def test_serve_endpoint_registered_then_removed(self):
        self.s.model("m", dict(memoryGiB=1, runtime="llama", status="ready", path="/m.gguf"))
        j = self.s.submit("default", self.spec(runtime="serve", kind="serve", modelId="m"))
        c = self.s.claim("mini")
        self.s.logs("mini", j, c["lease"], [json.dumps(dict(kind="endpoint", url="http://127.0.0.1:9000"))])
        m = next(x for x in self.s.snapshot()["models"] if x["id"] == "m")
        self.assertEqual((m["endpoints"], m["status"]), (["http://127.0.0.1:9000"], "deployed"))
        self.finish(c)
        m = next(x for x in self.s.snapshot()["models"] if x["id"] == "m")
        self.assertEqual((m["endpoints"], m["status"]), ([], "ready"))

    def test_mlx_replica_gets_default_worker_model(self):
        self.s.model("fm", dict(memoryGiB=1, runtime="mlx", status="ready", path="/models/_fused/fm", kind="fused"))
        j = self.s.submit("default", self.spec(runtime="serve", kind="serve", modelId="fm"))
        c = self.s.claim("mini")
        self.s.logs("mini", j, c["lease"], [json.dumps(dict(kind="endpoint", url="http://127.0.0.1:9100"))])
        m = next(x for x in self.s.snapshot()["models"] if x["id"] == "fm")
        self.assertEqual(m["workerModel"], "default_model")

    def test_cancelled_serve_job_clears_endpoint(self):
        self.s.model("m", dict(memoryGiB=1, runtime="llama", status="ready", path="/m.gguf"))
        j = self.s.submit("default", self.spec(runtime="serve", kind="serve", modelId="m"))
        c = self.s.claim("mini")
        self.s.logs("mini", j, c["lease"], [json.dumps(dict(kind="endpoint", url="http://127.0.0.1:9001"))])
        self.s.cancel(j)
        self.s.complete("mini", j, c["lease"], True, {})
        m = next(x for x in self.s.snapshot()["models"] if x["id"] == "m")
        self.assertEqual((m["endpoints"], m["status"]), ([], "ready"))

    def test_dataset_registered_from_download_result(self):
        j = self.s.submit("default", self.spec(runtime="download", kind="dataset", datasetId="ds1"))
        c = self.s.claim("mini")
        self.finish(c, [json.dumps(dict(kind="result", path="/d/train.jsonl", sha256="s", bytes=10, rows=3))])
        d = next(x for x in self.s.snapshot()["datasets"] if x["id"] == "ds1")
        self.assertEqual((d["rows"], d["path"], d["status"]), (3, "/d/train.jsonl", "ready"))

    def test_fuse_registers_fused_child_model(self):
        self.s.model("adapter", dict(memoryGiB=4, runtime="mlx", status="ready", path="/runs/a", kind="lora-adapter", parent="base", source="org/base"))
        j = self.s.submit("default", self.spec(runtime="fuse", kind="fuse", modelId="adapter"))
        c = self.s.claim("mini")
        self.finish(c, [json.dumps(dict(kind="result", path="/models/_fused/adapter", bytes=777))])
        m = next(x for x in self.s.snapshot()["models"] if x["id"] == "adapter-fused")
        self.assertEqual((m["kind"], m["path"], m["runtime"], m["parent"]), ("fused", "/models/_fused/adapter", "mlx", "adapter"))

    def test_tenant_budget_stored(self):
        self.s.tenant("team", 8, 1, 100)
        t = next(x for x in self.s.snapshot()["tenants"] if x["id"] == "team")
        self.assertEqual(t["budget"], 100)


if __name__ == "__main__":
    unittest.main()
