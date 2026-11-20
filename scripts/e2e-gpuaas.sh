#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# End-to-end checks of the GPUaaS features on a 2-node kind cluster with Gryvia installed and the
# opt-in flags on (quotaOperator.tenantRbac, quotaOperator.reservations, aiOperator.admissionGate).
# Run by .github/workflows/e2e-gpuaas.yml, one scenario per step:
#
#   scripts/e2e-gpuaas.sh rbac          # tenant RoleBindings + `kubectl auth can-i --as <user>`
#   scripts/e2e-gpuaas.sh budget        # hard budget: over-forecast job Rejected with no workload, affordable job Succeeds
#   scripts/e2e-gpuaas.sh reservation   # reservation taints the worker; owner's job lands, others cannot; expiry frees it
#
# ASSUMPTIONS (they hold in CI, where e2e-gpuaas.yml passes on kind; check them first on other clusters):
#  - kind has no GPUs. The worker advertises fake `nvidia.com/gpu: 8` by patching its node status (the documented
#    "advertise extended resources for a node" technique; kubelet keeps extended resources it does not manage) and
#    is labelled gryvia.io/gpu=E2EGPU so the AIJob controller's GPU node selector matches. Pods only *request* the
#    resource; busybox never touches a GPU.
#  - GPU type E2EGPU is priced by an extra GryviaGpuSku (10 USD per GPU-hour) so the demo SKUs are not involved.
#  - `kubectl auth can-i --as <user>` works with impersonation without an identity provider (the runner is cluster-admin).
#  - Requeue intervals: budget and tenant controllers reconcile every 1-2 minutes; every wait below is bounded.
set -euo pipefail

API=gryvia.io/v1alpha1
GPU_TYPE=E2EGPU
TENANT_A=gpuaas-a
TENANT_B=gpuaas-b
NS_A=tenant-$TENANT_A
NS_B=tenant-$TENANT_B

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

phase() { kubectl -n "$1" get gryviaaijob "$2" -o jsonpath='{.status.phase}' 2>/dev/null; }
is_phase() { [[ "$(phase "$1" "$2")" == "$3" ]]; }
gone() { ! kubectl -n "$1" get "$2" "$3" >/dev/null 2>&1; }
# out_is <expected> <command...>: succeeds when the command's stdout equals the expected text. Use it with wait_for
# so the command is re-run on every poll (a $(...) argument would be evaluated once, before the wait starts).
out_is() { local want="$1"; shift; [[ "$("$@" 2>/dev/null || true)" == "$want" ]]; }
# can_i <yes|no> <kubectl auth can-i args...>
can_i() {
  local want="$1"; shift
  local got; got="$(kubectl auth can-i "$@" 2>/dev/null || true)"
  [[ "$got" == "$want" ]] || fail "kubectl auth can-i $* -> '$got', want '$want'"
  echo "ok: can-i $* = $got"
}

worker() { kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[0].metadata.name}'; }

setup_gpu() {
  local w; w="$(worker)"
  kubectl label node "$w" "gryvia.io/gpu=$GPU_TYPE" gryvia.io/gpu-count=8 --overwrite >/dev/null
  kubectl patch node "$w" --subresource=status --type=json -p '[
    {"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"8"},
    {"op":"add","path":"/status/allocatable/nvidia.com~1gpu","value":"8"}]' >/dev/null
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaGpuSku
metadata: {name: e2e-gpu}
spec: {gpuType: $GPU_TYPE, gpusPerUnit: 1, hourlyRate: 10, currency: USD}
YAML
  wait_for 60 "worker $w advertises 8 GPUs" \
    out_is 8 kubectl get node "$w" -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}'
}

tenants() {
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaTenant
metadata: {name: $TENANT_A}
spec:
  displayName: GPUaaS e2e A
  members:
    - {username: alice, role: admin}
    - {username: bob, role: member}
    - {username: carol, role: viewer}
    - {username: erin, role: superuser}   # unknown role: must become viewer
  oidcGroups: [gpuaas-a-devs]
---
apiVersion: $API
kind: GryviaTenant
metadata: {name: $TENANT_B}
spec:
  displayName: GPUaaS e2e B
  members:
    - {username: dave, role: member}
YAML
  wait_for 120 "namespace $NS_A exists" kubectl get ns "$NS_A"
  wait_for 120 "namespace $NS_B exists" kubectl get ns "$NS_B"
}

# A small pod: explicit requests keep the tenant LimitRange defaults (1 CPU / 2Gi per container) out of the way.
gpu_job() { # name namespace nodes gpusPerNode timeout sleep-seconds [extra metadata lines]
  local name="$1" ns="$2" nodes="$3" per="$4" timeout="$5" secs="$6" meta="${7:-}"
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaAIJob
metadata:
  name: $name
  namespace: $ns
$meta
spec:
  type: training
  image: busybox:1.36
  gpus: $per
  gpuType: $GPU_TYPE
  retryLimit: 0
  timeout: $timeout
  distributed: {enabled: true, framework: pytorch, backend: gloo, nodes: $nodes, gpusPerNode: $per}
  resources:
    requests: {cpu: 20m, memory: 16Mi}
    limits: {cpu: 200m, memory: 128Mi}
  command: ["sh", "-c", "sleep $secs"]
YAML
}

usage_final() { [[ "$(kubectl -n "$1" get gryviausagerecord -l "gryvia.io/job=$2" -o jsonpath='{.items[0].spec.final}' 2>/dev/null)" == "true" ]]; }
rec() { kubectl -n "$1" get gryviausagerecord -l "gryvia.io/job=$2" -o "jsonpath={.items[0].spec.$3}"; }

scenario_rbac() {
  tenants
  wait_for 120 "RoleBinding gryvia-tenant-member in $NS_A" kubectl -n "$NS_A" get rolebinding gryvia-tenant-member
  wait_for 60 "RoleBinding gryvia-tenant-admin in $NS_A" kubectl -n "$NS_A" get rolebinding gryvia-tenant-admin
  wait_for 60 "RoleBinding gryvia-tenant-viewer in $NS_A" kubectl -n "$NS_A" get rolebinding gryvia-tenant-viewer
  kubectl -n "$NS_A" get rolebinding gryvia-tenant-member -o jsonpath='{.subjects[*].name}' | grep -qw bob \
    || fail "bob is not a subject of the member binding"
  kubectl -n "$NS_A" get rolebinding gryvia-tenant-member -o jsonpath='{.roleRef.name}' | grep -qx gryvia-tenant-member || fail "wrong roleRef"
  kubectl -n "$NS_A" get rolebinding gryvia-tenant-member -o jsonpath='{.subjects[*].name}' | grep -q gpuaas-a-devs || fail "oidc group is not bound"
  kubectl -n "$NS_A" get rolebinding gryvia-tenant-viewer -o jsonpath='{.subjects[*].name}' | grep -q erin || fail "unknown role must map to viewer"
  kubectl -n "$NS_A" get rolebinding gryvia-tenant-member -o jsonpath='{.metadata.labels.gryvia\.io/managed-by}' | grep -qx tenant-controller || fail "managed-by label missing"

  # member: manage jobs in the own tenant only.
  can_i yes create gryviaaijobs.gryvia.io -n "$NS_A" --as bob
  can_i yes delete gryviaaijobs.gryvia.io -n "$NS_A" --as bob
  can_i yes list pods -n "$NS_A" --as bob
  # subresources need --subresource: "pods/log" would be read as a pod named "log"
  can_i yes get pods --subresource=log -n "$NS_A" --as bob
  can_i yes create pods --subresource=portforward -n "$NS_A" --as bob
  can_i no create pods --subresource=exec -n "$NS_A" --as bob          # no exec, by design
  can_i no get secrets -n "$NS_A" --as bob
  can_i no delete resourcequotas -n "$NS_A" --as bob
  can_i no create rolebindings.rbac.authorization.k8s.io -n "$NS_A" --as bob
  can_i no create gryviausagerecords.gryvia.io -n "$NS_A" --as bob   # usage records are read-only
  can_i yes list gryviausagerecords.gryvia.io -n "$NS_A" --as bob
  can_i no create gryviaaijobs.gryvia.io -n "$NS_B" --as bob        # another tenant's namespace
  can_i no list pods -n "$NS_B" --as bob
  can_i no create gryviaaijobs.gryvia.io -n default --as bob
  # viewer: read only.
  can_i yes list gryviaaijobs.gryvia.io -n "$NS_A" --as carol
  can_i no create gryviaaijobs.gryvia.io -n "$NS_A" --as carol
  can_i no create gryviaaijobs.gryvia.io -n "$NS_A" --as erin
  # admin: member plus configmaps/services; still no secrets or quotas.
  can_i yes create configmaps -n "$NS_A" --as alice
  can_i no create configmaps -n "$NS_A" --as bob
  can_i no get secrets -n "$NS_A" --as alice
  can_i no delete resourcequotas -n "$NS_A" --as alice
  # OIDC group subject (the API server would take the groups from the token; impersonation supplies them here).
  can_i yes create gryviaaijobs.gryvia.io -n "$NS_A" --as some-dev --as-group gpuaas-a-devs
  can_i no create gryviaaijobs.gryvia.io -n "$NS_B" --as some-dev --as-group gpuaas-a-devs
  # Tenant B's member is not tenant A's.
  can_i yes create gryviaaijobs.gryvia.io -n "$NS_B" --as dave
  can_i no create gryviaaijobs.gryvia.io -n "$NS_A" --as dave

  # Membership changes are reconciled: bob leaves, carol is promoted.
  kubectl patch gryviatenant "$TENANT_A" --type=merge -p '{"spec":{"members":[{"username":"alice","role":"admin"},{"username":"carol","role":"member"}]}}' >/dev/null
  wait_for 180 "bob loses access after leaving the tenant" \
    out_is no kubectl auth can-i create gryviaaijobs.gryvia.io -n "$NS_A" --as bob
  can_i yes create gryviaaijobs.gryvia.io -n "$NS_A" --as carol
  wait_for 60 "stale viewer binding is removed" gone "$NS_A" rolebinding gryvia-tenant-viewer
}

scenario_budget() {
  setup_gpu
  tenants
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaBudget
metadata: {name: e2e-a}
spec:
  scope: {type: tenant, name: $TENANT_A}
  period: {type: monthly}
  limits: {costUSD: 50}
  alerts:
    - {threshold: 80, actions: [notify]}
  enforcement: {enabled: true, action: block}
YAML

  # Affordable (kind has one GPU worker): 1 node x 4 GPUs x 10m x 10 = 6.67 USD forecast < 50. It must run to Succeeded.
  gpu_job afford "$NS_A" 1 4 10m 20
  wait_for 300 "afford reaches Succeeded" is_phase "$NS_A" afford Succeeded
  wait_for 120 "usage record of afford is final" usage_final "$NS_A" afford
  [[ "$(rec "$NS_A" afford gpus)" == 4 ]] || fail "usage record gpus = $(rec "$NS_A" afford gpus), want 1 node x 4 GPUs = 4"
  [[ "$(rec "$NS_A" afford rate)" == 10 ]] || fail "usage record rate = $(rec "$NS_A" afford rate), want 10"
  local hours cost
  hours="$(rec "$NS_A" afford gpuHours)"; cost="$(rec "$NS_A" afford cost)"
  echo "afford: gpuHours=$hours cost=$cost"
  awk -v h="$hours" -v c="$cost" 'BEGIN { d = c - h * 10; if (d < 0) d = -d; exit !(c > 0 && d < 0.001) }' \
    || fail "cost $cost is not gpuHours $hours x 10 (or is zero)"

  # Over budget by forecast alone: 4 nodes x 8 GPUs x 24h x 10 = 7680 USD > 50. Rejected, nothing created.
  gpu_job toobig "$NS_A" 4 8 24h 600
  wait_for 120 "toobig is Rejected" is_phase "$NS_A" toobig Rejected
  local msg; msg="$(kubectl -n "$NS_A" get gryviaaijob toobig -o jsonpath='{.status.message}')"
  echo "message: $msg"
  [[ "$msg" == *forecast* && "$msg" == *e2e-a* ]] || fail "rejection message does not explain the budget: '$msg'"
  [[ "$(kubectl -n "$NS_A" get gryviaaijob toobig -o jsonpath='{.status.conditions[?(@.type=="Rejected")].reason}')" == BudgetExceeded ]] \
    || fail "Rejected condition reason is not BudgetExceeded"
  sleep 20
  for o in "job toobig" "statefulset toobig-training" "service toobig-headless" "pvc toobig-data"; do
    read -r k n <<<"$o"
    gone "$NS_A" "$k" "$n" || fail "$k/$n exists for a job the admission gate rejected"
  done
  [[ -z "$(kubectl -n "$NS_A" get pods -l gryvia.io/job=toobig -o name)" ]] || fail "pods exist for a rejected job"
  is_phase "$NS_A" toobig Rejected || fail "Rejected job changed phase"

  # The GryviaBudget status is computed from the usage records (controller requeues every minute).
  wait_for 240 "budget e2e-a reports the metered cost" budget_cost_positive
  local st; st="$(kubectl get gryviabudget e2e-a -o jsonpath='{.status.state}')"
  [[ "$st" == active || "$st" == warning ]] || fail "budget state = '$st' after ~1 USD of 50 (want active/warning)"
  kubectl delete gryviaaijob toobig afford -n "$NS_A" --wait=false >/dev/null
}

budget_cost_positive() {
  awk -v c="$(kubectl get gryviabudget e2e-a -o jsonpath='{.status.usage.costUSD}')" 'BEGIN { exit !(c > 0) }'
}
taint_value() { kubectl get node "$1" -o jsonpath='{.spec.taints[?(@.key=="gryvia.io/reserved")].value}'; }
reserved_for() { kubectl get node "$1" -o jsonpath='{.metadata.labels.gryvia\.io/reserved-for}'; }
pod_of() { kubectl -n "$1" get pods -l "gryvia.io/job=$2" -o jsonpath='{.items[0].metadata.name}'; }

scenario_reservation() {
  setup_gpu
  tenants
  local w; w="$(worker)"
  local end; end="$(date -u -d '+240 seconds' +%Y-%m-%dT%H:%M:%SZ)"
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaReservation
metadata: {name: e2e-res}
spec:
  owner: {type: tenant, name: $TENANT_A}
  resources: {gpuType: $GPU_TYPE, gpuCount: 8}
  schedule: {type: immediate, endTime: "$end"}
  guarantees: {exclusive: true}
YAML
  wait_for 120 "reservation is active" out_is active kubectl get gryviareservation e2e-res -o 'jsonpath={.status.state}'
  [[ "$(kubectl get gryviareservation e2e-res -o jsonpath='{.status.allocatedNodes[0]}')" == "$w" ]] || fail "reservation did not allocate $w"
  [[ "$(taint_value "$w")" == "$TENANT_A" ]] || fail "worker taint gryvia.io/reserved = '$(taint_value "$w")', want $TENANT_A"
  [[ "$(reserved_for "$w")" == e2e-res ]] || fail "worker label gryvia.io/reserved-for = '$(reserved_for "$w")'"

  # A job without the annotation cannot land on the tainted worker (its GPU node selector only matches the worker).
  gpu_job res-plain "$NS_A" 1 1 10m 5
  wait_for 90 "pod of res-plain exists" kubectl -n "$NS_A" get pods -l gryvia.io/job=res-plain -o name
  sleep 30
  local p; p="$(pod_of "$NS_A" res-plain)"
  [[ "$(kubectl -n "$NS_A" get pod "$p" -o jsonpath='{.status.phase}')" == Pending ]] || fail "res-plain pod is not Pending on a reserved node"
  [[ "$(kubectl -n "$NS_A" get pod "$p" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].message}')" == *taint* ]] \
    || fail "res-plain is pending for a reason other than the taint: $(kubectl -n "$NS_A" get pod "$p" -o jsonpath='{.status.conditions[*].message}')"

  # A job of another tenant naming the reservation is ignored (owner mismatch) and cannot land either.
  gpu_job res-thief "$NS_B" 1 1 10m 5 $'  annotations:\n    gryvia.io/reservation: e2e-res'
  wait_for 90 "res-thief carries a Reservation=False/OwnerMismatch condition" \
    out_is OwnerMismatch kubectl -n "$NS_B" get gryviaaijob res-thief -o 'jsonpath={.status.conditions[?(@.type=="Reservation")].reason}'

  # The owner's job with the annotation lands on the reserved worker and runs to completion.
  gpu_job res-owner "$NS_A" 1 1 10m 5 $'  annotations:\n    gryvia.io/reservation: e2e-res'
  wait_for 180 "res-owner reaches Succeeded" is_phase "$NS_A" res-owner Succeeded
  [[ "$(kubectl -n "$NS_A" get pod "$(pod_of "$NS_A" res-owner)" -o jsonpath='{.spec.nodeName}')" == "$w" ]] || fail "res-owner did not run on $w"
  [[ "$(kubectl -n "$NS_A" get pod "$(pod_of "$NS_A" res-owner)" -o jsonpath='{.spec.nodeSelector.gryvia\.io/reserved-for}')" == e2e-res ]] \
    || fail "res-owner pod has no reservation nodeSelector"
  [[ "$(kubectl -n "$NS_A" get gryviaaijob res-owner -o jsonpath='{.status.conditions[?(@.type=="Reservation")].status}')" == True ]] \
    || fail "Reservation condition of res-owner is not True"
  is_phase "$NS_A" res-plain Succeeded && fail "res-plain must still be blocked while the reservation is active"

  # Expiry removes the taint and the labels, and the blocked jobs then schedule.
  wait_for 300 "reservation expired" out_is expired kubectl get gryviareservation e2e-res -o 'jsonpath={.status.state}'
  [[ -z "$(taint_value "$w")" ]] || fail "taint gryvia.io/reserved still on $w after expiry"
  [[ -z "$(reserved_for "$w")" ]] || fail "label gryvia.io/reserved-for still on $w after expiry"
  [[ -z "$(kubectl get node "$w" -o jsonpath='{.metadata.labels.gryvia\.io/reserved-by}')" ]] || fail "label gryvia.io/reserved-by still on $w"
  wait_for 240 "res-plain runs once the worker is free" is_phase "$NS_A" res-plain Succeeded

  # Deleting an active reservation also cleans up (finalizer): reserve again, delete, check.
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: $API
kind: GryviaReservation
metadata: {name: e2e-res2}
spec:
  owner: {type: tenant, name: $TENANT_A}
  resources: {gpuType: $GPU_TYPE, gpuCount: 8}
  schedule: {type: immediate}
YAML
  wait_for 120 "second reservation taints the worker" out_is "$TENANT_A" taint_value "$w"
  kubectl delete gryviareservation e2e-res2 --wait=true >/dev/null
  wait_for 60 "deleting the reservation removes the taint" out_is "" taint_value "$w"
  kubectl delete gryviaaijob res-thief -n "$NS_B" --wait=false >/dev/null || true
}

case "${1:-}" in
  rbac) scenario_rbac ;;
  budget) scenario_budget ;;
  reservation) scenario_reservation ;;
  *) echo "usage: $0 rbac|budget|reservation" >&2; exit 2 ;;
esac
echo "E2E OK: ${1}"
