#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Tests scripts/apply-crds.sh with a fake kubectl: a CRD whose scope changed is recreated only when it has no objects.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/crds"
pass=0 fail=0
ok() { echo "  ok   $1"; pass=$((pass + 1)); }
bad() { echo "  FAIL $1"; fail=$((fail + 1)); }

for k in hooks:Namespaced quotas:Namespaced; do
	cat >"$TMP/crds/gryvia.io_gryvia${k%%:*}.yaml" <<EOF
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: gryvia${k%%:*}.gryvia.io
spec:
  group: gryvia.io
  scope: ${k##*:}
EOF
done

# FAKE_SCOPE: installed scope of gryviahooks; FAKE_OBJECTS: its object count. Every call is logged.
cat >"$TMP/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_LOG"
case "$*" in
*"get crd gryviahooks.gryvia.io -o jsonpath"*) printf '%s' "$FAKE_SCOPE" ;;
*"get crd gryviaquotas.gryvia.io -o jsonpath"*) printf 'Namespaced' ;;
*"get gryviahooks.gryvia.io -A --no-headers"*) i=0; while [ "$i" -lt "$FAKE_OBJECTS" ]; do echo obj; i=$((i + 1)); done ;;
esac
EOF
chmod +x "$TMP/bin/kubectl"

run() { # <scope> <objects>
	: >"$TMP/log"
	FAKE_LOG="$TMP/log" FAKE_SCOPE="$1" FAKE_OBJECTS="$2" PATH="$TMP/bin:$PATH" \
		bash "$ROOT/scripts/apply-crds.sh" "$TMP/crds" >"$TMP/out" 2>&1
}
logged() { grep -qF -- "$1" "$TMP/log"; }

if run Cluster 0 && logged "delete crd gryviahooks.gryvia.io" && ! logged "delete crd gryviaquotas" &&
	logged "apply --server-side --force-conflicts -f $TMP/crds" &&
	[ "$(grep -n 'delete crd' "$TMP/log" | cut -d: -f1)" -lt "$(grep -n 'apply ' "$TMP/log" | cut -d: -f1)" ]; then
	ok "scope changed, no objects: the CRD is deleted, then all CRDs are applied"
else bad "scope changed, no objects"; cat "$TMP/out" "$TMP/log"; fi

if ! run Cluster 2 && ! logged "delete crd" && ! logged "apply " && grep -q "has 2 object(s)" "$TMP/out"; then
	ok "scope changed with objects: stops before deleting or applying"
else bad "scope changed with objects"; cat "$TMP/out" "$TMP/log"; fi

if run Namespaced 0 && ! logged "delete crd" && logged "apply --server-side"; then
	ok "same scope: applied in place"
else bad "same scope"; cat "$TMP/out" "$TMP/log"; fi

if run "" 0 && ! logged "delete crd" && logged "apply --server-side"; then
	ok "not installed yet: applied"
else bad "not installed yet"; cat "$TMP/out" "$TMP/log"; fi

if KUBECTL="kubectl --context kind-x" run Cluster 0 && logged "--context kind-x apply --server-side"; then
	ok "KUBECTL with arguments is used for every call"
else bad "KUBECTL"; cat "$TMP/out" "$TMP/log"; fi

echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
