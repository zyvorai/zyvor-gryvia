#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Single source of truth for the version: VERSION. Rewrites (or with --check, verifies) every place it appears.

    scripts/set-version.py 0.3.1      write VERSION and update all targets
    scripts/set-version.py --check    exit 1 if any target disagrees with VERSION
"""
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SEMVER = re.compile(r"^\d+\.\d+\.\d+([.-][A-Za-z0-9.]+)?$")

# (relative path, regex with ONE capture group around the version, description)
TARGETS = [
    ("pyproject.toml", r'^version = "([^"]+)"', "python project"),
    ("package.json", r'"version": "([^"]+)"', "npm test harness"),
    ("native/Info.plist", r"<key>CFBundleShortVersionString</key><string>([^<]+)</string>", "app short version"),
    ("native/Info.plist", r"<key>CFBundleVersion</key><string>([^<]+)</string>", "app build version"),
    ("README.md", r"· (\d+\.\d+\.\d+(?:[.-][A-Za-z0-9.]+)?) (?:engineering beta )?· BUSL-1\.1", "README header"),
    ("native/Sources/GryviaDesktop/Desktop.swift", r"Gryvia Apple (\d+\.\d+\.\d+(?:[.-][A-Za-z0-9.]+)?) ·", "About panel"),
    ("web/index.html", r"<strong>Gryvia Apple</strong> (\d+\.\d+\.\d+(?:[.-][A-Za-z0-9.]+)?) ·", "web About"),
    ("scripts/gryvia.py", r'^VERSION = "([^"]+)"', "CLI fallback"),
]


def current() -> str:
    return (ROOT / "VERSION").read_text().strip()


def check() -> list[str]:
    want, bad = current(), []
    for rel, rx, what in TARGETS:
        text = (ROOT / rel).read_text(encoding="utf-8")
        m = re.search(rx, text, re.M)
        if not m:
            bad.append(f"{rel}: pattern for {what} not found")
        elif m.group(1) != want:
            bad.append(f"{rel}: {what} is {m.group(1)}, VERSION is {want}")
    return bad


def write(version: str) -> None:
    if not SEMVER.match(version):
        raise SystemExit(f"invalid version: {version}")
    (ROOT / "VERSION").write_text(version + "\n")
    for rel, rx, _ in TARGETS:
        p = ROOT / rel
        text = p.read_text(encoding="utf-8")
        new, n = re.subn(rx, lambda m: m.group(0).replace(m.group(1), version), text, count=1, flags=re.M)
        if n:
            p.write_text(new, encoding="utf-8")
    json.loads((ROOT / "package.json").read_text())  # still valid


if __name__ == "__main__":
    if len(sys.argv) == 2 and sys.argv[1] == "--check":
        problems = check()
        print("\n".join(problems) if problems else f"all targets at {current()}")
        sys.exit(1 if problems else 0)
    if len(sys.argv) == 2:
        write(sys.argv[1])
        print("version set to", current())
        sys.exit(1 if check() else 0)
    print(__doc__)
    sys.exit(2)
