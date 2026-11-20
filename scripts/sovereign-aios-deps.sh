#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Fill helm/sovereign-aios/charts/ from local checkouts: Gryvia from this repository, Zyntra and Netra from sibling
# checkouts (ZYNTRA_DIR, NETRA_DIR; default ../zyntra and ../netra next to this repository). Netra does not publish
# a chart, so `helm dependency build` cannot fetch it; this is the way to render or install from source.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
chart="$root/helm/sovereign-aios"
parent="$(dirname "$root")"
zyntra="${ZYNTRA_DIR:-$parent/zyntra}/deploy/helm/zyntra"
netra="${NETRA_DIR:-$parent/netra}/helm/netra"

for d in "$zyntra" "$netra"; do
  if [[ ! -f "$d/Chart.yaml" ]]; then
    echo "missing chart: $d (set ZYNTRA_DIR / NETRA_DIR to the checkouts)" >&2
    exit 1
  fi
done

rm -rf "$chart/charts"
mkdir -p "$chart/charts"
cp -R "$root/helm/gryvia" "$chart/charts/gryvia"
rm -rf "$chart/charts/gryvia/charts"
cp -R "$zyntra" "$chart/charts/zyntra"
cp -R "$netra" "$chart/charts/netra"

for c in gryvia zyntra netra; do
  printf '%-8s %s\n' "$c" "$(awk '/^version:/ {print $2; exit}' "$chart/charts/$c/Chart.yaml")"
done
