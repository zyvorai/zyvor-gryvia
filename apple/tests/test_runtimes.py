# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import contextlib, hashlib, http.server, io, json, os, stat, sys, tempfile, threading, unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "runtimes"))
import _common, eval_suite, hf_download, llama_bench, quantize  # noqa: E402


def run(mod, argv):
    out = io.StringIO()
    with contextlib.redirect_stdout(out):
        try:
            code = mod.main(argv)
        except SystemExit as e:
            code = e.code
    lines = [json.loads(l) for l in out.getvalue().splitlines() if l.startswith("{")]
    return code, lines


class RuntimeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.models = Path(self.tmp.name) / "models"
        self.bins = Path(self.tmp.name) / "bin"
        self.bins.mkdir()
        os.environ["GRYVIA_MODELS_DIR"] = str(self.models)
        os.environ["GRYVIA_LLAMA_BIN_DIR"] = str(self.bins)
        _common.models_root()

    def tearDown(self):
        os.environ.pop("GRYVIA_MODELS_DIR", None); os.environ.pop("GRYVIA_LLAMA_BIN_DIR", None)
        self.tmp.cleanup()

    def tool(self, name, script):
        p = self.bins / name
        p.write_text("#!/bin/sh\n" + script)
        p.chmod(p.stat().st_mode | stat.S_IEXEC)

    def model(self, name="m.gguf", data=b"gguf-bytes"):
        d = self.models / "repo"; d.mkdir(parents=True, exist_ok=True)
        (d / name).write_bytes(data)
        return str(d / name)

    def test_confine_blocks_traversal(self):
        with self.assertRaises(SystemExit):
            _common.confine("/etc/passwd")
        with self.assertRaises(SystemExit):
            _common.confine(str(self.models / ".." / "x"))

    def test_bench_parses_llama_bench_json(self):
        self.tool("llama-bench", 'echo \'[{"n_prompt":512,"n_gen":0,"avg_ts":812.5},{"n_prompt":0,"n_gen":128,"avg_ts":55.25}]\'\n')
        code, lines = run(llama_bench, ["--model", self.model()])
        self.assertEqual(code, 0)
        r = lines[-1]
        self.assertEqual((r["kind"], r["promptTokensPerSecond"], r["generationTokensPerSecond"]), ("result", 812.5, 55.25))

    def test_bench_rejects_model_outside_root(self):
        self.tool("llama-bench", "echo []\n")
        code, _ = run(llama_bench, ["--model", "/etc/hosts"])
        self.assertNotEqual(code, 0)

    def test_eval_suite_scores_cases(self):
        self.tool("llama-completion", 'case "$*" in *"2+2"*) echo "The answer is 4";; *) echo "no idea";; esac\n')
        cases = [dict(prompt="2+2", contains="4"), dict(prompt="capital", equals="Paris"), dict(prompt="2+2", regex=r"answer is \d")]
        code, lines = run(eval_suite, ["--model", self.model(), "--cases", json.dumps(cases)])
        r = lines[-1]
        cases = [l for l in lines if l.get("kind") == "case"]
        self.assertEqual((r["passed"], r["total"]), (2, 3))
        self.assertEqual([c["passed"] for c in cases], [True, False, True])

    def test_quantize_writes_output_with_hash(self):
        self.tool("llama-quantize", 'printf "quantized" > "$2"\n')
        src = self.model("big.gguf", b"x" * 100)
        code, lines = run(quantize, ["--input", src, "--type", "Q4_K_M"])
        r = lines[-1]
        self.assertEqual(code, 0)
        self.assertEqual(r["sha256"], hashlib.sha256(b"quantized").hexdigest())
        self.assertTrue(r["path"].endswith("big-Q4_K_M.gguf"))
        code, _ = run(quantize, ["--input", src, "--type", "NOPE"])
        self.assertNotEqual(code, 0)

    def test_download_refuses_foreign_hosts_and_caps_size(self):
        code, _ = run(hf_download, ["--url", "https://evil.example/m.gguf"])
        self.assertNotEqual(code, 0)
        code, _ = run(hf_download, ["--repo", "a/b", "--file", "../x.gguf"])
        self.assertNotEqual(code, 0)

    def test_download_streams_and_hashes_from_local_server(self):
        payload = os.urandom(300_000)

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200); self.send_header("Content-Length", str(len(payload))); self.end_headers(); self.wfile.write(payload)
            def log_message(self, *a): pass
        srv = http.server.HTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        # Origin allowlist is enforced on the *requested* URL, so exercise the transfer path with the allowlist extended for localhost.
        old = hf_download.HOSTS
        hf_download.HOSTS = ("127.0.0.1",)
        try:
            real = hf_download.build_url
            hf_download.build_url = lambda a: f"https://127.0.0.1:{srv.server_port}/x.gguf"
            # plain-http fixture: swap the opener to avoid TLS for this unit test only
            import urllib.request
            orig_build = urllib.request.build_opener
            urllib.request.build_opener = lambda *h: orig_build()
            hf_download.urllib.parse.urlsplit  # noqa: B018
            code, lines = run_http(srv.server_port)
        finally:
            hf_download.HOSTS = old; hf_download.build_url = real; urllib.request.build_opener = orig_build
            srv.shutdown()
        self.assertEqual(code, 0)
        r = lines[-1]
        self.assertEqual(r["sha256"], hashlib.sha256(payload).hexdigest())
        self.assertEqual(r["bytes"], len(payload))


def run_http(port):
    # call main() with a URL our patched build_url returns; scheme check uses the patched URL's https, and the opener hits http
    import urllib.request
    real_open = urllib.request.OpenerDirector.open
    def patched(self, req, *a, **k):
        req = urllib.request.Request(req.full_url.replace("https://", "http://"), headers=dict(req.header_items()))
        return real_open(self, req, *a, **k)
    urllib.request.OpenerDirector.open = patched
    try:
        return run(hf_download, ["--repo", "a/b", "--file", "x.gguf", "--max-gib", "1"])
    finally:
        urllib.request.OpenerDirector.open = real_open


if __name__ == "__main__":
    unittest.main()


class MlxEvalTests(RuntimeTests):
    test_confine_blocks_traversal = test_bench_parses_llama_bench_json = test_bench_rejects_model_outside_root = None
    test_eval_suite_scores_cases = test_quantize_writes_output_with_hash = None
    test_download_refuses_foreign_hosts_and_caps_size = test_download_streams_and_hashes_from_local_server = None

    def test_mlx_engine_runs_generate_with_adapter_and_scores(self):
        fake = Path(self.tmp.name) / "py"
        fake.write_text('#!/bin/sh\necho banner\necho ==========\nif echo "$*" | grep -q adapter; then echo "Drain stops new jobs."; else echo "A pipe."; fi\necho ==========\n')
        fake.chmod(0o755)
        os.environ["GRYVIA_MLX_PYTHON"] = str(fake)
        adapter = self.models / "ad"; adapter.mkdir(parents=True)
        cases = json.dumps([dict(prompt="Q: drain?\nA:", contains="stops new jobs")])
        code, lines = run(eval_suite, ["--engine", "mlx", "--model", "org/base", "--adapter", str(adapter), "--cases", cases])
        self.assertEqual((code, lines[-1]["passed"], lines[-1]["engine"]), (0, 1, "mlx"))
        code, lines = run(eval_suite, ["--engine", "mlx", "--model", "org/base", "--cases", cases])
        self.assertEqual(lines[-1]["passed"], 0)
        os.environ.pop("GRYVIA_MLX_PYTHON")


class TrainParseTests(unittest.TestCase):
    def test_parse_mlx_lm_lines(self):
        import mlx_lora
        m = mlx_lora.parse_line("Iter 10: Train loss 2.345, Learning Rate 1.000e-05, It/sec 0.812, Tokens/sec 123.4, Trained Tokens 1234, Peak mem 3.456 GB")
        self.assertEqual((m["step"], m["trainLoss"], m["lr"], m["peakGB"]), (10, 2.345, 1e-05, 3.456))
        v = mlx_lora.parse_line("Iter 20: Val loss 2.100, Val took 1.2s")
        self.assertEqual((v["step"], v["valLoss"]), (20, 2.1))
        self.assertIsNone(mlx_lora.parse_line("Loading pretrained model"))

    def test_parse_table_format_with_ansi(self):
        import mlx_lora
        t = mlx_lora.parse_line("  \x1b[38;5;244m   5\x1b[0m    \x1b[1;32m5.464 ▼\x1b[0m    \x1b[1m2,804\x1b[0m    \x1b[38;5;244m  0.2k\x1b[0m")
        self.assertEqual((t["step"], t["trainLoss"], t["tokensPerSec"]), (5, 5.464, 2804.0))
        v = mlx_lora.parse_line("  \x1b[38;5;244m  10\x1b[0m    \x1b[1;35mval\x1b[0m \x1b[1m5.090\x1b[0m    \x1b[38;5;244m0.01s\x1b[0m")
        self.assertEqual((v["step"], v["valLoss"]), (10, 5.09))

    def test_prepare_splits_and_rejects_tiny_dataset(self):
        import mlx_lora, tempfile
        d = Path(tempfile.mkdtemp())
        f = d / "d.jsonl"
        f.write_text("\n".join(json.dumps({"text": f"row {i}"}) for i in range(20)))
        t, v = mlx_lora.prepare(f, d / "run", 0.1)
        self.assertEqual((t, v), (18, 2))
        f.write_text('{"text":"a"}\n')
        with self.assertRaises(SystemExit):
            mlx_lora.prepare(f, d / "run2", 0.1)
