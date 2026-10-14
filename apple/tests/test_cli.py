# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import io, json, os, sys, tempfile, threading, unittest, contextlib
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
import gryvia
from server.app import make_server
from server.core import Store


class CliTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.store = Store(self.tmp.name + "/db")
        tokens = {"a" * 32: {"role": "admin"}, "g" * 32: {"role": "agent", "node": "n1"}}
        self.srv = make_server(self.store, tokens, port=0)
        self.port = self.srv.server_port
        self.thread = threading.Thread(target=self.srv.serve_forever, daemon=True)
        self.thread.start()
        os.environ["GRYVIA_ADMIN_TOKEN"] = "a" * 32

    def tearDown(self):
        self.srv.shutdown()
        self.srv.server_close()
        self.thread.join()
        self.store.db.close()
        self.tmp.cleanup()

    def run_cli(self, *argv):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = gryvia.main(["--server", f"http://127.0.0.1:{self.port}", *argv])
        return code, out.getvalue(), err.getvalue()

    def test_status_json_when_piped(self):
        code, out, _ = self.run_cli("status")
        self.assertEqual(code, 0)
        self.assertIn("jobs", json.loads(out))

    def test_doctor_reports_no_agents(self):
        code, out, _ = self.run_cli("doctor")
        self.assertEqual(code, 1)
        self.assertIn("token accepted", out)

    def test_bad_token_gives_hint(self):
        os.environ["GRYVIA_ADMIN_TOKEN"] = "x" * 32
        code, _, err = self.run_cli("status")
        self.assertEqual(code, 1)
        self.assertIn("hint", err)

    def test_table_alignment(self):
        t = gryvia.table(["A", "BB"], [["1", "22"], ["333", "4"]])
        self.assertEqual(len(t.splitlines()), 3)

    def test_completion(self):
        self.assertEqual(self.run_cli("completion")[0], 0)


if __name__ == "__main__":
    unittest.main()


class CliFeatureTests(CliTests):
    test_status_json_when_piped = test_doctor_reports_no_agents = test_bad_token_gives_hint = test_table_alignment = test_completion = None

    def test_jobs_csv_and_filters(self):
        self.store.submit("default", dict(runtime="demo", memoryGiB=1, model="a,b", labels=["x"]))
        code, out, _ = self.run_cli("--output", "csv", "jobs", "--label", "x")
        self.assertEqual(code, 0)
        self.assertTrue(out.startswith("id,tenant,state"))
        self.assertIn('"a,b"', out)

    def test_metrics_and_version(self):
        code, out, _ = self.run_cli("metrics")
        self.assertEqual(code, 0)
        self.assertIn("gryvia_nodes_online", out)

    def test_priority(self):
        ident = self.store.submit("default", dict(runtime="demo", memoryGiB=1))
        code, out, _ = self.run_cli("priority", ident, "80")
        self.assertEqual(code, 0)
        self.assertEqual(next(j for j in self.store.snapshot()["jobs"] if j["id"] == ident)["spec"]["priority"], 80)


class QuickstartTests(CliTests):
    test_status_json_when_piped = test_doctor_reports_no_agents = test_bad_token_gives_hint = test_table_alignment = test_completion = None

    def test_quickstart_points_to_first_missing_step(self):
        code, out, _ = self.run_cli("quickstart")
        self.assertEqual(code, 0)
        self.assertIn("Next: Connect a Mac", out)


class ModelCliTests(CliTests):
    test_status_json_when_piped = test_doctor_reports_no_agents = test_bad_token_gives_hint = test_table_alignment = test_completion = None

    def test_onboard_queues_download_and_models_lists_it(self):
        code, out, _ = self.run_cli("onboard", "tiny", "--repo", "owner/tiny", "--file", "t.gguf", "--params-b", "1.1", "--quant", "Q4_K_M")
        self.assertEqual(code, 0)
        self.assertIn("job", json.loads(out))
        code, out, _ = self.run_cli("models")
        self.assertEqual(json.loads(out)[0]["status"], "onboarding")

    def test_bench_refuses_model_not_ready(self):
        self.run_cli("onboard", "tiny", "--repo", "owner/tiny", "--file", "t.gguf")
        code, _, err = self.run_cli("bench", "tiny")
        self.assertEqual(code, 1)
        self.assertIn("not ready", err)


class TrainCliTests(CliTests):
    test_status_json_when_piped = test_doctor_reports_no_agents = test_bad_token_gives_hint = test_table_alignment = test_completion = None

    def test_train_requires_ready_dataset_and_queues_job(self):
        code, _, err = self.run_cli("train", "mlx-community/tiny", "--dataset", "nope")
        self.assertEqual(code, 1)
        self.store.dataset("d", dict(path="/m/_datasets/d.jsonl", status="ready", rows=10, node="mini"))
        code, out, _ = self.run_cli("train", "mlx-community/tiny", "--dataset", "d", "--iters", "5")
        self.assertEqual(code, 0)
        job = next(j for j in self.store.snapshot()["jobs"] if j["spec"].get("kind") == "train")
        self.assertIn("{{dataset.path}}", job["spec"]["args"])
        self.assertEqual(job["spec"]["datasetId"], "d")
        self.assertTrue(any(m["id"] == "mlx-community__tiny" for m in self.store.snapshot()["models"]))


class PolicyCliTests(CliTests):
    test_status_json_when_piped = test_doctor_reports_no_agents = test_bad_token_gives_hint = test_table_alignment = test_completion = None

    def test_policy_sets_idle_mode_and_nodes_shows_it(self):
        self.store.register("mac", dict(budgetGiB=8, runtimes=["demo"], presence=dict(idleSeconds=1, onBattery=False)))
        code, out, err = self.run_cli("policy", "mac", "--mode", "idle", "--idle-minutes", "10", "--allow-battery", "no")
        self.assertEqual(code, 0, err)
        pol = json.loads(out)["policy"]
        self.assertEqual((pol["mode"], pol["idleMinutes"], pol["allowOnBattery"]), ("idle", 10, False))
        node = self.store.snapshot()["nodes"][0]
        self.assertEqual(node["gate"]["reason"], "owner is active")

    def test_policy_unknown_node(self):
        code, _, err = self.run_cli("policy", "ghost", "--mode", "paused")
        self.assertEqual(code, 1)
        self.assertIn("unknown Mac", err)


class ProvenanceCliTests(CliTests):
    test_status_json_when_piped = test_doctor_reports_no_agents = test_bad_token_gives_hint = test_table_alignment = test_completion = None

    def test_provenance_unattested_and_rekey(self):
        self.store.model("m", dict(memoryGiB=1, runtime="llama", status="ready", path="/m.gguf"))
        code, out, _ = self.run_cli("provenance", "m")
        self.assertEqual((code, json.loads(out)), (0, {}))
        self.store.register("mac", dict(budgetGiB=8, runtimes=["demo"], attestKey="ab" * 32))
        code, out, _ = self.run_cli("rekey", "mac")
        self.assertEqual(code, 0)
        self.assertIsNone(self.store.db.execute("SELECT pub FROM node_keys WHERE node='mac'").fetchone())
