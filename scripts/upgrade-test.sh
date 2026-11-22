#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Upgrade and rollback test: install the previous Gryvia on a kind cluster, create objects, upgrade to
# this checkout the documented way (website/docs/guides/OPERATIONS.md), check that nothing was lost,
# roll back, then run a backup/restore round trip. CI: .github/workflows/upgrade.yml.
#
# Base version, in order of preference:
#   --base-chart PATH|oci://...   an explicit chart (a directory or an OCI reference); images must then
#                                 already be loadable: pass --base-image-tag TAG (default: from the chart)
#   --base-version TAG            a released chart: helm pull oci://ghcr.io/zyvorai/charts/gryvia --version TAG,
#                                 images pulled from ghcr.io
#   --base-ref REF                a git ref (default: the newest v* tag if its chart can be pulled, else
#                                 origin/main, or origin/main~1 when HEAD is origin/main): built from a git
#                                 worktree with locally built images
#
# NOT RUN LOCALLY by the author (no docker/kind on the development machine): it is exercised in CI and by
# scripts/tests/upgrade-test.test.sh with fake tools, which checks the argument handling and the step order.
set -euo pipefail

usage() {
	cat <<'USAGE'
usage: upgrade-test.sh [options]

  --base-chart PATH|OCI     chart to install first (directory or oci:// reference)
  --base-version TAG        released chart version/tag to install first (e.g. v0.2.0)
  --base-ref GIT_REF        build the base chart and images from this git ref
  --base-image-tag TAG      with --base-chart: the image tag the base chart should use
  --head-chart DIR          chart to upgrade to (default ./helm/gryvia)
  --head-crds DIR           CRDs to apply before the upgrade (default ./crds)
  --cli PATH                gryvia CLI binary (default ./cli/target/release/gryvia; the check is skipped when absent)
  --cluster NAME            kind cluster name (default gryvia-upgrade)
  --namespace NS            release namespace (default gryvia-system)
  --values-mode MODE        reset-then-reuse (--reset-then-reuse-values, what OPERATIONS.md documents; the default)
                            or reuse (--reuse-values; the chart refuses values that predate its sub-charts)
  --skip-build              do not build/load images (they are already in the cluster)
  --keep-cluster            do not delete the kind cluster at the end
  --dry-run                 print the plan and exit
  -h, --help
USAGE
}

BASE_CHART="" BASE_VERSION="" BASE_REF="" BASE_IMAGE_TAG=""
HEAD_CHART="" HEAD_CRDS="" CLI="" CLUSTER=gryvia-upgrade NS=gryvia-system VALUES_MODE=reset-then-reuse
SKIP_BUILD=0 KEEP=0 DRY=0
while [ $# -gt 0 ]; do
	case "$1" in
	--base-chart) BASE_CHART="${2:?}"; shift 2 ;;
	--base-version) BASE_VERSION="${2:?}"; shift 2 ;;
	--base-ref) BASE_REF="${2:?}"; shift 2 ;;
	--base-image-tag) BASE_IMAGE_TAG="${2:?}"; shift 2 ;;
	--head-chart) HEAD_CHART="${2:?}"; shift 2 ;;
	--head-crds) HEAD_CRDS="${2:?}"; shift 2 ;;
	--cli) CLI="${2:?}"; shift 2 ;;
	--cluster) CLUSTER="${2:?}"; shift 2 ;;
	--namespace) NS="${2:?}"; shift 2 ;;
	--values-mode) VALUES_MODE="${2:?}"; shift 2 ;;
	--skip-build) SKIP_BUILD=1; shift ;;
	--keep-cluster) KEEP=1; shift ;;
	--dry-run) DRY=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown option: $1" >&2; usage >&2; exit 64 ;;
	esac
done
case "$VALUES_MODE" in reuse | reset-then-reuse) ;; *) echo "bad --values-mode: $VALUES_MODE" >&2; exit 64 ;; esac
n=0
[ -n "$BASE_CHART" ] && n=$((n + 1))
[ -n "$BASE_VERSION" ] && n=$((n + 1))
[ -n "$BASE_REF" ] && n=$((n + 1))
[ "$n" -le 1 ] || { echo "give at most one of --base-chart, --base-version, --base-ref" >&2; exit 64; }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[ -n "$HEAD_CHART" ] || HEAD_CHART="$ROOT/helm/gryvia"
[ -n "$HEAD_CRDS" ] || HEAD_CRDS="$ROOT/crds"
[ -n "$CLI" ] || CLI="$ROOT/cli/target/release/gryvia"
REGISTRY_CHART=oci://ghcr.io/zyvorai/charts/gryvia
HEAD_TAG=dev
PF_PORT=18443
CTX="kind-$CLUSTER"

# ---- resolve the base ----
BASE_MODE="" # chart | version | ref
if [ -n "$BASE_CHART" ]; then BASE_MODE=chart
elif [ -n "$BASE_VERSION" ]; then BASE_MODE=version
elif [ -n "$BASE_REF" ]; then BASE_MODE=ref
else
	latest_tag="$(git -C "$ROOT" tag --list 'v*' --sort=-v:refname 2>/dev/null | head -n1 || true)"
	probe="$(mktemp -d)"
	if [ -n "$latest_tag" ] && { [ "$DRY" = 1 ] || helm pull "$REGISTRY_CHART" --version "${latest_tag#v}" --destination "$probe" >/dev/null 2>&1; }; then
		BASE_MODE=version BASE_VERSION="$latest_tag"
	else
		[ -n "$latest_tag" ] && echo "note: tag $latest_tag exists but its chart cannot be pulled from $REGISTRY_CHART; falling back to a git ref" >&2
		BASE_MODE=ref
		BASE_REF=origin/main
		if [ "$(git -C "$ROOT" rev-parse "$BASE_REF" 2>/dev/null || echo x)" = "$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || echo y)" ]; then BASE_REF=origin/main~1; fi
	fi
	rm -rf "$probe"
fi
case "$BASE_MODE" in
chart) BASE_DESC="chart $BASE_CHART" ;;
version) BASE_DESC="released chart $BASE_VERSION from $REGISTRY_CHART (images from ghcr.io)" ;;
ref) BASE_DESC="git ref $BASE_REF (chart and images built from a worktree)" ;;
esac

WORK=""
STEPS=() STATUS=() NOTES=()
FAILED=0
step_ok() { STEPS+=("$1") STATUS+=(PASS) NOTES+=("${2:-}"); echo "  PASS $1"; }
step_skip() { STEPS+=("$1") STATUS+=(SKIP) NOTES+=("${2:-}"); echo "  SKIP $1 ${2:-}"; }
step_fail() { STEPS+=("$1") STATUS+=(FAIL) NOTES+=("${2:-}"); FAILED=$((FAILED + 1)); echo "  FAIL $1 ${2:-}" >&2; }
say() { printf '\n== %s\n' "$*"; }

if [ "$DRY" = 1 ]; then
	cat <<PLAN
upgrade-test plan
  base:     $BASE_DESC
  head:     chart $HEAD_CHART (images built locally, tag $HEAD_TAG), CRDs $HEAD_CRDS
  cluster:  kind $CLUSTER, namespace $NS, values mode $VALUES_MODE
  steps:
   1. kind create cluster --name $CLUSTER
   2. build gryvia-{gpu-operator,ai-operator,quota-operator,api-gateway,ui} for head (tag $HEAD_TAG)$([ "$BASE_MODE" = ref ] && echo " and for the base worktree (tag base)") and kind load them
   3. helm install gryvia (base) -n $NS --set namespace.create=false --set nvidiaDevicePlugin.enabled=false --set dcgmExporter.enabled=false --set monitoring.enabled=false --wait
   4. kubectl apply -f examples/demo/demo.yaml -f scripts/tests/upgrade-fixtures.yaml
   5. snapshot every gryvia.io kind (spec only) -> before
   6. kubectl apply --server-side --force-conflicts -f $HEAD_CRDS
   7. helm upgrade gryvia $HEAD_CHART -n $NS $([ "$VALUES_MODE" = reuse ] && echo --reuse-values || echo --reset-then-reuse-values) --set global.imageTag=$HEAD_TAG --set global.imagePullPolicy=Never --wait; rollout status of every deployment
   8. assert: snapshot unchanged, gryvia status --brief == OK, gateway login works
   9. helm rollback gryvia 1 --wait; assert: deployments available, snapshot unchanged
  10. backup/restore: scripts/backup-crs.sh export, delete a quota and a SKU, restore, assert snapshot unchanged
  11. delete the kind cluster (unless --keep-cluster)
PLAN
	exit 0
fi

for tool in kind kubectl helm docker git python3 curl; do
	command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required" >&2; exit 69; }
done

WORK="$(mktemp -d)"
PF_PID=""
BASE_WT=""
cleanup() {
	set +e
	[ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
	[ -n "$BASE_WT" ] && git -C "$ROOT" worktree remove --force "$BASE_WT" 2>/dev/null
	if [ "$KEEP" = 0 ]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1; fi
	rm -rf "$WORK"
}
trap cleanup EXIT

kc() { kubectl --context "$CTX" "$@"; }
hm() { helm --kube-context "$CTX" "$@"; }

build_images() { # build_images SRC_DIR TAG
	local dir="$1" tag="$2" spec name ctx df
	for spec in "gpu-operator:operators/gpu-operator:operators/gpu-operator/Dockerfile" \
		"ai-operator:operators/ai-operator:operators/ai-operator/Dockerfile" \
		"quota-operator:operators/quota-operator:operators/quota-operator/Dockerfile" \
		"api-gateway:services/api-gateway:services/api-gateway/Dockerfile" \
		"ui:.:docker/Dockerfile.ui"; do
		IFS=: read -r name ctx df <<<"$spec"
		echo "building gryvia-$name:$tag from $dir"
		docker build -q -f "$dir/$df" -t "ghcr.io/zyvorai/gryvia-$name:$tag" "$dir/$ctx" >/dev/null
		kind load docker-image --name "$CLUSTER" "ghcr.io/zyvorai/gryvia-$name:$tag"
	done
}

# Demo values: the GPU add-ons need real GPUs; the namespace is created by the script, not the chart.
DEMO_VALUES=(--set namespace.create=false --set nvidiaDevicePlugin.enabled=false --set dcgmExporter.enabled=false --set monitoring.enabled=false)

# snapshot FILE: the spec of every gryvia.io object, without anything the API server or a controller owns.
snapshot() {
	local kinds
	kinds="$(kc get crd -o name | grep '\.gryvia\.io$' | sed 's#.*/##' | paste -sd, -)"
	kc get "$kinds" -A -o json | python3 -c '
import json, sys
d = json.load(sys.stdin)
out = {}
for it in d.get("items", []):
    md = it["metadata"]
    key = "%s/%s/%s" % (it["kind"], md.get("namespace", ""), md["name"])
    out[key] = it.get("spec", {})
json.dump(out, sys.stdout, indent=1, sort_keys=True)
' >"$1"
}

# same_snapshot BEFORE AFTER: every object in BEFORE is still there with the same spec (new objects that a
# controller created in the meantime are allowed and reported).
same_snapshot() {
	python3 - "$1" "$2" <<'PY'
import json, sys
a, b = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
bad = 0
for k, v in a.items():
    if k not in b:
        print("MISSING  ", k); bad += 1
    elif b[k] != v:
        print("CHANGED  ", k)
        for f in sorted(set(v) | set(b[k])):
            if v.get(f) != b[k].get(f):
                print("   .spec.%s: %r -> %r" % (f, v.get(f), b[k].get(f)))
        bad += 1
new = sorted(set(b) - set(a))
if new:
    print("new objects (created by controllers, allowed):", ", ".join(new))
print("%d objects compared, %d differ" % (len(a), bad))
sys.exit(1 if bad else 0)
PY
}

check_snapshot() { # check_snapshot STEP-NAME
	local out
	snapshot "$WORK/after.json"
	if out="$(same_snapshot "$WORK/before.json" "$WORK/after.json" 2>&1)"; then step_ok "$1" "$(printf '%s' "$out" | tail -n1)"; else printf '%s\n' "$out" >&2; step_fail "$1" "$(printf '%s' "$out" | head -n3 | tr '\n' ' ')"; fi
}

wait_deployments() {
	local d
	for d in $(kc -n "$NS" get deploy -o name); do kc -n "$NS" rollout status "$d" --timeout=300s || return 1; done
}

login_check() {
	kc -n "$NS" port-forward svc/gryvia-ui "$PF_PORT:443" >"$WORK/pf.log" 2>&1 &
	PF_PID=$!
	local i tok
	for i in $(seq 1 30); do curl -sk -o /dev/null "https://localhost:$PF_PORT/" && break; sleep 1; done
	tok="$(curl -sfk -X POST -H 'Content-Type: application/json' -d '{"username":"admin","password":"Admin@321"}' \
		"https://localhost:$PF_PORT/api/auth/login" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')" || tok=""
	local rc=1
	if [ -n "$tok" ] && curl -sfk -H "Authorization: Bearer $tok" "https://localhost:$PF_PORT/api/auth/me" >/dev/null; then rc=0; fi
	kill "$PF_PID" 2>/dev/null || true
	PF_PID=""
	return "$rc"
}

cli_check() {
	if [ ! -x "$CLI" ]; then step_skip "$1" "gryvia CLI not found at $CLI (build it: cd cli && cargo build --release)"; return; fi
	local out
	"$CLI" --context "$CTX" status --wait --wait-timeout 3m >/dev/null 2>&1 || true
	out="$("$CLI" --context "$CTX" status --brief 2>&1 || true)"
	if [ "$out" = OK ]; then step_ok "$1" "status --brief: OK"; else step_fail "$1" "status --brief printed: $out"; fi
}

summary() {
	echo
	echo "upgrade-test summary (base: $BASE_DESC)"
	local i
	for i in "${!STEPS[@]}"; do printf '  %-4s %-46s %s\n' "${STATUS[$i]}" "${STEPS[$i]}" "${NOTES[$i]}"; done
	if [ "$FAILED" -gt 0 ]; then echo "RESULT: FAIL ($FAILED step(s) failed)"; else echo "RESULT: PASS"; fi
}

# ---- run ----
run_step() { # run_step NAME CMD...: PASS when the command succeeds, FAIL (and abort) otherwise
	local name="$1"
	shift
	say "$name"
	if "$@"; then step_ok "$name"; else step_fail "$name"; summary; exit 1; fi
}

say "base: $BASE_DESC"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
	echo "kind cluster $CLUSTER already exists: refusing to reuse it (delete it or pick --cluster)" >&2
	exit 1
fi
run_step "kind cluster" kind create cluster --name "$CLUSTER" --wait 120s

# Base chart source.
BASE_CHART_DIR=""
BASE_SET=()
case "$BASE_MODE" in
chart)
	BASE_CHART_DIR="$BASE_CHART"
	[ -n "$BASE_IMAGE_TAG" ] && BASE_SET=(--set "global.imageTag=$BASE_IMAGE_TAG")
	;;
version)
	helm pull "$REGISTRY_CHART" --version "${BASE_VERSION#v}" --untar --untardir "$WORK/base" >/dev/null
	BASE_CHART_DIR="$WORK/base/gryvia"
	;;
ref)
	BASE_WT="$WORK/base-src"
	git -C "$ROOT" worktree add --detach "$BASE_WT" "$BASE_REF" >/dev/null
	BASE_CHART_DIR="$BASE_WT/helm/gryvia"
	BASE_SET=(--set global.imageTag=base --set global.imagePullPolicy=Never)
	;;
esac

if [ "$SKIP_BUILD" = 0 ]; then
	run_step "build and load head images" build_images "$ROOT" "$HEAD_TAG"
	[ "$BASE_MODE" = ref ] && run_step "build and load base images" build_images "$BASE_WT" base
fi

# Chart dependencies (the optional NVIDIA GPU Operator) for charts taken from a source tree.
prep_chart() {
	[ -f "$1/Chart.yaml" ] || return 1
	if grep -q '^dependencies:' "$1/Chart.yaml" && [ ! -d "$1/charts" ]; then
		helm repo add nvidia https://helm.ngc.nvidia.com/nvidia --force-update >/dev/null
		helm dependency build "$1" >/dev/null
	fi
}
run_step "prepare base chart" prep_chart "$BASE_CHART_DIR"
run_step "prepare head chart" prep_chart "$HEAD_CHART"

kc create namespace "$NS" --dry-run=client -o yaml | kc apply -f - >/dev/null
run_step "install base chart" hm upgrade --install gryvia "$BASE_CHART_DIR" -n "$NS" "${DEMO_VALUES[@]}" ${BASE_SET[@]+"${BASE_SET[@]}"} --wait --timeout 300s
run_step "base deployments rolled out" wait_deployments
# Today's demo data goes into the base release. kubectl applies each object on its own, so a base older than the
# data (a released chart predates newer kinds and fields) accepts what it can and rejects the rest. For a released
# or explicit chart that is expected: the objects it rejects are left out of the snapshot, listed below, and the
# snapshot-size check after this step still fails the test if too little was applied. A git-ref base is the
# previous main, which must accept today's demo data, so any rejection fails there.
apply_demo_data() {
	local out rc=0
	out="$(kc apply -f "$ROOT/examples/demo/demo.yaml" -f "$ROOT/scripts/tests/upgrade-fixtures.yaml" 2>&1)" || rc=$?
	printf '%s\n' "$out"
	[ "$rc" -eq 0 ] && return 0
	if [ "$BASE_MODE" = ref ]; then return 1; fi
	{
		echo "note: the base release did not accept every demo object; they are not part of the snapshot:"
		printf '%s\n' "$out" | grep -E '^(Error from server|error:)|resource mapping not found' | sed 's/^/  /' | cut -c1-200 || true
	} >&2
	return 0
}
run_step "apply demo data, a tenant and a quota" apply_demo_data
sleep "${UPGRADE_TEST_SETTLE:-10}" # let the controllers reconcile before the snapshot, so their own writes are not mistaken for changes
say "snapshot before the upgrade"
snapshot "$WORK/before.json"
echo "$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "$WORK/before.json") objects in the snapshot"
if [ "$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "$WORK/before.json")" -lt 3 ]; then step_fail "snapshot has objects" "fewer than 3 objects; the demo data did not apply"; summary; exit 1; fi
step_ok "snapshot before the upgrade"

# The documented upgrade: CRDs first (Helm never upgrades them), then the chart.
# apply-crds.sh recreates a CRD whose scope changed (GryviaJobHook: Cluster in v0.1.0-rc1, Namespaced since).
run_step "apply head CRDs server-side" env KUBECTL="kubectl --context $CTX" bash "$ROOT/scripts/apply-crds.sh" "$HEAD_CRDS"
values_flag=--reuse-values
[ "$VALUES_MODE" = reset-then-reuse ] && values_flag=--reset-then-reuse-values
run_step "helm upgrade to head" hm upgrade gryvia "$HEAD_CHART" -n "$NS" "$values_flag" --set "global.imageTag=$HEAD_TAG" --set global.imagePullPolicy=Never --wait --timeout 300s
run_step "head deployments rolled out" wait_deployments
say "after the upgrade"
check_snapshot "custom resources unchanged after upgrade"
cli_check "gryvia status --brief after upgrade"
say "gateway login"
if login_check; then step_ok "gateway login after upgrade"; else step_fail "gateway login after upgrade" "POST /api/auth/login or GET /api/auth/me failed"; fi

# Rollback.
run_step "helm rollback to revision 1" hm rollback gryvia 1 -n "$NS" --wait --timeout 300s
run_step "deployments healthy after rollback" wait_deployments
check_snapshot "custom resources intact after rollback"
say "gateway login after rollback"
if login_check; then step_ok "gateway login after rollback"; else step_fail "gateway login after rollback" "failed"; fi

# Backup and restore round trip.
say "backup and restore round trip"
export_ok=0
if "$ROOT/scripts/backup-crs.sh" export "$WORK/backup.yaml" 2>&1; then export_ok=1; fi
if [ "$export_ok" = 1 ]; then
	# Delete whichever of the two the base release could hold (an older base may have no SKU kind, or reject the fixture).
	to_delete=()
	for obj in gryviaquota/upgrade-test gryviagpusku/l40; do kc get "$obj" >/dev/null 2>&1 && to_delete+=("$obj"); done
	if [ "${#to_delete[@]}" -eq 0 ]; then
		step_fail "delete a quota and a SKU" "neither gryviaquota/upgrade-test nor gryviagpusku/l40 exists to delete"
		summary
		exit 1
	fi
	run_step "delete a quota and a SKU" kc delete "${to_delete[@]}" --wait=true
	snapshot "$WORK/after.json"
	if same_snapshot "$WORK/before.json" "$WORK/after.json" >/dev/null 2>&1; then step_fail "deleted objects are really gone" "the snapshot did not change after deleting"; else step_ok "deleted objects are really gone"; fi
	if "$ROOT/scripts/backup-crs.sh" restore "$WORK/backup.yaml"; then check_snapshot "restore from the export recreates the objects"; else step_fail "restore from the export recreates the objects" "restore failed"; fi
else
	step_fail "export of all custom resources" "backup-crs.sh export failed"
fi

summary
[ "$FAILED" -eq 0 ]
