#!/usr/bin/env bash
# Try Gryvia on a laptop, no GPUs needed: creates a kind cluster, installs Gryvia and loads demo data
# (fictional GPU nodes, a quota and a job).
#
#   ./scripts/kind-demo.sh                 # published release images
#   GRYVIA_LOCAL_IMAGES=1 ./scripts/kind-demo.sh   # build images from this checkout (needs docker)
#   ./scripts/kind-demo.sh --delete        # remove the cluster
#
# The dashboard is port-forwarded in the background and opened in the browser (GRYVIA_DEMO_NO_BROWSER=1 to skip).
# With GRYVIA_LOCAL_IMAGES=1 the workspace proxy is also enabled and a small CPU Jupyter workspace is created, opened
# from the dashboard's Workspaces page.
#
# Requires: kind, kubectl, helm (and docker when building local images).
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER="${GRYVIA_KIND_CLUSTER:-gryvia-demo}"
PORT="${GRYVIA_DEMO_PORT:-8443}"
FORWARD="port-forward svc/gryvia-ui ${PORT}:443"

stop_forward() { pkill -f "kubectl -n gryvia-system $FORWARD" 2>/dev/null || true; }

if [[ "${1:-}" == "--delete" ]]; then stop_forward; kind delete cluster --name "$CLUSTER"; exit 0; fi

for tool in kind kubectl helm; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required but not installed" >&2; exit 1; }
done

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "Reusing kind cluster $CLUSTER"
else
  echo "Creating kind cluster $CLUSTER ..."
  kind create cluster --name "$CLUSTER" --wait 120s
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

extra=()
if [[ "${GRYVIA_LOCAL_IMAGES:-}" == "1" ]]; then
  command -v docker >/dev/null 2>&1 || { echo "docker is required for GRYVIA_LOCAL_IMAGES=1" >&2; exit 1; }
  TAG="dev"
  for spec in "gpu-operator:operators/gpu-operator:operators/gpu-operator/Dockerfile" \
              "ai-operator:operators/ai-operator:operators/ai-operator/Dockerfile" \
              "quota-operator:operators/quota-operator:operators/quota-operator/Dockerfile" \
              "api-gateway:services/api-gateway:services/api-gateway/Dockerfile" \
              "ui:.:docker/Dockerfile.ui"; do
    IFS=: read -r name ctx df <<<"$spec"
    echo "Building gryvia-$name ..."
    docker build -f "$df" -t "ghcr.io/zyvorai/gryvia-$name:$TAG" "$ctx"
    kind load docker-image --name "$CLUSTER" "ghcr.io/zyvorai/gryvia-$name:$TAG"
  done
  extra=(--set "global.imageTag=$TAG" --set global.imagePullPolicy=Never)
fi
# Published release images predate the workspace proxy, and CI (which reuses this script) keeps its install unchanged.
workspace_demo=""
if [[ "${GRYVIA_LOCAL_IMAGES:-}" == "1" && -z "${CI:-}" ]]; then
  workspace_demo=1
  extra+=(--set workspaceProxy.enabled=true --set "workspaceProxy.publicURL=https://localhost:${PORT}")
fi

echo "Installing Gryvia ..."
kubectl create namespace gryvia-system --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# The GPU add-ons need real NVIDIA GPUs, so they are off in the demo.
# The chart bundles NVIDIA's GPU Operator and Kueue as optional dependencies; fetch them (both stay disabled here).
# Kueue is an oci:// dependency: it needs no `helm repo add`.
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia --force-update >/dev/null
helm dependency build ./helm/gryvia >/dev/null
helm upgrade --install gryvia ./helm/gryvia --namespace gryvia-system --set namespace.create=false \
  --set nvidiaDevicePlugin.enabled=false --set dcgmExporter.enabled=false --set monitoring.enabled=false \
  "${extra[@]}" --wait --timeout 300s

echo "Loading demo data ..."
kubectl apply -f examples/demo/demo.yaml
if [[ -n "$workspace_demo" ]]; then
  kubectl apply -f examples/demo/workspace.yaml
fi

URL="https://localhost:${PORT}"
if [[ -n "${CI:-}" ]]; then
  echo "Gryvia demo is installed; open it with: kubectl -n gryvia-system $FORWARD"
  exit 0
fi
stop_forward
LOG="${TMPDIR:-/tmp}/gryvia-demo-port-forward.log"
nohup kubectl -n gryvia-system port-forward svc/gryvia-ui "${PORT}:443" >"$LOG" 2>&1 &
disown
for _ in $(seq 1 30); do
  curl -sk -o /dev/null "$URL" && break
  sleep 1
done

echo
echo "Gryvia demo is ready at $URL (accept the self-signed certificate) and sign in as admin / Admin@321."
echo "The dashboard is port-forwarded in the background (log: $LOG); stop it with: pkill -f '$FORWARD'"
if [[ -n "$workspace_demo" ]]; then
  echo "Workspaces > demo-notebook opens JupyterLab through the gateway once its image is pulled."
fi
echo "Try a fractional-GPU job spec without a GPU: gryvia submit --image pytorch/pytorch --gpu 0.5 --dry-run -- python train.py"
echo "The GPU nodes are fictional demo data. Remove everything with: ./scripts/kind-demo.sh --delete"

if [[ -z "${GRYVIA_DEMO_NO_BROWSER:-}" ]]; then
  if command -v open >/dev/null 2>&1; then open "$URL" || true
  elif command -v xdg-open >/dev/null 2>&1; then xdg-open "$URL" >/dev/null 2>&1 || true
  fi
fi
