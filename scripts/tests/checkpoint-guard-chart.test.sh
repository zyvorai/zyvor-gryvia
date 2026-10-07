#!/usr/bin/env bash
# Helm render tests for aiOperator.checkpointGuard: off renders no flag and no roles/rolebindings rule; on adds
# the ai-operator flag (and only there) and the rbac.authorization.k8s.io rule. Needs helm; no cluster.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
CHART=helm/gryvia
FAILED=0
PASSED=0
ok()   { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }
render() { helm template gryvia "$CHART" -n gryvia-system --kube-version 1.31.0 "$@" 2>&1; }
has()   { if grep -qE -e "$2" <<<"$OUT"; then ok "$1"; else fail "$1 (expected: $2)"; fi; }
lacks() { if grep -qE -e "$2" <<<"$OUT"; then fail "$1 (unexpected: $2)"; else ok "$1"; fi; }

[[ -d "$CHART/charts" ]] || helm dependency build "$CHART" >/dev/null || { echo "helm dependency build failed"; exit 1; }

echo "defaults"
OUT="$(render)"
lacks "no checkpoint-guard flag" '--checkpoint-guard'
lacks "no roles rule" '^ +- roles$'

echo "aiOperator.checkpointGuard"
OUT="$(render --set aiOperator.checkpointGuard=true)"
has "flag" '--checkpoint-guard=true'
has "roles rule" '^ +- roles$'
has "rolebindings rule" '^ +- rolebindings$'
n="$(grep -c -e '--checkpoint-guard=true' <<<"$OUT")"
if [[ "$n" == 1 ]]; then ok "flag on the ai-operator only"; else fail "flag rendered $n times"; fi

echo
echo "passed $PASSED, failed $FAILED"
[[ $FAILED -eq 0 ]]
