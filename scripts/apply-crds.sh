#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Applies Gryvia's CRDs before a `helm upgrade` (Helm installs crds/ only on first install, never upgrades them).
#
#   scripts/apply-crds.sh [crds-dir]        # default: the repository's crds/
#   KUBECTL="kubectl --context kind-x" scripts/apply-crds.sh crds/
#
# A CRD's scope cannot change in place. When an installed CRD has another scope than the new one, it is deleted
# and recreated, but only when it holds no objects (deleting a CRD deletes its objects); otherwise the script stops
# and says what to do. Example: GryviaJobHook was cluster-scoped in v0.1.0-rc1 and is namespaced since.
set -euo pipefail

DIR="${1:-$(cd "$(dirname "$0")/.." && pwd)/crds}"
read -r -a KC <<<"${KUBECTL:-kubectl}"

for f in "$DIR"/*.yaml; do
	[ -f "$f" ] || continue
	crd="$(sed -n 's/^  name: \(.*\.gryvia\.io\)$/\1/p' "$f" | head -1)"
	want="$(sed -n 's/^  scope: //p' "$f" | head -1)"
	[ -n "$crd" ] && [ -n "$want" ] || continue
	have="$("${KC[@]}" get crd "$crd" -o jsonpath='{.spec.scope}' 2>/dev/null || true)"
	[ -n "$have" ] && [ "$have" != "$want" ] || continue
	n="$("${KC[@]}" get "$crd" -A --no-headers 2>/dev/null | wc -l | tr -d ' ')"
	if [ "$n" != 0 ]; then
		echo "CRD $crd is $have-scoped but is now $want and has $n object(s): back them up (kubectl get $crd -A -o yaml)," \
			"delete them and the CRD, then run this again" >&2
		exit 1
	fi
	echo "Recreating $crd (scope $have -> $want, no objects)"
	"${KC[@]}" delete crd "$crd" --wait=true
done
"${KC[@]}" apply --server-side --force-conflicts -f "$DIR"
