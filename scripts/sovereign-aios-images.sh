#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Every container image the Sovereign AI OS runs, for the air gap (deploy/airgap/images.txt).
#
#   scripts/sovereign-aios-images.sh            helm/sovereign-aios (with its default values) and the agent runtime
#   scripts/sovereign-aios-images.sh --addons   plus the add-on charts in deploy/airgap/charts.yaml (pulls them)
#   scripts/sovereign-aios-images.sh --check    exit 1 when deploy/airgap/images.txt is not current (implies --addons)
#
# Images come from the pod specs the charts render, plus the agent runtime the ai-operator starts (--agent-image).
# Images a workload pulls at run time (training jobs, model servers you deploy) are yours to add.
# Needs helm and python3 with PyYAML.
set -euo pipefail
cd "$(dirname "$0")/.."
ADDONS=0 CHECK=0
for a in "$@"; do
  case "$a" in
    --addons) ADDONS=1 ;;
    --check) ADDONS=1 CHECK=1 ;;
    *) echo "usage: $0 [--addons|--check]" >&2; exit 2 ;;
  esac
done
[[ -d helm/sovereign-aios/charts ]] || scripts/sovereign-aios-deps.sh >/dev/null

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

helm template sa helm/sovereign-aios -n gryvia-system --kube-version 1.31.0 >"$WORK/sovereign-aios.yaml"
if [[ $ADDONS -eq 1 ]]; then
  python3 -c 'import sys, yaml
for c in yaml.safe_load(open("deploy/airgap/charts.yaml"))["charts"]:
    print(c["name"], c["repo"], c["version"], c["namespace"])' | while read -r name repo version ns; do
    extra=()
    [[ "$name" == falco ]] && extra=(-f deploy/hardening/falco-values.yaml)
    helm template "$name" "$name" --repo "$repo" --version "$version" -n "$ns" --kube-version 1.31.0 "${extra[@]}" \
      >"$WORK/addon-$name.yaml"
  done
fi

python3 - "$WORK" <<'EOF' >"$WORK/images.txt"
import glob, os, sys, yaml

def pod_spec(d):
    spec = d.get("spec") or {}
    if d["kind"] == "Pod":
        return spec
    if d["kind"] == "CronJob":
        return spec.get("jobTemplate", {}).get("spec", {}).get("template", {}).get("spec", {})
    return spec.get("template", {}).get("spec", {})

def normalize(ref):
    # Spell out Docker Hub so the list says where each image is pulled from.
    first = ref.split("/")[0]
    if "/" not in ref:
        return "docker.io/library/" + ref
    if "." not in first and ":" not in first and first != "localhost":
        return "docker.io/" + ref
    return ref

images = set()
for f in sorted(glob.glob(os.path.join(sys.argv[1], "*.yaml"))):
    for d in yaml.safe_load_all(open(f)):
        if not d or d.get("kind") not in ("Deployment", "DaemonSet", "StatefulSet", "Job", "CronJob", "Pod"):
            continue
        spec = pod_spec(d)
        for c in (spec.get("initContainers") or []) + (spec.get("containers") or []):
            images.add(normalize(c["image"]))
            for arg in c.get("args", []) or []:
                if isinstance(arg, str) and arg.startswith("--agent-image="):
                    images.add(normalize(arg.split("=", 1)[1]))
for i in sorted(images):
    print(i)
EOF

if [[ $CHECK -eq 1 ]]; then
  if cmp -s "$WORK/images.txt" deploy/airgap/images.txt; then
    echo "deploy/airgap/images.txt is current ($(wc -l <deploy/airgap/images.txt | tr -d ' ') images)"
  else
    echo "deploy/airgap/images.txt is stale; regenerate: scripts/sovereign-aios-images.sh --addons > deploy/airgap/images.txt"
    command diff -u deploy/airgap/images.txt "$WORK/images.txt" || true
    exit 1
  fi
else
  cat "$WORK/images.txt"
fi
