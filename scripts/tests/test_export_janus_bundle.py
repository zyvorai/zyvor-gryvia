"""Tests for scripts/export-janus-bundle.py. Run: python3 -m unittest scripts/tests/test_export_janus_bundle.py

Set JANUS_BIN (the zyvor-janus binary) and JANUS_REPO (a janus checkout) to also replay the exported bundle through
the real simulator.
"""
import importlib.util
import json
import os
import subprocess
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("exporter", os.path.join(HERE, "..", "export-janus-bundle.py"))
exporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(exporter)


def node(name, gpus=8, gpu_type="H100"):
    return {"kind": "GryviaGpuNode", "metadata": {"name": name},
            "spec": {"nodeName": name, "gpuType": gpu_type, "gpuCount": gpus, "memoryGB": 80, "rdma": True,
                     "interconnect": "NVLink", "drivers": {"nvidia": "550"}}}


def job(name, ns="tenant-a", **spec):
    base = {"type": "training", "gpus": 1}
    base.update(spec)
    return {"kind": "GryviaAIJob", "metadata": {"name": name, "namespace": ns}, "spec": base}


def found(jobs, nodes=None, quotas=None, tenants=None):
    return {"jobs": jobs, "nodes": nodes or [node("n1"), node("n2")], "quotas": quotas or [], "tenants": tenants or []}


PROFILES = {"llama-7b": {"H100", "A100"}, "llama-70b": {"H100"}}


class ExportTest(unittest.TestCase):
    def run_export(self, f, **kw):
        out = tempfile.mkdtemp()
        res = exporter.export(f, out, **kw)
        docs = {}
        for d in ("jobs", "cluster", "quotas"):
            p = os.path.join(out, d, f"gryvia-{ {'jobs': 'jobs', 'cluster': 'gpu-nodes', 'quotas': 'quotas'}[d] }.yaml")
            if os.path.exists(p):
                with open(p, encoding="utf-8") as fh:
                    docs[d] = json.load(fh)["items"]
        return res, docs, out

    def test_translation(self):
        f = found([
            job("single", model="llama-7b", gpus=4, gpuType="H100", priority=5, network="rdma"),
            job("gang", model="llama-70b", gpus=8, gpuType="H100",
                distributed={"enabled": True, "nodes": 2}),
            job("slice", model="llama-7b", gpus=2, migProfile="1g.10gb", gpuType="H100"),
        ])
        res, docs, _ = self.run_export(f, profiles=PROFILES)
        by = {d["metadata"]["name"]: d for d in docs["jobs"]}
        self.assertEqual(res["jobs"], 3)
        self.assertEqual(by["single"]["apiVersion"], "zynera.ai/v1")
        self.assertEqual(by["single"]["kind"], "FabricAIJob")
        self.assertEqual(by["single"]["spec"]["gpus"], 4)
        self.assertEqual(by["single"]["spec"]["priority"], 5)
        g = by["gang"]
        # spec.gpus is per pod in Gryvia: it becomes gpusPerNode, and Janus multiplies by nodes
        self.assertEqual(g["spec"]["distributed"], {"enabled": True, "nodes": 2, "gpusPerNode": 8})
        self.assertNotIn("gpus", g["spec"])
        self.assertEqual(g["metadata"]["annotations"]["zynera.ai/gang-schedule"], "true")
        self.assertEqual(g["metadata"]["annotations"]["zynera.ai/gang-size"], "2")
        self.assertEqual(by["slice"]["spec"]["mig"], {"profile": "1g.10gb", "count": 2})

    def test_skips_what_cannot_be_simulated(self):
        f = found([
            job("cpu", model="llama-7b", gpus=0),
            job("nomodel", gpus=1),
            job("unknown", model="mystery", gpus=1, gpuType="H100"),
            job("nogputype-profile", model="llama-70b", gpus=1, gpuType="A100"),
            job("ok", model="llama-7b", gpus=1, gpuType="H100"),
        ])
        res, _, _ = self.run_export(f, profiles=PROFILES)
        reasons = dict(res["skipped"])
        self.assertEqual(res["jobs"], 1)
        self.assertIn("tenant-a/cpu", reasons)
        self.assertIn("no spec.model", reasons["tenant-a/nomodel"])
        self.assertIn("no calibrated profile", reasons["tenant-a/unknown"])
        self.assertIn("gpuType 'A100'", reasons["tenant-a/nogputype-profile"])

    def test_flags_jobs_larger_than_the_cluster(self):
        f = found([job("huge", model="llama-70b", gpus=32, gpuType="H100", distributed={"enabled": True, "nodes": 4}),
                   job("fits", model="llama-7b", gpus=4, gpuType="H100")])  # 2 nodes x 8 = 16 GPUs
        res, _, _ = self.run_export(f, profiles=PROFILES)
        self.assertEqual(res["totalGpus"], 16)
        self.assertEqual(res["oversized"], [("tenant-a/huge", 128, 16)])

    def test_strict_fails_instead_of_skipping(self):
        f = found([job("nomodel", gpus=1), job("ok", model="llama-7b", gpuType="H100")])
        with self.assertRaises(SystemExit):
            self.run_export(f, profiles=PROFILES, strict=True)

    def test_defaults_and_node_gpu_type(self):
        f = found([job("a", gpus=1), job("b", model="llama-7b", gpus=1, gpuType="any")],
                  nodes=[node("n1", gpu_type="A100"), node("n2", gpu_type="A100"), node("n3", gpu_type="H100")])
        res, docs, _ = self.run_export(f, default_model="llama-7b", profiles=PROFILES)
        self.assertEqual(res["defaultGpuType"], "A100")  # the most common node type
        self.assertEqual({d["spec"]["gpuType"] for d in docs["jobs"]}, {"A100"})

    def test_nodes_and_quotas(self):
        q = {"kind": "GryviaQuota", "metadata": {"name": "ml", "namespace": "ml"},
             "spec": {"team": "ml", "namespaces": ["tenant-a"], "gpuQuota": {"maxGPUs": 16, "maxRunningJobs": 4}}}
        t_covered = {"kind": "GryviaTenant", "metadata": {"name": "a"}, "spec": {"quotas": {"concurrentGPUs": 99}}}
        t_new = {"kind": "GryviaTenant", "metadata": {"name": "b"}, "spec": {"quotas": {"concurrentGPUs": 8}}}
        t_none = {"kind": "GryviaTenant", "metadata": {"name": "c"}, "spec": {}}
        f = found([job("ok", model="llama-7b", gpuType="H100")], quotas=[q], tenants=[t_covered, t_new, t_none])
        _, docs, _ = self.run_export(f, profiles=PROFILES)
        n = docs["cluster"][0]
        self.assertEqual(n["kind"], "FabricGpuNode")
        self.assertEqual(n["spec"]["gpuCount"], 8)
        self.assertNotIn("drivers", n["spec"])
        by = {d["spec"]["team"]: d for d in docs["quotas"]}
        self.assertEqual(by["ml"]["spec"]["gpuQuota"], {"maxGPUs": 16, "maxRunningJobs": 4})
        self.assertNotIn("a", by)  # its namespace is already covered by an explicit GryviaQuota
        self.assertEqual(by["b"]["spec"]["namespaces"], ["tenant-b"])
        self.assertEqual(by["b"]["spec"]["gpuQuota"]["maxGPUs"], 8)
        self.assertNotIn("c", by)

    def test_list_and_dir_input(self):
        d = tempfile.mkdtemp()
        with open(os.path.join(d, "all.json"), "w", encoding="utf-8") as fh:
            json.dump({"kind": "List", "items": [job("a", model="llama-7b", gpuType="H100"), node("n1")]}, fh)
        got = exporter.read_dir(d)
        self.assertEqual((len(got["jobs"]), len(got["nodes"])), (1, 1))

    @unittest.skipUnless(os.environ.get("JANUS_BIN") and os.environ.get("JANUS_REPO"), "needs a Janus build")
    def test_replay_in_janus(self):
        repo = os.environ["JANUS_REPO"]
        profiles = os.path.join(repo, "configs", "profiles")
        f = found([
            job("train", model="llama-7b", gpus=4, gpuType="H100", priority=1),
            job("big", model="llama-70b", gpus=8, gpuType="H100", priority=9, distributed={"enabled": True, "nodes": 2}),
        ], quotas=[{"kind": "GryviaQuota", "metadata": {"name": "q", "namespace": "tenant-a"},
                    "spec": {"team": "a", "namespaces": ["tenant-a"], "gpuQuota": {"maxGPUs": 32}}}])
        res, _, out = self.run_export(f, profiles=exporter.read_profiles(profiles))
        self.assertEqual(res["jobs"], 2)
        for policy in ("fifo", "priority", "bestfit"):
            p = subprocess.run([os.environ["JANUS_BIN"], "run", "--zynera-bundle", out, "--scheduler", policy],
                               cwd=repo, capture_output=True, text=True)
            self.assertEqual(p.returncode, 0, p.stderr)
            self.assertIn("jobs completed:    2/2", p.stdout)


if __name__ == "__main__":
    unittest.main()
