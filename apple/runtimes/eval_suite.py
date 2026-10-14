#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Run a prompt suite against a managed GGUF model with llama.cpp and report pass/fail per case.

A case is {"prompt": str, and one of "contains" | "equals" | "regex"}. Greedy decoding (temp 0) keeps runs repeatable."""
import argparse
import json
import re
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from _common import confine, emit, find_tool, mlx_python  # noqa: E402


def judge(case: dict, text: str) -> bool:
    out = text.strip()
    if "equals" in case:
        return out == str(case["equals"]).strip()
    if "contains" in case:
        return str(case["contains"]).lower() in out.lower()
    if "regex" in case:
        return re.search(case["regex"], out, re.S) is not None
    return False


def mlx_generate(python: str, model: str, adapter: str | None, prompt: str, max_tokens: int, timeout: int) -> tuple[str, bool]:
    cmd = [python, "-m", "mlx_lm", "generate", "--model", model, "--prompt", prompt, "--max-tokens", str(max_tokens), "--temp", "0"]
    if adapter:
        cmd += ["--adapter-path", adapter]
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
    out, inside = [], False
    for line in r.stdout.splitlines():
        if line.startswith("=========="):
            inside = not inside
            continue
        if inside:
            out.append(line)
    return "\n".join(out), r.returncode == 0


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--model", required=True, help="GGUF file (llama engine) or MLX repo id / directory (mlx engine)")
    p.add_argument("--engine", choices=["llama", "mlx"], default="llama"); p.add_argument("--adapter", help="LoRA adapter directory (mlx engine)")
    p.add_argument("--cases"); p.add_argument("--cases-file")
    p.add_argument("--max-tokens", type=int, default=64); p.add_argument("--timeout", type=int, default=180)
    a = p.parse_args(argv)
    if not (a.cases or a.cases_file):
        p.error("--cases or --cases-file required")
    cases = json.loads(a.cases) if a.cases else json.loads(confine(a.cases_file).read_text())
    if not isinstance(cases, list) or not 1 <= len(cases) <= 200:
        p.error("suite needs 1..200 cases")
    results, passed = [], 0
    if a.engine == "mlx":
        py = mlx_python()
        adapter = str(confine(a.adapter)) if a.adapter else None
        model = confine(a.model) if a.model.startswith("/") else a.model
        emit(kind="progress", stage="eval", engine="mlx", model=str(model), adapter=adapter)
        for i, case in enumerate(cases, 1):
            started = time.monotonic()
            try:
                text, ok = mlx_generate(py, str(model), adapter, case["prompt"], a.max_tokens, a.timeout)
                ok = ok and judge(case, text)
            except subprocess.TimeoutExpired:
                text, ok = "", False
            passed += ok
            results.append(dict(index=i, prompt=case["prompt"][:200], output=text.strip()[:400], passed=ok, seconds=round(time.monotonic() - started, 2)))
            emit(kind="progress", stage="eval", case=i, total=len(cases), passed=ok)
        emit(kind="result", passed=passed, total=len(cases), passRate=round(passed / len(cases), 4), cases=results, engine="mlx", adapter=adapter)
        return 0
    model = confine(a.model)
    if not model.is_file():
        emit(kind="error", message=f"model file not found: {model}")
        return 1
    try:
        tool = find_tool("llama-completion")
    except SystemExit:
        tool = find_tool("llama-cli")
    for i, case in enumerate(cases, 1):
        started = time.monotonic()
        try:
            r = subprocess.run([tool, "-m", str(model), "-p", case["prompt"], "-n", str(a.max_tokens), "--temp", "0", "--no-display-prompt", "-no-cnv"],
                               capture_output=True, text=True, timeout=a.timeout, stdin=subprocess.DEVNULL)
            text, ok = r.stdout, r.returncode == 0 and judge(case, r.stdout)
        except subprocess.TimeoutExpired:
            text, ok = "", False
        passed += ok
        results.append(dict(index=i, prompt=case["prompt"][:200], output=text.strip()[:400], passed=ok, seconds=round(time.monotonic() - started, 2)))
        emit(kind="progress", stage="eval", case=i, total=len(cases), passed=ok)
    emit(kind="result", passed=passed, total=len(cases), passRate=round(passed / len(cases), 4), cases=results)
    return 0 if passed == len(cases) else 0


if __name__ == "__main__":
    sys.exit(main())
