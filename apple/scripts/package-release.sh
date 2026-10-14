#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
# Package platform-independent release archives into release/ (runs on macOS or Linux):
#
#   scripts/package-release.sh [VERSION]      (default: the VERSION file)
#
#   gryvia-server-VERSION.tar.gz   control plane: server, runtimes, web, scripts, docs, LICENSE, NOTICE
#   gryvia-sdk-VERSION.tar.gz      Swift sources of the native agent and app (for building from source)
#   gryvia-VERSION.cdx.json        CycloneDX SBOM
#   SHA256SUMS                     covers every file in release/ at the time it runs
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
FILE_VERSION="$(tr -d '[:space:]' < VERSION)"
VERSION="${1:-$FILE_VERSION}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.]+)?$ ]] || { echo "invalid version: $VERSION" >&2; exit 2; }
[[ "$VERSION" == "$FILE_VERSION" ]] || { echo "VERSION file is $FILE_VERSION, not $VERSION" >&2; exit 2; }
python3 scripts/set-version.py --check >/dev/null || { python3 scripts/set-version.py --check >&2; exit 2; }

OUT="${OUT:-release}"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$OUT"
TAR=(tar --owner=0 --group=0 --numeric-owner)
tar --version 2>/dev/null | grep -q GNU || TAR=(tar --no-mac-metadata --no-xattrs)
export COPYFILE_DISABLE=1

srv="gryvia-server-$VERSION"
mkdir -p "$STAGE/$srv"
for d in server runtimes web scripts brand docs mcp; do [[ -d "$d" ]] && cp -R "$d" "$STAGE/$srv/"; done
cp LICENSE NOTICE VERSION README.md CHANGELOG.md SECURITY.md "$STAGE/$srv/"
find "$STAGE" -name __pycache__ -prune -exec rm -rf {} +
cat > "$STAGE/$srv/README.txt" <<TXT
Gryvia Apple control plane $VERSION (source-available, Business Source License 1.1).

  python3 scripts/configure.py --output local            # writes tokens (0600)
  python3 -m server.app --config local/config.json --db local/gryvia.db
  open http://127.0.0.1:8443

Requires Python 3.14 (standard library only). The native agent and app need macOS 15+ and Swift 6.
https://github.com/zyvorai/zyvor-gryvia-apple
TXT

sdk="gryvia-sdk-$VERSION"
mkdir -p "$STAGE/$sdk"
cp -R native "$STAGE/$sdk/native"
rm -rf "$STAGE/$sdk/native/.build" "$STAGE/$sdk/native/.swiftpm"
cp LICENSE NOTICE VERSION "$STAGE/$sdk/"

for name in "$srv" "$sdk"; do
  "${TAR[@]}" -C "$STAGE" -czf "$OUT/$name.tar.gz" "$name"
done
python3 scripts/make-sbom.py "$VERSION" > "$OUT/gryvia-$VERSION.cdx.json"
if command -v sha256sum >/dev/null; then SUM=(sha256sum); else SUM=(shasum -a 256); fi
(
  cd "$OUT"
  files=()
  for f in *; do
    case "$f" in SHA256SUMS|*.sha256) continue ;; esac
    files+=("$f")
  done
  "${SUM[@]}" "${files[@]}" > SHA256SUMS
)
ls -l "$OUT"
cat "$OUT/SHA256SUMS"
