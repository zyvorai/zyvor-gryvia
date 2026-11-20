#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# End-to-end checks of the GryviaAIJob run-to-completion lifecycle on a cluster that has Gryvia
# installed and no GPUs (kind). Run by .github/workflows/e2e-jobs.yml, one scenario per step:
#
#   scripts/e2e-jobs.sh success   # CPU-only 2-node job -> Succeeded, distinct RANKs, final usage record
#   scripts/e2e-jobs.sh failure   # exit 1, retryLimit 0 -> Failed
#   scripts/e2e-jobs.sh reject    # quota rejects a job -> no Job/StatefulSet/Service/PVC
#   scripts/e2e-jobs.sh cancel    # cancel running jobs (status patch like `gryvia cancel`, and the annotation)
#   scripts/e2e-jobs.sh sbatch    # `gryvia submit --sbatch`: a 2-node Slurm script and a 2-index array succeed
#                                 # (GRYVIA_CLI is the built CLI, default cli/target/release/gryvia)
#
# Needs kubectl pointed at the cluster. Every wait is bounded and prints what it saw on failure.
set -euo pipefail

API=gryvia.io/v1alpha1

fail() { echo "E2E FAIL: $*" >&2; exit 1; }

# wait_for <seconds> <description> <command...>: retry the command every 3s until it succeeds.
wait_for() {
  local secs="$1" what="$2"; shift 2
  local end=$((SECONDS + secs))
  until "$@" >/dev/null 2>&1; do
    (( SECONDS < end )) || fail "timed out after ${secs}s waiting for: $what"
    sleep 3
  done
  echo "ok: $what"
}

ensure_ns() { kubectl get ns "$1" >/dev/null 2>&1 || kubectl create ns "$1" >/dev/null; }

phase() { kubectl -n "$1" get gryviaaijob "$2" -o jsonpath='{.status.phase}' 2>/dev/null; }
is_phase() { [[ "$(phase "$1" "$2")" == "$3" ]]; }
gone() { ! kubectl -n "$1" get "$2" "$3" >/dev/null 2>&1; }
no_pods() { [[ -z "$(kubectl -n "$1" get pods -l "gryvia.io/job=$2" -o name 2>/dev/null)" ]]; }

# A CPU-only job: gpus 0 means no nvidia.com/gpu limit and no GPU placement step.
cpu_job() { # name namespace nodes retryLimit shell-script [extra spec lines]
  local name="$1" ns="$2" nodes="$3" retry="$4" script="$5" extra="${6:-}"
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaAIJob
metadata:
  name: $name
  namespace: $ns
spec:
  type: training
  image: busybox:1.36
  gpus: 0
  retryLimit: $retry
  timeout: 15m
  distributed:
    enabled: true
    framework: pytorch
    backend: gloo
    nodes: $nodes
  command: ["sh", "-c"]
  args: ["$script"]
$extra
YAML
}

quota_active() { [[ "$(kubectl get gryviaquota "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Active" ]]; }

usage_final() { # ns job -> succeeds when the job's usage record is final
  [[ "$(kubectl -n "$1" get gryviausagerecord -l "gryvia.io/job=$2" -o jsonpath='{.items[0].spec.final}' 2>/dev/null)" == "true" ]]
}

scenario_success() {
  local ns=e2e-jobs job=cpu-ok
  ensure_ns "$ns"
  # shellcheck disable=SC2016  # $RANK etc. must reach the pod's shell unexpanded
  cpu_job "$job" "$ns" 2 0 'echo rank=$RANK world=$WORLD_SIZE node_rank=$NODE_RANK master=$MASTER_ADDR; sleep 5'

  wait_for 240 "$job reaches Succeeded" is_phase "$ns" "$job" Succeeded

  # It is an Indexed batch Job (not a StatefulSet) with the headless Service as subdomain.
  [[ "$(kubectl -n "$ns" get job "$job" -o jsonpath='{.spec.completionMode}')" == "Indexed" ]] || fail "Job is not Indexed"
  [[ "$(kubectl -n "$ns" get job "$job" -o jsonpath='{.spec.completions}/{.spec.parallelism}')" == "2/2" ]] || fail "expected 2/2 completions/parallelism"
  gone "$ns" statefulset "$job-training" || fail "unexpected StatefulSet for a training job"
  kubectl -n "$ns" get svc "$job-headless" >/dev/null || fail "headless Service missing"

  # Both pods ran, with distinct completion indexes and distinct RANK values in their logs.
  local idx; idx="$(kubectl -n "$ns" get pods -l "gryvia.io/job=$job" \
    -o jsonpath='{range .items[*]}{.metadata.annotations.batch\.kubernetes\.io/job-completion-index}{"\n"}{end}' | sort | tr '\n' ' ')"
  [[ "$idx" == "0 1 " ]] || fail "completion indexes = '$idx', want '0 1 '"
  local sub; sub="$(kubectl -n "$ns" get pods -l "gryvia.io/job=$job" -o jsonpath='{.items[0].spec.subdomain}')"
  [[ "$sub" == "$job-headless" ]] || fail "pod subdomain = '$sub'"
  local logs; logs="$(kubectl -n "$ns" logs -l "gryvia.io/job=$job" --tail=-1 --prefix=false)"
  echo "$logs"
  local ranks; ranks="$(echo "$logs" | sed -n 's/^rank=\([0-9]*\) .*/\1/p' | sort | tr '\n' ' ')"
  [[ "$ranks" == "0 1 " ]] || fail "RANK values in the logs = '$ranks', want '0 1 '"
  echo "$logs" | grep -q "world=2 " || fail "WORLD_SIZE is not 2 (one process per node for a CPU-only job)"
  echo "$logs" | grep -q "master=$job-0.$job-headless.$ns.svc.cluster.local" || fail "MASTER_ADDR is not the qualified pod-0 name"

  # Status fields the usage record depends on.
  [[ -n "$(kubectl -n "$ns" get gryviaaijob "$job" -o jsonpath='{.status.startTime}')" ]] || fail "status.startTime missing"
  [[ -n "$(kubectl -n "$ns" get gryviaaijob "$job" -o jsonpath='{.status.completionTime}')" ]] || fail "status.completionTime missing"

  # The metering record is finalized (this needs a terminal phase, which the StatefulSet never reached).
  wait_for 120 "usage record of $job is final" usage_final "$ns" "$job"

  # Terminal state is sticky: it stays Succeeded and the operator does not recreate anything.
  sleep 40
  is_phase "$ns" "$job" Succeeded || fail "phase left Succeeded: $(phase "$ns" "$job")"

  # Deleting the GryviaAIJob (what DELETE /api/jobs does) garbage-collects the Job and pods.
  kubectl -n "$ns" delete gryviaaijob "$job" --wait=true >/dev/null
  wait_for 120 "Job of $job is garbage-collected" gone "$ns" job "$job"
  wait_for 120 "pods of $job are garbage-collected" no_pods "$ns" "$job"
}

scenario_failure() {
  local ns=e2e-jobs job=cpu-fail
  ensure_ns "$ns"
  cpu_job "$job" "$ns" 1 0 'echo about to fail; exit 1'
  wait_for 240 "$job reaches Failed" is_phase "$ns" "$job" Failed
  local msg; msg="$(kubectl -n "$ns" get gryviaaijob "$job" -o jsonpath='{.status.message}')"
  echo "message: $msg"
  [[ "$msg" == *BackoffLimitExceeded* ]] || fail "failure message does not carry the Job reason: '$msg'"
  [[ "$(kubectl -n "$ns" get gryviaaijob "$job" -o jsonpath='{.status.retries}')" == "1" ]] || fail "status.retries != 1"
  [[ -n "$(kubectl -n "$ns" get gryviaaijob "$job" -o jsonpath='{.status.completionTime}')" ]] || fail "status.completionTime missing"
  wait_for 120 "usage record of failed $job is final" usage_final "$ns" "$job"
  sleep 20
  is_phase "$ns" "$job" Failed || fail "phase left Failed"
}

scenario_reject() {
  local ns=e2e-reject job=reject-me
  ensure_ns "$ns"
  ensure_ns e2e-jobs

  # A GPU job passes the placement step only if a node advertises a free GPU; the kind node is
  # labelled so the placement step succeeds (no real GPU exists: its pods can never be scheduled).
  kubectl label node --all gryvia.io/gpu-count=1 --overwrite >/dev/null

  # Control: without a quota the same kind of job gets its Job, so "absent" below is meaningful.
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaAIJob
metadata: {name: ctrl-gpu, namespace: e2e-jobs}
spec: {type: training, image: busybox:1.36, gpus: 1, command: ["sleep", "600"]}
YAML
  wait_for 120 "control GPU job gets a batch Job" kubectl -n e2e-jobs get job ctrl-gpu
  kubectl -n e2e-jobs delete gryviaaijob ctrl-gpu --wait=true >/dev/null

  # The quota allows H100 only. A job without spec.gpuType passes the admission webhook (no type =
  # any) but the quota controller rejects it (GpuTypeNotAllowed).
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaQuota
metadata: {name: e2e-reject}
spec:
  team: e2e-reject
  namespaces: [$ns]
  gpuQuota: {maxGPUs: 8, allowedGPUTypes: [H100]}
YAML
  wait_for 90 "quota e2e-reject is Active" quota_active e2e-reject

  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaAIJob
metadata: {name: $job, namespace: $ns}
spec:
  type: training
  image: busybox:1.36
  gpus: 1
  storage: standard
  storageRequest: 1Gi
  command: ["sleep", "600"]
YAML
  wait_for 120 "$job is Rejected" is_phase "$ns" "$job" Rejected

  # Nothing may exist for it (an early workload that raced the rejection is torn down), and
  # nothing may come back.
  local objs=("job $job" "statefulset $job-training" "service $job-headless" "pvc $job-data") o k n
  for o in "${objs[@]}"; do
    read -r k n <<<"$o"
    wait_for 120 "$k/$n does not exist" gone "$ns" "$k" "$n"
  done
  wait_for 60 "no pods for $job" no_pods "$ns" "$job"
  sleep 30
  for o in "${objs[@]}"; do
    read -r k n <<<"$o"
    gone "$ns" "$k" "$n" || fail "$k/$n was (re)created for a Rejected job"
  done
  is_phase "$ns" "$job" Rejected || fail "Rejected job changed phase to $(phase "$ns" "$job")"
  kubectl -n "$ns" get gryviaaijob "$job" -o jsonpath='{.status.conditions}' | grep -q Rejected || fail "Rejected condition missing"
  kubectl label node --all gryvia.io/gpu-count- >/dev/null || true
}

scenario_cancel() {
  local ns=e2e-jobs
  ensure_ns "$ns"
  cpu_job cancel-status "$ns" 2 0 'sleep 600'
  cpu_job cancel-annot "$ns" 2 0 'sleep 600'
  wait_for 240 "cancel-status is Running" is_phase "$ns" cancel-status Running
  wait_for 240 "cancel-annot is Running" is_phase "$ns" cancel-annot Running

  # 1. What `gryvia cancel` does: write status.phase=Cancelled.
  kubectl -n "$ns" patch gryviaaijob cancel-status --subresource=status --type=merge \
    -p '{"status":{"phase":"Cancelled","message":"Cancelled by user"}}' >/dev/null
  # 2. The annotation route.
  kubectl -n "$ns" annotate gryviaaijob cancel-annot gryvia.io/cancel=true --overwrite >/dev/null

  for job in cancel-status cancel-annot; do
    wait_for 120 "Job of $job is deleted" gone "$ns" job "$job"
    wait_for 120 "pods of $job are gone" no_pods "$ns" "$job"
    wait_for 60 "$job is Cancelled" is_phase "$ns" "$job" Cancelled
    [[ -n "$(kubectl -n "$ns" get gryviaaijob "$job" -o jsonpath='{.status.completionTime}')" ]] || fail "$job: completionTime missing"
    wait_for 120 "usage record of cancelled $job is final" usage_final "$ns" "$job"
  done
  sleep 30
  for job in cancel-status cancel-annot; do
    gone "$ns" job "$job" || fail "Job of cancelled $job came back"
    is_phase "$ns" "$job" Cancelled || fail "$job left Cancelled"
  done
}

scenario_sbatch() {
  local ns=e2e-jobs cli="${GRYVIA_CLI:-cli/target/release/gryvia}" dir
  ensure_ns "$ns"
  dir="$(mktemp -d)"
  # shellcheck disable=SC2016  # the SLURM_* variables must reach the pod's shell unexpanded
  printf '%s\n' '#!/bin/sh' \
    '#SBATCH --job-name=slurm_two_nodes' \
    '#SBATCH --nodes=2' \
    '#SBATCH --ntasks-per-node=1' \
    '#SBATCH --time=00:10:00' \
    '#SBATCH --output=slurm-%j.out' \
    'echo "procid=$SLURM_PROCID ntasks=$SLURM_NTASKS nnodes=$SLURM_NNODES job=$SLURM_JOB_ID name=$SLURM_JOB_NAME"' \
    'srun --ntasks=1 -l echo "srun ran on $SLURM_PROCID"' \
    'echo "head=$(scontrol show hostnames "$SLURM_JOB_NODELIST" | head -n1)"' > "$dir/two.sh"
  # shellcheck disable=SC2016
  printf '%s\n' '#!/bin/sh' \
    '#SBATCH -J sweep' \
    '#SBATCH --array=0-1' \
    'echo "task=$SLURM_ARRAY_TASK_ID of $SLURM_ARRAY_TASK_COUNT array=$SLURM_ARRAY_JOB_ID"' > "$dir/array.sh"

  "$cli" submit --sbatch "$dir/two.sh" --image busybox:1.36 --dry-run > "$dir/two.yaml"
  grep -q 'timeout: 10m' "$dir/two.yaml" || fail "dry run did not map --time"
  timeout 300 "$cli" -n "$ns" submit --sbatch "$dir/two.sh" --image busybox:1.36 --wait \
    || fail "slurm-two-nodes did not succeed: $(phase "$ns" slurm-two-nodes)"
  is_phase "$ns" slurm-two-nodes Succeeded || fail "slurm-two-nodes is $(phase "$ns" slurm-two-nodes)"
  [[ "$(kubectl -n "$ns" get gryviaaijob slurm-two-nodes -o jsonpath='{.spec.distributed.nodes}')" == 2 ]] || fail "not 2 nodes"
  local logs; logs="$(kubectl -n "$ns" logs -l gryvia.io/job=slurm-two-nodes --tail=-1 --prefix=false)"
  echo "$logs"
  local procids; procids="$(echo "$logs" | sed -n 's/^procid=\([0-9]*\) .*/\1/p' | sort | tr '\n' ' ')"
  [[ "$procids" == "0 1 " ]] || fail "SLURM_PROCID values = '$procids', want '0 1 '"
  echo "$logs" | grep -q "ntasks=2 nnodes=2 job=slurm-two-nodes name=slurm_two_nodes" || fail "SLURM_NTASKS/NNODES/JOB_ID/JOB_NAME"
  [[ "$(echo "$logs" | grep -c '^srun ran on ')" == 2 ]] || fail "srun did not run its command once per pod"
  echo "$logs" | grep -q "^head=slurm-two-nodes-0.slurm-two-nodes-headless.$ns.svc" || fail "scontrol show hostnames is not pod 0"

  timeout 300 "$cli" -n "$ns" submit --sbatch "$dir/array.sh" --image busybox:1.36 --wait || fail "the array jobs did not succeed"
  for i in 0 1; do
    is_phase "$ns" "sweep-$i" Succeeded || fail "sweep-$i is $(phase "$ns" "sweep-$i")"
    kubectl -n "$ns" logs -l "gryvia.io/job=sweep-$i" --tail=-1 | grep -q "^task=$i of 2 array=sweep$" || fail "sweep-$i SLURM_ARRAY_*"
  done
  echo "ok: sbatch array"
  kubectl -n "$ns" delete gryviaaijob slurm-two-nodes sweep-0 sweep-1 --wait=true >/dev/null
}

case "${1:-}" in
  success) scenario_success ;;
  failure) scenario_failure ;;
  reject)  scenario_reject ;;
  cancel)  scenario_cancel ;;
  sbatch)  scenario_sbatch ;;
  *) echo "usage: $0 success|failure|reject|cancel|sbatch" >&2; exit 2 ;;
esac
echo "E2E OK: ${1}"
