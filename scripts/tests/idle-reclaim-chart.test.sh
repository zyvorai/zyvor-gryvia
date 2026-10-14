#!/usr/bin/env bash
# Helm render tests for aiOperator.idleReclaim: off renders nothing; on renders the four flags, also when the
# values come from an older release that has no idleReclaim defaults (helm upgrade --reuse-values).
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
CHART=helm/gryvia
FAILED=0; PASSED=0
ok()   { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }
render() { helm template gryvia "$CHART" -n gryvia-system --kube-version 1.31.0 "$@" 2>&1; }
has()   { if grep -qE -e "$2" <<<"$OUT"; then ok "$1"; else fail "$1 (expected: $2)"; fi; }
lacks() { if grep -qE -e "$2" <<<"$OUT"; then fail "$1 (unexpected: $2)"; else ok "$1"; fi; }

[[ -d "$CHART/charts" ]] || helm dependency build "$CHART" >/dev/null || { echo "helm dependency build failed"; exit 1; }

echo "default"
OUT="$(render)"
lacks "no idle-reclaim flag" '--idle-reclaim'

echo "enabled"
OUT="$(render --set aiOperator.idleReclaim.enabled=true)"
has "flag" '--idle-reclaim=true'
has "suspend off" '--idle-reclaim-suspend=false'
has "threshold" '--idle-reclaim-threshold=10'
has "window" '--idle-reclaim-window=30m'

echo "enabled=true with the nested defaults missing (--reuse-values from an older release)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
cp -R "$CHART" "$tmp/gryvia"
python3 - "$tmp/gryvia/values.yaml" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
s = re.sub(r"  idleReclaim:\n(?:    .*\n|  #.*\n)+", "", s, count=1)
open(p, "w").write(s)
PY
OUT="$(helm template gryvia "$tmp/gryvia" -n gryvia-system --kube-version 1.31.0 --set aiOperator.idleReclaim.enabled=true 2>&1)"
has "flag" '--idle-reclaim=true'
has "suspend has a value" '--idle-reclaim-suspend=false'
has "threshold has a value" '--idle-reclaim-threshold=10'
has "window has a value" '--idle-reclaim-window=30m'
lacks "no empty flag value" '--idle-reclaim-[a-z]+=$'

echo
echo "$PASSED passed, $FAILED failed"
[[ $FAILED -eq 0 ]]
