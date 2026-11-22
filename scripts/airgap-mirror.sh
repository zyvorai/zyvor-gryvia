#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Carry the Sovereign AI OS into an air gap through a Harbor registry.
#
# Connected side: save every image in deploy/airgap/images.txt (with its cosign signatures and attestations) and every
# chart (helm/sovereign-aios with its sub-charts, and the add-ons in deploy/airgap/charts.yaml) into a directory:
#
#   scripts/airgap-mirror.sh pull /media/transfer
#
# Air-gapped side: push them into Harbor as <harbor>/<image path without the registry host> and the charts to
# oci://<harbor>/charts, optionally re-signing the Zyvor images with your cosign key for Kyverno's key mode:
#
#   scripts/airgap-mirror.sh push /media/transfer harbor.sovereign.internal [--sign-key cosign.key] [--create-projects]
#
# push --create-projects creates the Harbor projects through its API (HARBOR_USER and HARBOR_PASSWORD). Log in first
# (docker login / cosign login / helm registry login <harbor>). Needs cosign, helm and python3 with PyYAML.
set -euo pipefail
cd "$(dirname "$0")/.."

usage() { sed -n '2,16p' "$0" >&2; exit 2; }
[[ $# -ge 2 ]] || usage
CMD="$1" DIR="$2"
shift 2

# ghcr.io/zyvorai/gryvia-ui:1.0.0 -> zyvorai/gryvia-ui:1.0.0; busybox -> library/busybox; drops a pinned digest's tag.
mirror_path() {
  local ref="$1" first="${1%%/*}"
  if [[ "$ref" != */* ]]; then ref="library/$ref"
  elif [[ "$first" == *.* || "$first" == *:* || "$first" == localhost ]]; then ref="${ref#*/}"
  fi
  echo "$ref"
}
# A file name for an image in the transfer directory.
slug() { echo "$1" | tr '/:@' '___'; }

charts() {
  python3 -c 'import yaml
for c in yaml.safe_load(open("deploy/airgap/charts.yaml"))["charts"]: print(c["name"], c["repo"], c["version"])'
}

case "$CMD" in
  pull)
    mkdir -p "$DIR/images" "$DIR/charts"
    cp deploy/airgap/images.txt deploy/airgap/charts.yaml "$DIR/"
    while read -r img; do
      [[ -n "$img" ]] || continue
      echo "save $img"
      cosign save "$img" --dir "$DIR/images/$(slug "$img")"
    done <deploy/airgap/images.txt
    [[ -d helm/sovereign-aios/charts ]] || scripts/sovereign-aios-deps.sh
    helm package helm/sovereign-aios -d "$DIR/charts"
    charts | while read -r name repo version; do
      helm pull "$name" --repo "$repo" --version "$version" -d "$DIR/charts"
    done
    sums="$(cd "$DIR" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 shasum -a 256)"
    echo "$sums" >"$DIR/SHA256SUMS"
    echo "saved $(wc -l <"$DIR/images.txt" | tr -d ' ') images and $(find "$DIR/charts" -name '*.tgz' | wc -l | tr -d ' ') charts to $DIR"
    ;;
  push)
    [[ $# -ge 1 ]] || usage
    REG="${1%/}"
    shift
    KEY="" CREATE=0
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --sign-key) KEY="$2"; shift 2 ;;
        --create-projects) CREATE=1; shift ;;
        *) usage ;;
      esac
    done
    (cd "$DIR" && shasum -a 256 -c --quiet SHA256SUMS) || { echo "transfer directory does not match SHA256SUMS"; exit 1; }
    if [[ $CREATE -eq 1 ]]; then
      : "${HARBOR_USER:?}" "${HARBOR_PASSWORD:?}"
      projects="$( (while read -r img; do mirror_path "$img"; done <"$DIR/images.txt"; echo charts/x) | cut -d/ -f1 | sort -u)"
      for p in $projects; do
        code="$(curl -s -o /dev/null -w '%{http_code}' -u "$HARBOR_USER:$HARBOR_PASSWORD" -H 'Content-Type: application/json' \
          -X POST "https://$REG/api/v2.0/projects" -d "{\"project_name\":\"$p\",\"metadata\":{\"public\":\"false\"}}")"
        case "$code" in 201) echo "created project $p" ;; 409) ;; *) echo "creating project $p: HTTP $code"; exit 1 ;; esac
      done
    fi
    while read -r img; do
      [[ -n "$img" ]] || continue
      dst="$REG/$(mirror_path "$img")"
      dst="${dst%@*}"
      echo "load $img -> $dst"
      cosign load --dir "$DIR/images/$(slug "$img")" "$dst"
      if [[ -n "$KEY" && "$img" == ghcr.io/zyvorai/* ]]; then
        # Offline: no Rekor upload and no TUF-provided signing config.
        cosign sign --yes --recursive --key "$KEY" --tlog-upload=false --use-signing-config=false "$dst"
      fi
    done <"$DIR/images.txt"
    for c in "$DIR"/charts/*.tgz; do
      helm push "$c" "oci://$REG/charts"
    done
    echo "pushed to $REG; install with deploy/airgap/values-airgap.yaml or deploy/argocd/sovereign-aios"
    ;;
  *) usage ;;
esac
