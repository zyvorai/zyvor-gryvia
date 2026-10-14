# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Shared helpers for model-lifecycle runtime wrappers (download, benchmark, evaluate, quantize)."""
import json
import os
import shutil
import sys
from pathlib import Path

HOSTS = ("huggingface.co", "hf.co")


def models_root() -> Path:
    root = Path(os.getenv("GRYVIA_MODELS_DIR") or (Path.home() / "gryvia/state/models"))
    root.mkdir(parents=True, exist_ok=True)
    return root.resolve()


def confine(path: str) -> Path:
    """Resolve `path` and require it to live under the models root or the training runs folder (no traversal, no symlink escape)."""
    p = Path(path).expanduser().resolve()
    roots = (models_root(), (models_root().parent / "runs").resolve())
    if not any(r == p or r in p.parents for r in roots):
        raise SystemExit(f"path outside managed directories: {p}")
    return p


def mlx_python() -> str:
    """Python interpreter that has mlx-lm: $GRYVIA_MLX_PYTHON, else ~/gryvia/venv, else the current interpreter."""
    for c in (os.getenv("GRYVIA_MLX_PYTHON"), str(Path.home() / "gryvia/venv/bin/python")):
        if c and Path(c).is_file():
            return c
    return sys.executable


def emit(**obj) -> None:
    print(json.dumps(obj, ensure_ascii=False), flush=True)


def find_tool(name: str) -> str:
    candidates = [Path(os.getenv("GRYVIA_LLAMA_BIN_DIR", "")) / name] if os.getenv("GRYVIA_LLAMA_BIN_DIR") else []
    candidates += [Path("/opt/homebrew/bin") / name, Path("/usr/local/bin") / name]
    for c in candidates:
        if c.is_file() and os.access(c, os.X_OK):
            return str(c)
    found = shutil.which(name)
    if found:
        return found
    emit(kind="error", message=f"{name} not found; install llama.cpp (brew install llama.cpp) or set GRYVIA_LLAMA_BIN_DIR")
    sys.exit(127)
