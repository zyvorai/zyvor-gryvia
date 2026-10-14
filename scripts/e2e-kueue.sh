#!/usr/bin/env bash
# End-to-end checks of the Kueue integration (docs/kueue-integration.md) on a kind cluster that has Gryvia
# installed WITH Kueue and both operator flags on. Run by .github/workflows/e2e-kueue.yml, one scenario per step:
#
#   scripts/e2e-kueue.sh setup     # tenant q1 -> ClusterQueue/LocalQueue/flavors; fake "slot" resource on the node
#   scripts/e2e-kueue.sh queue     # two jobs that do not fit together: first Running, second Queued, then Succeeded
#   scripts/e2e-kueue.sh gang      # a gang bigger than the quota never starts partially (zero pods), then starts whole
#   scripts/e2e-kueue.sh preempt   # a higher-priority job preempts a running lower-priority one, which is requeued
#   scripts/e2e-kueue.sh preempt-checkpoint  # the preempted job checkpoints in its preStop hook and resumes from it
#   scripts/e2e-kueue.sh elastic   # an elastic job is partially admitted (minNodes..nodes); its success policy completes it
#   scripts/e2e-kueue.sh elastic-torchrun  # the same with a real torchrun: the admitted workers train as one group
#
# Passes in CI (e2e-kueue.yml) with real Kueue on kind. The lines marked "ASSUMPTION" held there; they are still the
# first things to look at if a step fails on another Kueue or Kubernetes version.
#
# Quota resource: kind has no GPUs and its CPU is too small to give a nominal quota that means something, so the
# quota operator runs with --kueue-quota-resources=example.com/slot and the node advertises 100 fake slots (the
# documented "extended resource" node status patch). A job pod asks for one slot and tiny cpu/memory, so the
# scheduler is never the bottleneck: only Kueue's quota is. Tenant q1 has concurrentGPUs: 2 = 2 slots.
set -euo pipefail

API=gryvia.io/v1alpha1
NS=tenant-q1
CQ=gryvia-q1
SLOT=example.com/slot

fail() { echo "E2E FAIL: $*" >&2; exit 1; }

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
message() { kubectl -n "$NS" get gryviaaijob "$1" -o jsonpath='{.status.message}' 2>/dev/null; }
pod_count() { kubectl -n "$NS" get pods -l "gryvia.io/job=$1" -o name 2>/dev/null | wc -l | tr -d ' '; }
no_pods() { [[ "$(pod_count "$1")" == "0" ]]; }
# Finished pods may remain for logs; only non-terminal pods still hold resources.
no_active_pods() {
  kubectl -n "$NS" get pods -l "gryvia.io/job=$1" -o json |
    jq -e 'all(.items[]; .status.phase == "Succeeded" or .status.phase == "Failed")' >/dev/null
}
job_suspended() { [[ "$(kubectl -n "$NS" get job "$1" -o jsonpath='{.spec.suspend}' 2>/dev/null)" == "true" ]]; }
job_unsuspended() { [[ "$(kubectl -n "$NS" get job "$1" -o jsonpath='{.spec.suspend}' 2>/dev/null)" == "false" ]]; }
gone() { ! kubectl -n "$NS" get "$1" "$2" >/dev/null 2>&1; }
msg_matches() { [[ "$(message "$1")" =~ $2 ]]; }

# The Kueue Workload owned by the batch Job of a GryviaAIJob (Kueue names it job-<name>-<hash>).
wl_field() { # <job> <jq expression on the workload>
  kubectl -n "$NS" get workloads -o json |
    jq -r --arg j "$1" "[.items[] | select(any(.metadata.ownerReferences[]?; .kind==\"Job\" and .name==\$j))][0] | $2"
}
cq_field() { kubectl get clusterqueue "$CQ" -o jsonpath="$1" 2>/dev/null; }
rec_field() { kubectl -n "$NS" get gryviausagerecord "$1" -o jsonpath="$2" 2>/dev/null; }
rec_is() { [[ "$(rec_field "$1" "$2")" == "$3" ]]; }

kq_job() { # <name> <nodes> <priority> <sleep-seconds>
  local name="$1" nodes="$2" prio="$3" secs="$4"
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaAIJob
metadata: {name: $name, namespace: $NS}
spec:
  type: training
  image: busybox:1.36
  gpus: 0
  priority: $prio
  retryLimit: 0
  timeout: 20m
  distributed: {enabled: true, framework: pytorch, backend: gloo, nodes: $nodes}
  resources:
    requests: {cpu: 20m, memory: 16Mi, $SLOT: "1"}
    limits: {cpu: 200m, memory: 64Mi, $SLOT: "1"}
  command: ["sh", "-c"]
  args: ["echo rank=\$RANK; sleep $secs"]
YAML
}

cq_idle() { # nothing admitted or pending in the ClusterQueue
  [[ "$(cq_field '{.status.reservingWorkloads}')" =~ ^0?$ && "$(cq_field '{.status.pendingWorkloads}')" =~ ^0?$ ]]
}

scenario_setup() {
  # ASSUMPTION: a single kind node; the extended-resource capacity patch survives kubelet status updates
  # (it is the documented way to advertise a device-plugin-less resource).
  for n in $(kubectl get nodes -o name | sed 's#node/##'); do
    kubectl patch node "$n" --subresource=status --type=json \
      -p "[{\"op\":\"add\",\"path\":\"/status/capacity/example.com~1slot\",\"value\":\"100\"}]" >/dev/null
  done
  wait_for 60 "node advertises 100 $SLOT" bash -c "kubectl get nodes -o jsonpath='{.items[0].status.allocatable.example\\.com/slot}' | grep -qx 100"

  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaTenant
metadata: {name: q1}
spec:
  displayName: Kueue e2e
  quotas: {concurrentGPUs: 2}
YAML
  wait_for 120 "namespace $NS exists" kubectl get ns "$NS"
  wait_for 120 "ClusterQueue $CQ exists" kubectl get clusterqueue "$CQ"
  wait_for 120 "LocalQueue $NS/gryvia exists" kubectl -n "$NS" get localqueue gryvia
  wait_for 60 "ResourceFlavor gryvia-default exists" kubectl get resourceflavor gryvia-default
  wait_for 120 "ClusterQueue $CQ is Active" \
    bash -c "[[ \"\$(kubectl get clusterqueue $CQ -o jsonpath='{.status.conditions[?(@.type==\"Active\")].status}')\" == True ]]"

  # The objects are what docs/kueue-integration.md says.
  # v1beta2 (Kueue >= 0.15) calls the field cohortName, v1beta1 calls it cohort.
  [[ "$(cq_field '{.spec.cohortName}')$(cq_field '{.spec.cohort}')" == "gryvia" ]] || fail "cohort != gryvia"
  [[ "$(cq_field '{.spec.namespaceSelector.matchLabels.kubernetes\.io/metadata\.name}')" == "$NS" ]] || fail "namespaceSelector"
  [[ "$(cq_field '{.spec.preemption.withinClusterQueue}')" == "LowerPriority" ]] || fail "preemption.withinClusterQueue"
  [[ "$(cq_field '{.spec.resourceGroups[0].flavors[0].resources[0].name}')" == "$SLOT" ]] || fail "quota resource != $SLOT"
  [[ "$(cq_field '{.spec.resourceGroups[0].flavors[0].resources[0].nominalQuota}')" == "2" ]] || fail "nominalQuota != 2"
  [[ "$(kubectl -n "$NS" get localqueue gryvia -o jsonpath='{.spec.clusterQueue}')" == "$CQ" ]] || fail "LocalQueue points elsewhere"
}

# Per-tenant fairness settings reach Kueue and are enforced: a tenant with borrowingLimit 0 stays inside its
# quota although a cohort neighbour is idle, while a neighbour without a limit borrows that idle quota. The
# Queued job also explains itself in status.queueExplanation. Works with or without Kueue's fairSharing mode
# (the weight is only asserted when the quota operator was started with --kueue-fair-sharing).
scenario_fairshare() {
  local cap_ns=tenant-fs-cap free_ns=tenant-fs-free
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaTenant
metadata: {name: fs-cap}
spec:
  displayName: capped
  quotas: {concurrentGPUs: 1, borrowingLimit: 0, fairShareWeight: 3, cohort: fs-e2e}
---
apiVersion: $API
kind: GryviaTenant
metadata: {name: fs-free}
spec:
  displayName: unrestricted
  quotas: {concurrentGPUs: 1, cohort: fs-e2e}
YAML
  wait_for 120 "ClusterQueue gryvia-fs-cap exists" kubectl get clusterqueue gryvia-fs-cap
  wait_for 120 "ClusterQueue gryvia-fs-free exists" kubectl get clusterqueue gryvia-fs-free
  wait_for 120 "LocalQueue $cap_ns/gryvia exists" kubectl -n "$cap_ns" get localqueue gryvia
  wait_for 120 "LocalQueue $free_ns/gryvia exists" kubectl -n "$free_ns" get localqueue gryvia

  local cq
  cq() { kubectl get clusterqueue "$1" -o jsonpath="$2" 2>/dev/null; }
  [[ "$(cq gryvia-fs-cap '{.spec.cohortName}{.spec.cohort}')" == "fs-e2e" ]] || fail "fs-cap cohort != fs-e2e"
  [[ "$(cq gryvia-fs-cap '{.spec.resourceGroups[0].flavors[0].resources[0].borrowingLimit}')" == "0" ]] || fail "fs-cap borrowingLimit != 0"
  [[ -z "$(cq gryvia-fs-free '{.spec.resourceGroups[0].flavors[0].resources[0].borrowingLimit}')" ]] || fail "fs-free got a borrowingLimit"
  local weight; weight="$(cq gryvia-fs-cap '{.spec.fairSharing.weight}')"
  if [[ -n "$weight" ]]; then [[ "$weight" == "3" ]] || fail "fs-cap fairSharing weight $weight != 3"; echo "ok: fairSharing weight 3"; fi
  wait_for 120 "gryvia-fs-cap is Active" \
    bash -c "[[ \"\$(kubectl get clusterqueue gryvia-fs-cap -o jsonpath='{.status.conditions[?(@.type==\"Active\")].status}')\" == True ]]"

  local saved_ns="$NS"
  # 2 slots against quota 1 and borrowingLimit 0, with fs-free idle: must stay Queued.
  NS="$cap_ns"; kq_job capped 2 0 30
  wait_for 120 "capped is Queued" is_phase capped Queued
  sleep 15
  is_phase capped Queued || fail "capped left Queued although borrowingLimit is 0 (phase $(phase capped))"
  job_suspended capped || fail "capped: Job was unsuspended past its borrowing limit"
  [[ "$(pod_count capped)" == "0" ]] || fail "capped created pods"
  wait_for 60 "capped explains itself (QuotaExhausted)" \
    bash -c "[[ \"\$(kubectl -n $cap_ns get gryviaaijob capped -o jsonpath='{.status.queueExplanation.category}')\" == QuotaExhausted ]]"
  [[ "$(kubectl -n "$cap_ns" get gryviaaijob capped -o jsonpath='{.status.queueExplanation.clusterQueue}')" == "gryvia-fs-cap" ]] || fail "queueExplanation names the wrong ClusterQueue"

  # 2 slots against quota 1 and no limit: borrows the idle slot of fs-cap and runs.
  NS="$free_ns"; kq_job borrower 2 0 30
  wait_for 300 "borrower reaches Running (borrowed the idle slot)" is_phase borrower Running
  [[ "$(pod_count borrower)" == "2" ]] || fail "borrower: expected 2 pods, got $(pod_count borrower)"

  NS="$cap_ns"; kubectl -n "$cap_ns" delete gryviaaijob capped --wait=true --timeout=120s >/dev/null
  NS="$free_ns"; kubectl -n "$free_ns" delete gryviaaijob borrower --wait=true --timeout=120s >/dev/null
  NS="$saved_ns"
  kubectl delete gryviatenant fs-cap fs-free --wait=true --timeout=120s >/dev/null
  echo "ok: fair-share limits, cohort and queue explanation"
}

scenario_strict() {
  # A namespace with no LocalQueue must never bypass strict admission.
  kubectl create namespace tenant-no-queue --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaAIJob
metadata: {name: blocked, namespace: tenant-no-queue}
spec:
  type: training
  image: busybox:1.36
  gpus: 0
  command: ["sh", "-c", "echo should-not-run"]
YAML
  wait_for 120 "missing queue leaves job suspended" bash -c '[[ "$(kubectl -n tenant-no-queue get job blocked -o jsonpath="{.spec.suspend}")" == true ]]'
  [[ "$(kubectl -n tenant-no-queue get pods -l gryvia.io/job=blocked -o name)" == "" ]] || fail "missing queue created pods"
  [[ "$(kubectl -n tenant-no-queue get job blocked -o jsonpath='{.metadata.labels.kueue\.x-k8s\.io/queue-name}')" == gryvia ]] || fail "missing default queue label"
  kubectl delete namespace tenant-no-queue --wait=true --timeout=120s >/dev/null
}

scenario_queue() {
  # Two 2-slot jobs against a 2-slot quota. No spec.queueName: the default LocalQueue of the tenant namespace is used.
  kq_job qa 2 0 75
  wait_for 300 "qa reaches Running" is_phase qa Running
  [[ "$(kubectl -n "$NS" get job qa -o jsonpath="{.metadata.labels.kueue\.x-k8s\.io/queue-name}")" == "gryvia" ]] || fail "qa: no default queue label"
  job_unsuspended qa || fail "qa: Kueue did not unsuspend the Job"
  [[ "$(pod_count qa)" == "2" ]] || fail "qa: expected 2 pods, got $(pod_count qa)"

  kq_job qb 2 0 10
  wait_for 120 "qb is Queued" is_phase qb Queued
  # ASSUMPTION: Kueue's pending message is non-empty free text; the operator prefixes it.
  wait_for 60 "qb message comes from Kueue" msg_matches qb '^(Queued by Kueue|Waiting for Kueue)'
  echo "qb message: $(message qb)"
  job_suspended qb || fail "qb: Job must stay suspended while queued"
  no_pods qb || fail "qb: pods exist while queued"
  [[ "$(wl_field qb '.status.conditions[]? | select(.type=="QuotaReserved") | .status')" == "False" ]] || fail "qb: Workload has quota reserved"
  wait_for 90 "ClusterQueue reports 1 pending workload" bash -c "[[ \"\$(kubectl get clusterqueue $CQ -o jsonpath='{.status.pendingWorkloads}')\" == 1 ]]"
  is_phase qa Running || fail "qa left Running while qb waited"

  # When qa finishes its quota is released and qb is admitted (fair, first-come admission).
  wait_for 300 "qa reaches Succeeded" is_phase qa Succeeded
  wait_for 300 "qb is admitted and Running or Succeeded" bash -c "p=\$(kubectl -n $NS get gryviaaijob qb -o jsonpath='{.status.phase}'); [[ \$p == Running || \$p == Succeeded ]]"
  wait_for 300 "qb reaches Succeeded" is_phase qb Succeeded
  kubectl -n "$NS" delete gryviaaijob qa qb --wait=true >/dev/null
  wait_for 120 "ClusterQueue idle" cq_idle
}

scenario_gang() {
  # 5 pods, quota 2: the whole gang must wait. Without Kueue the scheduler would happily start all five pods
  # (the node has 100 slots), so "zero pods" can only come from gang admission.
  kq_job gang 5 0 20
  wait_for 120 "gang is Queued" is_phase gang Queued
  for _ in 1 2 3 4 5 6 7 8; do
    no_pods gang || fail "gang: $(pod_count gang) of 5 pods exist while the gang cannot be admitted (must be all-or-nothing)"
    job_suspended gang || fail "gang: Job was unsuspended although it does not fit"
    is_phase gang Queued || fail "gang: phase $(phase gang), want Queued"
    sleep 5
  done
  echo "gang message: $(message gang)"

  # Grow the tenant quota to 5: the quota operator updates the ClusterQueue, Kueue admits the whole gang at once.
  kubectl patch gryviatenant q1 --type=merge -p '{"spec":{"quotas":{"concurrentGPUs":5}}}' >/dev/null
  wait_for 120 "ClusterQueue nominalQuota is 5" bash -c "[[ \"\$(kubectl get clusterqueue $CQ -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[0].nominalQuota}')\" == 5 ]]"
  wait_for 300 "gang is Running" is_phase gang Running
  [[ "$(pod_count gang)" == "5" ]] || fail "gang: expected all 5 pods, got $(pod_count gang)"
  wait_for 300 "gang reaches Succeeded" is_phase gang Succeeded
  kubectl -n "$NS" delete gryviaaijob gang --wait=true >/dev/null

  kubectl patch gryviatenant q1 --type=merge -p '{"spec":{"quotas":{"concurrentGPUs":2}}}' >/dev/null
  wait_for 120 "ClusterQueue nominalQuota is back to 2" bash -c "[[ \"\$(kubectl get clusterqueue $CQ -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[0].nominalQuota}')\" == 2 ]]"
  wait_for 120 "ClusterQueue idle" cq_idle
}

scenario_preempt() {
  kq_job low 2 10 600
  wait_for 300 "low reaches Running" is_phase low Running
  kq_job high 2 90 90

  # Priority classes: spec.priority 10 -> gryvia-priority-10, 90 -> gryvia-priority-90 (created by the ai-operator).
  wait_for 60 "WorkloadPriorityClass gryvia-priority-90 exists" kubectl get workloadpriorityclass gryvia-priority-90
  kubectl get workloadpriorityclass gryvia-priority-10 >/dev/null || fail "gryvia-priority-10 missing"

  # LowerPriority preemption inside the ClusterQueue evicts low; its Job is suspended again and it goes back to Queued.
  wait_for 240 "low is requeued (Queued)" is_phase low Queued
  # ASSUMPTION: the operator turns "was admitted, is suspended again" into this message whatever Kueue's conditions say.
  wait_for 30 "low message says it was evicted" msg_matches low '^Evicted by Kueue'
  echo "low message: $(message low)"
  wait_for 60 "low has no non-terminal pods" no_active_pods low
  job_suspended low || fail "low: Job must be suspended after eviction"
  # Metering: the first run's usage record is closed at the eviction; no run is open while low is queued.
  local uid; uid="$(kubectl -n "$NS" get gryviaaijob low -o jsonpath='{.metadata.uid}')"
  wait_for 90 "low's first usage record is final" rec_is "usage-$uid" '{.spec.final}' true
  gone gryviausagerecord "usage-$uid-2" || fail "low: a second usage record exists while it is queued"
  [[ "$(wl_field high '.spec.priority')" == "90" ]] || fail "high workload priority = $(wl_field high '.spec.priority'), want 90"
  [[ "$(wl_field low '.spec.priority')" == "10" ]] || fail "low workload priority = $(wl_field low '.spec.priority'), want 10"

  wait_for 300 "high reaches Running or Succeeded" bash -c "p=\$(kubectl -n $NS get gryviaaijob high -o jsonpath='{.status.phase}'); [[ \$p == Running || \$p == Succeeded ]]"
  wait_for 300 "high reaches Succeeded" is_phase high Succeeded
  # Kueue requeues the evicted workload: with the quota free again low runs again (not terminal Preempted).
  wait_for 300 "low is admitted again and Running" is_phase low Running
  wait_for 90 "low's second run has an open usage record" rec_is "usage-$uid-2" '{.spec.final}' false

  kubectl -n "$NS" annotate gryviaaijob low gryvia.io/cancel=true --overwrite >/dev/null
  wait_for 120 "low is Cancelled" is_phase low Cancelled
  wait_for 90 "low's second usage record is final" rec_is "usage-$uid-2" '{.spec.final}' true
  # The queued time between the runs is in neither record.
  local end1 start2
  end1="$(rec_field "usage-$uid" '{.spec.end}')"
  start2="$(rec_field "usage-$uid-2" '{.spec.start}')"
  echo "low: first run ended $end1, second run started $start2"
  [[ -n "$end1" && ! "$start2" < "$end1" ]] || fail "low: second run starts ($start2) before the first ended ($end1)"
  kubectl -n "$NS" delete gryviaaijob high low --ignore-not-found --wait=true >/dev/null
}

scenario_preempt_checkpoint() {
  # A preempted job checkpoints through its preStop hook (gryvia.io/checkpoint-command) while Kueue evicts it, and
  # resumes from that checkpoint when it is admitted again. The trainer saves ONLY when the hook asks, so a resume
  # from a step > 0 proves the hook ran during the eviction. retryLimit 0: the eviction must not count as a failure.
  # The job PVC is ReadWriteMany; one kind node, so a static hostPath PV of class e2e-rwx serves it.
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolume
metadata: {name: e2e-kueue-ckpt}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteMany]
  storageClassName: e2e-rwx
  persistentVolumeReclaimPolicy: Retain
  hostPath: {path: /tmp/e2e-kueue-ckpt, type: DirectoryOrCreate}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: ck-scripts, namespace: $NS}
data:
  train.sh: |
$(sed 's/^/    /' <<'SH'
d="$GRYVIA_CHECKPOINT_DIR"; r="${RANK:-0}"; total="${TOTAL_STEPS:-90}"
mkdir -p "$d"
step=0
if [ "$GRYVIA_RESUME_IF_PRESENT" = true ] && [ -f "$d/step-$r" ]; then step="$(cat "$d/step-$r")"; fi
echo "rank $r: starting at step $step"
saved=""
trap 'exit 0' TERM
while [ "$step" -lt "$total" ]; do
  sleep 1
  [ -n "$saved" ] && continue
  step=$((step + 1))
  if [ -f "$d/request-$r" ]; then
    echo "$step" > "$d/step-$r.tmp" && mv "$d/step-$r.tmp" "$d/step-$r"
    rm -f "$d/request-$r"; echo "$step" > "$d/ack-$r"; saved=1
    echo "rank $r: checkpointed step $step on request"
  fi
done
echo "rank $r: done at step $step"
SH
)
  hook.sh: |
$(sed 's/^/    /' <<'SH'
d="$GRYVIA_CHECKPOINT_DIR"; r="${RANK:-0}"
rm -f "$d/ack-$r"; touch "$d/request-$r"
i=0
while [ ! -f "$d/ack-$r" ] && [ "$i" -lt 25 ]; do sleep 1; i=$((i + 1)); done
SH
)
---
apiVersion: $API
kind: GryviaAIJob
metadata:
  name: ck
  namespace: $NS
  annotations:
    gryvia.io/checkpoint-command: '["sh", "/scripts/hook.sh"]'
    gryvia.io/checkpoint-grace-seconds: "60"
spec:
  type: training
  image: busybox:1.36
  gpus: 0
  priority: 10
  retryLimit: 0
  timeout: 20m
  storage: e2e-rwx
  storageRequest: 1Gi
  distributed: {enabled: true, framework: pytorch, backend: gloo, nodes: 2}
  resources:
    requests: {cpu: 20m, memory: 16Mi, $SLOT: "1"}
    limits: {cpu: 200m, memory: 64Mi, $SLOT: "1"}
  command: ["sh", "/scripts/train.sh"]
  env: [{name: TOTAL_STEPS, value: "90"}]
  volumes: [{name: scripts, configMap: {name: ck-scripts}}]
  volumeMounts: [{name: scripts, mountPath: /scripts}]
YAML
  ck_logs() { kubectl -n "$NS" logs -l gryvia.io/job=ck --tail=-1 --max-log-requests=10 2>/dev/null; }
  ck_started() { [[ "$(ck_logs | grep -c 'starting at step 0$')" == 2 ]]; }
  wait_for 300 "ck reaches Running" is_phase ck Running
  [[ "$(kubectl -n "$NS" get pvc ck-data -o jsonpath='{.spec.volumeName}')" == e2e-kueue-ckpt ]] || fail "ck: PVC not bound to the e2e PV"
  wait_for 120 "both ck ranks train from step 0" ck_started
  sleep 10

  kq_job ckhigh 2 90 30
  wait_for 240 "ck is requeued (Queued)" is_phase ck Queued
  wait_for 90 "ck has no non-terminal pods" no_active_pods ck
  wait_for 300 "ckhigh reaches Succeeded" is_phase ckhigh Succeeded

  ck_resumed() { [[ "$(ck_logs | grep -cE 'starting at step [1-9][0-9]*$')" == 2 ]]; }
  wait_for 300 "both ck ranks resume from the checkpoint their hook requested" ck_resumed
  ck_logs | grep 'starting at step'
  wait_for 300 "ck reaches Succeeded (the eviction did not consume retryLimit 0)" is_phase ck Succeeded
  [[ "$(ck_logs | grep -c 'done at step 90$')" == 2 ]] || fail "ck: not both ranks finished at step 90: $(ck_logs)"

  kubectl -n "$NS" delete gryviaaijob ck ckhigh --wait=true >/dev/null
  kubectl -n "$NS" delete configmap ck-scripts --ignore-not-found >/dev/null
  kubectl -n "$NS" delete pvc ck-data --ignore-not-found --wait=true --timeout=60s >/dev/null || true
  kubectl delete pv e2e-kueue-ckpt --ignore-not-found --wait=false >/dev/null
  wait_for 120 "ClusterQueue idle" cq_idle
}

el_job() { # <name> <nodes> <minNodes> <script>
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaAIJob
metadata: {name: $1, namespace: $NS}
spec:
  type: training
  image: busybox:1.36
  gpus: 0
  retryLimit: 0
  timeout: 20m
  distributed: {enabled: true, framework: pytorch, backend: gloo, nodes: $2, elastic: {minNodes: $3}}
  resources:
    requests: {cpu: 20m, memory: 16Mi, $SLOT: "1"}
    limits: {cpu: 200m, memory: 64Mi, $SLOT: "1"}
  command: ["sh", "-c"]
  args: ['$4']
YAML
}
job_field() { kubectl -n "$NS" get job "$1" -o jsonpath="$2" 2>/dev/null; }

scenario_elastic() {
  # Partial admission: 4 workers asked, 2 slots of quota, minNodes 2. Kueue admits 2 instead of queueing the job,
  # and lowers completions with parallelism so the Job (and its success policy) count 2 workers.
  # shellcheck disable=SC2016 # expanded in the pod
  el_job el 4 2 'echo index=$JOB_COMPLETION_INDEX nnodes=$NNODES; sleep 20'
  wait_for 60 "the operator creates el's batch Job" kubectl -n "$NS" get job el
  [[ "$(job_field el '{.metadata.annotations.kueue\.x-k8s\.io/job-min-parallelism}')" == 2 ]] || fail "el: no min-parallelism annotation"
  [[ "$(job_field el '{.metadata.annotations.kueue\.x-k8s\.io/job-completions-equal-parallelism}')" == true ]] || fail "el: no completions annotation"
  wait_for 300 "el reaches Running with fewer workers than asked" is_phase el Running
  [[ "$(job_field el '{.spec.parallelism}/{.spec.completions}')" == 2/2 ]] || fail "el: parallelism/completions $(job_field el '{.spec.parallelism}/{.spec.completions}'), want 2/2"
  [[ "$(wl_field el '.status.admission.podSetAssignments[0].count')" == 2 ]] || fail "el: Workload admitted $(wl_field el '.status.admission.podSetAssignments[0].count') pods, want 2"
  [[ "$(job_field el '{.spec.successPolicy.rules[0].succeededCount}')" == 2 ]] || fail "el: success policy changed"
  sleep 10
  [[ "$(pod_count el)" == 2 ]] || fail "el: $(pod_count el) pods, want 2 (indexes 2 and 3 must not exist)"
  kubectl -n "$NS" get pods -l gryvia.io/job=el -L batch.kubernetes.io/job-completion-index
  wait_for 300 "el reaches Succeeded" is_phase el Succeeded
  kubectl -n "$NS" logs -l gryvia.io/job=el --prefix | sort
  kubectl -n "$NS" delete gryviaaijob el --wait=true >/dev/null
  wait_for 120 "ClusterQueue idle" cq_idle

  # The success policy under Kueue: 2 workers fit the quota and are both admitted; index 0 finishes, index 1 would
  # run for 10 minutes. succeededCount 1 completes the Job, the running pod is stopped and the quota is released.
  # shellcheck disable=SC2016 # expanded in the pod
  el_job sp 2 1 'if [ "$JOB_COMPLETION_INDEX" = 0 ]; then sleep 10; echo done; else sleep 600; fi'
  wait_for 300 "sp reaches Running" is_phase sp Running
  [[ "$(job_field sp '{.spec.parallelism}')" == 2 ]] || fail "sp: parallelism $(job_field sp '{.spec.parallelism}'), want 2 (it fits)"
  wait_for 180 "sp Succeeded through the success policy" is_phase sp Succeeded
  [[ "$(job_field sp '{.status.conditions[?(@.type=="SuccessCriteriaMet")].status}')" == True ]] || fail "sp: no SuccessCriteriaMet"
  [[ "$(job_field sp '{.status.succeeded}')" == 1 ]] || fail "sp: succeeded $(job_field sp '{.status.succeeded}'), want 1"
  wait_for 120 "sp's Workload is Finished" bash -c "[[ \"\$(kubectl -n $NS get workloads -o json | jq -r '[.items[] | select(any(.metadata.ownerReferences[]?; .kind==\"Job\" and .name==\"sp\"))][0].status.conditions[]? | select(.type==\"Finished\") | .status')\" == True ]]"
  wait_for 120 "sp has no running pods" no_active_pods sp
  wait_for 120 "ClusterQueue idle (quota released)" cq_idle
  kubectl -n "$NS" delete gryviaaijob sp --wait=true >/dev/null
}

scenario_elastic_torchrun() {
  # The real trainer (examples/training/elastic_train.py, image built by the workflow) under partial admission:
  # 4 workers asked, 2 slots. torchrun's NNODES=2:4 must form a group of the 2 admitted workers and train to the end.
  # One kind node, so a ReadWriteOnce volume is shared by both pods for the coordinated checkpoints.
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: el-ckpt, namespace: $NS}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}
---
apiVersion: $API
kind: GryviaAIJob
metadata: {name: eltr, namespace: $NS}
spec:
  type: training
  image: ghcr.io/zyvorai/gryvia-elastic-train:dev
  imagePullPolicy: IfNotPresent
  gpus: 0
  retryLimit: 2
  timeout: 20m
  distributed: {enabled: true, framework: pytorch, backend: gloo, nodes: 4, elastic: {minNodes: 2}}
  command: [torchrun]
  args:
    - --nnodes=\$(NNODES)
    - --nproc_per_node=\$(NPROC_PER_NODE)
    - --rdzv_backend=c10d
    - --rdzv_endpoint=\$(MASTER_ADDR):\$(MASTER_PORT)
    - --rdzv_conf=last_call_timeout=10
    - --rdzv_id=eltr
    - --max_restarts=2
    - /app/elastic_train.py
  env:
    - {name: CHECKPOINT_DIR, value: /ckpt}
    - {name: TOTAL_STEPS, value: "20"}
    - {name: STEP_SECONDS, value: "0.5"}
    - {name: COLLECTIVE_TIMEOUT, value: "60"}
  resources:
    requests: {cpu: 100m, memory: 384Mi, $SLOT: "1"}
    limits: {cpu: "1", memory: 1536Mi, $SLOT: "1"}
  volumes: [{name: ckpt, persistentVolumeClaim: {claimName: el-ckpt}}]
  volumeMounts: [{name: ckpt, mountPath: /ckpt}]
YAML
  wait_for 300 "eltr reaches Running" is_phase eltr Running
  [[ "$(job_field eltr '{.spec.parallelism}/{.spec.completions}')" == 2/2 ]] || fail "eltr: parallelism/completions $(job_field eltr '{.spec.parallelism}/{.spec.completions}'), want 2/2"
  eltr_logs() { kubectl -n "$NS" logs -l gryvia.io/job=eltr --tail=-1 --max-log-requests=10 2>/dev/null; }
  # Not "eltr_logs | grep -q": grep exits at the first match and pipefail turns kubectl's SIGPIPE into a failure.
  eltr_grouped() { grep -q 'of 2: starting after step 0 ' <<<"$(eltr_logs)"; }
  wait_for 300 "the 2 admitted workers form one group" eltr_grouped
  wait_for 600 "eltr reaches Succeeded" is_phase eltr Succeeded
  out="$(eltr_logs)"
  grep -E "starting after|committed step|done:" <<<"$out" || true
  done_json="$(sed -n 's/^done: //p' <<<"$out" | head -1)"
  [[ -n "$done_json" ]] || fail "eltr: no done line from rank 0"
  [[ "$(jq -r .steps <<<"$done_json")" == 20 ]] || fail "eltr: steps $(jq -r .steps <<<"$done_json"), want 20"
  [[ "$(jq -r .world <<<"$done_json")" == 2 ]] || fail "eltr: world $(jq -r .world <<<"$done_json"), want 2"
  grep -q "committed step 20 (world 2" <<<"$out" || fail "eltr: the last step was not committed by 2 ranks"
  idx="$(kubectl -n "$NS" get pods -l gryvia.io/job=eltr -o jsonpath='{range .items[*]}{.metadata.labels.batch\.kubernetes\.io/job-completion-index}{"\n"}{end}' | sort -u | tr '\n' ' ')"
  [[ "$idx" == "0 1 " ]] || fail "eltr: pod indexes '$idx', want only 0 and 1"
  kubectl -n "$NS" delete gryviaaijob eltr --wait=true >/dev/null
  kubectl -n "$NS" delete pvc el-ckpt --wait=false >/dev/null
  wait_for 120 "ClusterQueue idle" cq_idle
}

case "${1:-}" in
  setup)   scenario_setup ;;
  strict)  scenario_strict ;;
  queue)   scenario_queue ;;
  gang)    scenario_gang ;;
  preempt) scenario_preempt ;;
  preempt-checkpoint) scenario_preempt_checkpoint ;;
  elastic) scenario_elastic ;;
  elastic-torchrun) scenario_elastic_torchrun ;;
  fairshare) scenario_fairshare ;;
  *) echo "usage: $0 setup|strict|queue|gang|preempt|preempt-checkpoint|elastic|elastic-torchrun" >&2; exit 2 ;;
esac
echo "E2E OK: ${1}"
