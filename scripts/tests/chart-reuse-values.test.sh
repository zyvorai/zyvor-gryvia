#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Upgrading from the oldest tagged release's values.
#
# `helm upgrade --reuse-values` renders the new chart with the previous release's values only: a sub-chart
# condition added since (nvidia.enabled, kueue.enabled, ...) is missing and Helm would enable that sub-chart.
# The chart must refuse those values and name the missing key.
#
# `helm upgrade --reset-then-reuse-values` (what OPERATIONS.md documents) renders the new chart's defaults with
# the previous values on top. The worst case is every old default set explicitly: that must render, enable no
# sub-chart, and not fail on a value map added later ("nil pointer evaluating interface {}.c").
# Needs the chart dependencies (`helm dependency build helm/gryvia`) and the git tags.
set -euo pipefail
cd "$(dirname "$0")/../.."
base_ref="${BASE_REF:-$(git tag --list 'v*' --sort=creatordate | head -1)}"
[ -n "$base_ref" ] || { echo "no release tag to take values from" >&2; exit 1; }
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
git show "$base_ref:helm/gryvia/values.yaml" > "$work/old-values.yaml"

cp -R helm/gryvia "$work/gryvia"
cp "$work/old-values.yaml" "$work/gryvia/values.yaml"
if out="$(helm template gryvia "$work/gryvia" --kube-version 1.34.0 2>&1)"; then
  if grep -q '^# Source: gryvia/charts/' <<<"$out"; then
    echo "FAIL: the $base_ref values alone enable a sub-chart" >&2; exit 1
  fi
else
  grep -q 'enabled is not set: the values come from an older chart' <<<"$out" \
    || { echo "FAIL: the $base_ref values alone fail without naming the missing key:" >&2; echo "$out" >&2; exit 1; }
fi
echo "$base_ref values alone (helm upgrade --reuse-values): no sub-chart enabled"

render() { helm template gryvia helm/gryvia -f "$work/old-values.yaml" --kube-version 1.34.0 "$@"; }
out="$(render)"
if grep -q '^# Source: gryvia/charts/' <<<"$out"; then
  echo "FAIL: the $base_ref values on the new defaults enable a sub-chart" >&2; exit 1
fi
# The same with every opt-in gate that reads a newer value map turned on through --set, as a user would
# when enabling a feature after the upgrade.
render --set webhook.enabled=true --set quotaOperator.enabled=true \
  --set quotaOperator.usageRecordWebhook.enabled=true --set quotaOperator.budgetWebhook.enabled=true \
  --set aiOperator.modelWatch.enabled=true > /dev/null
echo "head chart renders with the $base_ref values on its defaults (helm upgrade --reset-then-reuse-values)"
