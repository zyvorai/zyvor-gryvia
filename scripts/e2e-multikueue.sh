#!/usr/bin/env bash
# Two-cluster MultiKueue check (docs/kueue-integration.md): a GryviaAIJob submitted on the MANAGER cluster is
# dispatched by Kueue to a WORKER cluster, runs there, and its status comes back. Both are kind clusters on the
# same docker network. Run by .github/workflows/e2e-multikueue.yml, one step each:
#
#   scripts/e2e-multikueue.sh worker      # worker: Kueue-ready namespace, flavor, ClusterQueue, LocalQueue, fake slots
#   scripts/e2e-multikueue.sh connect     # manager: worker kubeconfig Secret, MultiKueueCluster/Config, AdmissionCheck
#   (then scripts/e2e-kueue.sh setup: the tenant queues; they stay inactive until the check above is Active)
#   scripts/e2e-multikueue.sh verify      # the tenant ClusterQueue is Active and bound to the check
#   scripts/e2e-multikueue.sh dispatch    # the job runs on the worker, not on the manager, and finishes
#   WORKER_NAME=gryvia-worker2 scripts/e2e-multikueue.sh worker   # a second worker, same queues
#   scripts/e2e-multikueue.sh connect2    # manager: add worker2 to the MultiKueueConfig
#   scripts/e2e-multikueue.sh federation  # manager: GryviaFederation over both workers, both probed healthy
#   scripts/e2e-multikueue.sh failover    # a checkpointing job on one worker; that worker stops; the federation
#                                         # fences it, evicts the Workload, MultiKueue redispatches it to the other
#                                         # worker and the trainer resumes from the S3 replica (needs S3_ENDPOINT)
#
# Scope: this proves Kueue's MultiKueue binding as Gryvia configures it (tenant ClusterQueue bound to the
# admission check with platformCompletion.kueueAdmissionCheck=multikueue) with CPU pods, and GryviaFederation
# failover with fencing by lease timeout and checkpoint resume from an S3 replica. It does not prove GPU placement,
# fencing by remote delete against a real outage (unit-tested only) or cost aggregation across clusters.
#
# ASSUMPTIONS to look at first when a step fails: the manager's node image supports Job managedBy (Kubernetes
# >= 1.32 beta); the worker API server is reachable from the manager pods as https://<worker>-control-plane:6443
# (docker DNS on the kind network); Kueue's MultiKueue feature gate is on (default since Kueue 0.9).
set -euo pipefail

MANAGER_CTX="${MANAGER_CTX:-kind-gryvia-demo}"
WORKER_NAME="${WORKER_NAME:-gryvia-worker}"
WORKER_CTX="kind-$WORKER_NAME"
NS=tenant-q1
SLOT=example.com/slot
API=gryvia.io/v1alpha1

fail() { echo "E2E FAIL: $*" >&2; exit 1; }
km() { kubectl --context "$MANAGER_CTX" "$@"; }
kw() { kubectl --context "$WORKER_CTX" "$@"; }

wait_for() { # <seconds> <description> <command...>
  local secs="$1" what="$2"; shift 2
  local end=$((SECONDS + secs))
  until "$@" >/dev/null 2>&1; do
    (( SECONDS < end )) || fail "timed out after ${secs}s waiting for: $what"
    sleep 3
  done
  echo "ok: $what"
}

jsonpath_is() { # <kubectl-fn> <expected> <args...>: jsonpath value equals expected
  local fn="$1" want="$2"; shift 2
  [[ "$("$fn" "$@" 2>/dev/null)" == "$want" ]]
}

scenario_worker() {
  for n in $(kw get nodes -o name | sed 's#node/##'); do
    kw patch node "$n" --subresource=status --type=json \
      -p "[{\"op\":\"add\",\"path\":\"/status/capacity/example.com~1slot\",\"value\":\"100\"}]" >/dev/null
  done
  kw create namespace "$NS" --dry-run=client -o yaml | kw apply -f - >/dev/null
  wait_for 120 "Kueue CRDs on the worker" kw get crd clusterqueues.kueue.x-k8s.io localqueues.kueue.x-k8s.io
  # The manager's queue name is "gryvia" in $NS (the default the ai-operator uses); the worker must have the same.
  for _ in $(seq 1 30); do
    if cat <<YAML | kw apply -f - >/dev/null 2>&1; then break; fi
apiVersion: kueue.x-k8s.io/v1beta1
kind: ResourceFlavor
metadata: {name: default}
YAML
    sleep 4
  done
  cat <<YAML | kw apply -f - >/dev/null
apiVersion: kueue.x-k8s.io/v1beta1
kind: ClusterQueue
metadata: {name: worker-cq}
spec:
  namespaceSelector: {}
  resourceGroups:
    - coveredResources: [cpu, memory, $SLOT]
      flavors:
        - name: default
          resources:
            - {name: cpu, nominalQuota: "4"}
            - {name: memory, nominalQuota: 4Gi}
            - {name: $SLOT, nominalQuota: "50"}
---
apiVersion: kueue.x-k8s.io/v1beta1
kind: LocalQueue
metadata: {name: gryvia, namespace: $NS}
spec: {clusterQueue: worker-cq}
YAML
  wait_for 120 "worker ClusterQueue is Active" \
    jsonpath_is kw True get clusterqueue worker-cq -o 'jsonpath={.status.conditions[?(@.type=="Active")].status}'
}

scenario_connect() {
  local kcfg
  kcfg="$(mktemp)"
  kind get kubeconfig --internal --name "$WORKER_NAME" > "$kcfg"
  # Kueue reads the Secret from its own namespace (the chart installs it in gryvia-system).
  km -n gryvia-system create secret generic worker-kubeconfig --from-file=kubeconfig="$kcfg" \
    --dry-run=client -o yaml | km apply -f - >/dev/null
  rm -f "$kcfg"
  cat <<YAML | km apply -f - >/dev/null
apiVersion: kueue.x-k8s.io/v1beta1
kind: MultiKueueCluster
metadata: {name: worker1}
spec:
  kubeConfig: {locationType: Secret, location: worker-kubeconfig}
---
apiVersion: kueue.x-k8s.io/v1beta1
kind: MultiKueueConfig
metadata: {name: gryvia-workers}
spec: {clusters: [worker1]}
---
apiVersion: kueue.x-k8s.io/v1beta1
kind: AdmissionCheck
metadata: {name: multikueue}
spec:
  controllerName: kueue.x-k8s.io/multikueue
  parameters: {apiGroup: kueue.x-k8s.io, kind: MultiKueueConfig, name: gryvia-workers}
YAML
  wait_for 180 "MultiKueueCluster worker1 is Active (manager reaches the worker)" \
    jsonpath_is km True get multikueuecluster worker1 -o 'jsonpath={.status.conditions[?(@.type=="Active")].status}'
  wait_for 120 "AdmissionCheck multikueue is Active" \
    jsonpath_is km True get admissioncheck multikueue -o 'jsonpath={.status.conditions[?(@.type=="Active")].status}'
}

# After e2e-kueue.sh setup: the tenant ClusterQueue exists. It stays inactive while the admission check it
# references does not exist or is not Active, which is why `connect` must run before `setup`.
scenario_verify() {
  wait_for 180 "tenant ClusterQueue gryvia-q1 is Active with the check bound" \
    jsonpath_is km True get clusterqueue gryvia-q1 -o 'jsonpath={.status.conditions[?(@.type=="Active")].status}'
  km get clusterqueue gryvia-q1 -o jsonpath='{.spec.admissionChecksStrategy}' | grep -q multikueue \
    || fail "gryvia-q1 does not reference the multikueue admission check (platformCompletion.kueueAdmissionCheck)"
}

scenario_connect2() {
  local kcfg
  kcfg="$(mktemp)"
  kind get kubeconfig --internal --name gryvia-worker2 > "$kcfg"
  km -n gryvia-system create secret generic worker2-kubeconfig --from-file=kubeconfig="$kcfg" \
    --dry-run=client -o yaml | km apply -f - >/dev/null
  rm -f "$kcfg"
  cat <<YAML | km apply -f - >/dev/null
apiVersion: kueue.x-k8s.io/v1beta1
kind: MultiKueueCluster
metadata: {name: worker2}
spec:
  kubeConfig: {locationType: Secret, location: worker2-kubeconfig}
---
apiVersion: kueue.x-k8s.io/v1beta1
kind: MultiKueueConfig
metadata: {name: gryvia-workers}
spec: {clusters: [worker1, worker2]}
YAML
  wait_for 180 "MultiKueueCluster worker2 is Active" \
    jsonpath_is km True get multikueuecluster worker2 -o 'jsonpath={.status.conditions[?(@.type=="Active")].status}'
}

member_state() { km get gryviafederation e2e -o "jsonpath={.status.clusterStatus[?(@.name==\"$1\")].$2}"; }

# Both workers as federation members, probed through the same kubeconfig Secrets Kueue uses (inline client certs).
scenario_federation() {
  cat <<YAML | km apply -f - >/dev/null
apiVersion: $API
kind: GryviaFederation
metadata: {name: e2e}
spec:
  clusters:
    - {name: worker1, enabled: true, apiServer: "https://gryvia-worker-control-plane:6443", credentials: {secretRef: worker-kubeconfig}}
    - {name: worker2, enabled: true, apiServer: "https://gryvia-worker2-control-plane:6443", credentials: {secretRef: worker2-kubeconfig}}
  failover:
    enabled: true
    automatic: true
    leaseTimeout: 30s
    healthCheck: {interval: 5s, failureThreshold: 2}
YAML
  wait_for 120 "federation probes worker1 healthy" jsonpath_is member_state healthy worker1 state
  wait_for 60 "federation probes worker2 healthy" jsonpath_is member_state healthy worker2 state
}

worker_of() { # the worker cluster that has Job $1, or nothing
  local w
  for w in gryvia-worker gryvia-worker2; do
    if kubectl --context "kind-$w" -n "$NS" get job "$1" >/dev/null 2>&1; then echo "$w"; return; fi
  done
}
on_some_worker() { [[ -n "$(worker_of "$1")" ]]; }
worker_logs_have() { # <worker> <job> <regex>
  kubectl --context "kind-$1" -n "$NS" logs -l "job-name=$2" --tail=-1 2>/dev/null | grep -Eq "$3"
}
fed_field() { km get gryviafederation e2e -o "jsonpath=$1"; }
nonempty() { [[ -n "$("$@" 2>/dev/null)" ]]; }

scenario_failover() {
  : "${S3_ENDPOINT:?S3_ENDPOINT (reachable from the worker pods) is required}"
  cat <<YAML | km apply -f - >/dev/null
apiVersion: $API
kind: GryviaAIJob
metadata: {name: ft1, namespace: $NS}
spec:
  type: training
  image: ghcr.io/zyvorai/gryvia-elastic-train:dev
  imagePullPolicy: IfNotPresent
  gpus: 0
  retryLimit: 2
  timeout: 30m
  command: [torchrun, --standalone, --nnodes=1, --nproc_per_node=1, /app/elastic_train.py]
  env:
    - {name: CHECKPOINT_DIR, value: /tmp/ckpt}
    - {name: TOTAL_STEPS, value: "150"}
    - {name: STEP_SECONDS, value: "1"}
    - {name: CHECKPOINT_EVERY, value: "5"}
    - {name: GRYVIA_CHECKPOINT_REPLICA, value: "s3://ckpt/ft1"}
    - {name: AWS_ENDPOINT_URL, value: "$S3_ENDPOINT"}
    - {name: AWS_ACCESS_KEY_ID, value: "${S3_ACCESS_KEY:-e2eaccess}"}
    - {name: AWS_SECRET_ACCESS_KEY, value: "${S3_SECRET_KEY:-e2esecret123}"}
    - {name: AWS_DEFAULT_REGION, value: us-east-1}
  resources:
    requests: {cpu: 200m, memory: 512Mi, $SLOT: "1"}
    limits: {cpu: "1", memory: 1536Mi, $SLOT: "1"}
YAML
  wait_for 300 "Job ft1 is dispatched to a worker" on_some_worker ft1
  local first second
  first="$(worker_of ft1)"
  if [[ "$first" == gryvia-worker ]]; then second=gryvia-worker2; else second=gryvia-worker; fi
  echo "ft1 runs on $first"
  wait_for 420 "the trainer on $first committed and replicated step 15" worker_logs_have "$first" ft1 'committed step 15 '
  [[ "$(km -n "$NS" get pods -o name | grep -c ft1 || true)" == "0" ]] || fail "the manager ran a pod of ft1"

  # The worker's API server and node go away together; the federation can no longer reach it.
  docker stop "$first-control-plane" >/dev/null
  local member=worker1
  [[ "$first" == gryvia-worker2 ]] && member=worker2
  wait_for 120 "federation marks $member unhealthy" jsonpath_is member_state unhealthy "$member" state
  wait_for 180 "$member is fenced after its lease timeout (unreachable, no remote delete)" \
    jsonpath_is member_state LeaseExpired "$member" fenceMethod
  wait_for 120 "the Workload of ft1 was evicted off $member" \
    jsonpath_is fed_field "$member" '{.status.failovers[0].cluster}'
  fed_field '{.status.failovers[0].workload}' | grep -q ft1 || fail "failover record is not for ft1"
  wait_for 180 "the Workload was requeued for redispatch" nonempty fed_field '{.status.failovers[0].requeuedAt}'

  wait_for 420 "MultiKueue redispatched ft1 to $second" kubectl --context "kind-$second" -n "$NS" get job ft1
  wait_for 420 "the trainer on $second restored a committed step from the S3 replica" \
    worker_logs_have "$second" ft1 'restored committed step [0-9]+ from the replica'
  wait_for 600 "the trainer on $second finished" worker_logs_have "$second" ft1 'done: [{]'
  local done_json
  done_json="$(kubectl --context "kind-$second" -n "$NS" logs -l job-name=ft1 --tail=-1 | sed -n 's/.*done: //p' | tail -1)"
  echo "done: $done_json"
  python3 -c 'import json,sys; d=json.loads(sys.argv[1]); assert d["steps"]==150 and d["resumedFrom"]>=15, d' "$done_json" \
    || fail "ft1 did not finish 150 steps resuming from step >= 15"
  wait_for 300 "the GryviaAIJob on the manager reaches Succeeded" is_phase ft1 Succeeded
  km get gryviafederation e2e -o yaml | sed -n '/^status:/,$p'
}

phase() { km -n "$NS" get gryviaaijob "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
is_phase() { [[ "$(phase "$1")" == "$2" ]]; }

scenario_dispatch() {
  cat <<YAML | km apply -f - >/dev/null
apiVersion: $API
kind: GryviaAIJob
metadata: {name: remote1, namespace: $NS}
spec:
  type: training
  image: busybox:1.36
  gpus: 0
  retryLimit: 0
  timeout: 20m
  resources:
    requests: {cpu: 20m, memory: 16Mi, $SLOT: "1"}
    limits: {cpu: 200m, memory: 64Mi, $SLOT: "1"}
  command: ["sh", "-c"]
  args: ["echo ran-on-worker; hostname; sleep 20"]
YAML
  # Kueue mirrors the Job to the worker once the workload is admitted through the multikueue check.
  wait_for 300 "the Job appears on the worker cluster" kw -n "$NS" get job remote1
  wait_for 300 "a pod of the job runs on the worker" \
    bash -c "kubectl --context $WORKER_CTX -n $NS get pods -l job-name=remote1 -o jsonpath='{.items[*].status.phase}' | grep -Eq 'Running|Succeeded'"
  # Read the log while the pod exists: after the job finishes Kueue removes the remote Job and its pods.
  wait_for 60 "the worker pod printed its marker line" \
    bash -c "kubectl --context $WORKER_CTX -n $NS logs -l job-name=remote1 | grep -q ran-on-worker"
  [[ "$(km -n "$NS" get pods -o name | grep -c remote1 || true)" == "0" ]] || fail "the manager cluster ran a pod of remote1; it must run on the worker only"
  wait_for 300 "the GryviaAIJob on the manager reaches Succeeded (status came back from the worker)" is_phase remote1 Succeeded
  km -n "$NS" delete gryviaaijob remote1 --wait=true >/dev/null
}

case "${1:-}" in
  worker)   scenario_worker ;;
  connect)  scenario_connect ;;
  verify)   scenario_verify ;;
  dispatch) scenario_dispatch ;;
  connect2) scenario_connect2 ;;
  federation) scenario_federation ;;
  failover) scenario_failover ;;
  *) echo "usage: $0 worker|connect|verify|dispatch|connect2|federation|failover" >&2; exit 2 ;;
esac
echo "E2E OK: ${1}"
