#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Install Gryvia from this checkout's Helm chart and apply the example manifest.
#
#   ./deploy.sh                         # install, wait, then apply production-deployment.yaml
#   SKIP_EXAMPLES=1 ./deploy.sh         # install only
#   GRYVIA_API_KEY=... ./deploy.sh      # sign-in key (default: the well-known lab key Admin@321)
#
# This runs scripts/install.sh with GRYVIA_CHART=./helm/gryvia (helm install of the chart with
# namespace.create=false and --create-namespace, waiting for the workloads), then waits for the gateway and
# UI and applies production-deployment.yaml. That manifest holds example endpoints, hardware and capacities and
# <REPLACE_WITH_*> credential placeholders: edit it first, or set SKIP_EXAMPLES=1. Not run on real GPU, VAST or
# InfiniBand hardware.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
NS="${GRYVIA_NAMESPACE:-gryvia-system}"

for tool in kubectl helm; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required but not installed" >&2; exit 1; }
done

echo "Installing Gryvia from $ROOT/helm/gryvia into namespace $NS ..."
GRYVIA_CHART="$ROOT/helm/gryvia" GRYVIA_NAMESPACE="$NS" "$ROOT/scripts/install.sh"

echo "Waiting for the gateway and UI ..."
kubectl -n "$NS" wait --for=condition=available --timeout=300s deployment/gryvia-api-gateway deployment/gryvia-ui

if [[ "${SKIP_EXAMPLES:-}" == "1" ]]; then
  echo "SKIP_EXAMPLES=1: not applying the example manifest."
  exit 0
fi
if grep -q '<REPLACE_WITH_' "$HERE/production-deployment.yaml"; then
  echo "production-deployment.yaml still contains <REPLACE_WITH_*> placeholders; edit it, then run:" >&2
  echo "  kubectl apply -f $HERE/production-deployment.yaml" >&2
  exit 1
fi
kubectl apply -f "$HERE/production-deployment.yaml"
echo "Applied production-deployment.yaml. Check: kubectl get gryviaaijobs -n default"
