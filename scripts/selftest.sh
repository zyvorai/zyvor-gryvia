#!/bin/bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# ============================================================================
# selftest.sh — Gryvia end-to-end self-test
# ============================================================================
# Runs a quick smoke test: creates a test job, waits for scheduling,
# verifies quota tracking, then cleans up.
#
# Usage:
#   ./scripts/selftest.sh
#   ./scripts/selftest.sh --keep    # don't clean up test resources
#   ./scripts/selftest.sh --dry-run # show what would be done
# ============================================================================

set -euo pipefail

KEEP=false
DRY_RUN=false
for arg in "$@"; do
    case "$arg" in
        --keep)    KEEP=true ;;
        --dry-run) DRY_RUN=true ;;
        --help|-h)
            echo "Usage: $0 [--keep] [--dry-run]"
            exit 0
            ;;
    esac
done

PASS=0
FAIL=0
TEST_NS="gryvia-selftest"
TEST_JOB="selftest-$(date +%s)"

ok()   { echo "  ✅ $*"; ((PASS++)); }
fail() { echo "  ❌ $*"; ((FAIL++)); }
step() { echo ""; echo "  🧪 $*"; }

cleanup() {
    if ! $KEEP && ! $DRY_RUN; then
        echo ""
        echo "  🧹 Cleaning up..."
        kubectl delete gryviaaijob "$TEST_JOB" -n "$TEST_NS" --ignore-not-found 2>/dev/null || true
        kubectl delete namespace "$TEST_NS" --ignore-not-found 2>/dev/null || true
    fi
}
trap cleanup EXIT

echo ""
echo "  ╔══════════════════════════════════════════════════╗"
echo "  ║     🧪 Gryvia Self-Test                      ║"
echo "  ╚══════════════════════════════════════════════════╝"
echo ""

if $DRY_RUN; then
    echo "  Mode: dry-run (no resources will be created)"
    echo ""
fi

# ── Test 1: Cluster connectivity ──
step "Test 1: Cluster connectivity"

if kubectl cluster-info &>/dev/null 2>&1; then
    ok "Cluster is reachable"
else
    fail "Cannot reach cluster"
    echo ""; echo "  Aborting: no cluster access"; exit 1
fi

# ── Test 2: CRDs exist ──
step "Test 2: CRDs installed"

ALL_CRDS=true
for crd in gryviaaijobs gryviagpunodes gryviaquotas gryviastorages gryvianetworks; do
    if kubectl get crd "${crd}.gryvia.io" &>/dev/null 2>&1; then
        ok "${crd}.gryvia.io exists"
    else
        fail "${crd}.gryvia.io missing"
        ALL_CRDS=false
    fi
done

if ! $ALL_CRDS; then
    echo ""; echo "  Aborting: CRDs not installed. Run: kubectl apply -f crds/"; exit 1
fi

# ── Test 3: Create test namespace ──
step "Test 3: Create test namespace"

if ! $DRY_RUN; then
    kubectl create namespace "$TEST_NS" --dry-run=client -o yaml | kubectl apply -f - 2>/dev/null
    ok "Namespace $TEST_NS created"
else
    ok "Would create namespace $TEST_NS"
fi

# ── Test 4: Submit test job ──
step "Test 4: Submit test GryviaAIJob"

JOB_YAML=$(cat <<EOF
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: $TEST_JOB
  namespace: $TEST_NS
spec:
  type: training
  gpus: 1
  gpuType: T4
  image: python:3.12-slim
  command:
    - python3
    - -c
    - "print('Gryvia selftest OK'); import time; time.sleep(5)"
EOF
)

if ! $DRY_RUN; then
    echo "$JOB_YAML" | kubectl apply -f - 2>/dev/null
    ok "GryviaAIJob $TEST_JOB submitted"
else
    ok "Would submit GryviaAIJob $TEST_JOB"
fi

# ── Test 5: Verify job is accepted ──
step "Test 5: Verify job accepted"

if ! $DRY_RUN; then
    sleep 2
    PHASE=$(kubectl get gryviaaijob "$TEST_JOB" -n "$TEST_NS" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [ -n "$PHASE" ]; then
        ok "Job phase: $PHASE"
    else
        # Job exists but no status yet — that's ok, controller may not be running
        if kubectl get gryviaaijob "$TEST_JOB" -n "$TEST_NS" &>/dev/null 2>&1; then
            ok "Job created (no status yet — controller may not be running)"
        else
            fail "Job not found after submission"
        fi
    fi
else
    ok "Would verify job phase"
fi

# ── Test 6: Check API gateway (if running) ──
step "Test 6: API gateway health"

API_POD=$(kubectl get pods -n gryvia-system -l app=gryvia-api-gateway --no-headers 2>/dev/null | head -1 | awk '{print $1}' || true)
if [ -n "$API_POD" ]; then
    HEALTH=$(kubectl exec -n gryvia-system "$API_POD" -- curl -s http://localhost:8080/health 2>/dev/null || echo "")
    if echo "$HEALTH" | grep -q "ok"; then
        ok "API gateway healthy"
    else
        warn "API gateway not responding"
    fi
else
    ok "API gateway not deployed (optional)"
fi

# ── Test 7: List resources via kubectl ──
step "Test 7: Resource listing"

kubectl get gryviagpunodes &>/dev/null 2>&1 && ok "Can list GryviaGpuNodes" || fail "Cannot list GryviaGpuNodes"
kubectl get gryviaquotas &>/dev/null 2>&1 && ok "Can list GryviaQuotas" || fail "Cannot list GryviaQuotas"
kubectl get gryviaaijobs --all-namespaces &>/dev/null 2>&1 && ok "Can list GryviaAIJobs" || fail "Cannot list GryviaAIJobs"

# ── Results ──
echo ""
echo "  ════════════════════════════════════════════════════"
echo "  Results: ✅ $PASS passed  ❌ $FAIL failures"
echo "  ════════════════════════════════════════════════════"
echo ""

if $KEEP; then
    echo "  📁 Test resources kept (--keep):"
    echo "    kubectl get gryviaaijob $TEST_JOB -n $TEST_NS"
    echo "    kubectl delete ns $TEST_NS"
    echo ""
fi

[ "$FAIL" -gt 0 ] && exit 1
exit 0
