#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Regenerate every CRD from the operators' Go types with controller-gen.
#
# The Go types under operators/*/api/v1 are the source of truth. Output goes to
# crds/ (canonical) and is copied to helm/gryvia/crds/, which Helm installs.
# Two kinds are defined in more than one operator; the owning operator wins:
#   GryviaAIJob   -> ai-operator   (quota-operator holds a read-only subset)
#   GryviaGpuNode -> gpu-operator  (ai-operator holds an identical copy)
#
# Usage: ./scripts/gen-crds.sh        (needs controller-gen on PATH or in ~/go/bin)
set -euo pipefail
cd "$(dirname "$0")/.."

CG="$(command -v controller-gen || true)"
[[ -x "$CG" ]] || CG="$HOME/go/bin/controller-gen"
[[ -x "$CG" ]] || { echo "controller-gen not found: go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest" >&2; exit 1; }

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

# Owners last so they overwrite non-owners' copies of the same kind.
for op in quota-operator storage-operator network-operator network-intelligence ai-operator gpu-operator; do
  (cd "operators/$op" && "$CG" crd:allowDangerousTypes=true paths=./api/... output:crd:dir="$stage/$op")
done
mkdir -p "$stage/all"
for op in quota-operator storage-operator network-operator network-intelligence gpu-operator ai-operator; do
  cp "$stage/$op"/*.yaml "$stage/all/"
done
# GryviaGpuNode: gpu-operator owns it (copied before ai-operator above), restore it.
cp "$stage/gpu-operator/gryvia.io_gryviagpunodes.yaml" "$stage/all/"

rm -f crds/*.yaml helm/gryvia/crds/*.yaml
mkdir -p crds helm/gryvia/crds
cp "$stage/all"/*.yaml crds/
cp "$stage/all"/*.yaml helm/gryvia/crds/
echo "generated $(ls crds | wc -l | tr -d ' ') CRDs into crds/ and helm/gryvia/crds/"
