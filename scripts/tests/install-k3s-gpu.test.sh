#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Tests for scripts/install-k3s-gpu.sh. They run the script in --dry-run with faked detection, so they need no
# root, no GPU and no network, and assert on the commands it would run.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
SCRIPT="scripts/install-k3s-gpu.sh"
FAILED=0
PASSED=0

# run <env assignments...> -- <args...>: prints combined output, sets $CODE
CODE=0
run() {
  local envs=()
  while [[ $# -gt 0 && "$1" != "--" ]]; do envs+=("$1"); shift; done
  shift
  OUT="$(env "${envs[@]}" bash "$SCRIPT" "$@" 2>&1)"
  CODE=$?
}

ok()   { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; printf '%s\n' "$OUT" | sed 's/^/       | /' | head -25; }

has()  { if grep -qF -- "$2" <<<"$OUT"; then ok "$1"; else fail "$1 (expected: $2)"; fi; }
lacks(){ if grep -qF -- "$2" <<<"$OUT"; then fail "$1 (unexpected: $2)"; else ok "$1"; fi; }
code() { if [[ "$CODE" == "$2" ]]; then ok "$1"; else OUT="exit=$CODE"$'\n'"$OUT"; fail "$1 (expected exit $2, got $CODE)"; fi; }

UBUNTU=(GRYVIA_FAKE_OS=ubuntu:22.04 GRYVIA_FAKE_ARCH=x86_64 GRYVIA_FAKE_NOUVEAU=0 GRYVIA_FAKE_SECURE_BOOT=0)

echo "server without a GPU"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=0 -- server --dry-run
code "succeeds" 0
has "installs k3s" "get.k3s.io"
has "pins the k3s version" "INSTALL_K3S_VERSION='v1.31.4+k3s1'"
has "installs the chart" "helm upgrade --install gryvia oci://ghcr.io/zyvorai/charts/gryvia"
has "waits for the deployments" "rollout status deployment/gryvia-ui"
has "says there is no GPU" "none (installing without GPU support)"
lacks "does not enable NVIDIA" "nvidia.enabled=true"
lacks "does not label the namespace without a GPU" "pod-security.kubernetes.io/enforce"
lacks "does not touch nouveau" "blacklist"

echo "server with a GPU and no host driver"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=1 GRYVIA_FAKE_HOST_DRIVER=0 -- server --dry-run
code "succeeds" 0
has "enables the NVIDIA GPU Operator" "nvidia.enabled=true"
has "points the toolkit at k3s containerd config" "CONTAINERD_CONFIG"
has "points the toolkit at the k3s socket" "/run/k3s/containerd/containerd.sock"
has "makes nvidia the default runtime" "nvidia.toolkit.env[3].value=true"
lacks "lets the operator install the driver" "nvidia.driver.enabled=false"
has "explains the driver container" "will run in a container"
has "labels the namespace privileged for the driver pods" "pod-security.kubernetes.io/enforce=privileged"
has "chart does not also create the namespace" "namespace.create=false"

echo "server with a GPU and a host driver"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=1 GRYVIA_FAKE_HOST_DRIVER=1 -- server --dry-run
has "keeps the host driver" "nvidia.driver.enabled=false"
has "says so" "already on the host"

echo "explicit flags beat detection"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=1 -- server --dry-run --no-gpu
lacks "--no-gpu skips NVIDIA" "nvidia.enabled=true"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=0 -- server --dry-run --gpu
has "--gpu forces NVIDIA" "nvidia.enabled=true"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=1 GRYVIA_FAKE_HOST_DRIVER=0 -- server --dry-run --host-driver yes
has "--host-driver yes" "nvidia.driver.enabled=false"

echo "nouveau and Secure Boot"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=1 GRYVIA_FAKE_HOST_DRIVER=0 GRYVIA_FAKE_NOUVEAU=1 -- server --dry-run
has "blacklists nouveau" "blacklist-nouveau.conf"
has "rebuilds the initramfs" "update-initramfs -u"
has "asks for a reboot" "Reboot this machine"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=1 GRYVIA_FAKE_HOST_DRIVER=0 GRYVIA_FAKE_SECURE_BOOT=1 -- server --dry-run
has "warns about Secure Boot" "Secure Boot is enabled"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=1 GRYVIA_FAKE_HOST_DRIVER=1 GRYVIA_FAKE_NOUVEAU=1 -- server --dry-run
lacks "a host driver makes nouveau irrelevant" "blacklist"

echo "options"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=0 -- server --dry-run --api-key s3cret --nodeport 30443 --set gpuOperator.autoRegister=false --chart-version 0.2.0
has "sets the API key" "auth.apiKey=s3cret"
has "uses the NodePort" "ui.service.nodePort=30443"
has "passes extra values through" "gpuOperator.autoRegister=false"
has "pins the chart version" "--version 0.2.0"
lacks "no lab-key warning with a custom key" "well-known lab key"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=0 -- server --dry-run
has "warns about the default key" "well-known lab key"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=0 -- server --dry-run --chart ./helm/gryvia --chart-version 9.9.9
has "builds the dependency for a local chart" "helm dependency build ./helm/gryvia"
lacks "ignores --chart-version for a local chart" "--version 9.9.9"

echo "agent"
run "${UBUNTU[@]}" GRYVIA_FAKE_HAS_GPU=0 -- agent --dry-run --server https://10.0.0.5:6443 --token abc
code "succeeds" 0
has "joins the server" "K3S_URL='https://10.0.0.5:6443'"
has "with the token" "K3S_TOKEN='abc'"
lacks "does not install Gryvia on an agent" "helm upgrade"
run "${UBUNTU[@]}" -- agent --dry-run
code "requires --server and --token" 1
has "explains" "agent mode needs --server"

echo "guards"
run GRYVIA_FAKE_OS=debian:12 GRYVIA_FAKE_ARCH=x86_64 -- server --dry-run
code "rejects an unsupported OS" 1
has "names the OS" "unsupported OS 'debian:12'"
run GRYVIA_FAKE_OS=debian:12 GRYVIA_FAKE_ARCH=x86_64 GRYVIA_FAKE_HAS_GPU=0 -- server --dry-run --skip-os-check
code "--skip-os-check allows it" 0
run GRYVIA_FAKE_OS=ubuntu:24.04 GRYVIA_FAKE_ARCH=riscv64 -- server --dry-run
code "rejects an unsupported CPU" 1
run "${UBUNTU[@]}" -- --bogus
code "rejects unknown arguments" 1
run "${UBUNTU[@]}" --
code "asks for a mode" 1
run "${UBUNTU[@]}" GRYVIA_FAKE_OS=ubuntu:24.04 GRYVIA_FAKE_ARCH=aarch64 GRYVIA_FAKE_HAS_GPU=0 -- server --dry-run
code "supports Ubuntu 24.04 on arm64" 0
run "${UBUNTU[@]}" -- --uninstall --dry-run
code "uninstall dry-run" 0

echo
echo "$PASSED passed, $FAILED failed"
[[ "$FAILED" == 0 ]]
