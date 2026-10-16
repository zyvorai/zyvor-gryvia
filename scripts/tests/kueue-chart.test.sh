#!/usr/bin/env bash
# Helm render tests for the opt-in Kueue integration of helm/gryvia: with every kueue value at its default nothing
# Kueue-related is rendered (no flag, no RBAC, no sub-chart); each switch adds exactly its own flag/RBAC; the ON render
# is valid YAML and lints. Needs helm (and python3 with PyYAML for the YAML check); no cluster.
# The sub-charts must be present: `helm dependency build helm/gryvia` (done here when charts/ is missing).
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
CHART=helm/gryvia
FAILED=0
PASSED=0
ok()   { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }
# helm 3.14 (CI) renders for Kubernetes 1.29 unless told otherwise, and the Kueue sub-chart requires >= 1.30
render() { helm template gryvia "$CHART" -n gryvia-system --kube-version 1.31.0 "$@" 2>&1; }
has()   { if grep -qE -e "$2" <<<"$OUT"; then ok "$1"; else fail "$1 (expected: $2)"; fi; }
lacks() { if grep -qE -e "$2" <<<"$OUT"; then fail "$1 (unexpected: $2)"; else ok "$1"; fi; }

[[ -d "$CHART/charts" ]] || helm dependency build "$CHART" >/dev/null || { echo "helm dependency build failed"; exit 1; }

echo "defaults"
OUT="$(render)"
lacks "no kueue flag" '--kueue'
lacks "no kueue.x-k8s.io rule or object" 'kueue\.x-k8s\.io'
lacks "no kueue sub-chart resource" 'name: kueue'

echo "aiOperator.kueueIntegration"
OUT="$(render --set aiOperator.kueueIntegration=true)"
has   "ai-operator flag" '--kueue-integration=true'
has   "default queue flag" '--kueue-default-queue=gryvia'
has   "workloads read rule" '- workloadpriorityclasses'
has   "clusterqueues read rule (status.queueExplanation)" '- clusterqueues'
lacks "no resourceflavors rule (quota operator switch is off)" '- resourceflavors'
lacks "sub-chart still off" 'name: kueue-controller-manager'

echo "strict admission"
OUT="$(render --set aiOperator.kueueIntegration=true --set aiOperator.kueueStrictAdmission=true)"
has "strict flag" '--kueue-strict-admission=true'
if render --set aiOperator.kueueStrictAdmission=true >/dev/null; then fail "strict without integration rejected"; else ok "strict without integration rejected"; fi

echo "quotaOperator.kueueIntegration"
OUT="$(render --set quotaOperator.kueueIntegration=true --set quotaOperator.kueueQuotaResources=cpu --set quotaOperator.kueueGpuTypeFlavors=true)"
has   "quota flag" '--kueue-quota-resources=cpu'
has   "gpu type flavors flag" '--kueue-gpu-type-flavors=true'
has   "clusterqueue rule" '- clusterqueues'
lacks "no workloadpriorityclass rule" '- workloadpriorityclasses'

echo "everything on"
OUT="$(render --set kueue.enabled=true --set aiOperator.kueueIntegration=true --set quotaOperator.kueueIntegration=true)"
has   "kueue controller rendered" 'name: kueue-controller-manager'
has   "waitForPodsReady configured" 'waitForPodsReady:'
has   "batch/job integration registered" '^ +- batch/job$'
has   "leader election tolerates a slow API server" 'leaseDuration: 60s'
has   "renew deadline below the lease" 'renewDeadline: 45s'
has   "readiness probe timeout is not 1s" 'timeoutSeconds: 5'
lacks "no 1s probe timeout left on the manager" 'timeoutSeconds: 1$'
if command -v python3 >/dev/null && python3 -c 'import yaml' 2>/dev/null; then
  if python3 -c 'import sys,yaml; list(yaml.safe_load_all(sys.stdin))' <<<"$OUT" 2>/dev/null; then ok "valid YAML"; else fail "valid YAML"; fi
fi
if helm lint "$CHART" --set kueue.enabled=true --set aiOperator.kueueIntegration=true --set quotaOperator.kueueIntegration=true >/dev/null 2>&1; then ok "helm lint (on)"; else fail "helm lint (on)"; fi
if helm lint "$CHART" >/dev/null 2>&1; then ok "helm lint (default)"; else fail "helm lint (default)"; fi

echo
echo "$PASSED passed, $FAILED failed"
[[ "$FAILED" == 0 ]]
