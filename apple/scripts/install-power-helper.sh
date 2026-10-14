#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
# Install (or --uninstall) the optional power helper as a root launchd daemon. Needs sudo; opt in deliberately.
set -euo pipefail
LABEL=dev.zyvor.gryvia.power
PLIST=/Library/LaunchDaemons/$LABEL.plist
DEST=/usr/local/libexec/gryvia-power-helper.py
if [[ "${1:-}" == "--uninstall" ]]; then
  sudo launchctl bootout system/$LABEL 2>/dev/null || true
  sudo rm -f "$PLIST" "$DEST" /var/run/gryvia-power.json
  echo "removed $LABEL"; exit 0
fi
HERE="$(cd "$(dirname "$0")" && pwd)"
PY="$(command -v /opt/homebrew/bin/python3.14 || command -v python3)"
sudo mkdir -p /usr/local/libexec
sudo install -m 0755 -o root -g wheel "$HERE/power-helper.py" "$DEST"
sudo tee "$PLIST" >/dev/null <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key><array><string>$PY</string><string>$DEST</string><string>--output</string><string>/var/run/gryvia-power.json</string></array>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
</dict></plist>
PL
sudo chown root:wheel "$PLIST"; sudo chmod 644 "$PLIST"
sudo launchctl bootout system/$LABEL 2>/dev/null || true
sudo launchctl bootstrap system "$PLIST"
echo "installed $LABEL; the agent reports power within a few seconds. Remove with: $0 --uninstall"
