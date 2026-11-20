#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Tests for scripts/validate-gpu.sh: --dry-run, and real runs against a fake `kubectl` on PATH that
# returns canned output for the pass and fail cases. No cluster, no GPU, no network.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
SCRIPT="scripts/validate-gpu.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILED=0
PASSED=0
CODE=0
OUT=""

mkdir -p "$TMP/bin"
# The fake kubectl answers from FAKE_* environment variables and logs every call.
cat >"$TMP/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
echo "$*" >>"${FAKE_LOG:-/dev/null}"
args="$*"
case "$args" in
"version --client"*) echo "Client Version: v1.31.0" ;;
"config current-context") [ "${FAKE_CONTEXT-fake-ctx}" = "" ] && exit 1; echo "${FAKE_CONTEXT-fake-ctx}" ;;
*"get namespace "*) [ "${FAKE_NS_OK:-1}" = 1 ] || exit 1; echo "namespace/gryvia-system" ;;
*"get clusterpolicy"*) printf '%s' "${FAKE_POLICY-ready}" ;;
*"get pods -l app=nvidia-driver-daemonset"*) printf '%s' "${FAKE_DRIVER_PODS-Running
}" ;;
*"get pods -l app=nvidia-container-toolkit-daemonset"*) printf 'Running\n' ;;
*"get pods -l app=nvidia-device-plugin-daemonset"*) printf '%s' "${FAKE_PLUGIN_PODS-Running
}" ;;
*"get nodes"*) printf 'gpu-node-1 %s\ncpu-node-1 \n' "${FAKE_GPUS-1}" ;;
*"apply -f -"*) cat >/dev/null ;;
*"apply -f "*) ;;
*"get pod gryvia-validate-smi"*) printf '%s' "${FAKE_SMI_PHASE-Succeeded}" ;;
*"logs gryvia-validate-smi"*) printf '%s' "${FAKE_SMI_LOGS-| NVIDIA-SMI 550.54  Driver Version: 550.54  CUDA Version: 12.4 |}" ;;
*"get gryviagpunode"*) printf '%s' "${FAKE_GPUNODE-Ready|550.54|12.4}" ;;
*"get gryviaaijob"*) printf '%s' "${FAKE_JOB-Succeeded}" ;;
*"delete "*) ;;
*) echo "fake kubectl: unexpected call: $args" >&2; exit 99 ;;
esac
FAKE
chmod +x "$TMP/bin/kubectl"

REPORT="$TMP/report.json"
# run [ENV=VAL ...] -- args...
run() {
	local envs=()
	while [[ $# -gt 0 && "$1" != "--" ]]; do envs+=("$1"); shift; done
	shift
	OUT="$(env "${envs[@]}" PATH="$TMP/bin:$PATH" FAKE_LOG="$TMP/calls.log" bash "$SCRIPT" --timeout 1 --poll 0 --report "$REPORT" "$@" 2>&1)"
	CODE=$?
}
ok()   { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; printf '%s\n' "$OUT" | sed 's/^/       | /' | head -30; }
has()  { if grep -qF -- "$2" <<<"$OUT"; then ok "$1"; else fail "$1 (expected: $2)"; fi; }
lacks(){ if grep -qF -- "$2" <<<"$OUT"; then fail "$1 (unexpected: $2)"; else ok "$1"; fi; }
code() { if [[ "$CODE" == "$2" ]]; then ok "$1"; else OUT="exit=$CODE"$'\n'"$OUT"; fail "$1 (expected exit $2, got $CODE)"; fi; }
# status NAME EXPECTED: read one check's status out of the JSON report.
status() {
	local got
	got="$(python3 - "$REPORT" "$2" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print(next((c["status"] for c in d["checks"] if c["name"] == sys.argv[2]), "missing"))
PY
)"
	if [[ "$got" == "$3" ]]; then ok "$2 is $3"; else OUT="$2: got $got"; fail "$2 expected $3"; fi
}
report_result() {
	local got
	got="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["result"])' "$REPORT")"
	if [[ "$got" == "$1" ]]; then ok "report result is $1"; else OUT="result: $got"; fail "report result expected $1"; fi
}

echo "dry run"
: >"$TMP/calls.log"
run -- --dry-run --rdma
code "succeeds" 0
has "prints the ClusterPolicy command" "kubectl get clusterpolicy"
has "prints the job submit" "kubectl apply -f examples/training/simple-pytorch-training.yaml"
has "prints the RDMA check" "ls /sys/class/infiniband"
has "summary says dry-run" "result: dry-run"
if [[ ! -s "$TMP/calls.log" ]]; then ok "runs no kubectl"; else OUT="$(cat "$TMP/calls.log")"; fail "dry-run called kubectl"; fi
report_result dry-run
if python3 -m json.tool "$REPORT" >/dev/null; then ok "report is valid JSON"; else fail "report is not valid JSON"; fi
run -- --dry-run --context prod --namespace foo
has "honours --context" "kubectl --context prod get clusterpolicy"
has "honours --namespace" "-n foo get pods"

echo "everything passes"
run -- --skip-job
code "exit 0" 0
report_result pass
for c in preflight.kubectl preflight.context preflight.namespace nvidia.clusterpolicy nvidia.pods.driver nvidia.pods.toolkit \
	nvidia.pods.device-plugin node.allocatable pod.nvidia-smi gryvia.gpunode; do status "$OUT" "$c" pass; done
status "$OUT" gryvia.job skip
run --
code "with the job too" 0
status "$OUT" gryvia.job pass
if python3 - "$REPORT" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert d["node"] == "gpu-node-1", d["node"]
assert d["schema"] == "gryvia.validate-gpu/v1"
for c in d["checks"]:
    assert c["started"] and c["finished"] and c["detail"] and c["status"] in ("pass", "fail", "skip"), c
PY
then ok "report has node, timestamps and details"; else fail "report content"; fi

echo "failures"
run FAKE_POLICY=notReady -- --skip-job
code "ClusterPolicy not ready exits 1" 1
status "$OUT" nvidia.clusterpolicy fail
has "explains" "timed out after 1s waiting for ClusterPolicy ready"
report_result fail
run FAKE_GPUS=0 -- --skip-job
code "allocatable 0 exits 1" 1
status "$OUT" node.allocatable fail
status "$OUT" pod.nvidia-smi skip
status "$OUT" gryvia.gpunode skip
run FAKE_DRIVER_PODS=$'CrashLoopBackOff\n' -- --skip-job
code "crashing driver pod exits 1" 1
status "$OUT" nvidia.pods.driver fail
run FAKE_DRIVER_PODS="" -- --skip-job --host-driver
code "--host-driver does not need a driver pod" 0
status "$OUT" nvidia.pods.driver skip
run FAKE_SMI_LOGS="Failed to initialize NVML" -- --skip-job
code "nvidia-smi without NVIDIA-SMI output exits 1" 1
status "$OUT" pod.nvidia-smi fail
run FAKE_SMI_PHASE=Failed -- --skip-job
status "$OUT" pod.nvidia-smi fail
run "FAKE_GPUNODE=WaitingForDrivers||" -- --skip-job
code "GryviaGpuNode not Ready exits 1" 1
status "$OUT" gryvia.gpunode fail
run FAKE_JOB=Failed --
code "failed job exits 1" 1
status "$OUT" gryvia.job fail
run --job-file /nonexistent.yaml --
status "$OUT" gryvia.job fail
run FAKE_NS_OK=0 -- --skip-job
code "missing namespace exits 1" 1
status "$OUT" preflight.namespace fail
status "$OUT" nvidia.clusterpolicy skip
run FAKE_CONTEXT= -- --skip-job
code "no context exits 1" 1
status "$OUT" preflight.context fail

echo "rdma"
mkdir -p "$TMP/ib/mlx5_0"
run -- --skip-job --rdma --ib-root "$TMP/ib"
code "device present passes" 0
status "$OUT" rdma.sysfs pass
status "$OUT" rdma.collector skip
run -- --skip-job --rdma --ib-root "$TMP/nope"
code "no device with --rdma exits 1" 1
status "$OUT" rdma.sysfs fail
run -- --skip-job
lacks "no rdma checks without --rdma" "rdma.sysfs"

echo "arguments"
run -- --bogus
code "unknown option exits 64" 64
run -- --timeout abc
code "bad timeout exits 64" 64

echo
echo "$PASSED passed, $FAILED failed"
[[ "$FAILED" == 0 ]]
