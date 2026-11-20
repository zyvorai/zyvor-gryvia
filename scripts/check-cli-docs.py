#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Check that every `gryvia ...` command shown in the docs is accepted by the real CLI's argument parser.

Each documented command is run against the built binary with no cluster reachable, so a command that
the parser accepts fails later at "cannot connect" (fine), while an unknown subcommand or flag exits with
clap's usage error (reported). Shell pipes, redirects and `&&` tails are stripped first.

Usage: python3 scripts/check-cli-docs.py [path/to/gryvia]   (default: cli/target/debug/gryvia)
"""
import glob
import os
import re
import shlex
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = sys.argv[1] if len(sys.argv) > 1 else os.path.join(ROOT, "cli", "target", "debug", "gryvia")
SKIP_DIRS = ("node_modules", "website/build", "cli/target", ".git/")
USAGE_ERR = re.compile(r"unexpected argument|unrecognized subcommand|required arguments were not provided|invalid value")


SHELL_LANGS = {"", "bash", "sh", "shell", "console", "zsh"}


def shell_blocks(text: str):
    """Bodies of fenced code blocks whose language is a shell (or unlabeled), found by scanning fence lines.

    Blocks in other languages (text, yaml, json, ...) are skipped, and they cannot throw off the pairing of
    fences the way a regex over the whole file would.
    """
    lang = None
    body: list = []
    for line in text.split("\n"):
        stripped = line.strip()
        if stripped.startswith("```"):
            if lang is None:
                lang = stripped[3:].strip().split()[0] if stripped[3:].strip() else ""
                body = []
            else:
                if lang in SHELL_LANGS:
                    yield "\n".join(body)
                lang = None
        elif lang is not None:
            body.append(line)


def commands():
    seen = {}
    for path in glob.glob(os.path.join(ROOT, "**", "*.md"), recursive=True):
        rel = os.path.relpath(path, ROOT)
        if any(s in rel for s in SKIP_DIRS):
            continue
        text = open(path, errors="ignore").read()
        for block in shell_blocks(text):
            pending = ""
            for line in block.split("\n"):
                s = line.strip()
                if pending:
                    s, pending = pending + " " + s, ""
                if s.endswith("\\"):
                    pending = s[:-1].strip()
                    continue
                m = re.match(r"^\$?\s*(gryvia\s+.*)$", s)
                if not m:
                    continue
                cmd = re.sub(r"\s+#.*$", "", m.group(1))
                cmd = re.split(r"\s(?:\||&&|;|\d?>>?)\s|\s\d?>\S", cmd)[0].strip()
                seen.setdefault(cmd, set()).add(rel)
    return seen


def main() -> int:
    if not os.path.exists(BIN):
        print(f"CLI binary not found: {BIN} (run `cargo build` in cli/)", file=sys.stderr)
        return 2
    env = dict(os.environ, KUBECONFIG="/nonexistent/kubeconfig", HOME="/nonexistent")
    bad = []
    seen = commands()
    for cmd, rels in sorted(seen.items()):
        try:
            argv = shlex.split(cmd)[1:]
        except ValueError:
            continue
        if not argv:
            continue
        try:
            r = subprocess.run([BIN] + argv, capture_output=True, text=True, timeout=10, env=env)
        except subprocess.TimeoutExpired:
            continue
        if r.returncode == 2 and USAGE_ERR.search(r.stderr):
            err = next((l for l in r.stderr.split("\n") if l.startswith("error:")), r.stderr[:100])
            for rel in sorted(rels):
                bad.append((rel, cmd, err))
    print(f"{len(seen)} documented commands checked, {len(bad)} not accepted by the CLI")
    for rel, cmd, err in bad:
        print(f"{rel}: {cmd}\n    {err}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
