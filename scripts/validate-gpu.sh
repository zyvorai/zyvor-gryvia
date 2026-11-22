#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Runs docs/gpu-validation.md as far as a script can and writes a JSON report.
#
# NOT VERIFIED ON HARDWARE: this script is exercised in CI only with a fake kubectl
# (scripts/tests/validate-gpu.test.sh). Nobody has run it against a real GPU node yet; run it on
# one, then paste the report into an issue (format: docs/gpu-validation.md, "Report").
#
# Checks (each is pass, fail or skip):
#   preflight.kubectl, preflight.context, preflight.namespace
#   nvidia.clusterpolicy   ClusterPolicy status.state == ready
#   nvidia.pods            driver, toolkit and device-plugin pods Running/Succeeded
#   node.allocatable       some node advertises nvidia.com/gpu > 0
#   pod.nvidia-smi         a pod that requests one GPU runs nvidia-smi
#   gryvia.gpunode         the auto-registered GryviaGpuNode is Ready with driver/CUDA versions
#   gryvia.job             examples/training/simple-pytorch-training.yaml reaches Succeeded
#   rdma.sysfs, rdma.collector   only with --rdma
# Exit status: 0 when nothing failed, 1 when any check failed, 64 for bad arguments.
set -uo pipefail

usage() {
	cat <<'USAGE'
usage: validate-gpu.sh [options]

  --namespace NS        Gryvia namespace (default gryvia-system)
  --context CTX         kubectl context (default: the current one)
  --node NAME           node to check (default: the first node with allocatable nvidia.com/gpu)
  --timeout SECONDS     wait budget per check (default 900; the driver container can take 15 min)
  --poll SECONDS        poll interval (default 10)
  --host-driver         the GPU driver is installed on the host: do not expect a driver pod
  --smi-image IMAGE     image for the nvidia-smi pod (default nvidia/cuda:12.4.1-base-ubuntu22.04)
  --job-file FILE       job to submit (default examples/training/simple-pytorch-training.yaml)
  --skip-job            do not submit a job
  --keep                keep the smi pod and the job after the run
  --rdma                also check RDMA devices and NIC counters
  --ib-root DIR         RDMA sysfs root (default /sys/class/infiniband; run on the node itself)
  --collector-url URL   collector API base URL, for the NIC counter check
  --collector-token-file FILE   the collector's -api-token-file token (HMAC-signs the request)
  --report FILE         JSON report path (default ./gpu-validation-report.json)
  --dry-run             print the commands, run nothing
  -h, --help
USAGE
}

NS=gryvia-system CTX="" NODE="" TIMEOUT=900 POLL=10 HOST_DRIVER=0
SMI_IMAGE="nvidia/cuda:12.4.1-base-ubuntu22.04" JOB_FILE="examples/training/simple-pytorch-training.yaml"
SKIP_JOB=0 KEEP=0 RDMA=0 IB_ROOT=/sys/class/infiniband COLLECTOR_URL="" COLLECTOR_TOKEN_FILE=""
REPORT=./gpu-validation-report.json DRY=0
while [ $# -gt 0 ]; do
	case "$1" in
	--namespace) NS="${2:?}"; shift 2 ;;
	--context) CTX="${2:?}"; shift 2 ;;
	--node) NODE="${2:?}"; shift 2 ;;
	--timeout) TIMEOUT="${2:?}"; shift 2 ;;
	--poll) POLL="${2:?}"; shift 2 ;;
	--host-driver) HOST_DRIVER=1; shift ;;
	--smi-image) SMI_IMAGE="${2:?}"; shift 2 ;;
	--job-file) JOB_FILE="${2:?}"; shift 2 ;;
	--skip-job) SKIP_JOB=1; shift ;;
	--keep) KEEP=1; shift ;;
	--rdma) RDMA=1; shift ;;
	--ib-root) IB_ROOT="${2:?}"; shift 2 ;;
	--collector-url) COLLECTOR_URL="${2:?}"; shift 2 ;;
	--collector-token-file) COLLECTOR_TOKEN_FILE="${2:?}"; shift 2 ;;
	--report) REPORT="${2:?}"; shift 2 ;;
	--dry-run) DRY=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown option: $1" >&2; usage >&2; exit 64 ;;
	esac
done
case "$TIMEOUT$POLL" in '' | *[!0-9]*) echo "--timeout and --poll must be whole seconds" >&2; exit 64 ;; esac

KUBECTL=(kubectl)
[ -n "$CTX" ] && KUBECTL+=(--context "$CTX")

now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
json_str() { printf '%s' "$1" | tr '\n\t\r' '   ' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | cut -c1-600; }

STARTED="$(now)"
RESULTS=() # one JSON object per check
NAMES=() STATUSES=() DETAILS=()
CUR="" CUR_START=""
FAILS=0

begin() { CUR="$1"; CUR_START="$(now)"; }
finish() { # finish STATUS DETAIL
	local st="$1" detail="$2"
	[ "$st" = fail ] && FAILS=$((FAILS + 1))
	NAMES+=("$CUR") STATUSES+=("$st") DETAILS+=("$detail")
	RESULTS+=("{\"name\":\"$CUR\",\"status\":\"$st\",\"detail\":\"$(json_str "$detail")\",\"started\":\"$CUR_START\",\"finished\":\"$(now)\"}")
}
plan() { printf '  + %s\n' "$*"; }

# kc runs kubectl; in dry-run it never gets here (every check returns earlier).
kc() { "${KUBECTL[@]}" "$@"; }

# wait_for DESCRIPTION CMD...: re-run CMD (a function) until it succeeds or the budget is used up.
wait_for() {
	local desc="$1"
	shift
	local deadline=$((SECONDS + TIMEOUT))
	while :; do
		if "$@"; then return 0; fi
		[ "$SECONDS" -ge "$deadline" ] && { WAIT_ERR="timed out after ${TIMEOUT}s waiting for $desc"; return 1; }
		sleep "$POLL"
	done
}

# ---- checks ----
check_preflight() {
	begin preflight.kubectl
	if [ "$DRY" = 1 ]; then
		plan "${KUBECTL[*]} version --client"; finish skip "dry-run"
	elif command -v "${KUBECTL[0]}" >/dev/null 2>&1 && kc version --client >/dev/null 2>&1; then
		finish pass "kubectl found"
	else
		finish fail "kubectl not found or not working"
		return 1
	fi
	begin preflight.context
	if [ "$DRY" = 1 ]; then
		plan "${KUBECTL[*]} config current-context"; finish skip "dry-run"
	else
		local c
		if c="$(kc config current-context 2>&1)" && [ -n "$c" ]; then
			finish pass "context $c"
			CONTEXT_NAME="$c"
		else
			finish fail "no kubectl context: $c"
			return 1
		fi
	fi
	begin preflight.namespace
	if [ "$DRY" = 1 ]; then
		plan "${KUBECTL[*]} get namespace $NS -o name"; finish skip "dry-run"
	elif kc get namespace "$NS" -o name >/dev/null 2>&1; then
		finish pass "namespace $NS exists"
	else
		finish fail "namespace $NS not found (was Gryvia installed there? use --namespace)"
		return 1
	fi
}

policy_ready() {
	local s
	s="$(kc get clusterpolicy -o jsonpath='{.items[0].status.state}' 2>/dev/null)"
	POLICY_STATE="$s"
	[ "$s" = ready ]
}

check_clusterpolicy() {
	begin nvidia.clusterpolicy
	if [ "$DRY" = 1 ]; then
		plan "${KUBECTL[*]} get clusterpolicy -o jsonpath='{.items[0].status.state}'   # until 'ready', up to ${TIMEOUT}s"; finish skip "dry-run"
		return
	fi
	POLICY_STATE=""
	if wait_for "ClusterPolicy ready" policy_ready; then
		finish pass "ClusterPolicy state ready"
	else
		finish fail "${WAIT_ERR}; last state '${POLICY_STATE:-none}' (no ClusterPolicy means the GPU Operator is not installed)"
	fi
}

# pods_ok LABEL: at least one pod, every pod Running or Succeeded.
pods_ok() {
	local phases
	phases="$(kc -n "$NS" get pods -l "$1" -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null)"
	PODS_SEEN="$(printf '%s' "$phases" | tr '\n' ' ')"
	[ -n "$phases" ] && ! printf '%s\n' "$phases" | grep -qvE '^(Running|Succeeded)$'
}

check_pods() {
	local label what
	for what in "driver:app=nvidia-driver-daemonset" "toolkit:app=nvidia-container-toolkit-daemonset" "device-plugin:app=nvidia-device-plugin-daemonset"; do
		label="${what#*:}"
		begin "nvidia.pods.${what%%:*}"
		if [ "${what%%:*}" = driver ] && [ "$HOST_DRIVER" = 1 ]; then
			finish skip "--host-driver: the driver is on the host"
			continue
		fi
		if [ "$DRY" = 1 ]; then
			plan "${KUBECTL[*]} -n $NS get pods -l $label -o jsonpath='{...status.phase}'   # all Running/Succeeded"; finish skip "dry-run"
			continue
		fi
		PODS_SEEN=""
		if wait_for "${what%%:*} pods" pods_ok "$label"; then
			finish pass "pods: $PODS_SEEN"
		else
			finish fail "${WAIT_ERR}; pods seen: ${PODS_SEEN:-none} (label $label; driver logs: kubectl -n $NS logs -l app=nvidia-driver-daemonset)"
		fi
	done
}

alloc_ok() {
	local line name n
	line="$(kc get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}' 2>/dev/null)"
	ALLOC_SEEN="$(printf '%s' "$line" | tr '\n' ';')"
	while read -r name n; do
		[ -n "$name" ] || continue
		[ -n "$NODE" ] && [ "$name" != "$NODE" ] && continue
		case "${n:-0}" in '' | *[!0-9]*) continue ;; esac
		if [ "$n" -gt 0 ]; then NODE="$name"; GPUS="$n"; return 0; fi
	done <<<"$line"
	return 1
}

check_allocatable() {
	begin node.allocatable
	if [ "$DRY" = 1 ]; then
		plan "${KUBECTL[*]} get nodes -o jsonpath='{name} {status.allocatable.nvidia.com/gpu}'   # > 0"; finish skip "dry-run"
		return
	fi
	GPUS=0
	if wait_for "allocatable nvidia.com/gpu" alloc_ok; then
		finish pass "node $NODE allocatable nvidia.com/gpu=$GPUS"
	else
		finish fail "${WAIT_ERR}; nodes: ${ALLOC_SEEN:-none} (0 means the device plugin has not registered: see nvidia.pods)"
	fi
}

smi_pod_manifest() {
	cat <<MANIFEST
apiVersion: v1
kind: Pod
metadata:
  name: gryvia-validate-smi
  namespace: $NS
  labels: {app.kubernetes.io/managed-by: validate-gpu}
spec:
  restartPolicy: Never
  nodeName: $NODE
  containers:
    - name: smi
      image: $SMI_IMAGE
      command: ["nvidia-smi"]
      resources:
        limits: {nvidia.com/gpu: 1}
MANIFEST
}

smi_done() {
	SMI_PHASE="$(kc -n "$NS" get pod gryvia-validate-smi -o jsonpath='{.status.phase}' 2>/dev/null)"
	[ "$SMI_PHASE" = Succeeded ] || [ "$SMI_PHASE" = Failed ]
}

check_smi() {
	begin pod.nvidia-smi
	if [ "$DRY" = 1 ]; then
		plan "${KUBECTL[*]} apply -f - <<< (pod gryvia-validate-smi running nvidia-smi with nvidia.com/gpu: 1 in $NS)"
		plan "${KUBECTL[*]} -n $NS get pod gryvia-validate-smi -o jsonpath='{.status.phase}'   # until Succeeded"
		plan "${KUBECTL[*]} -n $NS logs gryvia-validate-smi   # must contain NVIDIA-SMI"
		finish skip "dry-run"
		return
	fi
	if [ -z "$NODE" ]; then finish skip "no GPU node found (see node.allocatable)"; return; fi
	kc -n "$NS" delete pod gryvia-validate-smi --ignore-not-found >/dev/null 2>&1
	if ! smi_pod_manifest | kc apply -f - >/dev/null 2>&1; then finish fail "could not create the nvidia-smi pod"; return; fi
	SMI_PHASE=""
	if ! wait_for "the nvidia-smi pod to finish" smi_done; then
		finish fail "${WAIT_ERR}; phase '${SMI_PHASE:-unknown}'"
	else
		local logs
		logs="$(kc -n "$NS" logs gryvia-validate-smi 2>&1)"
		if [ "$SMI_PHASE" = Succeeded ] && printf '%s' "$logs" | grep -q 'NVIDIA-SMI'; then
			finish pass "$(printf '%s' "$logs" | grep -m1 -E 'Driver Version|NVIDIA-SMI' | tr -s ' ')"
		else
			finish fail "phase $SMI_PHASE; logs: $(printf '%s' "$logs" | head -n 5)"
		fi
	fi
	[ "$KEEP" = 1 ] || kc -n "$NS" delete pod gryvia-validate-smi --ignore-not-found >/dev/null 2>&1
}

gpunode_ok() {
	GN="$(kc get gryviagpunode "$NODE" -o jsonpath='{.status.phase}|{.status.driverVersion}|{.status.cudaVersion}' 2>/dev/null)"
	local phase drv cuda
	IFS='|' read -r phase drv cuda <<<"$GN"
	[ "$phase" = Ready ] && [ -n "$drv" ] && [ -n "$cuda" ]
}

check_gpunode() {
	begin gryvia.gpunode
	if [ "$DRY" = 1 ]; then
		plan "${KUBECTL[*]} get gryviagpunode <node> -o jsonpath='{.status.phase}|{.status.driverVersion}|{.status.cudaVersion}'   # Ready, versions set"; finish skip "dry-run"
		return
	fi
	if [ -z "$NODE" ]; then finish skip "no GPU node found (see node.allocatable)"; return; fi
	GN=""
	if wait_for "GryviaGpuNode $NODE Ready" gpunode_ok; then
		finish pass "phase|driver|cuda = $GN"
	else
		finish fail "${WAIT_ERR}; last phase|driver|cuda = '${GN:-object not found}' (WaitingForDrivers means allocatable nvidia.com/gpu is still 0)"
	fi
}

job_done() {
	JOB_PHASE="$(kc -n "$JOB_NS" get gryviaaijob "$JOB_NAME" -o jsonpath='{.status.phase}' 2>/dev/null)"
	[ "$JOB_PHASE" = Succeeded ] || [ "$JOB_PHASE" = Failed ]
}

check_job() {
	begin gryvia.job
	if [ "$SKIP_JOB" = 1 ]; then finish skip "--skip-job"; return; fi
	if [ "$DRY" = 1 ]; then
		plan "${KUBECTL[*]} apply -f $JOB_FILE"
		plan "${KUBECTL[*]} get gryviaaijob <name> -o jsonpath='{.status.phase}'   # until Succeeded"
		finish skip "dry-run"
		return
	fi
	if [ ! -f "$JOB_FILE" ]; then finish fail "job file $JOB_FILE not found (run from the repository root or pass --job-file)"; return; fi
	JOB_NAME="$(awk '/^metadata:/{m=1;next} m&&/^[[:space:]]+name:/{print $2;exit}' "$JOB_FILE")"
	JOB_NS="$(awk '/^metadata:/{m=1;next} m&&/^[[:space:]]+namespace:/{print $2;exit}' "$JOB_FILE")"
	JOB_NS="${JOB_NS:-default}"
	if [ -z "$JOB_NAME" ]; then finish fail "cannot read metadata.name from $JOB_FILE"; return; fi
	if ! kc apply -f "$JOB_FILE" >/dev/null 2>&1; then finish fail "kubectl apply -f $JOB_FILE failed"; return; fi
	JOB_PHASE=""
	if ! wait_for "job $JOB_NS/$JOB_NAME to finish" job_done; then
		finish fail "${WAIT_ERR}; phase '${JOB_PHASE:-none}'"
	elif [ "$JOB_PHASE" = Succeeded ]; then
		finish pass "job $JOB_NS/$JOB_NAME Succeeded"
	else
		finish fail "job $JOB_NS/$JOB_NAME phase $JOB_PHASE"
	fi
	[ "$KEEP" = 1 ] || kc -n "$JOB_NS" delete gryviaaijob "$JOB_NAME" --ignore-not-found >/dev/null 2>&1
}

check_rdma() {
	begin rdma.sysfs
	if [ "$DRY" = 1 ]; then
		plan "ls $IB_ROOT   # at least one RDMA device (run on the node itself)"; finish skip "dry-run"
	elif [ -d "$IB_ROOT" ] && [ -n "$(command ls -A "$IB_ROOT" 2>/dev/null)" ]; then
		finish pass "devices: $(command ls "$IB_ROOT" | tr '\n' ' ')"
	else
		finish fail "no RDMA devices under $IB_ROOT (run this on the GPU node; without RDMA hardware drop --rdma)"
	fi
	begin rdma.collector
	if [ -z "$COLLECTOR_URL" ] || [ -z "$COLLECTOR_TOKEN_FILE" ]; then
		finish skip "give --collector-url and --collector-token-file to check the NIC counters"
		return
	fi
	if [ "$DRY" = 1 ]; then
		plan "curl -H 'X-Gryvia-Time: ..' -H 'X-Gryvia-Signature: ..' $COLLECTOR_URL/metrics   # gryvia_nic_counter_rate present (collector run with -nic-counters)"
		finish skip "dry-run"
		return
	fi
	local tok stamp sig body
	tok="$(tr -d '\n' <"$COLLECTOR_TOKEN_FILE")"
	stamp="$(date +%s)"
	sig="$(printf 'GRYVIA-API-V1\nGET\n/metrics\n%s' "$stamp" | openssl dgst -sha256 -hmac "$tok" -r | cut -d' ' -f1)"
	body="$(curl -fsS --max-time 20 -H "X-Gryvia-Time: $stamp" -H "X-Gryvia-Signature: $sig" "$COLLECTOR_URL/metrics" 2>&1)"
	if printf '%s' "$body" | grep -q '^gryvia_nic_counter_rate'; then
		finish pass "$(printf '%s' "$body" | grep -c '^gryvia_nic_counter_rate') gryvia_nic_counter_rate series"
	else
		finish fail "no gryvia_nic_counter_rate series (is the collector started with -nic-counters?): $(printf '%s' "$body" | head -c 200)"
	fi
}

# ---- run ----
CONTEXT_NAME="$CTX"
[ "$DRY" = 1 ] && echo "dry-run: commands that would run (context ${CTX:-current}, namespace $NS)"
if check_preflight; then
	check_clusterpolicy
	check_pods
	check_allocatable
	check_smi
	check_gpunode
	check_job
else
	for n in nvidia.clusterpolicy nvidia.pods node.allocatable pod.nvidia-smi gryvia.gpunode gryvia.job; do
		begin "$n"; finish skip "preflight failed"
	done
fi
[ "$RDMA" = 1 ] && check_rdma

# ---- report ----
FINISHED="$(now)"
if [ "$DRY" = 1 ]; then RESULT=dry-run; elif [ "$FAILS" -gt 0 ]; then RESULT=fail; else RESULT=pass; fi
{
	printf '{\n  "schema": "gryvia.validate-gpu/v1",\n  "result": "%s",\n  "started": "%s",\n  "finished": "%s",\n' "$RESULT" "$STARTED" "$FINISHED"
	printf '  "context": "%s",\n  "namespace": "%s",\n  "node": "%s",\n  "checks": [\n' "$(json_str "$CONTEXT_NAME")" "$NS" "$(json_str "$NODE")"
	for i in "${!RESULTS[@]}"; do
		sep=","
		[ "$i" -eq $((${#RESULTS[@]} - 1)) ] && sep=""
		printf '    %s%s\n' "${RESULTS[$i]}" "$sep"
	done
	printf '  ]\n}\n'
} >"$REPORT"

echo
printf '%-28s %-6s %s\n' CHECK STATUS DETAIL
for i in "${!NAMES[@]}"; do
	printf '%-28s %-6s %s\n' "${NAMES[$i]}" "${STATUSES[$i]}" "$(printf '%s' "${DETAILS[$i]}" | cut -c1-110)"
done
echo
echo "result: $RESULT ($FAILS failed); report: $REPORT"
[ "$FAILS" -eq 0 ]
