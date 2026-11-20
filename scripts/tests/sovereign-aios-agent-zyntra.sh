#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# The agent runtime's zyntra tool against a real Zyntra, wired the way helm/sovereign-aios wires them: the agent token
# and Zyntra's policy come from the rendered chart, Zyntra serves the chart's pack against a stand-in Gryvia gateway,
# and the reference runtime (examples/agents) calls Zyntra over HTTP. No cluster.
#
# Needs go, helm, and python3 with the runtime's requirements (examples/agents/requirements.txt) and PyYAML. Zyntra
# is built from ZYNTRA_DIR (default ../zyntra); it must have service tokens and policyExistingSecret.
set -euo pipefail
cd "$(dirname "$0")/../.."
ROOT="$PWD"
ZYNTRA_DIR="$(cd "${ZYNTRA_DIR:-../zyntra}" && pwd)"
PY="${PYTHON:-python3}"
GPORT="${GRYVIA_PORT:-18181}"
ZPORT="${ZYNTRA_PORT:-18080}"
WORK="$(mktemp -d)"
PIDS=()
cleanup() { for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$WORK"; }
trap cleanup EXIT

echo "build Zyntra from $ZYNTRA_DIR"
(cd "$ZYNTRA_DIR" && go build -o "$WORK/zyntra" ./cmd/zyntra)

echo "render the chart's agent token and Zyntra policy"
ZYNTRA_DIR="$ZYNTRA_DIR" scripts/sovereign-aios-deps.sh >/dev/null
helm template sa helm/sovereign-aios -n gryvia-system --set 'agentToken.namespaces={tenant-alpha}' >"$WORK/render.yaml"
"$PY" - "$WORK" <<'EOF'
import sys, yaml
work = sys.argv[1]
sec = {d["metadata"]["name"]: d for d in yaml.safe_load_all(open(f"{work}/render.yaml")) if d and d["kind"] == "Secret"}
open(f"{work}/policy.yaml", "w").write(sec["sovereign-aios-zyntra-policy"]["stringData"]["policy.yaml"])
open(f"{work}/token", "w").write(sec["zyntra-agent-token"]["stringData"]["token"])
EOF

mkdir -p "$WORK/pack"
for f in helm/sovereign-aios/files/zyntra-pack/*; do
  sed "s#https://gryvia-api-gateway.gryvia-system.svc:8080#http://127.0.0.1:$GPORT#g" "$f" >"$WORK/pack/$(basename "$f")"
done

"$PY" scripts/tests/fake_gryvia_gateway.py "$GPORT" gryvia-test-key &
PIDS+=($!)
# Zyntra ingests once at startup and then on its refresh interval, so the fake gateway must be up first.
for _ in $(seq 1 30); do
  curl -fsS -H 'Authorization: Bearer gryvia-test-key' "http://127.0.0.1:$GPORT/api/nodes" >/dev/null 2>&1 && break
  sleep 0.5
done
ZYNTRA_API_KEY=admin-test-key ZYNTRA_GRAVIA_TOKEN=gryvia-test-key ZYNTRA_STATE_DIR="$WORK/state" \
  ZYNTRA_LISTEN="127.0.0.1:$ZPORT" ZYNTRA_POLICY="$WORK/policy.yaml" \
  "$WORK/zyntra" serve -f "$WORK/pack" >"$WORK/zyntra.log" 2>&1 &
PIDS+=($!)

echo "wait for Zyntra to ingest Gryvia's objects"
ingested=false
for _ in $(seq 1 60); do
  if curl -fsS -H 'Authorization: Bearer admin-test-key' \
      "http://127.0.0.1:$ZPORT/api/v1/ontology/objects?type=InferenceService" 2>/dev/null | grep -q InferenceService:; then
    ingested=true
    break
  fi
  sleep 1
done
if ! $ingested; then
  echo "FAIL Zyntra did not ingest Gryvia's objects"; cat "$WORK/zyntra.log"; exit 1
fi

echo "agent against Zyntra"
if ZYNTRA_URL="http://127.0.0.1:$ZPORT" ZYNTRA_TOKEN_OPS="$(cat "$WORK/token")" ZYNTRA_ADMIN_KEY=admin-test-key \
    "$PY" scripts/tests/agent_zyntra_e2e.py; then
  echo "PASS"
else
  echo "--- zyntra.log"; tail -30 "$WORK/zyntra.log"; echo "FAIL"; exit 1
fi
