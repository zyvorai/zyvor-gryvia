#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Regression: terminal pods retained for logs must not fail the eviction check.
set -euo pipefail
cd "$(dirname "$0")/../.."
source <(sed '/^case /,$d' scripts/e2e-kueue.sh)
kubectl() { printf '%s\n' "$PODS_FIXTURE"; }
PODS_FIXTURE='{"items":[{"status":{"phase":"Failed"}},{"status":{"phase":"Succeeded"}}]}'
no_active_pods low || fail "terminal retained pods counted as active"
PODS_FIXTURE='{"items":[{"status":{"phase":"Running"}}]}'
if no_active_pods low; then fail "running pod counted as released"; fi
PODS_FIXTURE='{"items":[{"status":{"phase":"Pending"}}]}'
if no_active_pods low; then fail "pending pod counted as released"; fi
PODS_FIXTURE='{"items":[]}'
no_active_pods low || fail "empty pod list counted as active"
echo '4 pod lifecycle assertions passed'
