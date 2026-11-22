#!/bin/bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# ============================================================================
# doctor.sh — Gryvia health diagnostic
# ============================================================================
# Checks all components and prerequisites for a working Gryvia installation.
#
# Usage:
#   ./scripts/doctor.sh
#   ./scripts/doctor.sh --verbose
# ============================================================================

set -euo pipefail

VERBOSE=false
[ "${1:-}" = "--verbose" ] || [ "${1:-}" = "-v" ] && VERBOSE=true

PASS=0
WARN=0
FAIL=0

ok()   { echo "  ✅ $*"; ((PASS++)); }
warn() { echo "  ⚠️  $*"; ((WARN++)); }
fail() { echo "  ❌ $*"; ((FAIL++)); }

echo ""
echo "  ╔══════════════════════════════════════════════════╗"
echo "  ║     🩺 Gryvia Doctor                         ║"
echo "  ╚══════════════════════════════════════════════════╝"
echo ""

# ── Prerequisites ──
echo "  ── Prerequisites ──"

if command -v kubectl &>/dev/null; then
    VER=$(kubectl version --client -o json 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin)["clientVersion"]["gitVersion"])' 2>/dev/null || echo 'unknown')
    ok "kubectl: $VER"
else
    fail "kubectl: not found"
fi

if command -v helm &>/dev/null; then
    ok "helm: $(helm version --short 2>/dev/null)"
else
    warn "helm: not found (optional, for Helm-based deployment)"
fi

if command -v go &>/dev/null; then
    ok "go: $(go version 2>/dev/null | awk '{print $3}')"
else
    warn "go: not found (needed for building operators)"
fi

if command -v cargo &>/dev/null; then
    ok "cargo: $(cargo --version 2>/dev/null | awk '{print $2}')"
else
    warn "cargo: not found (needed for building CLI)"
fi

if command -v node &>/dev/null; then
    ok "node: $(node --version 2>/dev/null)"
else
    warn "node: not found (needed for building Web UI)"
fi

if command -v python3 &>/dev/null; then
    ok "python3: $(python3 --version 2>/dev/null | awk '{print $2}')"
else
    warn "python3: not found (needed for API gateway)"
fi

echo ""

# ── Cluster Access ──
echo "  ── Cluster Access ──"

if kubectl cluster-info &>/dev/null 2>&1; then
    NODES=$(kubectl get nodes --no-headers 2>/dev/null | wc -l)
    ok "cluster: reachable ($NODES node(s))"

    # Check for GPU nodes
    GPU_NODES=$(kubectl get nodes -l nvidia.com/gpu.present=true --no-headers 2>/dev/null | wc -l || echo 0)
    if [ "$GPU_NODES" -gt 0 ]; then
        ok "gpu nodes: $GPU_NODES detected"
    else
        warn "gpu nodes: none detected (label nvidia.com/gpu.present=true)"
    fi
else
    fail "cluster: not reachable (check kubeconfig)"
fi

echo ""

# ── CRDs ──
echo "  ── CRDs ──"

for crd in gryviaaijobs gryviagpunodes gryviaquotas gryviastorages gryvianetworks; do
    if kubectl get crd "${crd}.gryvia.io" &>/dev/null 2>&1; then
        ok "${crd}.gryvia.io"
    else
        fail "${crd}.gryvia.io: not installed"
    fi
done

echo ""

# ── Namespace & Pods ──
echo "  ── Namespace & Pods ──"

for ns in gryvia gryvia-system; do
    if kubectl get namespace "$ns" &>/dev/null 2>&1; then
        PODS=$(kubectl get pods -n "$ns" --no-headers 2>/dev/null | wc -l)
        RUNNING=$(kubectl get pods -n "$ns" --no-headers 2>/dev/null | grep -c Running || true)
        if [ "$PODS" -gt 0 ]; then
            ok "$ns: $RUNNING/$PODS pods running"
        else
            warn "$ns: namespace exists but no pods"
        fi
    else
        warn "$ns: namespace not found"
    fi
done

echo ""

# ── Resources ──
echo "  ── Resources ──"

GPU_NODE_COUNT=$(kubectl get gryviagpunodes --no-headers 2>/dev/null | wc -l || echo 0)
JOB_COUNT=$(kubectl get gryviaaijobs --all-namespaces --no-headers 2>/dev/null | wc -l || echo 0)
QUOTA_COUNT=$(kubectl get gryviaquotas --no-headers 2>/dev/null | wc -l || echo 0)

ok "GPU nodes: $GPU_NODE_COUNT registered"
ok "AI jobs: $JOB_COUNT total"
ok "Quotas: $QUOTA_COUNT configured"

if $VERBOSE; then
    echo ""
    echo "  ── Detailed Status ──"

    echo ""
    echo "  GPU Nodes:"
    kubectl get gryviagpunodes -o wide 2>/dev/null || echo "  (none)"

    echo ""
    echo "  Recent Jobs:"
    kubectl get gryviaaijobs --all-namespaces 2>/dev/null | head -10 || echo "  (none)"

    echo ""
    echo "  Quotas:"
    kubectl get gryviaquotas 2>/dev/null || echo "  (none)"
fi

echo ""
echo "  ════════════════════════════════════════════════════"
echo "  Results: ✅ $PASS passed  ⚠️  $WARN warnings  ❌ $FAIL failures"
echo "  ════════════════════════════════════════════════════"
echo ""

[ "$FAIL" -gt 0 ] && exit 1
exit 0
