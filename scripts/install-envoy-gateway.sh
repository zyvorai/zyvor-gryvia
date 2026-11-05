#!/usr/bin/env bash
# Install Envoy Gateway (the Gateway API controller behind ui.gateway) and the `eg` GatewayClass, but only
# when the cluster has no Envoy Gateway controller yet. Re-running is a no-op. Additive: it creates the
# envoy-gateway-system namespace, the Gateway API + Envoy Gateway CRDs and one controller Deployment.
#
#   ./scripts/install-envoy-gateway.sh          # install if missing
#   ENVOY_GATEWAY_VERSION=v1.9.2 ./scripts/install-envoy-gateway.sh
set -euo pipefail
VERSION="${ENVOY_GATEWAY_VERSION:-v1.9.2}"

for tool in kubectl helm; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required but not installed" >&2; exit 1; }
done

if kubectl get gatewayclass -o jsonpath='{.items[*].spec.controllerName}' 2>/dev/null | grep -q 'gateway.envoyproxy.io'; then
  echo "Envoy Gateway is already installed in this cluster, leaving it as is."
  exit 0
fi

echo "Installing Envoy Gateway ${VERSION} ..."
helm upgrade --install eg oci://docker.io/envoyproxy/gateway-helm --version "${VERSION}" \
  -n envoy-gateway-system --create-namespace
kubectl -n envoy-gateway-system rollout status deploy/envoy-gateway --timeout=180s
kubectl apply -f - <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: eg
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
EOF
kubectl wait --for=condition=Accepted gatewayclass/eg --timeout=60s
