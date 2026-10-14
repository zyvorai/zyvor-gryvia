#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
set -euo pipefail
cd "$(dirname "$0")/.."
[[ "$(uname -s)" == Darwin ]] || { echo 'macOS required'; exit 1; }
(cd native && swift test && swift build -c release)
appdir="$PWD/dist/Gryvia.app"
mkdir -p "$appdir/Contents/MacOS" "$appdir/Contents/Resources"
[[ -f native/Gryvia.icns ]] || bash scripts/make-icon.sh
cp native/Gryvia.icns "$appdir/Contents/Resources/Gryvia.icns"
cp native/.build/release/GryviaDesktop "$appdir/Contents/MacOS/GryviaDesktop"
cat > "$appdir/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>dev.zyvor.gryvia.desktop</string><key>CFBundleName</key><string>Gryvia</string><key>CFBundleIconFile</key><string>Gryvia</string><key>NSHumanReadableCopyright</key><string>Copyright © 2026 Zyvor AI Labs Private Limited. BUSL-1.1.</string><key>CFBundleExecutable</key><string>GryviaDesktop</string><key>CFBundlePackageType</key><string>APPL</string><key>CFBundleShortVersionString</key><string>0.1.0</string><key>LSMinimumSystemVersion</key><string>15.0</string><key>NSHighResolutionCapable</key><true/></dict></plist>
PLIST
if [[ -n "${GRYVIA_SIGNING_IDENTITY:-}" ]]; then
  codesign --force --options runtime --sign "$GRYVIA_SIGNING_IDENTITY" "$appdir"
else
  codesign --force --sign - "$appdir"
  echo 'Ad hoc signed development build. Public distribution needs Developer ID and notarization.'
fi
(cd dist && ditto -c -k --keepParent Gryvia.app Gryvia-macOS.zip)
echo "$appdir"
