#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Turn a fresh Ubuntu server into a Gryvia node: k3s, and NVIDIA GPU support when the machine has GPUs.
#
#   sudo ./scripts/install-k3s-gpu.sh server                       # first node: k3s + Gryvia (+ NVIDIA GPU Operator)
#   sudo ./scripts/install-k3s-gpu.sh agent --server https://10.0.0.5:6443 --token <token>   # add a GPU node
#   sudo ./scripts/install-k3s-gpu.sh --uninstall                  # remove Gryvia and k3s from this machine
#
# On a machine with an NVIDIA GPU the Gryvia chart installs NVIDIA's GPU Operator, which builds and runs the driver
# in a container, configures the container runtime and starts the device plugin, so nothing NVIDIA-specific is
# installed on the host. Gryvia then registers the GPU nodes automatically (check with `gryvia status`).
#
# Options:
#   --dry-run              print what would run, change nothing
#   --no-gpu               skip all NVIDIA setup (also the default when no NVIDIA PCI device is found)
#   --gpu                  force NVIDIA setup even when no GPU is detected
#   --host-driver auto|yes|no   the NVIDIA driver is already installed on the host (default: auto-detect)
#   --k3s-version VERSION  k3s release to install (default: $K3S_VERSION_DEFAULT)
#   --chart REF            Gryvia chart: oci reference or local directory (default: the published chart)
#   --chart-version V      chart version (published chart only)
#   --api-key KEY          dashboard/API key (default: the well-known lab key Admin@321; change it)
#   --nodeport PORT        expose the dashboard on this NodePort (default 32443)
#   --set key=value        extra `helm --set` (repeatable)
#   --skip-os-check        do not require Ubuntu 22.04/24.04
# Exit codes: 0 ok, 1 error, 3 reboot needed (rerun afterwards).
set -euo pipefail

K3S_VERSION_DEFAULT="v1.31.4+k3s1"
HELM_VERSION="v3.16.4"
CHART_DEFAULT="oci://ghcr.io/zyvorai/charts/gryvia"
NAMESPACE="gryvia-system"
KUBECONFIG_K3S="/etc/rancher/k3s/k3s.yaml"

MODE=""
DRY_RUN=false
GPU_MODE="auto"            # auto | yes | no
HOST_DRIVER_MODE="auto"    # auto | yes | no
K3S_VERSION="${K3S_VERSION:-$K3S_VERSION_DEFAULT}"
CHART="${GRYVIA_CHART:-$CHART_DEFAULT}"
CHART_VERSION=""
API_KEY="${GRYVIA_API_KEY:-Admin@321}"
NODEPORT="32443"
SERVER_URL=""
TOKEN=""
SKIP_OS_CHECK=false
EXTRA_SETS=()

say()  { printf '%s\n' "$*"; }
info() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m✗\033[0m %s\n' "$*" >&2; exit 1; }

# Run a command, or only print it in --dry-run.
run() {
  if $DRY_RUN; then
    printf '+ %s\n' "$*"
  else
    "$@"
  fi
}

# Run a shell snippet (needed for pipes), or only print it in --dry-run.
run_sh() {
  if $DRY_RUN; then
    printf '+ %s\n' "$1"
  else
    bash -c "$1"
  fi
}

# ── Detection (each can be overridden by an environment variable, which the tests use) ──────────────────────

os_id_version() {
  if [[ -n "${GRYVIA_FAKE_OS:-}" ]]; then printf '%s' "$GRYVIA_FAKE_OS"; return; fi
  # shellcheck disable=SC1091
  ( . /etc/os-release 2>/dev/null && printf '%s:%s' "${ID:-unknown}" "${VERSION_ID:-0}" )
}

machine_arch() {
  printf '%s' "${GRYVIA_FAKE_ARCH:-$(uname -m)}"
}

# True when an NVIDIA PCI device (vendor 0x10de) is present.
detect_gpu() {
  if [[ -n "${GRYVIA_FAKE_HAS_GPU:-}" ]]; then [[ "$GRYVIA_FAKE_HAS_GPU" == "1" ]]; return; fi
  local dev
  for dev in /sys/bus/pci/devices/*/vendor; do
    [[ -r "$dev" ]] || continue
    if [[ "$(cat "$dev")" == "0x10de" ]]; then
      # class 0x03xxxx (display) or 0x0302xx (3D controller)
      local class; class="$(cat "$(dirname "$dev")/class" 2>/dev/null || echo 0)"
      [[ "$class" == 0x03* ]] && return 0
    fi
  done
  return 1
}

# True when the NVIDIA driver already works on the host.
detect_host_driver() {
  if [[ -n "${GRYVIA_FAKE_HOST_DRIVER:-}" ]]; then [[ "$GRYVIA_FAKE_HOST_DRIVER" == "1" ]]; return; fi
  command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L >/dev/null 2>&1
}

nouveau_loaded() {
  if [[ -n "${GRYVIA_FAKE_NOUVEAU:-}" ]]; then [[ "$GRYVIA_FAKE_NOUVEAU" == "1" ]]; return; fi
  lsmod 2>/dev/null | grep -q '^nouveau'
}

secure_boot_enabled() {
  if [[ -n "${GRYVIA_FAKE_SECURE_BOOT:-}" ]]; then [[ "$GRYVIA_FAKE_SECURE_BOOT" == "1" ]]; return; fi
  command -v mokutil >/dev/null 2>&1 && mokutil --sb-state 2>/dev/null | grep -qi 'enabled'
}

# Decide the effective GPU and host-driver settings from the flags and detection.
resolve_gpu() {
  case "$GPU_MODE" in
    yes) HAS_GPU=true ;;
    no)  HAS_GPU=false ;;
    *)   if detect_gpu; then HAS_GPU=true; else HAS_GPU=false; fi ;;
  esac
  HOST_DRIVER=false
  if $HAS_GPU; then
    case "$HOST_DRIVER_MODE" in
      yes) HOST_DRIVER=true ;;
      no)  HOST_DRIVER=false ;;
      *)   if detect_host_driver; then HOST_DRIVER=true; fi ;;
    esac
  fi
}

# The `helm upgrade --install` arguments for the current settings, one per line (so tests can read them).
helm_args() {
  local ip="${1:-127.0.0.1}"
  printf '%s\n' upgrade --install gryvia "$CHART" --namespace "$NAMESPACE" --create-namespace --set namespace.create=false
  [[ -n "$CHART_VERSION" && ! -d "$CHART" ]] && printf '%s\n' --version "$CHART_VERSION"
  printf '%s\n' --set "auth.apiKey=$API_KEY"
  printf '%s\n' --set ui.service.type=NodePort --set "ui.service.nodePort=$NODEPORT"
  printf '%s\n' --set "tls.extraSANs={$ip}"
  if $HAS_GPU; then
    printf '%s\n' --set nvidia.enabled=true
    $HOST_DRIVER && printf '%s\n' --set nvidia.driver.enabled=false
    # k3s runs its own containerd: tell NVIDIA's container toolkit where it lives.
    printf '%s\n' \
      --set "nvidia.toolkit.env[0].name=CONTAINERD_CONFIG" \
      --set "nvidia.toolkit.env[0].value=/var/lib/rancher/k3s/agent/etc/containerd/config.toml.tmpl" \
      --set "nvidia.toolkit.env[1].name=CONTAINERD_SOCKET" \
      --set "nvidia.toolkit.env[1].value=/run/k3s/containerd/containerd.sock" \
      --set "nvidia.toolkit.env[2].name=CONTAINERD_RUNTIME_CLASS" \
      --set "nvidia.toolkit.env[2].value=nvidia" \
      --set "nvidia.toolkit.env[3].name=CONTAINERD_SET_AS_DEFAULT" \
      --set-string "nvidia.toolkit.env[3].value=true"
  fi
  local s
  for s in "${EXTRA_SETS[@]+"${EXTRA_SETS[@]}"}"; do printf '%s\n' --set "$s"; done
  printf '%s\n' --wait --timeout 15m
}

# ── Steps ─────────────────────────────────────────────────────────────────────────────────────────────────

preflight() {
  info "Checking this machine"
  if [[ $EUID -ne 0 ]] && ! $DRY_RUN; then die "run as root (sudo)"; fi

  if ! $SKIP_OS_CHECK; then
    case "$(os_id_version)" in
      ubuntu:22.04|ubuntu:24.04) ;;
      *) die "unsupported OS '$(os_id_version)': this script supports Ubuntu 22.04 and 24.04 (use --skip-os-check to try anyway)" ;;
    esac
  fi
  case "$(machine_arch)" in
    x86_64|aarch64) ;;
    *) die "unsupported CPU architecture '$(machine_arch)' (need x86_64 or aarch64)" ;;
  esac

  resolve_gpu
  if $HAS_GPU; then
    say "  NVIDIA GPU:      yes"
    if $HOST_DRIVER; then
      say "  NVIDIA driver:   already on the host (the GPU Operator will not install one)"
    else
      say "  NVIDIA driver:   will run in a container (installed by the NVIDIA GPU Operator)"
    fi
  else
    say "  NVIDIA GPU:      none (installing without GPU support)"
  fi

  if $HAS_GPU && ! $HOST_DRIVER; then
    if secure_boot_enabled; then
      warn "Secure Boot is enabled: the driver container builds an unsigned kernel module, which will not load."
      warn "Disable Secure Boot, or install a signed driver on the host and use --host-driver yes."
    fi
    if nouveau_loaded; then
      info "The open-source nouveau driver is loaded and must be disabled before the NVIDIA driver can run"
      run_sh "printf 'blacklist nouveau\\noptions nouveau modeset=0\\n' > /etc/modprobe.d/blacklist-nouveau.conf"
      run update-initramfs -u
      say "Reboot this machine, then run the same command again."
      $DRY_RUN || exit 3
    fi
  fi
}

install_prereqs() {
  info "Installing prerequisites"
  run apt-get update -y
  run apt-get install -y curl ca-certificates
}

install_k3s() {
  local mode="$1"
  if command -v k3s >/dev/null 2>&1 && systemctl is-active --quiet k3s 2>/dev/null; then
    info "k3s is already running, skipping the install"
    return
  fi
  info "Installing k3s $K3S_VERSION ($mode)"
  if [[ "$mode" == "server" ]]; then
    run_sh "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='$K3S_VERSION' sh -s - server --write-kubeconfig-mode 644"
  else
    run_sh "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='$K3S_VERSION' K3S_URL='$SERVER_URL' K3S_TOKEN='$TOKEN' sh -s - agent"
  fi
}

install_helm() {
  if command -v helm >/dev/null 2>&1; then return; fi
  info "Installing helm $HELM_VERSION"
  run_sh "curl -fsSL https://raw.githubusercontent.com/helm/helm/$HELM_VERSION/scripts/get-helm-3 | DESIRED_VERSION=$HELM_VERSION bash"
}

wait_for_node() {
  info "Waiting for the node to become Ready"
  # `kubectl wait --all` fails with "no matching resources found" while the node has not registered yet.
  run_sh "for i in \$(seq 1 100); do KUBECONFIG=$KUBECONFIG_K3S k3s kubectl get nodes --no-headers 2>/dev/null | grep -q . && break; sleep 3; done"
  run env KUBECONFIG="$KUBECONFIG_K3S" k3s kubectl wait --for=condition=Ready node --all --timeout=300s
}

host_ip() {
  { hostname -I 2>/dev/null || true; } | awk '{print $1}'
}

install_gryvia() {
  info "Installing Gryvia"
  if $HAS_GPU; then
    # NVIDIA's driver and toolkit pods are privileged: create the namespace with the Pod Security label first, so the
    # label is there before any pod is admitted (helm's --create-namespace would create it without).
    run_sh "KUBECONFIG=$KUBECONFIG_K3S k3s kubectl create namespace $NAMESPACE --dry-run=client -o yaml | KUBECONFIG=$KUBECONFIG_K3S k3s kubectl apply -f -"
    run env KUBECONFIG="$KUBECONFIG_K3S" k3s kubectl label namespace "$NAMESPACE" pod-security.kubernetes.io/enforce=privileged --overwrite
  fi
  local ip; ip="$(host_ip)"; ip="${ip:-127.0.0.1}"
  if [[ -d "$CHART" ]]; then
    # A chart from a checkout needs its NVIDIA GPU Operator and Kueue (oci://, no repo add) dependencies downloaded first.
    run helm repo add nvidia https://helm.ngc.nvidia.com/nvidia --force-update
    run helm dependency build "$CHART"
  fi
  local -a args=()
  local line
  while IFS= read -r line; do args+=("$line"); done < <(helm_args "$ip")
  run env KUBECONFIG="$KUBECONFIG_K3S" helm "${args[@]}"
}

wait_for_gryvia() {
  info "Waiting for Gryvia to be ready"
  run env KUBECONFIG="$KUBECONFIG_K3S" k3s kubectl -n "$NAMESPACE" rollout status deployment/gryvia-api-gateway --timeout=300s
  run env KUBECONFIG="$KUBECONFIG_K3S" k3s kubectl -n "$NAMESPACE" rollout status deployment/gryvia-ui --timeout=300s
}

print_summary() {
  local ip; ip="$(host_ip)"; ip="${ip:-<this-server>}"
  say ""
  say "Gryvia is installed."
  say "  Dashboard:  https://$ip:$NODEPORT   (self-signed certificate: accept the browser warning once)"
  say "  Sign in:    admin / $API_KEY"
  if [[ "$API_KEY" == "Admin@321" ]]; then
    warn "This is the well-known lab key. Re-run with --api-key <secret> before exposing the server."
  fi
  if $HAS_GPU; then
    say ""
    say "The NVIDIA GPU Operator is now preparing the GPU (driver build can take several minutes)."
    say "  Watch it:   KUBECONFIG=$KUBECONFIG_K3S k3s kubectl -n $NAMESPACE get pods"
    say "  Node ready: gryvia status   (GPU nodes appear as soon as GPU feature discovery labels them)"
  fi
  say ""
  say "Add another GPU node with:"
  say "  sudo ./scripts/install-k3s-gpu.sh agent --server https://$ip:6443 --token \$(sudo cat /var/lib/rancher/k3s/server/node-token)"
}

uninstall() {
  info "Removing Gryvia and k3s from this machine"
  if command -v helm >/dev/null 2>&1 && [[ -r "$KUBECONFIG_K3S" ]]; then
    run env KUBECONFIG="$KUBECONFIG_K3S" helm -n "$NAMESPACE" uninstall gryvia || true
  fi
  if [[ -x /usr/local/bin/k3s-uninstall.sh ]]; then
    run /usr/local/bin/k3s-uninstall.sh
  elif [[ -x /usr/local/bin/k3s-agent-uninstall.sh ]]; then
    run /usr/local/bin/k3s-agent-uninstall.sh
  else
    say "k3s is not installed."
  fi
}

# ── Arguments ─────────────────────────────────────────────────────────────────────────────────────────────

usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      server|agent) MODE="$1" ;;
      --uninstall) MODE="uninstall" ;;
      --dry-run) DRY_RUN=true ;;
      --no-gpu) GPU_MODE="no" ;;
      --gpu) GPU_MODE="yes" ;;
      --host-driver) shift; HOST_DRIVER_MODE="${1:-}"
        case "$HOST_DRIVER_MODE" in auto|yes|no) ;; *) die "--host-driver takes auto, yes or no" ;; esac ;;
      --k3s-version) shift; K3S_VERSION="${1:?--k3s-version needs a value}" ;;
      --chart) shift; CHART="${1:?--chart needs a value}" ;;
      --chart-version) shift; CHART_VERSION="${1:?--chart-version needs a value}" ;;
      --api-key) shift; API_KEY="${1:?--api-key needs a value}" ;;
      --nodeport) shift; NODEPORT="${1:?--nodeport needs a value}" ;;
      --server) shift; SERVER_URL="${1:?--server needs a URL}" ;;
      --token) shift; TOKEN="${1:?--token needs a value}" ;;
      --set) shift; EXTRA_SETS+=("${1:?--set needs key=value}") ;;
      --skip-os-check) SKIP_OS_CHECK=true ;;
      -h|--help) usage; exit 0 ;;
      *) die "unknown argument: $1 (see --help)" ;;
    esac
    shift
  done
  [[ -n "$MODE" ]] || { usage; die "choose server, agent or --uninstall"; }
  if [[ "$MODE" == "agent" ]]; then
    [[ -n "$SERVER_URL" && -n "$TOKEN" ]] || die "agent mode needs --server https://<server>:6443 and --token <token>"
  fi
}

main() {
  parse_args "$@"
  case "$MODE" in
    uninstall) uninstall ;;
    server)
      preflight
      install_prereqs
      install_k3s server
      wait_for_node
      install_helm
      install_gryvia
      wait_for_gryvia
      print_summary
      ;;
    agent)
      preflight
      install_prereqs
      install_k3s agent
      say ""
      say "This node joined the cluster. If it has a GPU, the NVIDIA GPU Operator on the server prepares it and"
      say "Gryvia registers it automatically. Check from the server with: gryvia status"
      ;;
  esac
}

# Tests source this file to call the pure functions without running anything.
if [[ "${GRYVIA_INSTALLER_SOURCE_ONLY:-}" != "1" ]]; then
  main "$@"
fi
