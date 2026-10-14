#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
# Full verification. SKIP_SWIFT_TEST=1 skips XCTest on hosts with only Command Line Tools. Set REQUIRE_SWIFT=1 to fail (not skip) when Swift is missing.
set -euo pipefail
cd "$(dirname "$0")/.."
PY="${PYTHON:-python3}"
"$PY" -m unittest discover -s tests -v
"$PY" -m compileall -q server scripts runtimes
node --check web/app.js
if command -v swift >/dev/null; then
  cd native
  if [ "${SKIP_SWIFT_TEST:-0}" = 1 ]; then echo 'NOTE: swift test skipped (SKIP_SWIFT_TEST=1; XCTest needs Xcode)'; else swift test; fi
  # SWIFT_BUILD_ARGS e.g. "--product gryvia-agent" on hosts without full SwiftUI macro plugins
  swift build -c release ${SWIFT_BUILD_ARGS:-}
elif [ "${REQUIRE_SWIFT:-0}" = 1 ]; then
  echo 'FAILED: Swift compiler required (REQUIRE_SWIFT=1).' >&2; exit 1
else
  echo 'SKIPPED: Swift compiler unavailable. Run native checks on macOS.'
fi
