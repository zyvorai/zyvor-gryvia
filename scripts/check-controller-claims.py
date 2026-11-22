#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Fail when Markdown says a kind has no controller although one is registered.

The source of truth is the Controller column of website/docs/reference/crds.md, which scripts/gen-crd-docs.py
generates and CI keeps current. A sentence (or table cell) that names a kind whose column is not "none" and says
"no controller", "CRD only", "not registered", "nothing reconciles" or similar is reported with its file and line.
CHANGELOG.md (history) and the generated reference itself are skipped.

Usage: python3 scripts/check-controller-claims.py
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REFERENCE = os.path.join(ROOT, "website", "docs", "reference", "crds.md")
SKIP_DIRS = {".git", "node_modules", "build", ".docusaurus", "target", "vendor"}
SKIP_FILES = {os.path.join(ROOT, "CHANGELOG.md"), REFERENCE}
CLAIM = re.compile(
    r"no controller|crd[- ]only|without (?:a )?controller|not registered|nothing reconciles|"
    r"no operator acts|no reconciler (?:is|for|registered)|not wired",
    re.IGNORECASE,
)
KIND = re.compile(r"\bGryvia[A-Z][A-Za-z]+\b")
SENTENCE_END = re.compile(r"(?<=[.!?])\s+(?=[A-Z`*(\[])")


def controlled_kinds():
    kinds = {}
    with open(REFERENCE, encoding="utf-8") as f:
        for line in f:
            cells = [c.strip().strip("`") for c in line.strip().strip("|").split("|")]
            if len(cells) >= 4 and KIND.fullmatch(cells[0]):
                kinds[cells[0]] = cells[3]
    if not kinds:
        sys.exit(f"no kinds parsed from {os.path.relpath(REFERENCE, ROOT)}")
    return {k for k, c in kinds.items() if c and c != "none"}


def units(path):
    """(line number, text) for every table cell and every prose sentence, outside code blocks."""
    out, para, start, fenced = [], [], 0, False
    with open(path, encoding="utf-8") as f:
        lines = f.read().splitlines()

    def flush():
        if para:
            for sentence in SENTENCE_END.split(" ".join(para)):
                out.append((start, sentence))
            para.clear()

    for n, line in enumerate(lines, 1):
        stripped = line.strip()
        if stripped.startswith("```"):
            flush()
            fenced = not fenced
            continue
        if fenced:
            continue
        if stripped.startswith("|"):
            flush()
            out.extend((n, cell) for cell in stripped.strip("|").split("|"))
            continue
        if not stripped or stripped.startswith("#"):
            flush()
            continue
        if not para:
            start = n
        para.append(stripped.lstrip("> ").strip())
    flush()
    return out


def markdown_files():
    for base, dirs, files in os.walk(ROOT):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        for name in files:
            path = os.path.join(base, name)
            if name.endswith(".md") and path not in SKIP_FILES:
                yield path


def main():
    controlled = controlled_kinds()
    problems = []
    for path in sorted(markdown_files()):
        for line, text in units(path):
            if not CLAIM.search(text):
                continue
            named = sorted(set(KIND.findall(text)) & controlled)
            if named:
                problems.append(f"{os.path.relpath(path, ROOT)}:{line}: {', '.join(named)} has a controller, "
                                f"but the text says otherwise: {text[:160]}")
    for p in problems:
        print(p)
    if problems:
        print(f"\n{len(problems)} statement(s) contradict the Controller column of "
              f"{os.path.relpath(REFERENCE, ROOT)}")
        return 1
    print(f"ok: no Markdown claims a controller-backed kind ({len(controlled)}) has no controller")
    return 0


if __name__ == "__main__":
    sys.exit(main())
