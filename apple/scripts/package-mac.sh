#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
# Package the native Mac app, agent and control plane into release/:
#
#   scripts/package-mac.sh VERSION      (must equal VERSION and CFBundleShortVersionString in native/Info.plist)
#
#   Gryvia-VERSION.dmg (+ .sha256)   drag-to-Applications disk image
#   Gryvia-VERSION.pkg (+ .sha256)   installer into /Applications (Apple silicon, macOS 15+)
#   Gryvia-VERSION.zip (+ .sha256)   the .app alone, for scripts and CI artifacts
#
# Gryvia.app contains the SwiftUI app, the native agent (Contents/Helpers/gryvia-agent) and the control plane
# (Contents/Resources/gryvia: server, runtimes, web, scripts), so a pkg install is self-contained.
#
# Signed and notarized only when these are set; otherwise the app is ad hoc signed, the DMG/pkg are unsigned and
# Gatekeeper blocks the first launch on other Macs. The script prints which of those you got.
#   DEVELOPER_ID_APP        "Developer ID Application: NAME (TEAMID)"   signs the app and the DMG
#   DEVELOPER_ID_INSTALLER  "Developer ID Installer: NAME (TEAMID)"     signs the pkg
#   NOTARY_PROFILE          keychain profile from `xcrun notarytool store-credentials`
set -euo pipefail
export COPYFILE_DISABLE=1   # no AppleDouble (._*) files in archives
cd "$(dirname "${BASH_SOURCE[0]}")/.."
[[ "$(uname -s)" == Darwin ]] || { echo "Run on macOS." >&2; exit 1; }
VERSION="${1:?usage: scripts/package-mac.sh VERSION}"
FILE_VERSION="$(tr -d '[:space:]' < VERSION)"
PLIST_VERSION="$(plutil -extract CFBundleShortVersionString raw native/Info.plist)"
[[ "$VERSION" == "$FILE_VERSION" ]] || { echo "VERSION file is $FILE_VERSION, not $VERSION" >&2; exit 2; }
[[ "$VERSION" == "$PLIST_VERSION" ]] || { echo "native/Info.plist is $PLIST_VERSION, not $VERSION" >&2; exit 2; }

APP_ID="${DEVELOPER_ID_APP:-}"
PKG_ID="${DEVELOPER_ID_INSTALLER:-}"
NOTARY="${NOTARY_PROFILE:-}"
OUT="${OUT:-release}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT"
DMG="$OUT/Gryvia-$VERSION.dmg"; PKG="$OUT/Gryvia-$VERSION.pkg"; ZIP="$OUT/Gryvia-$VERSION.zip"
rm -f "$DMG" "$DMG.sha256" "$PKG" "$PKG.sha256" "$ZIP" "$ZIP.sha256"

[[ -f native/Gryvia.icns ]] || bash scripts/make-icon.sh
(cd native && swift build -c release)
BIN=native/.build/release

APP="$WORK/Gryvia.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Helpers" "$APP/Contents/Resources/gryvia"
cp native/Info.plist "$APP/Contents/Info.plist"
cp "$BIN/GryviaDesktop" "$APP/Contents/MacOS/GryviaDesktop"
cp "$BIN/gryvia-agent" "$APP/Contents/Helpers/gryvia-agent"
xattr -c "$APP/Contents/MacOS/GryviaDesktop" "$APP/Contents/Helpers/gryvia-agent" 2>/dev/null || true
cp native/Gryvia.icns "$APP/Contents/Resources/Gryvia.icns"
rsync -a --exclude __pycache__ server runtimes web scripts brand docs LICENSE NOTICE VERSION "$APP/Contents/Resources/gryvia/"

if [[ -n "$APP_ID" ]]; then
  # Inside-out: helper first, then the app (hardened runtime) over it.
  codesign --force --options runtime --timestamp --sign "$APP_ID" "$APP/Contents/Helpers/gryvia-agent"
  codesign --force --options runtime --timestamp --sign "$APP_ID" "$APP"
else
  codesign --force --sign - "$APP/Contents/Helpers/gryvia-agent"
  codesign --force --sign - "$APP"
fi
codesign --verify --deep --strict "$APP"

ditto -c -k --norsrc --noextattr --keepParent "$APP" "$ZIP"

mkdir -p "$WORK/dmg"
ditto --norsrc --noextattr "$APP" "$WORK/dmg/Gryvia.app"
ln -s /Applications "$WORK/dmg/Applications"
hdiutil create -volname "Gryvia $VERSION" -srcfolder "$WORK/dmg" -fs HFS+ -format UDZO -ov "$DMG" >/dev/null
[[ -n "$APP_ID" ]] && codesign --force --timestamp --sign "$APP_ID" "$DMG"

mkdir -p "$WORK/root/Applications" "$WORK/pkgs"
ditto --norsrc --noextattr "$APP" "$WORK/root/Applications/Gryvia.app"
pkgbuild --analyze --root "$WORK/root" "$WORK/component.plist" >/dev/null
# Without this, Installer "relocates" the update into any other Gryvia.app it finds (e.g. a dev build).
plutil -replace 0.BundleIsRelocatable -bool NO "$WORK/component.plist"
pkgbuild --root "$WORK/root" --component-plist "$WORK/component.plist" --identifier dev.zyvor.gryvia.pkg \
  --version "$VERSION" --install-location / "$WORK/pkgs/Gryvia.pkg" >/dev/null
cat > "$WORK/distribution.xml" <<XML
<?xml version="1.0" encoding="utf-8"?>
<installer-gui-script minSpecVersion="1">
    <title>Gryvia $VERSION</title>
    <allowed-os-versions><os-version min="15.0"/></allowed-os-versions>
    <options customize="never" require-scripts="false" hostArchitectures="arm64"/>
    <choices-outline><line choice="default"><line choice="dev.zyvor.gryvia.pkg"/></line></choices-outline>
    <choice id="default"/>
    <choice id="dev.zyvor.gryvia.pkg" visible="false"><pkg-ref id="dev.zyvor.gryvia.pkg"/></choice>
    <pkg-ref id="dev.zyvor.gryvia.pkg" version="$VERSION" onConclusion="none">Gryvia.pkg</pkg-ref>
</installer-gui-script>
XML
sign_pkg=()
[[ -n "$PKG_ID" ]] && sign_pkg=(--sign "$PKG_ID" --timestamp)
productbuild --distribution "$WORK/distribution.xml" --package-path "$WORK/pkgs" ${sign_pkg[@]+"${sign_pkg[@]}"} "$PKG" >/dev/null

notarize() { xcrun notarytool submit "$1" --keychain-profile "$NOTARY" --wait; xcrun stapler staple "$1"; }
status="ad hoc signed app, unsigned DMG and pkg, not notarized: Gatekeeper blocks the first launch on other Macs"
if [[ -n "$APP_ID" ]]; then
  status="Developer ID signed, not notarized (set NOTARY_PROFILE)"
  if [[ -n "$NOTARY" ]]; then
    notarize "$DMG"; spctl -a -t open --context context:primary-signature -vv "$DMG"
    status="DMG signed and notarized"
    if [[ -n "$PKG_ID" ]]; then
      notarize "$PKG"; spctl -a -t install -vv "$PKG"
      status="DMG and pkg signed and notarized"
    else
      status="$status; pkg unsigned (set DEVELOPER_ID_INSTALLER)"
    fi
  fi
fi
(cd "$OUT" && for f in "$(basename "$DMG")" "$(basename "$PKG")" "$(basename "$ZIP")"; do shasum -a 256 "$f" > "$f.sha256"; done)
echo "wrote $DMG, $PKG and $ZIP ($status)"
