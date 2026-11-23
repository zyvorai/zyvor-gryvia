#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Install Setura (the Gateway API controller and built-in proxy behind ui.gateway) and the `setura` GatewayClass,
# but only when the cluster has no Setura controller yet. Re-running is a no-op. Additive: it creates the
# setura-system namespace and one controller Deployment, and the standard Gateway API CRDs if they are missing.
# Needs the `setura` CLI on PATH (https://github.com/zyvorai/zyvor-setura-gateway/releases).
#
#   ./scripts/install-setura-gateway.sh          # install if missing
#   SETURA_IMAGE=ghcr.io/zyvorai/zyvor-setura-gateway:0.7.0 ./scripts/install-setura-gateway.sh
set -euo pipefail
IMAGE="${SETURA_IMAGE:-ghcr.io/zyvorai/zyvor-setura-gateway:0.7.0}"
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.4.0}"

for tool in kubectl setura; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required but not installed" >&2; exit 1; }
done

if kubectl get gatewayclass -o jsonpath='{.items[*].spec.controllerName}' 2>/dev/null | grep -q 'zyvor.dev/setura-gateway'; then
  echo "Setura is already installed in this cluster, leaving it as is."
  exit 0
fi

if ! kubectl get crd gateways.gateway.networking.k8s.io >/dev/null 2>&1; then
  echo "Installing the Gateway API ${GATEWAY_API_VERSION} standard CRDs ..."
  kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"
fi

echo "Installing Setura (${IMAGE}) ..."
setura gateway install --image "${IMAGE}" --namespace setura-system | kubectl apply -f -
kubectl -n setura-system rollout status deploy/setura-gateway --timeout=180s
kubectl wait --for=condition=Accepted gatewayclass/setura --timeout=60s
