#!/usr/bin/env bash
# End-to-end checks of the KAI Scheduler integration (docs/kai-scheduler.md) on a kind cluster that has Gryvia
# installed (scripts/kind-demo.sh with GRYVIA_LOCAL_IMAGES=1). Run by .github/workflows/e2e-kai.yml, one step each:
#
#   scripts/e2e-kai.sh install   # fake GPU operator (1 GPU), KAI Scheduler with GPU sharing, a capped queue,
#                                # Gryvia's ai-operator with --kai-scheduler
#   scripts/e2e-kai.sh place     # a CPU job is placed by KAI in its queue and Succeeds
#   scripts/e2e-kai.sh fraction  # three half-GPU jobs: two run at once on the one GPU, the third waits for a half
#   scripts/e2e-kai.sh gang      # a 3-pod job bigger than the queue's CPU limit starts no pod; raising the limit starts all
#   scripts/e2e-kai.sh all
#
# kind has no GPUs. Run:ai's fake-gpu-operator (the one KAI's own e2e uses) advertises nvidia.com/gpu and ships the
# ClusterPolicy CRD KAI's operator waits for; the GPU is not real, so this shows placement and sharing, not CUDA.
set -euo pipefail

# KAI's latest release (2026-10-11). The registry also holds an older v0.20.1 tag (2025-11) that sorts higher but
# makes one PodGroup per pod of a batch Job, so it cannot gang schedule.
KAI_VERSION="${KAI_VERSION:-v0.18.5}"
FAKE_GPU_VERSION="${FAKE_GPU_VERSION:-0.2.0}"
API=gryvia.io/v1alpha1
NS=kai-e2e
QUEUE=gryvia-e2e

fail() { echo "E2E FAIL: $*" >&2; diagnose; exit 1; }

diagnose() {
  kubectl -n "$NS" get gryviaaijobs,jobs,pods -o wide 2>/dev/null || true
  kubectl get podgroups -A 2>/dev/null | head -20 || true
  kubectl -n kai-scheduler get pods 2>/dev/null || true
  kubectl -n kai-resource-reservation get pods 2>/dev/null || true
  kubectl -n "$NS" get events --field-selector type=Warning 2>/dev/null | tail -15 || true
  kubectl -n gryvia-system logs deploy/gryvia-ai-operator --tail=40 2>/dev/null || true
}

wait_for() { # <seconds> <description> <command...>
  local secs="$1" what="$2"; shift 2
  local end=$((SECONDS + secs))
  until "$@" >/dev/null 2>&1; do
    (( SECONDS < end )) || fail "timed out after ${secs}s waiting for: $what"
    sleep 3
  done
  echo "ok: $what"
}

phase() { kubectl -n "$NS" get gryviaaijob "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
is_phase() { [[ "$(phase "$1")" == "$2" ]]; }
pods_in() { kubectl -n "$NS" get pods -l "gryvia.io/job=$1" --field-selector="status.phase=$2" -o name 2>/dev/null | wc -l | tr -d ' '; }
running_is() { [[ "$(pods_in "$1" Running)" == "$2" ]]; }

queue_limit() { # <millicores>
  kubectl apply -f - <<YAML
apiVersion: scheduling.run.ai/v2
kind: Queue
metadata: {name: ${QUEUE}}
spec:
  parentQueue: default-parent-queue
  resources:
    cpu: {quota: 0, limit: $1, overQuotaWeight: 1}
    memory: {quota: 0, limit: -1, overQuotaWeight: 1}
    gpu: {quota: 0, limit: -1, overQuotaWeight: 1}
YAML
}

job() { # <name> <nodes> <seconds>
  local dist=""
  if (( $2 > 1 )); then dist="distributed: {enabled: true, framework: pytorch, nodes: $2}"; fi
  kubectl apply -f - <<YAML
apiVersion: ${API}
kind: GryviaAIJob
metadata: {name: $1, namespace: ${NS}}
spec:
  type: training
  image: busybox:1.36
  command: ["sh", "-c", "sleep $3"]
  gpus: 0
  queueName: ${QUEUE}
  ${dist}
  resources:
    requests: {cpu: 400m, memory: 32Mi}
    limits: {memory: 64Mi}
YAML
}

half_job() { # <name>
  kubectl apply -f - <<YAML
apiVersion: ${API}
kind: GryviaAIJob
metadata:
  name: $1
  namespace: ${NS}
  annotations: {gryvia.io/gpu-share: "0.5"}
spec:
  type: training
  image: busybox:1.36
  command: ["sh", "-c", "echo gpu=\$NVIDIA_VISIBLE_DEVICES; sleep 45"]
  gpus: 1
  queueName: ${QUEUE}
  resources:
    requests: {cpu: 100m, memory: 32Mi}
    limits: {memory: 64Mi}
YAML
}

node_gpus_is() { [[ "$(kubectl get nodes -o jsonpath='{.items[*].status.allocatable.nvidia\.com/gpu}')" == "$1" ]]; }

install() {
  # KAI's operator does not start its scheduler until the nvidia.com ClusterPolicy CRD exists, and its binder reads
  # a shared GPU's size (MiB) from the node label nvidia.com/gpu.memory, which GPU feature discovery sets on real nodes.
  kubectl label nodes --all run.ai/simulated-gpu-node-pool=default nvidia.com/gpu.deploy.device-plugin=true \
    nvidia.com/gpu.memory=40960 --overwrite
  helm upgrade --install gpu-operator oci://ghcr.io/run-ai/fake-gpu-operator/fake-gpu-operator \
    --version "$FAKE_GPU_VERSION" -n gpu-operator --create-namespace --wait --timeout 300s -f - <<'YAML'
environment:
  resourceReservationNamespace: kai-resource-reservation
statusUpdater:
  disableNodeLabeling: true
migFaker: {enabled: false}
kwokGpuDevicePlugin: {enabled: false}
topology:
  nodePoolLabelKey: run.ai/simulated-gpu-node-pool
  nodePools:
    default:
      gpu:
        backend: fake
        overrides:
          device_defaults:
            name: NVIDIA-A100-SXM4-40GB
            memory: {total_bytes: 42949672960}
          devices: [ {} ]
YAML
  wait_for 180 "the node advertises 1 fake GPU" node_gpus_is 1
  helm upgrade --install kai-scheduler oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler \
    --version "$KAI_VERSION" -n kai-scheduler --create-namespace --set global.gpuSharingMode=NonMemoryEnforced --wait --timeout 600s
  wait_for 300 "KAI default queues exist" kubectl get queue default-queue
  local d
  for d in kai-scheduler-default binder admission pod-grouper podgroup-controller queue-controller; do
    wait_for 300 "KAI $d is available" kubectl -n kai-scheduler rollout status "deploy/$d" --timeout=5s
  done
  # The fake operator's status updater marks KAI's GPU reservation pods with the GPU they got.
  wait_for 120 "KAI reservation namespace exists" kubectl get namespace kai-resource-reservation
  kubectl create clusterrole pods-patcher --verb=patch --resource=pods --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n kai-resource-reservation create rolebinding fake-status-updater --clusterrole=pods-patcher \
    --serviceaccount=gpu-operator:status-updater --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n kai-scheduler get pods
  queue_limit 1000
  kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
  # Same flags as kind-demo.sh plus KAI (not --reuse-values: new chart defaults must apply).
  helm upgrade gryvia ./helm/gryvia --namespace gryvia-system --set namespace.create=false \
    --set nvidiaDevicePlugin.enabled=false --set dcgmExporter.enabled=false --set monitoring.enabled=false \
    --set global.imageTag=dev --set global.imagePullPolicy=Never \
    --set aiOperator.kaiScheduler=true --wait --timeout 300s
  kubectl -n gryvia-system rollout restart deploy/gryvia-ai-operator
  kubectl -n gryvia-system rollout status deploy/gryvia-ai-operator --timeout=180s
  kubectl -n gryvia-system get deploy gryvia-ai-operator -o jsonpath='{.spec.template.spec.containers[0].args}' |
    grep -q -- '--kai-scheduler=true' || fail "ai-operator runs without --kai-scheduler"
  echo "ok: ai-operator runs with --kai-scheduler"
}

place() {
  job kai-one 1 5
  wait_for 120 "kai-one has a pod" bash -c "[[ \$(kubectl -n $NS get pods -l gryvia.io/job=kai-one -o name | wc -l) -ge 1 ]]"
  local pod; pod="$(kubectl -n "$NS" get pods -l gryvia.io/job=kai-one -o name | head -1)"
  [[ "$(kubectl -n "$NS" get "$pod" -o jsonpath='{.spec.schedulerName}')" == kai-scheduler ]] || fail "pod not handed to kai-scheduler"
  [[ "$(kubectl -n "$NS" get "$pod" -o jsonpath='{.metadata.labels.kai\.scheduler/queue}')" == "$QUEUE" ]] || fail "pod has no KAI queue label"
  echo "ok: pod uses kai-scheduler in queue $QUEUE"
  wait_for 180 "kai-one Succeeded" is_phase kai-one Succeeded
  kubectl -n "$NS" get gryviaaijob kai-one -o jsonpath='{.status.conditions[?(@.type=="Scheduled")].message}'; echo
}

HALVES="gryvia.io/job in (kai-half-a,kai-half-b,kai-half-c)"
halves_running() { kubectl -n "$NS" get pods -l "$HALVES" --field-selector=status.phase=Running -o name 2>/dev/null; }
halves_running_is() { [[ "$(halves_running | wc -l | tr -d ' ')" == "$1" ]]; }
gpu_of() { kubectl -n "$NS" logs "$1" 2>/dev/null | sed -n 's/^gpu=//p' | head -1; }
has_gpu() { [[ -n "$(gpu_of "$1")" ]]; }

fraction() {
  for j in kai-half-a kai-half-b kai-half-c; do half_job "$j"; done
  wait_for 120 "the three half-GPU jobs have pods" bash -c \
    "[[ \$(kubectl -n $NS get pods -l '$HALVES' -o name | wc -l) -eq 3 ]]"
  local pod
  for pod in $(kubectl -n "$NS" get pods -l "$HALVES" -o name); do
    [[ "$(kubectl -n "$NS" get "$pod" -o jsonpath='{.metadata.annotations.gpu-fraction}')" == 0.5 ]] ||
      fail "$pod has no gpu-fraction=0.5 annotation"
    [[ -z "$(kubectl -n "$NS" get "$pod" -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}')" ]] ||
      fail "$pod still has an nvidia.com/gpu limit"
  done
  echo "ok: pods carry gpu-fraction=0.5 and no nvidia.com/gpu limit"
  wait_for 240 "two half-GPU jobs Running at once" halves_running_is 2
  local running=() gpus=()
  mapfile -t running < <(halves_running)
  for pod in "${running[@]}"; do
    wait_for 30 "$pod reports its GPU" has_gpu "$pod"
    gpus+=("$(gpu_of "$pod")")
  done
  [[ "${gpus[0]}" == "${gpus[1]}" ]] || fail "the two halves got different GPUs: ${gpus[*]}"
  echo "ok: ${running[*]} share ${gpus[0]}"
  local end=$((SECONDS + 20))
  while (( SECONDS < end )); do
    (( $(halves_running | wc -l) <= 2 )) || fail "three halves ran on one GPU"
    sleep 2
  done
  echo "ok: the third half waits while the GPU is full"
  for j in kai-half-a kai-half-b kai-half-c; do wait_for 240 "$j Succeeded" is_phase "$j" Succeeded; done
}

gang() {
  queue_limit 1000
  job kai-gang 3 20 # 3 x 400m = 1200m > 1000m
  wait_for 120 "kai-gang has 3 pods" bash -c "[[ \$(kubectl -n $NS get pods -l gryvia.io/job=kai-gang -o name | wc -l) -eq 3 ]]"
  [[ "$(kubectl -n "$NS" get job kai-gang -o jsonpath='{.metadata.annotations.kai\.scheduler/batch-min-member}')" == 3 ]] ||
    fail "batch Job has no kai.scheduler/batch-min-member=3"
  wait_for 60 "KAI made one PodGroup of 3 for kai-gang" bash -c \
    "[[ \$(kubectl -n $NS get podgroups -o jsonpath='{range .items[*]}{.metadata.name} {.spec.minMember}{\"\\n\"}{end}' | grep -c '^pg-kai-gang-.* 3\$') -eq 1 && \$(kubectl -n $NS get podgroups -o name | grep -c kai-gang) -eq 1 ]]"
  local end=$((SECONDS + 30))
  while (( SECONDS < end )); do
    [[ "$(pods_in kai-gang Running)" == 0 ]] || fail "a gang larger than the queue limit started partially"
    sleep 3
  done
  echo "ok: no pod of kai-gang started for 30s while the gang does not fit"
  queue_limit 2000
  wait_for 180 "all 3 pods of kai-gang Running" running_is kai-gang 3
  wait_for 180 "kai-gang Succeeded" is_phase kai-gang Succeeded
}

case "${1:-all}" in
  install) install ;;
  place) place ;;
  fraction) fraction ;;
  gang) gang ;;
  all) install; place; fraction; gang ;;
  *) echo "usage: $0 install|place|fraction|gang|all" >&2; exit 2 ;;
esac
