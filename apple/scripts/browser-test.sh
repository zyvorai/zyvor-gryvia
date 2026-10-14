#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
set -euo pipefail
cd "$(dirname "$0")/.."
python3 -m tests.browser_server >/tmp/gryvia-browser-test.log 2>&1 &
test_server_pid=$!
trap 'kill "$test_server_pid" 2>/dev/null || true' EXIT
for i in {1..30}; do
  if curl -fsS http://127.0.0.1:8765/healthz >/dev/null; then break; fi
  sleep 0.2
done
node tests/browser.cjs
