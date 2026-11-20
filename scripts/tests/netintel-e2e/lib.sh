#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Helpers for .github/workflows/e2e-netintel.yml (sourced by each step).
#
#   py NS KIND NAME 'python assertions on the dict s (= .status)'   one attempt; non-zero when an assertion fails
#   wait_for DESCRIPTION SECONDS COMMAND...                          retry every 5 s until COMMAND succeeds
#   py_now NS KIND NAME 'assertions'                                 py, but fails the step at once
set -u

py() {
  local ns="$1" kind="$2" name="$3" code="$4"
  kubectl -n "$ns" get "$kind" "$name" -o json | python3 -c "
import json, sys
o = json.load(sys.stdin)
s = o.get('status') or {}
$code
"
}

py_now() {
  py "$@" || { echo "assertion failed: $*" >&2; kubectl -n "$1" get "$2" "$3" -o yaml >&2; exit 1; }
}

wait_for() {
  local desc="$1" secs="$2"
  shift 2
  local i
  for i in $(seq 1 $((secs / 5))); do
    if "$@" >/dev/null 2>&1; then
      echo "ok: $desc"
      return 0
    fi
    sleep 5
  done
  echo "TIMEOUT: $desc" >&2
  "$@" || true
  exit 1
}
