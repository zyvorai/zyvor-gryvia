#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Idempotently (re)write the `runtimes` section of an agent.json for this Mac.

Detects what is actually installed: llama.cpp tools (bench/evaluate/quantize/llama), and an MLX-capable Python
(a venv at ~/gryvia/venv, or --mlx-python). Always enables `download` and `demo`. Prints what was enabled."""
import argparse
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

root = Path(__file__).resolve().parents[1]
p = argparse.ArgumentParser(description=__doc__)
p.add_argument("agent_json")
p.add_argument("--python", default=sys.executable)
p.add_argument("--mlx-python", help="python with mlx-lm installed (default: ~/gryvia/venv/bin/python if it can import mlx_lm)")
p.add_argument("--llama-bin-dir", help="directory containing llama-bench, llama-cli, ...")
a = p.parse_args()

path = Path(a.agent_json)
cfg = json.loads(path.read_text())
rt = cfg.setdefault("runtimes", {})
py = str(Path(a.python).resolve())


def tool(name):
    if a.llama_bin_dir and (Path(a.llama_bin_dir) / name).is_file():
        return str(Path(a.llama_bin_dir) / name)
    for d in ("/opt/homebrew/bin", "/usr/local/bin"):
        if (Path(d) / name).is_file():
            return str(Path(d) / name)
    return shutil.which(name)


rt["demo"] = {"executable": py, "prefix": [str(root / "runtimes/demo.py")]}
rt["download"] = {"executable": py, "prefix": [str(root / "runtimes/hf_download.py")]}
if tool("llama-bench"):
    rt["bench"] = {"executable": py, "prefix": [str(root / "runtimes/llama_bench.py")]}
if tool("llama-completion") or tool("llama-cli"):
    rt["evaluate"] = {"executable": py, "prefix": [str(root / "runtimes/eval_suite.py")]}
if tool("llama-quantize"):
    rt["quantize"] = {"executable": py, "prefix": [str(root / "runtimes/quantize.py")]}
if tool("llama-server"):
    rt["serve"] = {"executable": py, "prefix": [str(root / "runtimes/llama_serve.py")]}
if tool("llama-cli"):
    rt.setdefault("llama", {"executable": tool("llama-cli"), "prefix": []})
mlx_py = a.mlx_python or str(Path.home() / "gryvia/venv/bin/python")
if Path(mlx_py).is_file() and subprocess.run([mlx_py, "-c", "import mlx_lm"], capture_output=True).returncode == 0:
    rt["mlx"] = {"executable": mlx_py, "prefix": [str(root / "runtimes/mlx_generate.py")]}
    rt["train"] = {"executable": mlx_py, "prefix": [str(root / "runtimes/mlx_lora.py")]}
    rt["fuse"] = {"executable": py, "prefix": [str(root / "runtimes/mlx_fuse.py")]}

tmp = path.with_suffix(".json.tmp")
fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump(cfg, f, indent=2)
os.replace(tmp, path)
print("runtimes:", ", ".join(sorted(rt)))
