#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Copy monitoring/ assets into the Helm charts (Helm's .Files only reads inside a chart directory).
#   scripts/sync-monitoring-assets.sh          copy
#   scripts/sync-monitoring-assets.sh --check  exit 1 if a chart copy differs from monitoring/ (CI drift check)
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
mode="${1:-copy}"
rc=0
for chart in gryvia network-intelligence; do
  dest="$root/helm/$chart/monitoring-assets"
  if [ "$mode" = "--check" ]; then
    if ! diff -r -q <(cd "$root/monitoring" && find prometheus-rules.yaml grafana-dashboards -type f | sort) \
                    <(cd "$dest" 2>/dev/null && find . -type f | sed 's|^\./||' | sort) >/dev/null 2>&1 \
       || ! diff -r -q "$root/monitoring/prometheus-rules.yaml" "$dest/prometheus-rules.yaml" >/dev/null 2>&1 \
       || ! diff -r -q "$root/monitoring/grafana-dashboards" "$dest/grafana-dashboards" >/dev/null 2>&1; then
      echo "helm/$chart/monitoring-assets is out of date: run scripts/sync-monitoring-assets.sh and commit" >&2
      rc=1
    fi
  else
    rm -rf "$dest"
    mkdir -p "$dest/grafana-dashboards"
    cp "$root/monitoring/prometheus-rules.yaml" "$dest/"
    cp "$root"/monitoring/grafana-dashboards/*.json "$dest/grafana-dashboards/"
  fi
done
exit $rc
