#!/bin/sh
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Collect the docs/gpu-validation.md checklist into one file for the PR evidence.
set -eu
ns="${NS:-gryvia-system}"
out="${1:-gpu-validation-report.txt}"
{
  echo "# Gryvia GPU validation"
  echo "date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
  echo "## nodes"
  kubectl get nodes -o wide || true
  echo
  echo "## allocatable gpu"
  kubectl get nodes -o json | jq -r '.items[] | "\(.metadata.name) \(.status.allocatable["nvidia.com/gpu"] // "0")"' || true
  echo
  echo "## gryviagpunodes"
  kubectl get gryviagpunodes -o wide || true
  echo
  echo "## smoke job"
  kubectl -n "$ns" get job gryvia-gpu-smoke -o yaml || true
  kubectl -n "$ns" logs job/gryvia-gpu-smoke || true
} > "$out"
echo "wrote $out"
