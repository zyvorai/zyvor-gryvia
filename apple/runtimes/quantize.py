#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Quantize a managed GGUF model with llama-quantize into the models directory."""
import argparse
import hashlib
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from _common import confine, emit, find_tool, models_root  # noqa: E402

TYPES = {"Q2_K", "Q3_K_S", "Q3_K_M", "Q4_0", "Q4_K_S", "Q4_K_M", "Q5_0", "Q5_K_S", "Q5_K_M", "Q6_K", "Q8_0", "F16"}


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--input", required=True); p.add_argument("--type", required=True); p.add_argument("--name")
    a = p.parse_args(argv)
    if a.type not in TYPES:
        p.error("type must be one of " + ", ".join(sorted(TYPES)))
    src = confine(a.input)
    if not src.is_file():
        emit(kind="error", message=f"input not found: {src}")
        return 1
    name = a.name or f"{src.stem}-{a.type}.gguf"
    if "/" in name or ".." in name or not name.endswith(".gguf"):
        p.error("name must be a plain .gguf filename")
    out = confine(str(models_root() / src.parent.name / name))
    out.parent.mkdir(parents=True, exist_ok=True)
    emit(kind="progress", stage="quantize", type=a.type)
    r = subprocess.run([find_tool("llama-quantize"), str(src), str(out), a.type], capture_output=True, text=True, timeout=7200)
    if r.returncode != 0 or not out.is_file():
        emit(kind="error", message=(r.stderr or r.stdout)[-400:])
        return r.returncode or 1
    h = hashlib.sha256()
    with open(out, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    emit(kind="result", path=str(out), sha256=h.hexdigest(), bytes=out.stat().st_size, quant=a.type, parent=str(src))
    return 0


if __name__ == "__main__":
    sys.exit(main())
