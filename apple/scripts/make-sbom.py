#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Emit a CycloneDX 1.5 SBOM (JSON) for Gryvia Apple on stdout.

Honest scope: the shipped product has no third-party runtime packages (Python standard library, Swift/Foundation
and Apple system frameworks). Build- and test-time tools are listed as such. Optional runtimes (llama.cpp, MLX) are
user-installed and recorded in provenance, not bundled."""
import json
import re
import subprocess
import sys
import uuid
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def tool_version(cmd: list[str], rx: str) -> str:
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=20)
        m = re.search(rx, out.stdout + out.stderr)
        return m.group(1) if m else "unknown"
    except (OSError, subprocess.TimeoutExpired):
        return "unavailable"


def main(version: str) -> dict:
    pkg = json.loads((ROOT / "package.json").read_text())
    dev = pkg.get("devDependencies", {})
    components = [
        {"type": "library", "name": "python-standard-library", "version": sys.version.split()[0], "scope": "required",
         "description": "The control plane, CLI and runtime wrappers import only the Python standard library."},
        {"type": "framework", "name": "Swift/Foundation/SwiftUI", "scope": "required", "description": "Apple system frameworks, not bundled."},
    ]
    for name, ver in sorted(dev.items()):
        components.append({"type": "library", "name": name, "version": ver, "scope": "excluded",
                           "description": "Test-time only (browser tests); not shipped.", "purl": f"pkg:npm/{name}@{ver}"})
    swift = tool_version(["swift", "--version"], r"Swift version ([0-9.]+)")
    components.append({"type": "application", "name": "swift", "version": swift, "scope": "excluded", "description": "Build tool."})
    return {
        "bomFormat": "CycloneDX", "specVersion": "1.5", "serialNumber": "urn:uuid:" + str(uuid.uuid4()), "version": 1,
        "metadata": {
            "timestamp": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "component": {"type": "application", "name": "gryvia-apple", "version": version,
                          "licenses": [{"license": {"id": "BUSL-1.1"}}], "supplier": {"name": "Zyvor AI Labs Private Limited"}},
        },
        "components": components,
    }


if __name__ == "__main__":
    print(json.dumps(main(sys.argv[1] if len(sys.argv) > 1 else (ROOT / "VERSION").read_text().strip()), indent=2))
