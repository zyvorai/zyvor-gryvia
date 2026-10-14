#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Serve a managed GGUF model with llama-server on loopback and register the endpoint with the control plane.

Binds 127.0.0.1 only: it is reachable by a control plane on the same Mac (or through an ssh tunnel), never the LAN.
Runs until cancelled (SIGTERM)."""
import argparse
import os
import signal
import socket
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from _common import confine, emit, find_tool, mlx_python  # noqa: E402


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--model", required=True); p.add_argument("--ctx", type=int, default=4096); p.add_argument("--port", type=int, default=0)
    p.add_argument("--engine", choices=["llama", "mlx"], default="llama", help="llama: GGUF via llama-server; mlx: MLX model directory via mlx_lm server")
    a = p.parse_args(argv)
    model = confine(a.model)
    port = a.port or free_port()
    if a.engine == "mlx":
        if not model.is_dir():
            emit(kind="error", message=f"MLX model directory not found: {model}")
            return 1
        cmd = [mlx_python(), "-m", "mlx_lm", "server", "--model", str(model), "--host", "127.0.0.1", "--port", str(port)]
    else:
        if not model.is_file():
            emit(kind="error", message=f"model file not found: {model}")
            return 1
        cmd = [find_tool("llama-server"), "-m", str(model), "--host", "127.0.0.1", "--port", str(port), "-c", str(a.ctx)]
    proc = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    stop = {"now": False}
    signal.signal(signal.SIGTERM, lambda *_: stop.update(now=True))
    signal.signal(signal.SIGINT, lambda *_: stop.update(now=True))
    url = f"http://127.0.0.1:{port}"
    deadline = time.monotonic() + 180
    ready = False
    while time.monotonic() < deadline and not stop["now"] and proc.poll() is None:
        try:
            with urllib.request.urlopen(url + ("/v1/models" if a.engine == "mlx" else "/health"), timeout=2) as r:
                if r.status == 200:
                    ready = True
                    break
        except OSError:
            time.sleep(0.5)
    if not ready:
        proc.terminate()
        emit(kind="error", message="llama-server did not become healthy")
        return 1
    emit(kind="endpoint", url=url)
    parent = os.getppid()
    # If the agent that launched us goes away (restart, crash) we are re-parented: stop instead of leaking a server.
    while not stop["now"] and proc.poll() is None and os.getppid() == parent:
        time.sleep(0.5)
    proc.terminate()
    try:
        proc.wait(10)
    except subprocess.TimeoutExpired:
        proc.kill()
    return 0


if __name__ == "__main__":
    sys.exit(main())
