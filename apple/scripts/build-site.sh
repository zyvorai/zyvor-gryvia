#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
# Assemble the GitHub Pages site into _site/:  /  = site/   and   /demo/ = the real dashboard in demo mode.
# The dashboard is static, so the demo needs no backend; the "Try demo" button starts the simulated fleet.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT="${OUT:-_site}"
rm -rf "$OUT" && mkdir -p "$OUT/demo"
cp -R site/. "$OUT/"
cp web/tokens.css "$OUT/tokens.css"
cp web/index.html web/app.js web/style.css web/tokens.css web/favicon.svg web/icon.png "$OUT/demo/"
# keep the demo out of search results and make it start in demo mode
python3 - "$OUT/demo/index.html" <<'PY'
import sys, pathlib
p = pathlib.Path(sys.argv[1]); s = p.read_text()
s = s.replace("<title>", '<meta name="robots" content="noindex">\n<title>', 1)
p.write_text(s)
PY
touch "$OUT/.nojekyll"
# every local src/href must resolve
python3 - "$OUT" <<'PY'
import re, sys, pathlib
root = pathlib.Path(sys.argv[1]); bad = []
for page in list(root.rglob("*.html")):
    for ref in re.findall(r'(?<![\w-])(?:src|href)="([^"#?]+)', page.read_text()):
        if re.match(r'^(https?:|mailto:|data:|//)', ref): continue
        target = (page.parent / ref).resolve()
        if ref.endswith('/'): target = target / 'index.html'
        if not target.exists(): bad.append(f"{page.relative_to(root)} -> {ref}")
if bad:
    print("unresolved references:\n  " + "\n  ".join(bad)); sys.exit(1)
print(f"site ok: {len(list(root.rglob('*')))} files")
PY
