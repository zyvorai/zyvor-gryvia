# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import json
import os
import sys
import tempfile
import unittest

import convert_gguf
from download import download, target_dir
import evaluate
from evaluate import aggregate, exact_match, openai_generator, output_key
from finetune_lora import filter_kwargs, output_dir, validate_jsonl
import outputs
from outputs import write_outputs
from quantize import calibration_texts
from quantize import parse_args as parse_quantize_args


class OutputsTests(unittest.TestCase):
    def test_writes_json_strings(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "termination-log")
            write_outputs({"path": "/models/x", "steps": 12, "ok": True}, path)
            with open(path) as f:
                self.assertEqual(json.load(f), {"path": "/models/x", "steps": "12", "ok": "true"})

    def test_rejects_bad_keys_and_oversize(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "log")
            self.assertRaises(ValueError, write_outputs, {"bad key": "x"}, path)
            self.assertRaises(ValueError, write_outputs, {"big": "x" * 5000}, path)


class DownloadTests(unittest.TestCase):
    def test_downloads_once_per_revision(self):
        calls = []

        def fake_snapshot(repo_id, revision, local_dir, allow_patterns):
            calls.append((repo_id, revision))
            open(os.path.join(local_dir, "config.json"), "w").write("{}")

        with tempfile.TemporaryDirectory() as cache:
            p1 = download("Qwen/Qwen3-8B", "abc1234", cache, snapshot=fake_snapshot)
            p2 = download("Qwen/Qwen3-8B", "abc1234", cache, snapshot=fake_snapshot)
            self.assertEqual(p1, p2)
            self.assertEqual(p1, target_dir(cache, "Qwen/Qwen3-8B", "abc1234"))
            self.assertEqual(len(calls), 1)
            download("Qwen/Qwen3-8B", "def5678", cache, snapshot=fake_snapshot)
            self.assertEqual(len(calls), 2)


class FinetuneTests(unittest.TestCase):
    def test_output_dir_and_kwargs(self):
        self.assertEqual(output_dir("/models/ft", "qwen3-8b", "0123456789abcdef"), "/models/ft/qwen3-8b/0123456789ab")
        self.assertEqual(output_dir("/m", "x", ""), "/m/x/main")

        def new_api(output_dir, max_length=1):
            pass

        def flexible(output_dir, **kw):
            pass

        args = {"output_dir": "o", "max_length": 2, "max_seq_length": 2}
        self.assertEqual(filter_kwargs(new_api, args), {"output_dir": "o", "max_length": 2})
        self.assertEqual(filter_kwargs(flexible, args), args)

    def test_validate_jsonl(self):
        with tempfile.TemporaryDirectory() as d:
            good = os.path.join(d, "good.jsonl")
            open(good, "w").write('{"messages": [{"role": "user", "content": "hi"}]}\n\n{"text": "x"}\n')
            validate_jsonl(good)
            bad = os.path.join(d, "bad.jsonl")
            open(bad, "w").write('{"prompt": "x"}\n')
            self.assertRaises(ValueError, validate_jsonl, bad)


class EvaluateTests(unittest.TestCase):
    def test_aggregate_prefers_primary_metrics(self):
        results = {
            "hellaswag": {"acc,none": 0.5, "acc_norm,none": 0.6, "alias": "hellaswag"},
            "gsm8k": {"exact_match,strict-match": 0.3},
            "weird": {"perplexity,none": 12.0},
        }
        score, per = aggregate(results, custom=0.9)
        self.assertEqual(per, {"gsm8k": 0.3, "hellaswag": 0.6, "custom": 0.9})
        self.assertAlmostEqual(score, 0.6)
        self.assertRaises(ValueError, aggregate, {"weird": {"perplexity,none": 1.0}})

    def test_exact_match_and_keys(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "eval.jsonl")
            open(path, "w").write('{"prompt": "2+2=", "expected": "4"}\n{"prompt": "3+3=", "expected": 6}\n')
            answers = {"2+2=": " 4, of course", "3+3=": "7"}
            self.assertEqual(exact_match(answers.get, path), 0.5)
        self.assertEqual(output_key("mmlu/abstract algebra"), "mmlu_abstract_algebra")

    def test_endpoint_generator_and_main(self):
        seen = []

        class Resp:
            def __init__(self, body):
                self.body = body

            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

            def read(self):
                return self.body

        def opener(req, timeout):
            body = json.loads(req.data)
            seen.append((req.full_url, req.get_header("Authorization"), body))
            answer = "4" if body["prompt"] == "2+2=" else "5"
            return Resp(json.dumps({"choices": [{"text": " " + answer}]}).encode())

        os.environ["OPENAI_API_KEY"] = "sk-test"
        try:
            gen = openai_generator("http://svc:8080/", "chat", opener=opener)
            self.assertEqual(gen("2+2="), " 4")
        finally:
            del os.environ["OPENAI_API_KEY"]
        url, auth, body = seen[0]
        self.assertEqual(url, "http://svc:8080/v1/completions")
        self.assertEqual(auth, "Bearer sk-test")
        self.assertEqual((body["model"], body["temperature"]), ("chat", 0))

        with tempfile.TemporaryDirectory() as d:
            data = os.path.join(d, "eval.jsonl")
            open(data, "w").write('{"prompt": "2+2=", "expected": "4"}\n{"prompt": "3+3=", "expected": "6"}\n')
            written = {}
            orig_gen, orig_write = evaluate.openai_generator, evaluate.write_outputs
            evaluate.openai_generator = lambda e, m: openai_generator(e, m, opener=opener)
            evaluate.write_outputs = written.update
            try:
                self.assertEqual(evaluate.main(["--endpoint", "http://svc:8080", "--served-model", "chat",
                                                "--custom", data]), 0)
            finally:
                evaluate.openai_generator, evaluate.write_outputs = orig_gen, orig_write
            self.assertEqual(written["score"], "0.500000")
        with self.assertRaises(SystemExit):
            evaluate.main(["--model", "/m", "--endpoint", "http://x"])

    def test_endpoint_tasks_use_local_completions_with_the_tokenizer(self):
        self.assertEqual(evaluate.endpoint_model_args("http://svc:8080/", "chat"),
                         "model=chat,base_url=http://svc:8080/v1/completions,tokenized_requests=False")
        self.assertTrue(evaluate.endpoint_model_args("http://svc:8080", "chat", "/models/m").endswith(
            ",tokenizer=/models/m"))
        calls = []
        fake = type(sys)("lm_eval")
        result = {"results": {"gsm8k": {"exact_match,strict-match": 0.25}}}
        fake.simple_evaluate = lambda **kw: calls.append(kw) or result
        written = {}
        orig_mod, orig_write = sys.modules.get("lm_eval"), evaluate.write_outputs
        sys.modules["lm_eval"], evaluate.write_outputs = fake, written.update
        try:
            self.assertEqual(evaluate.main(["--endpoint", "http://svc:8080", "--served-model", "chat",
                                            "--tasks", "gsm8k", "--limit", "8", "--tokenizer", "/models/m"]), 0)
        finally:
            evaluate.write_outputs = orig_write
            if orig_mod is None:
                del sys.modules["lm_eval"]
            else:
                sys.modules["lm_eval"] = orig_mod
        self.assertEqual((calls[0]["model"], calls[0]["tasks"], calls[0]["limit"]), ("local-completions", ["gsm8k"], 8))
        self.assertIn("tokenizer=/models/m", calls[0]["model_args"])
        self.assertEqual((written["score"], written["gsm8k"]), ("0.250000", "0.250000"))


class QuantizeTests(unittest.TestCase):
    def test_args_defaults_and_validation(self):
        a = parse_quantize_args(["--model", "/models/ft/qwen3-8b/abc/", "--calibration", "/data/chat.jsonl"])
        self.assertEqual((a.method, a.scheme, a.output), ("awq", "W4A16_ASYM", "/models/ft/qwen3-8b/abc-awq"))
        g = parse_quantize_args(["--model", "/m/x", "--calibration", "c", "--method", "gptq", "--scheme", "W8A16"])
        self.assertEqual((g.scheme, g.output), ("W8A16", "/m/x-gptq"))
        for bad in (["--method", "awq", "--scheme", "W8A16"], ["--method", "fp8"], ["--samples", "0"]):
            with self.assertRaises(SystemExit):
                parse_quantize_args(["--model", "/m", "--calibration", "c"] + bad)

    def test_calibration_texts(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "chat.jsonl")
            open(path, "w").write('{"text": "plain"}\n\n{"messages": [{"role": "user", "content": "hi"}]}\n'
                                  '{"text": "third"}\n')
            render = lambda m: "<chat>" + m[0]["content"]  # noqa: E731
            self.assertEqual(calibration_texts(path, 10, render), ["plain", "<chat>hi", "third"])
            self.assertEqual(calibration_texts(path, 2, render), ["plain", "<chat>hi"])
            bad = os.path.join(d, "bad.jsonl")
            open(bad, "w").write('{"prompt": "x"}\n')
            self.assertRaises(ValueError, calibration_texts, bad, 10, render)
            empty = os.path.join(d, "empty.jsonl")
            open(empty, "w").write("\n")
            self.assertRaises(ValueError, calibration_texts, empty, 10, render)


class ConvertGGUFTests(unittest.TestCase):
    def test_converts_once_and_reports_outputs(self):
        calls = []

        def fake_run(cmd, check):
            calls.append(cmd)
            self.assertTrue(check)
            open(cmd[cmd.index("--outfile") + 1], "wb").write(b"GGUF" + b"\0" * 12)

        with tempfile.TemporaryDirectory() as root:
            model = os.path.join(root, "finetuned", "m", "abc")
            os.makedirs(model)
            open(os.path.join(model, "config.json"), "w").write("{}")
            out = convert_gguf.convert(model, "q8_0", "/opt/llama.cpp", run=fake_run)
            self.assertEqual(out, os.path.join(model, "model-q8_0.gguf"))
            self.assertEqual(calls[0][1:], ["/opt/llama.cpp/convert_hf_to_gguf.py", model, "--outtype", "q8_0",
                                            "--outfile", out + ".tmp"])
            self.assertFalse(os.path.exists(out + ".tmp"))
            convert_gguf.convert(model, "q8_0", "/opt/llama.cpp", run=fake_run)
            self.assertEqual(len(calls), 1)

            log = os.path.join(root, "log")
            orig_convert, orig_log = convert_gguf.convert, outputs.TERMINATION_LOG
            convert_gguf.convert, outputs.TERMINATION_LOG = (lambda m, t, d: out), log
            try:
                convert_gguf.main(["--model", model, "--pvc-root", root])
            finally:
                convert_gguf.convert, outputs.TERMINATION_LOG = orig_convert, orig_log
            with open(log) as f:
                self.assertEqual(json.load(f), {"path": model, "subPath": "finetuned/m/abc", "file": "model-q8_0.gguf",
                                                "format": "gguf", "bytes": "16"})

    def test_rejects_bad_input(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertRaises(FileNotFoundError, convert_gguf.convert, d, "q8_0", "/x", run=None)
            open(os.path.join(d, "config.json"), "w").write("{}")
            self.assertRaises(ValueError, convert_gguf.convert, d, "q4_k_m", "/x", run=None)


if __name__ == "__main__":
    unittest.main()
