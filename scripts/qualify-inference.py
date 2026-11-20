#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Measure Gateway request success, complete-stream latency and canary distribution."""
import argparse
import json
from pathlib import Path
import math
import time
import urllib.request
import sys


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--url', required=True)
    p.add_argument('--body', type=Path, required=True)
    p.add_argument('--requests', type=int, default=100)
    p.add_argument('--timeout', type=float, default=120)
    p.add_argument('--max-error-rate', type=float, default=.01)
    p.add_argument('--max-p95-seconds', type=float, required=True)
    p.add_argument('--canary-weight', type=float)
    p.add_argument('--tolerance-percent', type=float, default=10)
    p.add_argument('--output', type=Path, required=True)
    a = p.parse_args()
    if (a.requests < 1 or not math.isfinite(a.timeout) or a.timeout <= 0
            or not 0 <= a.max_error_rate <= 1 or not math.isfinite(a.max_p95_seconds) or a.max_p95_seconds <= 0
            or not 0 <= a.tolerance_percent <= 100
            or (a.canary_weight is not None and not 0 <= a.canary_weight <= 100)):
        p.error('invalid request count, timeout or thresholds')
    body = a.body.read_bytes(); json.loads(body)
    durations, errors, tracks = [], 0, {'stable': 0, 'canary': 0, 'unknown': 0}
    for _ in range(a.requests):
        start = time.monotonic()
        try:
            req = urllib.request.Request(a.url, data=body, headers={'Content-Type': 'application/json'})
            with urllib.request.urlopen(req, timeout=a.timeout) as response:
                track = response.headers.get('X-Gryvia-Track', 'unknown')
                tracks[track if track in tracks else 'unknown'] += 1
                while response.read(65536): pass
        except Exception:
            errors += 1
        durations.append(time.monotonic() - start)
    p95 = sorted(durations)[math.ceil(.95 * len(durations)) - 1]; rate = errors / a.requests
    passed = rate <= a.max_error_rate and p95 <= a.max_p95_seconds
    if a.canary_weight is not None:
        passed = passed and tracks['unknown'] == 0 and abs(100 * tracks['canary'] / a.requests - a.canary_weight) <= a.tolerance_percent
    result = {'requests': a.requests, 'errors': errors, 'error_rate': rate, 'p95_seconds': p95, 'tracks': tracks, 'passed': passed}
    a.output.write_text(json.dumps(result, indent=2))
    return 0 if passed else 1

if __name__ == '__main__': sys.exit(main())
