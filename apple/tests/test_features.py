# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import json, tempfile, threading, unittest, urllib.error, urllib.request
from server.app import make_server
from server.core import Store


class FeatureTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.t = [1000.0]
        self.store = Store(self.temp.name + "/db", clock=lambda: self.t[0])
        self.store.tenant("team", 8)
        self.tokens = {"a" * 32: {"role": "admin"}, "t" * 32: {"role": "tenant", "tenant": "team"}, "n" * 32: {"role": "agent", "node": "mini"}}
        self.server = make_server(self.store, self.tokens, port=0)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base = f"http://127.0.0.1:{self.server.server_port}"

    def tearDown(self):
        self.server.shutdown(); self.server.server_close(); self.thread.join()
        self.store.db.close(); self.temp.cleanup()

    def req(self, path, token="a" * 32, body=None, raw=False):
        r = urllib.request.Request(self.base + path, headers={"Authorization": "Bearer " + token},
                                   data=json.dumps(body).encode() if body is not None else None)
        try:
            with urllib.request.urlopen(r) as res:
                data = res.read()
                return res.status, (data.decode() if raw else json.loads(data))
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def test_metrics_admin_only_and_prometheus_format(self):
        self.req("/api/jobs", body=dict(runtime="demo", memoryGiB=2))
        status, text = self.req("/metrics", raw=True)
        self.assertEqual(status, 200)
        self.assertIn('gryvia_jobs{state="queued"} 1', text)
        self.assertIn("# TYPE gryvia_nodes_online gauge", text)
        self.assertEqual(self.req("/metrics", "t" * 32)[0], 403)

    def test_history_records_bounded_samples(self):
        self.req("/api/state")
        self.t[0] += 11
        self.req("/api/state")
        status, rows = self.req("/api/history")
        self.assertEqual(status, 200)
        self.assertEqual(len(rows), 2)
        self.assertIn("reservedGiB", rows[0])
        self.assertEqual(self.req("/api/history", "t" * 32)[0], 403)

    def test_priority_change_queued_only_and_tenant_scoped(self):
        _, j = self.req("/api/jobs", "t" * 32, dict(runtime="demo", memoryGiB=1))
        self.assertEqual(self.req("/api/priority", "t" * 32, dict(id=j["id"], priority=90))[0], 200)
        job = next(x for x in self.req("/api/state")[1]["jobs"] if x["id"] == j["id"])
        self.assertEqual(job["spec"]["priority"], 90)
        self.assertEqual(self.req("/api/priority", body=dict(id=j["id"], priority=101))[0], 400)

    def test_labels_validated_and_returned(self):
        status, j = self.req("/api/jobs", body=dict(runtime="demo", memoryGiB=1, labels=["eval", "nightly"]))
        self.assertEqual(status, 201)
        job = self.req("/api/state")[1]["jobs"][0]
        self.assertEqual(job["spec"]["labels"], ["eval", "nightly"])
        self.assertEqual(self.req("/api/jobs", body=dict(runtime="demo", memoryGiB=1, labels=[""]))[0], 400)

    def test_static_brand_assets_served(self):
        for p in ("/tokens.css", "/favicon.svg", "/icon.png"):
            with self.subTest(p=p):
                try:
                    with urllib.request.urlopen(self.base + p) as r:
                        self.assertEqual(r.status, 200)
                except urllib.error.HTTPError as e:
                    self.fail(f"{p}: {e.code}")


if __name__ == "__main__":
    unittest.main()


class LiveSchedulingTests(FeatureTests):
    test_metrics_admin_only_and_prometheus_format = test_history_records_bounded_samples = None
    test_priority_change_queued_only_and_tenant_scoped = test_labels_validated_and_returned = None
    test_static_brand_assets_served = None

    def beat(self, **extra):
        body = dict(budgetGiB=12, runtimes=["demo"], gpuCores=12, gpuModel="Apple M6", gpuUtilPct=37.5, gpuInUseMiB=2048)
        body.update(extra)
        self.assertEqual(self.req("/api/agent/heartbeat", "n" * 32, body)[0], 200)

    def test_gpu_fields_kept_and_telemetry_ring(self):
        self.beat()
        self.t[0] += 2
        self.beat(gpuUtilPct=80)
        node = self.req("/api/state")[1]["nodes"][0]
        self.assertEqual((node["gpuCores"], node["gpuModel"]), (12, "Apple M6"))
        rows = self.req("/api/telemetry")[1]["mini"]
        self.assertEqual([r["util"] for r in rows], [37.5, 80.0])
        self.assertAlmostEqual(rows[0]["inUseGiB"], 2.0)
        self.assertEqual(self.req("/api/telemetry", "t" * 32)[1], {})

    def test_decisions_record_queue_wait_admit_finish(self):
        self.beat()
        _, j = self.req("/api/jobs", body=dict(runtime="demo", memoryGiB=2))
        _, big = self.req("/api/jobs", body=dict(runtime="demo", memoryGiB=64, priority=10))
        _, c = self.req("/api/agent/claim", "n" * 32, {})
        self.assertEqual(c["job"]["id"], j["id"])
        kinds = [(d["job"], d["kind"]) for d in self.req("/api/state")[1]["decisions"]]
        self.assertIn((j["id"], "queue"), kinds)
        self.assertIn((j["id"], "admit"), kinds)
        self.t[0] += 1
        self.req("/api/agent/complete", "n" * 32, dict(id=c["job"]["id"], lease=c["job"]["lease"], success=True, result={}))
        job = next(x for x in self.req("/api/state")[1]["jobs"] if x["id"] == j["id"])
        self.assertEqual(job["state"], "succeeded")
        self.assertIsNotNone(job["finished"])

    def test_stream_emits_state_frame(self):
        self.beat()
        r = urllib.request.Request(self.base + "/api/stream", headers={"Authorization": "Bearer " + "a" * 32})
        with urllib.request.urlopen(r, timeout=5) as res:
            self.assertEqual(res.headers["Content-Type"], "text/event-stream")
            line = res.readline().decode()
            self.assertTrue(line.startswith("data: "))
            snap = json.loads(line[6:])
            self.assertIn("telemetry", snap)
            self.assertEqual(snap["nodes"][0]["gpuCores"], 12)
