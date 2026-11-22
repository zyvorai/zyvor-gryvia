#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Tests for scripts/upgrade-test.sh and scripts/backup-crs.sh: argument handling, --dry-run, and a whole run
# against fake kind/kubectl/helm/docker/curl/gryvia binaries that record their calls. No cluster needed.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
SCRIPT="scripts/upgrade-test.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILED=0
PASSED=0
CODE=0
OUT=""

ok()   { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; printf '%s\n' "$OUT" | sed 's/^/       | /' | tail -25; }
has()  { if grep -qF -- "$2" <<<"$OUT"; then ok "$1"; else fail "$1 (expected: $2)"; fi; }
lacks(){ if grep -qF -- "$2" <<<"$OUT"; then fail "$1 (unexpected: $2)"; else ok "$1"; fi; }
code() { if [[ "$CODE" == "$2" ]]; then ok "$1"; else OUT="exit=$CODE"$'\n'"$OUT"; fail "$1 (expected exit $2, got $CODE)"; fi; }
# called LABEL TEXT: the recorded calls contain TEXT
called() { if grep -qF -- "$2" "$TMP/calls.log"; then ok "$1"; else OUT="$(cat "$TMP/calls.log")"; fail "$1 (expected call: $2)"; fi; }
# order A B: the recorded call A comes before call B in the log
order() {
	local a b
	a="$(grep -nF -m1 -- "$2" "$TMP/calls.log" | cut -d: -f1)"
	b="$(grep -nF -m1 -- "$3" "$TMP/calls.log" | cut -d: -f1)"
	if [[ -n "$a" && -n "$b" && "$a" -lt "$b" ]]; then ok "$1"; else OUT="$(cat "$TMP/calls.log")"; fail "$1 ('$2' should come before '$3')"; fi
}

# ---- fakes ----
mkdir -p "$TMP/bin" "$TMP/state"
for t in kind docker; do
	cat >"$TMP/bin/$t" <<'FAKE'
#!/usr/bin/env bash
echo "$(basename "$0") $*" >>"$FAKE_LOG"
exit 0
FAKE
done
cat >"$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
echo "curl $*" >>"$FAKE_LOG"
case "$*" in
*"/api/auth/login"*) [ "${FAKE_LOGIN_FAIL:-0}" = 1 ] && exit 22; echo '{"token":"t0k"}' ;;
*) echo '{}' ;;
esac
FAKE
cat >"$TMP/bin/helm" <<'FAKE'
#!/usr/bin/env bash
echo "helm $*" >>"$FAKE_LOG"
case "$*" in
*"upgrade gryvia "*) touch "$FAKE_STATE/upgraded" ;;
*"rollback "*) touch "$FAKE_STATE/rolledback" ;;
esac
exit 0
FAKE
cat >"$TMP/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
echo "kubectl $*" >>"$FAKE_LOG"
args="$*"
case "$args" in
*"get crd -o name"*) echo "customresourcedefinition.apiextensions.k8s.io/gryviaquotas.gryvia.io"; echo "customresourcedefinition.apiextensions.k8s.io/gryviagpuskus.gryvia.io" ;;
*"get gryviaquotas.gryvia.io,gryviagpuskus.gryvia.io -A -o json"*)
	python3 - <<'PY'
import json, os
st = os.environ["FAKE_STATE"]
items = [
  {"apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaQuota", "metadata": {"name": n, "resourceVersion": "9", "uid": "u"}, "spec": {"team": n}, "status": {"x": 1}}
  for n in ("demo-team", "upgrade-test")
] + [{"apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaGpuSku", "metadata": {"name": "l40", "uid": "z"}, "spec": {"price": 1}}]
if os.path.exists(st + "/deleted") and not os.path.exists(st + "/restored"):
    items = [i for i in items if i["metadata"]["name"] not in ("upgrade-test", "l40")]
if os.environ.get("FAKE_DRIFT") == "1" and os.path.exists(st + "/upgraded") and not os.path.exists(st + "/rolledback"):
    items[0]["spec"] = {"team": "changed-by-upgrade"}
print(json.dumps({"items": items}))
PY
	;;
*" delete "*) touch "$FAKE_STATE/deleted" ;;
*"apply -f "*backup.yaml*) touch "$FAKE_STATE/restored" ;;
*"apply -f -"*) cat >/dev/null ;;
*"create namespace"*) echo "apiVersion: v1" ;;
*"get deploy -o name"*) echo "deployment.apps/gryvia-ui" ;;
*"port-forward"*) sleep 30 ;;
esac
exit 0
FAKE
cat >"$TMP/bin/gryvia" <<'FAKE'
#!/usr/bin/env bash
echo "gryvia $*" >>"$FAKE_LOG"
case "$*" in *"--brief"*) echo "${FAKE_STATUS:-OK}" ;; esac
FAKE
chmod +x "$TMP"/bin/*
mkdir -p "$TMP/base-chart" "$TMP/head-chart" "$TMP/crds"
printf 'apiVersion: v2\nname: gryvia\nversion: 0.0.1\n' | tee "$TMP/base-chart/Chart.yaml" >"$TMP/head-chart/Chart.yaml"

run() { # run [ENV=VAL ...] -- args
	local envs=()
	while [[ $# -gt 0 && "$1" != "--" ]]; do envs+=("$1"); shift; done
	shift
	rm -f "$TMP/state"/*
	: >"$TMP/calls.log"
	OUT="$(env "${envs[@]}" PATH="$TMP/bin:$PATH" FAKE_LOG="$TMP/calls.log" FAKE_STATE="$TMP/state" UPGRADE_TEST_SETTLE=0 \
		bash "$SCRIPT" --head-chart "$TMP/head-chart" --head-crds "$TMP/crds" --cli "$TMP/bin/gryvia" "$@" 2>&1)"
	CODE=$?
}

echo "arguments"
OUT="$(bash "$SCRIPT" --bogus 2>&1)"; CODE=$?
code "unknown option exits 64" 64
OUT="$(bash "$SCRIPT" --base-ref a --base-version v1 2>&1)"; CODE=$?
code "two base sources exit 64" 64
has "explains" "at most one of"
OUT="$(bash "$SCRIPT" --values-mode bogus 2>&1)"; CODE=$?
code "bad values mode exits 64" 64
OUT="$(bash "$SCRIPT" --help 2>&1)"; CODE=$?
code "--help exits 0" 0
has "documents --base-ref" "--base-ref GIT_REF"

echo "dry run"
OUT="$(bash "$SCRIPT" --dry-run --base-ref origin/main~2 2>&1)"; CODE=$?
code "succeeds" 0
has "names the base ref" "git ref origin/main~2"
has "applies CRDs server-side" "kubectl apply --server-side --force-conflicts -f"
has "resets then reuses values by default" "--reset-then-reuse-values"
has "rolls back" "helm rollback gryvia 1"
has "round trips the backup" "backup-crs.sh export"
OUT="$(bash "$SCRIPT" --dry-run --base-version v1.2.3 2>&1)"; CODE=$?
has "released chart mode" "released chart v1.2.3 from oci://ghcr.io/zyvorai/charts/gryvia"
lacks "released chart needs no base images" "and for the base worktree"
OUT="$(bash "$SCRIPT" --dry-run --base-chart ./old --values-mode reuse 2>&1)"; CODE=$?
has "explicit chart" "chart ./old"
has "reuse-values on request" "--reuse-values"
lacks "no reset with reuse" "--reset-then-reuse-values"

echo "full run, nothing lost"
run -- --base-chart "$TMP/base-chart" --base-image-tag old
code "passes" 0
has "summary passes" "RESULT: PASS"
has "custom resources unchanged after upgrade" "PASS custom resources unchanged after upgrade"
has "restore recreates the objects" "PASS restore from the export recreates the objects"
has "status is checked" "PASS gryvia status --brief after upgrade"
has "login is checked twice" "PASS gateway login after rollback"
order "base installed before the CRDs are applied" "upgrade --install gryvia $TMP/base-chart" "apply --server-side --force-conflicts -f $TMP/crds"
order "CRDs before helm upgrade" "apply --server-side --force-conflicts -f $TMP/crds" "upgrade gryvia $TMP/head-chart"
order "upgrade before rollback" "upgrade gryvia $TMP/head-chart" "rollback gryvia 1"
order "rollback before the backup" "rollback gryvia 1" "delete gryviaquota/upgrade-test"
called "uses the given base image tag" "global.imageTag=old"
called "head upgrade sets the dev tag" "global.imageTag=dev"
if grep -q "kind delete cluster --name gryvia-upgrade" "$TMP/calls.log"; then ok "kind cluster deleted"; else OUT="$(cat "$TMP/calls.log")"; fail "kind cluster not deleted"; fi
run -- --base-chart "$TMP/base-chart" --skip-build --keep-cluster --namespace foo --cluster c1
called "honours --cluster" "kind create cluster --name c1"
if grep -q "docker build" "$TMP/calls.log"; then OUT="built images"; fail "--skip-build still builds"; else ok "--skip-build builds nothing"; fi
if grep -q "kind delete" "$TMP/calls.log"; then OUT="deleted"; fail "--keep-cluster deletes"; else ok "--keep-cluster keeps the cluster"; fi
called "honours --namespace" "-n foo"
run -- --base-chart "$TMP/base-chart"
if grep -q "docker build.*-t ghcr.io/zyvorai/gryvia-ui:dev" "$TMP/calls.log"; then ok "builds head images"; else OUT="$(cat "$TMP/calls.log")"; fail "no head image build"; fi

echo "failures are detected"
run FAKE_DRIFT=1 -- --base-chart "$TMP/base-chart"
code "a changed spec fails the run" 1
has "reports the change" "FAIL custom resources unchanged after upgrade"
has "names the object" "CHANGED   GryviaQuota//demo-team"
has "but the rollback restores it" "PASS custom resources intact after rollback"
run FAKE_STATUS=DEGRADED -- --base-chart "$TMP/base-chart"
code "status not OK fails" 1
has "shows what status printed" "status --brief printed: DEGRADED"
run FAKE_LOGIN_FAIL=1 -- --base-chart "$TMP/base-chart"
code "login failure fails" 1
has "names the login" "FAIL gateway login after upgrade"
run -- --base-chart "$TMP/base-chart" --cli /nonexistent
code "a missing CLI is a skip, not a failure" 0
has "skipped" "SKIP gryvia status --brief after upgrade"

echo "backup-crs.sh"
OUT="$(PATH="$TMP/bin:$PATH" FAKE_LOG="$TMP/calls.log" FAKE_STATE="$TMP/state" bash scripts/backup-crs.sh export - 2>&1)"; CODE=$?
code "export succeeds" 0
if python3 - "$OUT" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
assert len(d["items"]) == 3, d
for it in d["items"]:
    assert "status" not in it and "resourceVersion" not in it["metadata"] and "uid" not in it["metadata"], it
    assert it["spec"], it
PY
then ok "export drops status, uid and resourceVersion"; else fail "export content"; fi
OUT="$(bash scripts/backup-crs.sh 2>&1)"; CODE=$?
code "usage error exits 64" 64

echo
echo "$PASSED passed, $FAILED failed"
[[ "$FAILED" == 0 ]]
