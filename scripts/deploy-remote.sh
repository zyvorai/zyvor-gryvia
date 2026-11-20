#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Gryvia — remote deploy (SSH + rsync + on-host image build + Helm), modeled on
# netra's scripts/deploy-remote.sh.
#
# Deploys to a single-node k3s (or any cluster the remote user can reach):
#   1. rsync the repo to ~/.deployments/gryvia on the host
#   2. build the gpu-operator, ai-operator, api-gateway and ui images with podman
#      and import them into k3s's containerd
#   3. install the CRDs, then the gryvia Helm chart (operators, API gateway, dashboard)
#   4. deploy the API gateway and web UI (both HTTPS, self-signed certificates minted by
#      an init container in each pod), expose the UI on NodePort 32443
#   5. wait for every rollout, then smoke-test the UI, the API and a custom resource
#
# Usage:
#   ./scripts/deploy-remote.sh user@10.0.1.5
#   ./scripts/deploy-remote.sh 10.0.1.5 user
#   ./scripts/deploy-remote.sh user@10.0.1.5 --quick        # skip image builds
#   ./scripts/deploy-remote.sh user@10.0.1.5 --verify-only  # only run the checks
#   ./scripts/deploy-remote.sh user@10.0.1.5 --uninstall
#   ./scripts/deploy-remote.sh user@10.0.1.5 --dry-run      # print the remote script
#
# Environment:
#   GRYVIA_API_KEY          Gateway bearer / dashboard password. Unset: the installed key is kept (the
#                           gryvia-api-key Secret, then ~/.gryvia/api-key on the host), and a first install
#                           uses the lab default Admin@321, so set your own for anything reachable from an
#                           untrusted network. The key in use is saved to ~/.gryvia/api-key (mode 600).
#   GRYVIA_REMOTE_SUBDIR    checkout dir relative to the remote $HOME
#                           (default: .deployments/gryvia)
#   GRYVIA_NETRA_URL        Netra controller for real network flows, e.g. https://<netra-public-ip>:30870
#   GRYVIA_NETRA_TOKEN_SECRET  Secret (in gryvia-system, key `token`) with a Netra API token
#   GRYVIA_NETRA_INSECURE   1 = accept Netra's self-signed certificate
#   GRYVIA_ZYNTRA_URL       Zyntra for the Sovereign AIOS overview, e.g. http://zyntra.zyntra.svc:8080
#   GRYVIA_ZYNTRA_TOKEN_SECRET  Secret (in gryvia-system, key `token`) with a Zyntra viewer token
#   GRYVIA_ZYNTRA_CONSOLE_URL, GRYVIA_NETRA_CONSOLE_URL  console links on the overview page
#   GRYVIA_UI_NODEPORT      NodePort for the web UI (default: 32443)
#   GRYVIA_DEPLOY_*         see scripts/lib/deploy-guards.sh (disk guard, timeouts)
#
# The kubeconfig is read from k3s with sudo into ~/.kube/gryvia-k3s.yaml and used
# only by this script; no shell startup files or cluster settings are changed.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

MODE="deploy"
DRY_RUN=false
POSITIONAL=()
SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ServerAliveInterval=30)

usage() { sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help) usage ;;
    --quick) MODE="quick"; shift ;;
    --verify-only) MODE="verify"; shift ;;
    --uninstall) MODE="uninstall"; shift ;;
    --dry-run) DRY_RUN=true; shift ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *) POSITIONAL+=("$1"); shift ;;
  esac
done

TARGET=""
if [[ ${#POSITIONAL[@]} -eq 1 ]]; then
  TARGET="${POSITIONAL[0]}"
elif [[ ${#POSITIONAL[@]} -eq 2 ]]; then
  # HOST USER, or user@host followed by anything
  if [[ "${POSITIONAL[0]}" == *@* ]]; then
    TARGET="${POSITIONAL[0]}"
  elif [[ "${POSITIONAL[1]}" == *@* ]]; then
    TARGET="${POSITIONAL[1]}"
  else
    TARGET="${POSITIONAL[1]}@${POSITIONAL[0]}"
  fi
else
  echo "usage: $0 user@host [--quick|--verify-only|--uninstall|--dry-run]" >&2
  echo "   or: $0 HOST USER [...]" >&2
  exit 2
fi

VERSION="$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "${ROOT}/helm/gryvia/Chart.yaml")"
[[ -n "$VERSION" ]] || { echo "cannot read appVersion from helm/gryvia/Chart.yaml" >&2; exit 1; }
UI_NODEPORT="${GRYVIA_UI_NODEPORT:-32443}"
TARGET_HOST="${TARGET#*@}"
if [[ "$TARGET_HOST" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then TARGET_SAN="IP:${TARGET_HOST}"; else TARGET_SAN="DNS:${TARGET_HOST}"; fi
API_KEY_LOCAL="${GRYVIA_API_KEY:-}"
REMOTE_SUBDIR="${GRYVIA_REMOTE_SUBDIR:-.deployments/gryvia}"

log() { printf '[gryvia-deploy] %s\n' "$*"; }
ssh_host() { ssh "${SSH_OPTS[@]}" "$TARGET" "$@"; }

# Kubeconfig for this script only, and PATH. Runs on the host.
preamble() {
  cat <<'EOF'
mkdir -p "$HOME/.kube"
if [[ ! -r "$HOME/.kube/gryvia-k3s.yaml" ]] || [[ /etc/rancher/k3s/k3s.yaml -nt "$HOME/.kube/gryvia-k3s.yaml" ]]; then
  sudo cat /etc/rancher/k3s/k3s.yaml > "$HOME/.kube/gryvia-k3s.yaml"
  chmod 600 "$HOME/.kube/gryvia-k3s.yaml"
fi
export KUBECONFIG="$HOME/.kube/gryvia-k3s.yaml"
export PATH="/usr/local/bin:/usr/bin:$PATH"
EOF
}

if [[ "$MODE" == "uninstall" ]]; then
  remote_script=$(cat <<EOF
set -uo pipefail
$(preamble)
helm -n gryvia-system uninstall gryvia 2>/dev/null || true
helm -n gryvia-system uninstall gryvia-core 2>/dev/null || true
kubectl -n gryvia-system delete deployment gryvia-api-gateway gryvia-ui --ignore-not-found
kubectl -n gryvia-system delete service gryvia-api-gateway gryvia-ui --ignore-not-found
kubectl delete clusterrole,clusterrolebinding gryvia-api-gateway gryvia-ui --ignore-not-found
kubectl delete namespace gryvia-system --ignore-not-found
echo "Gryvia removed. CRDs and ~/${REMOTE_SUBDIR} are kept; delete them by hand to purge data."
EOF
)
  if $DRY_RUN; then echo "$remote_script"; exit 0; fi
  ssh_host 'bash -s' <<<"$remote_script"
  exit 0
fi

# Smoke test, run on the host against the NodePort and the cluster. The certificate is
# self-signed, so curl runs with -k.
smoke_script=$(cat <<EOF
set -uo pipefail
$(preamble)
KEY="\$(cat "\$HOME/.gryvia/api-key" 2>/dev/null || true)"
BASE="https://127.0.0.1:${UI_NODEPORT}"
fail=0
check() { # check <name> <command...>
  local name="\$1"; shift
  if "\$@" >/dev/null 2>&1; then echo "  PASS  \$name"; else echo "  FAIL  \$name"; fail=1; fi
}
authed() { curl -sfk -H "Authorization: Bearer \$KEY" "\$@"; }
login_ok() { curl -sfk -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"\$KEY\"}" "\$BASE/api/auth/login" | grep -q '"token"'; }
login_rejected() { local c; c="\$(curl -sk -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d '{"username":"admin","password":"definitely-wrong"}' "\$BASE/api/auth/login")"; [[ "\$c" == 401 ]]; }
unauth_rejected() { local c; c="\$(curl -sk -o /dev/null -w '%{http_code}' "\$BASE/api/cluster/stats")"; [[ "\$c" == 401 || "\$c" == 403 ]]; }
echo "Gryvia smoke test"
check "namespace exists" kubectl get ns gryvia-system
check "CRD gryviaaijobs.gryvia.io established" kubectl wait --for=condition=Established crd/gryviaaijobs.gryvia.io --timeout=30s
for d in gryvia-gpu-operator gryvia-ai-operator gryvia-quota-operator gryvia-api-gateway gryvia-ui; do
  check "deployment \$d available" kubectl -n gryvia-system wait --for=condition=Available deployment/\$d --timeout=60s
done
check "UI serves index" curl -sfk "\$BASE/"
check "API rejects a request without a key" unauth_rejected
check "API accepts the key (cluster stats)" authed "\$BASE/api/cluster/stats"
check "login accepts admin and the key" login_ok
check "login rejects a wrong password" login_rejected
check "auth/me identifies admin" bash -c "curl -sfk -H 'Authorization: Bearer \$KEY' '\$BASE/api/auth/me' | grep -q '\\"name\\": *\\"admin\\"'"
bad_bearer_rejected() { local c; c="\$(curl -sk -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer not-the-key' "\$BASE/api/auth/me")"; [[ "\$c" == 401 || "\$c" == 403 ]]; }
check "auth/me rejects a wrong bearer" bad_bearer_rejected
check "API lists jobs" authed "\$BASE/api/jobs"
check "API lists nodes" authed "\$BASE/api/nodes"
# Every read endpoint the dashboard calls must answer 200 with JSON.
for path in cluster/stats jobs quotas nodes metrics/gpu metrics/costs \\
    network/flows network/policies network/insights network/graph network/anomalies network/costs network/traces \\
    security/alerts security/policies ai/training/insight ai/training/nccl gpu/memory \\
    workspaces models inference workflows tuners sovereign; do
  check "GET /api/\$path" bash -c "curl -sfk -H 'Authorization: Bearer \$KEY' '\$BASE/api/\$path' | python3 -c 'import sys,json; json.load(sys.stdin)'"
done
# Custom resource: accepted by the API server and visible through the Gryvia API.
kubectl delete gryviaquota gryvia-smoke --ignore-not-found >/dev/null 2>&1 || true
if kubectl apply -f "\$HOME/${REMOTE_SUBDIR}/scripts/lib/smoke-quota.yaml" >/dev/null 2>&1; then
  check "GryviaQuota visible through the API" bash -c "curl -sfk -H 'Authorization: Bearer \$KEY' '\$BASE/api/quotas' | grep -q gryvia-smoke"
  kubectl delete gryviaquota gryvia-smoke --ignore-not-found >/dev/null 2>&1 || true
else
  echo "  FAIL  apply GryviaQuota"; fail=1
fi
echo
kubectl -n gryvia-system get pods
if [[ \$fail -eq 0 ]]; then echo "ALL CHECKS PASSED"; else echo "SOME CHECKS FAILED"; fi
exit \$fail
EOF
)

if [[ "$MODE" == "verify" ]]; then
  if $DRY_RUN; then echo "$smoke_script"; exit 0; fi
  ssh_host 'bash -s' <<<"$smoke_script"
  exit $?
fi

REMOTE_HOME="$(ssh_host 'printf %s "$HOME"')"
REMOTE_DIR="${REMOTE_HOME}/${REMOTE_SUBDIR}"

log "sync → ${TARGET}:${REMOTE_DIR}"
if ! $DRY_RUN; then
  ssh_host "mkdir -p ${REMOTE_DIR}"
  rsync -az --delete -e "ssh ${SSH_OPTS[*]}" \
    --exclude '.git' --exclude 'node_modules' --exclude 'dist' --exclude 'target' \
    --exclude 'build' --exclude '.docusaurus' --exclude '.DS_Store' --exclude 'bin' \
    "${ROOT}/" "${TARGET}:${REMOTE_DIR}/"
fi

remote_script=$(cat <<EOF
set -euo pipefail
cd ${REMOTE_DIR}
$(preamble)
MODE="${MODE}"
VERSION="${VERSION}"
UI_NODEPORT="${UI_NODEPORT}"
REG="ghcr.io/zyvorai"

source scripts/lib/deploy-guards.sh
deploy_disk_guard /

# API key (dashboard password for user "admin"): explicit GRYVIA_API_KEY, else the key already
# installed (so a redeploy does not reset a rotated key), else the lab default on a first install.
deploy_resolve_api_key $(printf '%q' "${API_KEY_LOCAL}")
echo "API key source: \$API_KEY_SOURCE"

if command -v podman >/dev/null 2>&1; then RT=podman
elif command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then RT=docker
else RT=""; fi

# name:build context:Dockerfile
IMAGES=(
  "gpu-operator:operators/gpu-operator:operators/gpu-operator/Dockerfile"
  "ai-operator:operators/ai-operator:operators/ai-operator/Dockerfile"
  "quota-operator:operators/quota-operator:operators/quota-operator/Dockerfile"
  "api-gateway:services/api-gateway:services/api-gateway/Dockerfile"
  "ui:.:docker/Dockerfile.ui"
)

if [[ "\$MODE" != "quick" ]]; then
  [[ -n "\$RT" ]] || { echo "podman or a working docker is required to build images" >&2; exit 1; }
  for spec in "\${IMAGES[@]}"; do
    IFS=: read -r name ctx df <<<"\$spec"
    echo "Building \$REG/gryvia-\$name:\$VERSION ..."
    \$RT build -f "\$df" -t "\$REG/gryvia-\$name:\$VERSION" "\$ctx"
  done
  # Build all, then import all: an imported image no pod uses yet is what kubelet's
  # image GC collects, and each build pushes the disk toward its threshold.
  for spec in "\${IMAGES[@]}"; do
    IFS=: read -r name _ _ <<<"\$spec"
    deploy_import_image "\$REG/gryvia-\$name:\$VERSION"
  done
fi

echo "Installing CRDs ..."
# A CRD's scope cannot be changed in place. If the installed scope differs from the
# generated one, recreate the CRD, but only when it has no objects (deleting a CRD
# deletes them); otherwise stop and say so.
for f in crds/*.yaml; do
  crd="\$(sed -n 's/^  name: \\(.*\\.gryvia\\.io\\)\$/\\1/p' "\$f" | head -1)"
  want="\$(sed -n 's/^  scope: //p' "\$f" | head -1)"
  have="\$(kubectl get crd "\$crd" -o jsonpath='{.spec.scope}' 2>/dev/null || true)"
  # The API moved from v1 to v1alpha1. A CRD that still stores v1 cannot be updated in place, so recreate
  # it, but only when it has no objects (deleting a CRD deletes them).
  wantv="\$(grep -m1 -E '^ +(- )?name: v1' "\$f" | sed 's/.*name: //' || true)"
  havev="\$(kubectl get crd "\$crd" -o jsonpath='{.status.storedVersions[*]}' 2>/dev/null || true)"
  if [[ -n "\$havev" && " \$havev " != *" \$wantv "* ]]; then
    n="\$(kubectl get "\$crd" -A --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    if [[ "\$n" != "0" ]]; then
      echo "CRD \$crd stores version \$havev but the API is now \$wantv and it has \$n object(s); export them (kubectl get \$crd -A -o yaml), change apiVersion to gryvia.io/\$wantv, delete them and the CRD, then redeploy" >&2
      exit 1
    fi
    echo "Recreating \$crd (version \$havev -> \$wantv, no objects)"
    kubectl delete crd "\$crd"
    have=""
  fi
  if [[ -n "\$have" && "\$have" != "\$want" ]]; then
    n="\$(kubectl get "\$crd" -A --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    if [[ "\$n" != "0" ]]; then
      echo "CRD \$crd is \$have-scoped but should be \$want and has \$n object(s); back them up, delete them and the CRD, then redeploy" >&2
      exit 1
    fi
    echo "Recreating \$crd (scope \$have -> \$want, no objects)"
    kubectl delete crd "\$crd"
  fi
done
# The kinds were renamed Fabric* -> Gryvia*. Remove the legacy CRDs, but only when they hold
# no objects (deleting a CRD deletes them); otherwise leave them and say how to migrate.
for legacy in \$(kubectl get crd -o name 2>/dev/null | sed -n 's#^customresourcedefinition.apiextensions.k8s.io/\\(fabric.*\\.gryvia\\.io\\)\$#\\1#p'); do
  n="\$(kubectl get "\$legacy" -A --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  if [[ "\$n" == "0" ]]; then
    echo "Removing legacy CRD \$legacy (no objects)"
    kubectl delete crd "\$legacy"
  else
    echo "WARNING: legacy CRD \$legacy still has \$n object(s); re-create them under the new Gryvia* kind and delete the CRD by hand" >&2
  fi
done
kubectl apply --server-side --force-conflicts -f crds/

echo "Installing gryvia (operators, API gateway, dashboard) ..."
# Earlier deployments used a gryvia-core release plus kubectl-applied gateway/UI manifests.
# Remove those so the single gryvia release owns everything; adopt the TLS and API-key Secrets.
if helm -n gryvia-system status gryvia-core >/dev/null 2>&1; then
  echo "Removing legacy gryvia-core release"
  helm -n gryvia-system uninstall gryvia-core
fi
managed() { [[ "\$(kubectl -n gryvia-system get "\$1" "\$2" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}' 2>/dev/null)" == "Helm" ]]; }
for res in deployment/gryvia-api-gateway deployment/gryvia-ui service/gryvia-api-gateway service/gryvia-ui \\
           serviceaccount/gryvia-api-gateway serviceaccount/gryvia-ui; do
  if kubectl -n gryvia-system get "\$res" >/dev/null 2>&1 && ! managed "\${res%%/*}" "\${res##*/}"; then
    echo "Replacing unmanaged \$res"
    kubectl -n gryvia-system delete "\$res"
  fi
done
for res in clusterrole/gryvia-api-gateway clusterrolebinding/gryvia-api-gateway clusterrole/gryvia-ui clusterrolebinding/gryvia-ui; do
  if kubectl get "\$res" >/dev/null 2>&1 && [[ "\$(kubectl get "\$res" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}')" != "Helm" ]]; then
    kubectl delete "\$res"
  fi
done
for sec in gryvia-tls gryvia-api-key; do
  if kubectl -n gryvia-system get secret "\$sec" >/dev/null 2>&1 && ! managed secret "\$sec"; then
    kubectl -n gryvia-system label secret "\$sec" app.kubernetes.io/managed-by=Helm --overwrite
    kubectl -n gryvia-system annotate secret "\$sec" meta.helm.sh/release-name=gryvia meta.helm.sh/release-namespace=gryvia-system --overwrite
  fi
done
HOST_IP="\$(hostname -I | awk '{print \$1}')"
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia --force-update >/dev/null
helm dependency build ./helm/gryvia >/dev/null
NETRA_ARGS=()
if [[ -n "${GRYVIA_NETRA_URL:-}" ]]; then
  NETRA_ARGS+=(--set "apiGateway.netra.url=${GRYVIA_NETRA_URL:-}")
  [[ -n "${GRYVIA_NETRA_TOKEN_SECRET:-}" ]] && NETRA_ARGS+=(--set "apiGateway.netra.tokenSecret=${GRYVIA_NETRA_TOKEN_SECRET:-}")
  [[ "${GRYVIA_NETRA_INSECURE:-}" == "1" ]] && NETRA_ARGS+=(--set apiGateway.netra.insecureTLS=true)
fi
[[ -n "${GRYVIA_ZYNTRA_URL:-}" ]] && NETRA_ARGS+=(--set "apiGateway.sovereign.zyntraURL=${GRYVIA_ZYNTRA_URL:-}")
[[ -n "${GRYVIA_ZYNTRA_TOKEN_SECRET:-}" ]] && NETRA_ARGS+=(--set "apiGateway.sovereign.zyntraTokenSecret=${GRYVIA_ZYNTRA_TOKEN_SECRET:-}")
[[ -n "${GRYVIA_ZYNTRA_CONSOLE_URL:-}" ]] && NETRA_ARGS+=(--set "apiGateway.sovereign.zyntraConsoleURL=${GRYVIA_ZYNTRA_CONSOLE_URL:-}")
[[ -n "${GRYVIA_NETRA_CONSOLE_URL:-}" ]] && NETRA_ARGS+=(--set "apiGateway.sovereign.netraConsoleURL=${GRYVIA_NETRA_CONSOLE_URL:-}")
helm upgrade --install gryvia ./helm/gryvia \\
  --namespace gryvia-system --create-namespace \\
  --set namespace.create=false \\
  --set monitoring.enabled=false \\
  --set nvidiaDevicePlugin.enabled=false \\
  --set dcgmExporter.enabled=false \\
  --set global.imageRegistry="\$REG" --set global.imageTag="\$VERSION" \\
  --set auth.apiKey="\$API_KEY" \\
  --set ui.service.type=NodePort --set ui.service.nodePort="\$UI_NODEPORT" \\
  --set "tls.extraSANs={\$HOST_IP,${TARGET_HOST}}" \\
  \${NETRA_ARGS[@]+"\${NETRA_ARGS[@]}"} \\
  --wait --timeout 300s

# A fixed tag never changes the pod template, so restart to pick up freshly imported images.
# image-name:deployment:label selector
WORKLOADS=(
  "gpu-operator:gryvia-gpu-operator:app.kubernetes.io/component=gpu-operator"
  "ai-operator:gryvia-ai-operator:app.kubernetes.io/component=ai-operator"
  "quota-operator:gryvia-quota-operator:app.kubernetes.io/component=quota-operator"
  "api-gateway:gryvia-api-gateway:app=gryvia-api-gateway"
  "ui:gryvia-ui:app=gryvia-ui"
)
for spec in "\${WORKLOADS[@]}"; do
  IFS=: read -r img dep sel <<<"\$spec"
  ref="\$REG/gryvia-\$img:\$VERSION"
  deploy_ensure_image "\$ref"
  kubectl -n gryvia-system rollout restart deployment/\$dep
  deploy_wait_ready deployment/\$dep "\$sel" "\$ref"
done

echo "GRYVIA_UI=https://\$(hostname -I | awk '{print \$1}'):\$UI_NODEPORT"
if [[ "\$API_KEY" == "Admin@321" ]]; then
  echo "Sign in as: admin / \$API_KEY  (the lab default; saved in ~/.gryvia/api-key on the host)"
else
  echo "Sign in as: admin  (key saved in ~/.gryvia/api-key on the host; not printed)"
fi
echo "Read the key: kubectl -n gryvia-system get secret gryvia-api-key -o jsonpath='{.data.GRYVIA_API_KEY}' | base64 -d"
if [[ "\$API_KEY" == "Admin@321" ]]; then
  echo "WARNING: this is the default lab credential. Set GRYVIA_API_KEY for anything reachable from an untrusted network."
fi
EOF
)

if $DRY_RUN; then
  log "dry-run remote script:"
  echo "$remote_script"
  exit 0
fi

ssh_host 'bash -s' <<<"$remote_script"
log "deployed; running smoke test"
ssh_host 'bash -s' <<<"$smoke_script"
