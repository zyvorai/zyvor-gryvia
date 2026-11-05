#!/usr/bin/env bash
# Install Gryvia from the published Helm chart into the current kubectl context.
#
#   ./scripts/install.sh                         # release chart from ghcr.io
#   GRYVIA_VERSION=0.1.0 ./scripts/install.sh    # a specific version
#   GRYVIA_CHART=./helm/gryvia ./scripts/install.sh   # a local chart (development)
#
# Environment:
#   GRYVIA_VERSION    chart version (default: latest)
#   GRYVIA_CHART      chart reference (default: oci://ghcr.io/zyvorai/charts/gryvia)
#   GRYVIA_NAMESPACE  namespace (default: gryvia-system)
#   GRYVIA_API_KEY    sign-in key; the dashboard login is admin / <key>
#                     (default: the well-known lab key Admin@321 - change it on shared clusters)
#   GRYVIA_NODEPORT   expose the dashboard on this NodePort (default: ClusterIP; use port-forward)
#   GRYVIA_NVIDIA     1 = also install NVIDIA's GPU Operator (driver, container toolkit, device plugin,
#                     DCGM) so GPU nodes get ready without manual setup: sets nvidia.enabled=true
#   GRYVIA_HOST_DRIVER 1 = the nodes already have the NVIDIA driver (sets nvidia.driver.enabled=false)
#   GRYVIA_GATEWAY_HOST  serve the dashboard through Envoy Gateway at this hostname (ui.gateway.*): installs Envoy
#                     Gateway first only if the cluster has none (scripts/install-envoy-gateway.sh) and creates
#                     the gryvia-ui-ca ConfigMap Envoy uses to verify the dashboard's certificate
#   GRYVIA_GATEWAY_TLS_SECRET  kubernetes.io/tls Secret for the HTTPS listener (port ui.gateway.httpsPort, 7443)
#   HELM_EXTRA_ARGS   extra arguments passed to `helm upgrade --install`
set -euo pipefail

NS="${GRYVIA_NAMESPACE:-gryvia-system}"
CHART="${GRYVIA_CHART:-oci://ghcr.io/zyvorai/charts/gryvia}"
KEY="${GRYVIA_API_KEY:-Admin@321}"

for tool in kubectl helm; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required but not installed" >&2; exit 1; }
done
kubectl version --request-timeout=10s >/dev/null 2>&1 || { echo "cannot reach a cluster with kubectl (current context: $(kubectl config current-context 2>&1))" >&2; exit 1; }

args=(upgrade --install gryvia "$CHART" --namespace "$NS" --create-namespace --set namespace.create=false --set "auth.apiKey=$KEY" --wait --timeout 300s)
[[ -n "${GRYVIA_VERSION:-}" ]] && args+=(--version "$GRYVIA_VERSION")
if [[ "${GRYVIA_NVIDIA:-}" == "1" ]]; then args+=(--set nvidia.enabled=true); fi
if [[ "${GRYVIA_HOST_DRIVER:-}" == "1" ]]; then args+=(--set nvidia.driver.enabled=false); fi
# A chart from a checkout needs its dependencies (NVIDIA's GPU Operator and Kueue, both disabled by default; Kueue is an
# oci:// dependency and needs no `helm repo add`) downloaded first.
if [[ -d "$CHART" ]]; then
  helm repo add nvidia https://helm.ngc.nvidia.com/nvidia --force-update >/dev/null
  helm dependency build "$CHART" >/dev/null
fi
if [[ -n "${GRYVIA_GATEWAY_HOST:-}" ]]; then
  "$(dirname "${BASH_SOURCE[0]}")/install-envoy-gateway.sh"
  args+=(--set ui.gateway.enabled=true --set "ui.gateway.host=$GRYVIA_GATEWAY_HOST" --set ui.gateway.backendTLS.caConfigMapName=gryvia-ui-ca)
  [[ -n "${GRYVIA_GATEWAY_TLS_SECRET:-}" ]] && args+=(--set "ui.gateway.tlsSecretName=$GRYVIA_GATEWAY_TLS_SECRET")
fi
if [[ -n "${GRYVIA_NODEPORT:-}" ]]; then args+=(--set ui.service.type=NodePort --set "ui.service.nodePort=$GRYVIA_NODEPORT"); fi
# shellcheck disable=SC2206
[[ -n "${HELM_EXTRA_ARGS:-}" ]] && args+=(${HELM_EXTRA_ARGS})

echo "Installing Gryvia into namespace $NS ..."
kubectl get namespace "$NS" >/dev/null 2>&1 || kubectl create namespace "$NS"
helm "${args[@]}"
if [[ -n "${GRYVIA_GATEWAY_HOST:-}" ]]; then
  # The chart's dashboard certificate (secret gryvia-tls) is what Envoy must trust on the backend hop.
  kubectl -n "$NS" get secret gryvia-tls -o jsonpath='{.data.tls\.crt}' | base64 -d > "${TMPDIR:-/tmp}/gryvia-ui-ca.crt"
  kubectl -n "$NS" create configmap gryvia-ui-ca --from-file=ca.crt="${TMPDIR:-/tmp}/gryvia-ui-ca.crt" --dry-run=client -o yaml | kubectl apply -f -
  rm -f "${TMPDIR:-/tmp}/gryvia-ui-ca.crt"
fi

echo
echo "Gryvia is installed."
if [[ -n "${GRYVIA_NODEPORT:-}" ]]; then
  echo "Dashboard: https://<node-ip>:${GRYVIA_NODEPORT}"
else
  echo "Dashboard: kubectl -n $NS port-forward svc/gryvia-ui 8443:443   then open https://localhost:8443"
fi
echo "Sign in as: admin / $KEY   (self-signed certificate: accept the browser warning once)"
if [[ "$KEY" == "Admin@321" ]]; then
  echo "WARNING: this is the default lab credential. Set GRYVIA_API_KEY for anything reachable from an untrusted network."
fi
