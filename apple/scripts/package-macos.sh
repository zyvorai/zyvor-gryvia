#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
# Backwards-compatible wrapper: builds release/ via package-mac.sh and mirrors the app to dist/Gryvia.app.
set -euo pipefail
cd "$(dirname "$0")/.."
bash scripts/package-mac.sh "$(tr -d '[:space:]' < VERSION)"
rm -rf dist/Gryvia.app && mkdir -p dist
V="$(tr -d '[:space:]' < VERSION)"
ditto -x -k "release/Gryvia-$V.zip" dist/
cp "release/Gryvia-$V.zip" dist/Gryvia-macOS.zip
