#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Run llama.cpp's llama-bench on a managed model and report measured tokens/second."""
import argparse
import json
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from _common import confine, emit, find_tool, run_group  # noqa: E402


def parse(output: str) -> dict:
    rows = json.loads(output[output.index("["):])
    res = {}
    for r in rows:
        if r.get("n_gen", 0) == 0 and r.get("n_prompt", 0) > 0:
            res["promptTokensPerSecond"] = r["avg_ts"]
        elif r.get("n_gen", 0) > 0 and r.get("n_prompt", 0) == 0:
            res["generationTokensPerSecond"] = r["avg_ts"]
    if not res:
        raise ValueError("no throughput rows in llama-bench output")
    return res


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--model", required=True); p.add_argument("--prompt-tokens", type=int, default=512)
    p.add_argument("--gen-tokens", type=int, default=128); p.add_argument("--repetitions", type=int, default=3)
    a = p.parse_args(argv)
    if not (1 <= a.prompt_tokens <= 8192 and 1 <= a.gen_tokens <= 2048 and 1 <= a.repetitions <= 10):
        p.error("bounds: prompt 1..8192, gen 1..2048, repetitions 1..10")
    model = confine(a.model)
    if not model.is_file():
        emit(kind="error", message=f"model file not found: {model}")
        return 1
    tool = find_tool("llama-bench")
    emit(kind="progress", stage="bench", model=model.name)
    r = run_group([tool, "-m", str(model), "-p", str(a.prompt_tokens), "-n", str(a.gen_tokens), "-r", str(a.repetitions), "-o", "json"],
                       timeout=1800)
    if r.returncode != 0:
        emit(kind="error", message=(r.stderr or r.stdout)[-400:])
        return r.returncode
    try:
        emit(kind="result", promptTokens=a.prompt_tokens, genTokens=a.gen_tokens, repetitions=a.repetitions, **parse(r.stdout))
    except (ValueError, KeyError) as e:
        emit(kind="error", message=f"cannot parse llama-bench output: {e}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
