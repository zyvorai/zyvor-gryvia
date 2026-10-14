#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Install the Gryvia control plane as a per-user launchd agent (no sudo).

Binds to loopback by default; reach it remotely through an ssh tunnel.
Idempotent: re-running replaces the existing job. Use --uninstall to remove.
"""
import argparse
import os
import plistlib
import subprocess
import sys
from pathlib import Path

if sys.platform != "darwin":
    raise SystemExit("macOS required")

p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
p.add_argument("--python", default=sys.executable)
p.add_argument("--root", help="directory containing the `server` package", default=str(Path(__file__).resolve().parents[1]))
p.add_argument("--config"); p.add_argument("--db")
p.add_argument("--host", default="127.0.0.1"); p.add_argument("--port", default="8443")
p.add_argument("--uninstall", action="store_true")
a = p.parse_args()

label = "dev.zyvor.gryvia.server"
plist = Path.home() / "Library/LaunchAgents" / f"{label}.plist"
domain = f"gui/{os.getuid()}"

subprocess.run(["launchctl", "bootout", f"{domain}/{label}"], check=False, capture_output=True)
if a.uninstall:
    plist.unlink(missing_ok=True)
    raise SystemExit(0)
if not (a.config and a.db):
    p.error("--config and --db are required")
config, db, root = Path(a.config).resolve(), Path(a.db).resolve(), Path(a.root).resolve()
if not config.is_file():
    p.error("config not found: " + str(config))
logs = Path.home() / "Library/Logs/Gryvia"
logs.mkdir(parents=True, exist_ok=True)
plist.parent.mkdir(parents=True, exist_ok=True)
obj = {
    "Label": label,
    "ProgramArguments": [a.python, "-m", "server.app", "--config", str(config), "--db", str(db), "--host", a.host, "--port", a.port, "--inference-loopback"],
    "WorkingDirectory": str(root),
    "RunAtLoad": True, "KeepAlive": True, "ThrottleInterval": 10,
    "StandardOutPath": str(logs / "server.log"), "StandardErrorPath": str(logs / "server-error.log"),
}
plist.unlink(missing_ok=True)
fd = os.open(plist, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "wb") as f:
    plistlib.dump(obj, f)
# bootout is asynchronous: wait for the old job to disappear, then bootstrap with a few retries.
import time
for _ in range(50):
    if subprocess.run(["launchctl", "print", f"{domain}/{label}"], capture_output=True).returncode != 0:
        break
    time.sleep(0.2)
for attempt in range(5):
    r = subprocess.run(["launchctl", "bootstrap", domain, str(plist)], capture_output=True, text=True)
    if r.returncode == 0:
        break
    time.sleep(1 + attempt)
else:
    raise SystemExit("launchctl bootstrap failed: " + r.stderr.strip())
print("Installed", label, f"on http://{a.host}:{a.port}")
