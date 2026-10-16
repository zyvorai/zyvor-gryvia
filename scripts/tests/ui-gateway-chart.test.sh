#!/usr/bin/env bash
# Helm render tests for the dashboard's Gateway API (Envoy) exposure: off by default, mutually exclusive with
# ui.ingress, requires the backend CA ConfigMap, HTTPS listener + redirect only with a certificate, and the
# attach-to-existing mode creates no Gateway. No cluster needed.
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
count() { grep -cE -e "$1" <<<"$OUT"; }

[[ -d "$CHART/charts" ]] || helm dependency build "$CHART" >/dev/null || { echo "helm dependency build failed"; exit 1; }
GW=(--set ui.gateway.enabled=true --set ui.gateway.backendTLS.caConfigMapName=gryvia-ui-ca)

echo "defaults"
OUT="$(render)"
lacks "no Gateway API objects" 'gateway\.networking\.k8s\.io'

echo "plain HTTP listener"
OUT="$(render "${GW[@]}")"
has   "Gateway" '^kind: Gateway$'
has   "HTTPRoute" '^kind: HTTPRoute$'
has   "BackendTLSPolicy pins the CA ConfigMap" 'name: "gryvia-ui-ca"'
has   "validation hostname is a chart SAN" 'hostname: "gryvia-ui\.gryvia-system\.svc"'
lacks "no redirect without a certificate" 'RequestRedirect'
lacks "no https listener without a certificate" '- name: https$'

echo "with a client-facing certificate"
OUT="$(render "${GW[@]}" --set ui.gateway.tlsSecretName=gryvia-ui-tls)"
has   "https listener" '- name: https$'
has   "redirect route" 'type: RequestRedirect'
[[ "$(count '^kind: HTTPRoute$')" == 2 ]] && ok "two routes (redirect + app)" || fail "two routes (redirect + app)"

echo "attach to an existing Gateway"
OUT="$(render "${GW[@]}" --set ui.gateway.createGateway=false --set ui.gateway.gatewayName=shared --set ui.gateway.gatewayNamespace=envoy-gateway-system)"
lacks "no Gateway created" '^kind: Gateway$'
has   "parentRef name" 'name: shared'
has   "parentRef namespace" 'namespace: envoy-gateway-system'

echo "guards"
if render "${GW[@]}" --set ui.ingress.enabled=true >/dev/null; then fail "ingress+gateway rejected"; else ok "ingress+gateway rejected"; fi
if render --set ui.gateway.enabled=true >/dev/null; then fail "missing CA ConfigMap rejected"; else ok "missing CA ConfigMap rejected"; fi
if helm lint "$CHART" "${GW[@]}" >/dev/null 2>&1; then ok "helm lint (gateway on)"; else fail "helm lint (gateway on)"; fi

echo
echo "$PASSED passed, $FAILED failed"
[[ "$FAILED" == 0 ]]
