#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Fuse a LoRA adapter into its MLX base model, producing a standalone model directory under the models folder."""
import argparse
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from _common import confine, emit, mlx_python, models_root  # noqa: E402


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--model", required=True); p.add_argument("--adapter", required=True); p.add_argument("--name", required=True)
    a = p.parse_args(argv)
    if not a.name.replace("-", "").replace("_", "").replace(".", "").isalnum() or ".." in a.name:
        p.error("name must be letters, digits, - _ .")
    adapter = confine(a.adapter)
    if not adapter.is_dir():
        emit(kind="error", message=f"adapter not found: {adapter}")
        return 1
    out = confine(str(models_root() / "_fused" / a.name))
    out.parent.mkdir(parents=True, exist_ok=True)
    model = str(confine(a.model)) if a.model.startswith("/") else a.model
    emit(kind="progress", stage="fuse", model=model, adapter=str(adapter))
    r = subprocess.run([mlx_python(), "-m", "mlx_lm", "fuse", "--model", model, "--adapter-path", str(adapter), "--save-path", str(out)],
                       capture_output=True, text=True, timeout=3600)
    if r.returncode != 0 or not (out / "config.json").exists():
        emit(kind="error", message=(r.stderr or r.stdout)[-400:])
        return r.returncode or 1
    size = sum(f.stat().st_size for f in out.rglob("*") if f.is_file())
    emit(kind="result", path=str(out), bytes=size, parent=model, adapter=str(adapter))
    return 0


if __name__ == "__main__":
    sys.exit(main())
