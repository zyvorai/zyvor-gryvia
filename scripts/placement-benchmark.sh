#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Run the Janus-format fixtures in tests/placement/janus through placement-sim and compare with
# tests/placement/baseline.json. Usage: scripts/placement-benchmark.sh [--update] [--tolerance PCT]
set -euo pipefail
cd "$(dirname "$0")/.."

update=0
tol=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --update) update=1 ;;
    --tolerance) tol="${2:?--tolerance needs a percentage}"; shift ;;
    -h|--help) sed -n '2,3p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

fixtures=tests/placement/janus/clusters
baseline=tests/placement/baseline.json
out="$(mktemp)"
trap 'rm -f "$out"' EXIT

(cd operators/ai-operator && go build -o "$OLDPWD/.placement-sim.bin" ./cmd/placement-sim)
trap 'rm -f "$out" .placement-sim.bin' EXIT

python3 - "$fixtures" "$out" <<'PY'
import json, subprocess, sys, pathlib
fixtures, out = pathlib.Path(sys.argv[1]), sys.argv[2]
res = {}
for cfg in sorted(fixtures.glob("*.yaml")):
    p = subprocess.run(["./.placement-sim.bin", "-config", str(cfg), "-json"], capture_output=True, text=True)
    if p.returncode != 0:
        sys.exit(f"{cfg.name}: placement-sim failed: {p.stderr.strip()}")
    res[cfg.stem] = json.loads(p.stdout)
json.dump(res, open(out, "w"), indent=2, sort_keys=True)
PY

if [[ $update -eq 1 ]]; then
  cp "$out" "$baseline"
  echo "baseline updated: $baseline"
  exit 0
fi

python3 - "$baseline" "$out" "$tol" <<'PY'
import json, sys
base, cur, tol = json.load(open(sys.argv[1])), json.load(open(sys.argv[2])), float(sys.argv[3]) / 100
# metric -> True when higher is better
metrics = {"makespan": False, "mean_wait_time": False, "gpu_utilization": True,
           "jobs_completed": True, "jobs_unschedulable": False, "memory_violations": False}
bad, better = [], []
for name in sorted(set(base) | set(cur)):
    if name not in base or name not in cur:
        bad.append(f"{name}: present in only one of baseline/current (run --update if the fixtures changed)")
        continue
    for m, high in metrics.items():
        b, c = base[name][m], cur[name][m]
        slack = abs(b) * tol + 1e-9
        worse = (c < b - slack) if high else (c > b + slack)
        improved = (c > b + slack) if high else (c < b - slack)
        if worse:
            bad.append(f"{name}: {m} {b:g} -> {c:g}")
        elif improved:
            better.append(f"{name}: {m} {b:g} -> {c:g}")
for line in better:
    print("improved:", line)
if bad:
    print("PLACEMENT REGRESSION:", *bad, sep="\n  ")
    sys.exit(1)
print(f"placement benchmark ok ({len(cur)} fixtures, tolerance {tol*100:g}%)")
PY
