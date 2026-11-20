# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""CPU protocol demo: a trainer owns state, its preStop hook requests a save.

Run this file normally to advance a counter; run with --request from the hook.
Real frameworks must replace the counter with a coordinated model checkpoint.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import time
import uuid


def atomic_json(target, data):
    temp = target.with_name(target.name + "." + uuid.uuid4().hex + ".tmp")
    try:
        with temp.open("x") as f:
            json.dump(data, f)
            f.flush()
            os.fsync(f.fileno())
        os.replace(temp, target)
        fd = os.open(target.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        temp.unlink(missing_ok=True)


def run(root, rank, interval):
    state = root / f"state-{rank}.json"
    request = root / f"request-{rank}.json"
    ack = root / f"ack-{rank}.json"
    step = 0
    if os.getenv("GRYVIA_RESUME_IF_PRESENT", "true") == "true" and state.exists():
        step = json.loads(state.read_text())["step"]
    print(f"resumed step={step}", flush=True)
    stopping = False

    def stop(_signum, _frame):
        nonlocal stopping
        stopping = True

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    last_save = time.monotonic()
    handled = None
    while not stopping:
        step += 1
        token = json.loads(request.read_text())["token"] if request.exists() else None
        if (token and token != handled) or time.monotonic() - last_save >= 1:
            atomic_json(state, {"step": step})
            if token and token != handled:
                atomic_json(ack, {"token": token, "step": step})
                handled = token
            last_save = time.monotonic()
        time.sleep(interval)
    atomic_json(state, {"step": step})


def request_save(root, rank, timeout):
    token = uuid.uuid4().hex
    atomic_json(root / f"request-{rank}.json", {"token": token})
    deadline = time.monotonic() + timeout
    ack = root / f"ack-{rank}.json"
    while time.monotonic() < deadline:
        if ack.exists() and json.loads(ack.read_text()).get("token") == token:
            return
        time.sleep(0.05)
    raise TimeoutError("trainer did not acknowledge checkpoint request")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--request", action="store_true")
    parser.add_argument("--timeout", type=float, default=90)
    parser.add_argument("--interval", type=float, default=0.1)
    args = parser.parse_args()
    root = Path(os.environ["GRYVIA_CHECKPOINT_DIR"])
    root.mkdir(parents=True, exist_ok=True)
    rank = str(int(os.getenv("RANK", "0")))
    if args.request:
        request_save(root, rank, args.timeout)
    else:
        run(root, rank, args.interval)
