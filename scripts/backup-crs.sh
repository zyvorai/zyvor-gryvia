#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Export and restore every gryvia.io custom resource. Replaces the old tools/backup-restore.sh.
#
#   scripts/backup-crs.sh export gryvia-backup.yaml     # all kinds, all namespaces
#   scripts/backup-crs.sh restore gryvia-backup.yaml    # re-create them (existing objects are updated)
#   scripts/backup-crs.sh export - | less               # "-" is stdout
#
# The export drops what the API server owns (status, resourceVersion, uid, creationTimestamp, generation,
# managedFields, ownerReferences, finalizers), so a restore can create objects that were deleted; a raw
# `kubectl get -o yaml` cannot (the API server rejects a resourceVersion on create). Status is recomputed
# by the operators. Namespaces, Secrets (for example gryvia-api-key) and the CRDs themselves are NOT
# included: back them up separately (see website/docs/guides/OPERATIONS.md).
# Needs kubectl and python3. Honours KUBECTL_CONTEXT.
set -euo pipefail

usage() { echo "usage: backup-crs.sh export|restore FILE   (FILE '-' is stdout for export, stdin for restore)" >&2; exit 64; }
[ $# -eq 2 ] || usage
mode="$1" file="$2"

kc=(kubectl)
[ -n "${KUBECTL_CONTEXT:-}" ] && kc+=(--context "$KUBECTL_CONTEXT")

normalize() {
	python3 -c '
import json, sys
d = json.load(sys.stdin)
out = []
for it in d.get("items", []):
    md = it.get("metadata", {})
    ann = {k: v for k, v in (md.get("annotations") or {}).items() if k != "kubectl.kubernetes.io/last-applied-configuration"}
    m = {"name": md["name"]}
    if md.get("namespace"):
        m["namespace"] = md["namespace"]
    if md.get("labels"):
        m["labels"] = md["labels"]
    if ann:
        m["annotations"] = ann
    o = {"apiVersion": it["apiVersion"], "kind": it["kind"], "metadata": m}
    if "spec" in it:
        o["spec"] = it["spec"]
    for k, v in it.items():
        if k not in ("apiVersion", "kind", "metadata", "spec", "status"):
            o[k] = v
    out.append(o)
out.sort(key=lambda o: (o["kind"], o["metadata"].get("namespace", ""), o["metadata"]["name"]))
json.dump({"apiVersion": "v1", "kind": "List", "items": out}, sys.stdout, indent=1, sort_keys=True)
print()
'
}

case "$mode" in
export)
	kinds="$("${kc[@]}" get crd -o name | grep '\.gryvia\.io$' | sed 's#.*/##' | paste -sd, -)"
	[ -n "$kinds" ] || { echo "no gryvia.io CRDs found on this cluster" >&2; exit 1; }
	if [ "$file" = "-" ]; then
		"${kc[@]}" get "$kinds" -A -o json | normalize
	else
		"${kc[@]}" get "$kinds" -A -o json | normalize >"$file"
		echo "exported $(grep -c '"kind": "Gryvia' "$file" || true) objects to $file" >&2
	fi
	;;
restore)
	if [ "$file" = "-" ]; then "${kc[@]}" apply -f -; else "${kc[@]}" apply -f "$file"; fi
	;;
*) usage ;;
esac
