#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Check the references inside examples/**/README.md and examples/**/*.sh.

Fails when
  * a relative file path referenced from a README (a markdown link, or a file argument in a shell code block)
    or from a shell script does not exist;
  * a `gryvia <subcommand> ...` command shown there is not accepted by the real CLI's argument parser (the same
    validation as scripts/check-cli-docs.py: the command is run with no cluster reachable, so only usage errors
    count).

A file that a code block or script creates itself (`> file`, `-o file`, `tee file`, a heredoc target) is not
required to exist. Paths that contain variables other than the recognised script-directory ones ($HERE, $ROOT,
$SCRIPT_DIR, $REPO_ROOT), placeholders (<...>), globs or URLs are skipped.

Usage: python3 scripts/check-example-refs.py [path/to/gryvia]   (default: cli/target/debug/gryvia)
"""
import glob
import importlib.util
import os
import re
import shlex
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = sys.argv[1] if len(sys.argv) > 1 else os.path.join(ROOT, "cli", "target", "debug", "gryvia")

_spec = importlib.util.spec_from_file_location("check_cli_docs", os.path.join(ROOT, "scripts", "check-cli-docs.py"))
_cli = importlib.util.module_from_spec(_spec)
_argv, sys.argv = sys.argv, [sys.argv[0]]  # check-cli-docs reads argv[1] at import
_spec.loader.exec_module(_cli)
sys.argv = _argv

FILE_EXT = (".yaml", ".yml", ".sh", ".json", ".py", ".md", ".toml", ".tpl")
TOP_DIRS = ("examples/", "scripts/", "crds/", "helm/", "manifests/", "monitoring/", "operators/", "docs/", "cli/")
LINK = re.compile(r"\]\(([^)\s]+)\)")
SCRIPT_DIR_VARS = ("$HERE", "${HERE}", "$SCRIPT_DIR", "${SCRIPT_DIR}", "$(dirname \"$0\")")
ROOT_VARS = ("$ROOT", "${ROOT}", "$REPO_ROOT", "${REPO_ROOT}")
# A token is a file reference only after one of these (or when it starts with ./, ../ or a repository directory).
FILE_FLAGS = {"-f", "--filename", "-k", "--kustomize", "--file", "cp", "source", ".", "bash", "sh", "python3", "python"}


def rel(path: str) -> str:
    return os.path.relpath(path, ROOT)


def is_placeholder(tok: str) -> bool:
    return any(c in tok for c in "<>*{}") or "://" in tok or tok.startswith("-") or tok in ("-",)


def resolve(tok: str, base_dir: str):
    """Absolute path a token refers to, or None when it cannot be checked statically."""
    for v in SCRIPT_DIR_VARS:
        if tok.startswith(v + "/"):
            return os.path.normpath(os.path.join(base_dir, tok[len(v) + 1:]))
    for v in ROOT_VARS:
        if tok.startswith(v + "/"):
            return os.path.normpath(os.path.join(ROOT, tok[len(v) + 1:]))
    if "$" in tok or "`" in tok or is_placeholder(tok):
        return None
    if tok.startswith("~"):
        return None
    if tok.startswith("/"):
        return None  # absolute paths are the reader's system, not the repository
    # relative: from the file's directory, else from the repository root (docs say "from the repo root")
    first = os.path.normpath(os.path.join(base_dir, tok))
    if os.path.exists(first):
        return first
    return first if not tok.startswith(TOP_DIRS) else os.path.normpath(os.path.join(ROOT, tok))


def looks_like_file(tok: str, prev: str) -> bool:
    if is_placeholder(tok) and not tok.startswith(tuple("$")):
        return False
    if tok.startswith(("./", "../")) or tok.startswith(TOP_DIRS):
        return tok.endswith(FILE_EXT) or tok.endswith("/")
    return prev in FILE_FLAGS and tok.endswith(FILE_EXT)


def shell_lines(text: str):
    """Logical lines (continuations joined) of a shell snippet, heredoc bodies skipped."""
    out, pending, heredoc = [], "", None
    for raw in text.split("\n"):
        if heredoc is not None:
            if raw.strip() == heredoc:
                heredoc = None
            continue
        s = raw.strip()
        if pending:
            s, pending = pending + " " + s, ""
        if s.endswith("\\"):
            pending = s[:-1].strip()
            continue
        m = re.search(r"<<-?\s*['\"]?(\w+)['\"]?", s)
        if m:
            heredoc = m.group(1)
            s = s[: m.start()].strip()
        if s and not s.startswith("#"):
            out.append(s)
    return out


def created_files(lines):
    made = set()
    for s in lines:
        for m in re.finditer(r"(?:>>?|\btee(?:\s+-a)?|\s-o|\s--output(?:=|\s))\s*(\S+)", s):
            made.add(os.path.basename(m.group(1).strip("\"'")))
    return made


def file_refs(lines, base_dir):
    """(token, absolute path) for every checkable file reference of the lines."""
    made = created_files(lines)
    refs = []
    for s in lines:
        if re.match(r"^(echo|printf)\b", s):
            continue
        try:
            toks = shlex.split(re.sub(r"\s[|&;]+\s.*$", "", s) if "$(" not in s else s, comments=True, posix=True)
        except ValueError:
            continue
        prev = ""
        for tok in toks:
            if looks_like_file(tok, prev) and os.path.basename(tok) not in made:
                path = resolve(tok, base_dir)
                if path is not None:
                    refs.append((tok, path))
            prev = tok
    return refs


GRYVIA_CMD = re.compile(r"(?:^|\$\(|\|\s*|&&\s*|;\s*|\bthen\s+|\bdo\s+)(gryvia\s+[^|;&)]*)")


def gryvia_commands(lines):
    cmds = []
    for s in lines:
        if re.match(r"^(echo|printf)\b", s):
            continue
        for m in GRYVIA_CMD.finditer(s):
            cmd = re.sub(r"\s+#.*$", "", m.group(1)).strip()
            cmd = re.split(r"\s\d?>>?\s|\s\d?>\S", cmd)[0].strip()
            cmds.append(cmd)
    return cmds


def check_cli(cmd: str):
    try:
        argv = shlex.split(cmd)[1:]
    except ValueError:
        return None
    if not argv:
        return None
    env = dict(os.environ, KUBECONFIG="/nonexistent/kubeconfig", HOME="/nonexistent")
    try:
        r = subprocess.run([BIN] + argv, capture_output=True, text=True, timeout=10, env=env)
    except subprocess.TimeoutExpired:
        return None
    if r.returncode == 2 and _cli.USAGE_ERR.search(r.stderr):
        return next((l for l in r.stderr.split("\n") if l.startswith("error:")), r.stderr[:100])
    return None


def main() -> int:
    if not os.path.exists(BIN):
        print(f"CLI binary not found: {BIN} (run `cargo build` in cli/)", file=sys.stderr)
        return 2
    problems, cli_seen = [], {}

    def check_refs(where, lines, base_dir):
        for tok, path in file_refs(lines, base_dir):
            if not os.path.exists(path):
                problems.append(f"{where}: `{tok}` does not exist ({rel(path)})")
        for cmd in gryvia_commands(lines):
            cli_seen.setdefault(cmd, set()).add(where)

    files = sorted(glob.glob(os.path.join(ROOT, "examples", "**", "README.md"), recursive=True))
    for path in files:
        base = os.path.dirname(path)
        text = open(path, errors="ignore").read()
        for line in text.split("\n"):
            if line.lstrip().startswith(">"):
                continue  # status notes describe what does not exist
            for target in LINK.findall(line):
                if target.startswith(("http://", "https://", "mailto:", "#")):
                    continue
                target = target.split("#")[0]
                if target and not os.path.exists(os.path.normpath(os.path.join(base, target))):
                    problems.append(f"{rel(path)}: link target `{target}` does not exist")
        for block in _cli.shell_blocks(text):
            check_refs(rel(path), shell_lines(block), base)
    for path in sorted(glob.glob(os.path.join(ROOT, "examples", "**", "*.sh"), recursive=True)):
        check_refs(rel(path), shell_lines(open(path, errors="ignore").read()), os.path.dirname(path))

    for cmd, wheres in sorted(cli_seen.items()):
        err = check_cli(cmd)
        if err:
            for where in sorted(wheres):
                problems.append(f"{where}: `{cmd}` is not accepted by the CLI ({err})")

    print(f"{len(files)} example READMEs and {len(glob.glob(os.path.join(ROOT, 'examples', '**', '*.sh'), recursive=True))} "
          f"scripts checked, {len(cli_seen)} gryvia commands, {len(problems)} problems")
    for p in problems:
        print(p)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
