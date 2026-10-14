#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Optional privileged helper: publish this Mac's package power so Gryvia can estimate per-job energy.

`powermetrics` needs root, so this runs as a launchd daemon (see scripts/install-power-helper.sh) and only writes a
small world-readable JSON file; the unprivileged agent reads it and reports it. It does nothing else: no network, no
input, no files besides the output. Estimates only: package power is the whole SoC, not one process.

    sudo python3 power-helper.py --output /var/run/gryvia-power.json
"""
import argparse
import json
import os
import plistlib
import subprocess
import sys
import time


def _find(d, suffix):
    """First numeric value whose key ends with `suffix`, searching nested dicts (layout differs across macOS releases)."""
    if isinstance(d, dict):
        for k, v in d.items():
            if isinstance(k, str) and k.endswith(suffix) and isinstance(v, (int, float)) and not isinstance(v, bool):
                return float(v)
        for v in d.values():
            r = _find(v, suffix)
            if r is not None:
                return r
    elif isinstance(d, list):
        for v in d:
            r = _find(v, suffix)
            if r is not None:
                return r
    return None


def parse(plist_bytes: bytes):
    """powermetrics reports milliwatts. Returns {packageW, cpuW, gpuW, aneW} (None for what is missing)."""
    sample = plistlib.loads(plist_bytes.split(b"\x00")[0])
    out = {}
    for name, key in (("packageW", "combined_power"), ("cpuW", "cpu_power"), ("gpuW", "gpu_power"), ("aneW", "ane_power")):
        mw = _find(sample, key)
        out[name] = None if mw is None else round(mw / 1000.0, 3)
    return out


def sample_once(interval_ms=1000):
    r = subprocess.run(["/usr/bin/powermetrics", "--samplers", "cpu_power,gpu_power", "-n", "1", "-i", str(interval_ms), "--format", "plist"],
                       capture_output=True, timeout=30)
    if r.returncode != 0:
        raise RuntimeError(r.stderr.decode(errors="replace")[:200])
    return parse(r.stdout)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--output", default="/var/run/gryvia-power.json")
    p.add_argument("--interval", type=float, default=2.0)
    p.add_argument("--once", action="store_true")
    a = p.parse_args(argv)
    if os.geteuid() != 0:
        print("powermetrics needs root: run with sudo (or install the launchd daemon).", file=sys.stderr)
        return 1
    while True:
        try:
            data = sample_once()
            data["at"] = time.time()
            tmp = a.output + ".tmp"
            with open(tmp, "w") as f:
                json.dump(data, f)
            os.chmod(tmp, 0o644)
            os.replace(tmp, a.output)
        except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired) as e:
            print("power-helper:", e, file=sys.stderr)
        if a.once:
            return 0
        time.sleep(max(0.5, a.interval))


if __name__ == "__main__":
    sys.exit(main())
