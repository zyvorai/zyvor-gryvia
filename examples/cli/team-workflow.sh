#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Example: team manager workflow (read-only). Needs a kubeconfig for the cluster; `jq` is optional.
#
#   TEAM=ml-research NAMESPACE=ml-training ./team-workflow.sh
set -euo pipefail

TEAM="${TEAM:-ml-research}"
NAMESPACE="${NAMESPACE:-ml-training}"

echo "Team: $TEAM"
echo ""

# 1. Quota and budget status (the team is a positional argument)
echo "=== Quota Status ==="
gryvia quota "$TEAM" --budget

echo ""

# 2. Current spending
echo "=== Monthly Costs ==="
gryvia cost "$TEAM" --period month

echo ""

# 3. The team's jobs
echo "=== Jobs ==="
gryvia list jobs -n "$NAMESPACE"

echo ""

# 4. Is the team approaching its budget? Read status.budgetStatus.percentUsed from the JSON output.
if command -v jq >/dev/null 2>&1; then
  PERCENT=$(gryvia quota "$TEAM" -o json | jq -r '[.[].status.budgetStatus.percentUsed // empty] | first // empty')
  if [[ -z "$PERCENT" ]]; then
    echo "No budget status reported for team $TEAM"
  else
    echo "Budget usage: ${PERCENT}%"
    if awk -v p="$PERCENT" 'BEGIN { exit !(p > 80) }'; then
      echo "WARNING: team is approaching its budget limit"
    else
      echo "Budget status is healthy"
    fi
  fi
else
  echo "Install jq to check the budget percentage from the command line."
fi
