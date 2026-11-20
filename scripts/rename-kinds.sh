#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Renames the legacy Fabric* API kinds to Gryvia* (Kinds, CRD plurals, Go identifiers, file names).
# Idempotent: running it again on an already-renamed tree changes nothing.
# Prose uses of the word "fabric" (RDMA fabric, GPU fabric, Fabric Manager) are left alone:
# only CamelCase identifiers (FabricAIJob) and lowercase resource names (fabricaijobs) are matched.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# 1. Rename paths (files only; git tracks directories through their files).
while IFS= read -r path; do
  new=$(printf '%s' "$path" | perl -pe 's/Fabric(?=[A-Z])/Gryvia/g; s/(^|[\/._-])fabric(?=[a-z])/${1}gryvia/g')
  [ "$new" = "$path" ] && continue
  mkdir -p "$(dirname "$new")"
  git mv -k "$path" "$new"
done < <(git ls-files | grep -iE 'fabric' | grep -vE '(package-lock\.json|\.lock)$' || true)

# 2. Rewrite text content.
git ls-files -z | grep -zvE '(^scripts/rename-kinds\.sh$|package-lock\.json|Cargo\.lock|go\.sum|\.(png|jpg|jpeg|gif|ico|svg|woff2?|ttf|pdf|tar|gz))$' \
  | xargs -0 grep -lIE 'Fabric[A-Z]|(^|[^A-Za-z0-9])fabric[A-Za-z]' 2>/dev/null \
  | xargs perl -pi -e 's/Fabric(?=[A-Z])/Gryvia/g; s/(?<![A-Za-z0-9])fabric(?=[A-Za-z])/gryvia/g' || true

# 3. Report anything that still looks like a legacy identifier.
if git grep -nE 'Fabric[A-Z]|\bfabric[a-z]+(s|es)?\.gryvia\.io' -- . ':!scripts/rename-kinds.sh' ':!scripts/deploy-remote.sh' ':!scripts/lib/deploy-guards.sh' ':!CHANGELOG.md' >/dev/null; then
  echo "Legacy identifiers remain:" >&2
  git grep -nE 'Fabric[A-Z]' -- . ':!scripts/rename-kinds.sh' ':!scripts/deploy-remote.sh' ':!scripts/lib/deploy-guards.sh' ':!CHANGELOG.md' | head -20 >&2
  exit 1
fi
echo "Rename complete."
