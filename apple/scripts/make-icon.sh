#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
# Render brand/gryvia-icon.svg into native/Gryvia.icns, web/icon.png and web/favicon.svg.
set -euo pipefail
cd "$(dirname "$0")/.."
command -v rsvg-convert >/dev/null || { echo 'rsvg-convert required (brew install librsvg)' >&2; exit 1; }
set_dir=$(mktemp -d)/Gryvia.iconset
mkdir -p "$set_dir"
for size in 16 32 128 256 512; do
  rsvg-convert -w $size -h $size brand/gryvia-icon.svg -o "$set_dir/icon_${size}x${size}.png"
  rsvg-convert -w $((size*2)) -h $((size*2)) brand/gryvia-icon.svg -o "$set_dir/icon_${size}x${size}@2x.png"
done
iconutil -c icns "$set_dir" -o native/Gryvia.icns
rsvg-convert -w 180 -h 180 brand/gryvia-icon.svg -o web/icon.png
cp brand/gryvia-icon.svg web/favicon.svg
echo "wrote native/Gryvia.icns web/icon.png web/favicon.svg"
