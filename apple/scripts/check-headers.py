#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Verify (or with --fix, add) SPDX + copyright headers on first-party source files."""
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SPDX = "SPDX-License-Identifier: BUSL-1.1"
COPY = "Copyright © 2026 Zyvor AI Labs Private Limited"
SKIP = {".build", "node_modules", "local", "dist", "__pycache__", ".git"}
STYLES = {".py": "#", ".sh": "#", ".swift": "//", ".js": "//", ".cjs": "//", ".css": None}


def files():
    for p in ROOT.rglob("*"):
        if p.is_file() and p.suffix in STYLES and not (set(p.relative_to(ROOT).parts) & SKIP):
            if p.name in ("tokens.css", "Brand.swift"):
                continue  # generated; header written by gen-brand.py
            yield p


def header(p: Path) -> str:
    c = STYLES[p.suffix]
    if c is None:
        return f"/* {SPDX}\n * {COPY} */\n"
    return f"{c} {SPDX}\n{c} {COPY}\n"


def main() -> int:
    fix = "--fix" in sys.argv
    missing = []
    for p in files():
        text = p.read_text(encoding="utf-8")
        if SPDX in "\n".join(text.splitlines()[:6]):
            continue
        missing.append(p)
        if fix:
            lines = text.splitlines(keepends=True)
            shebang = lines[0] if lines and lines[0].startswith("#!") else ""
            body = text[len(shebang):]
            p.write_text(shebang + header(p) + body, encoding="utf-8")
    for p in missing:
        print(("fixed " if fix else "missing header: ") + str(p.relative_to(ROOT)))
    return 0 if fix or not missing else 1


if __name__ == "__main__":
    sys.exit(main())
