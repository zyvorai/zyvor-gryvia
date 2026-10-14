#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""LoRA fine-tuning on this Mac with mlx-lm, reporting live loss/learning-rate metrics.

Single-Mac, single-GPU training: no multi-node or distributed claims. The dataset is a JSONL file under the managed
models directory (rows like {"text": ...} or {"prompt":..., "completion":...}); a 90/10 split is made per run."""
import argparse
import json
import os
import re
import signal
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from _common import confine, emit, models_root  # noqa: E402

TRAIN = re.compile(r"Iter (\d+): Train loss ([\d.]+), Learning Rate ([\d.eE+-]+), It/sec ([\d.]+), Tokens/sec ([\d.]+).*?Peak mem ([\d.]+) GB")
VAL = re.compile(r"Iter (\d+): Val loss ([\d.]+)")


ANSI = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")
# mlx-lm >= 0.3x prints a table:  "   5  5.464 ▼  2,804  0.2k"  (train)   and   "   5  val 5.253  0.01s"  (validation)
TABLE_TRAIN = re.compile(r"^\s*(\d+)\s+([\d.]+)\s*[▼▲=]?\s+([\d,]+)\s+[\d.]+k?\s*$")
TABLE_VAL = re.compile(r"^\s*(\d+)\s+val\s+([\d.]+)")


def parse_line(line: str):
    line = ANSI.sub("", line)
    m = TABLE_VAL.match(line)
    if m:
        return dict(kind="metric", step=int(m.group(1)), valLoss=float(m.group(2)))
    m = TABLE_TRAIN.match(line)
    if m:
        return dict(kind="metric", step=int(m.group(1)), trainLoss=float(m.group(2)), tokensPerSec=float(m.group(3).replace(",", "")))
    m = TRAIN.search(line)
    if m:
        step, loss, lr, its, tps, peak = m.groups()
        return dict(kind="metric", step=int(step), trainLoss=float(loss), lr=float(lr), itPerSec=float(its), tokensPerSec=float(tps), peakGB=float(peak))
    m = VAL.search(line)
    if m:
        return dict(kind="metric", step=int(m.group(1)), valLoss=float(m.group(2)))
    return None


def prepare(data: Path, run_dir: Path, valid_fraction: float) -> tuple[int, int]:
    rows = [l for l in data.read_text(encoding="utf-8").splitlines() if l.strip()]
    for r in rows[:5]:
        json.loads(r)
    if len(rows) < 4:
        raise SystemExit("dataset needs at least 4 rows")
    n_valid = max(1, int(len(rows) * valid_fraction))
    split = run_dir / "data"
    split.mkdir(parents=True, exist_ok=True)
    (split / "train.jsonl").write_text("\n".join(rows[n_valid:]) + "\n")
    (split / "valid.jsonl").write_text("\n".join(rows[:n_valid]) + "\n")
    (split / "test.jsonl").write_text("\n".join(rows[:n_valid]) + "\n")
    return len(rows) - n_valid, n_valid


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--model", required=True, help="Hugging Face repo id or local MLX model directory")
    p.add_argument("--data", required=True, help="JSONL dataset under the managed models directory")
    p.add_argument("--iters", type=int, default=100); p.add_argument("--batch-size", type=int, default=2)
    p.add_argument("--num-layers", type=int, default=8); p.add_argument("--learning-rate", type=float, default=1e-5)
    p.add_argument("--max-seq-length", type=int, default=512)
    a = p.parse_args(argv)
    if not (1 <= a.iters <= 5000 and 1 <= a.batch_size <= 32 and -1 <= a.num_layers <= 64 and 16 <= a.max_seq_length <= 4096):
        p.error("bounds: iters 1..5000, batch 1..32, layers -1..64, seq 16..4096")
    data = confine(a.data)
    if not data.is_file():
        emit(kind="error", message=f"dataset not found: {data}")
        return 1
    if "/" in a.model and not a.model.startswith("/"):
        pass  # Hugging Face repo id: mlx-lm downloads it itself
    elif a.model.startswith("/"):
        confine(a.model)
    run_dir = models_root().parent / "runs" / (os.getenv("GRYVIA_JOB_ID") or str(int(time.time())))
    run_dir.mkdir(parents=True, exist_ok=True)
    n_train, n_valid = prepare(data, run_dir, 0.1)
    adapter = run_dir / "adapter"
    # Resume after the owner returned / the agent restarted: the same job id keeps its run folder, so reuse the last
    # saved adapter weights. (mlx-lm restarts its iteration counter; the weights are what carries over.)
    resume = adapter / "adapters.safetensors"
    emit(kind="progress", stage="train", trainRows=n_train, validRows=n_valid, model=a.model)
    cmd = [sys.executable, "-m", "mlx_lm", "lora", "--model", a.model, "--train", "--data", str(run_dir / "data"), "--iters", str(a.iters),
           "--batch-size", str(a.batch_size), "--num-layers", str(a.num_layers), "--learning-rate", str(a.learning_rate),
           "--adapter-path", str(adapter), "--steps-per-report", "1" if a.iters <= 20 else "5", "--steps-per-eval", str(max(1, a.iters // 4)),
           "--val-batches", "2", "--max-seq-length", str(a.max_seq_length), "--save-every", str(max(10, min(50, a.iters // 4)))]
    if resume.is_file():
        cmd += ["--resume-adapter-file", str(resume)]
        emit(kind="progress", stage="resume", adapter=str(resume))
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, bufsize=1, start_new_session=True)

    def _term(*_):
        try:
            os.killpg(proc.pid, signal.SIGTERM)
        except (ProcessLookupError, PermissionError):
            pass
        emit(kind="progress", stage="yielded")
        sys.exit(143)
    signal.signal(signal.SIGTERM, _term)
    last_train = last_val = None
    tail = []
    for line in proc.stdout:
        tail = (tail + [line.rstrip()])[-8:]
        m = parse_line(line)
        if m:
            emit(**m)
            last_train = m.get("trainLoss", last_train)
            last_val = m.get("valLoss", last_val)
    code = proc.wait()
    if code != 0 or not adapter.exists():
        emit(kind="error", message="mlx_lm lora failed: " + " | ".join(tail[-3:]))
        return code or 1
    emit(kind="result", adapterPath=str(adapter), finalTrainLoss=last_train, finalValLoss=last_val, iters=a.iters, trainRows=n_train, model=a.model)
    return 0


if __name__ == "__main__":
    sys.exit(main())
