#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Fail if a dashboard or alert rule under monitoring/ references a metric nobody exports.

Defined metrics = every "gryvia_*" string literal in non-test Go and Python sources of collector/, operators/ and
services/api-gateway/ (plus their _bucket/_sum/_count series), and every `record:` name in the rule files.
External metrics = the allow-list monitoring/external-metrics.txt (dcgm-exporter, kube-state-metrics,
controller-runtime, process/go collectors, up).

PromQL is not parsed: expressions are tokenised tolerantly (strings, label matchers, `by (...)` lists, durations and
Grafana variables are stripped; identifiers followed by `(` are functions). This finds metric NAMES only; it cannot
check labels, and it does not prove a series has data.

Usage: python3 scripts/check-metrics-refs.py [--root DIR] [--monitoring DIR]    (exit 1 on unknown metrics)
"""
import argparse
import fnmatch
import json
import os
import re
import sys

import yaml

SOURCE_DIRS = ("collector", "operators", "services/api-gateway")
DEFINE_RE = re.compile(r'["\'](gryvia_[a-z0-9_]+)["\']')
KEYWORDS = {"by", "without", "on", "ignoring", "group_left", "group_right", "and", "or", "unless", "offset", "bool",
            "inf", "nan", "start", "end"}
SUFFIXES = ("_bucket", "_sum", "_count")


def is_test_file(path):
    base = os.path.basename(path)
    return base.endswith("_test.go") or base.startswith("test_") or "/tests/" in path.replace(os.sep, "/")


def defined_metrics(root):
    names = set()
    for src in SOURCE_DIRS:
        for dirpath, dirnames, files in os.walk(os.path.join(root, src)):
            dirnames[:] = [d for d in dirnames if d not in ("node_modules", "vendor", ".git", "target", "venv", ".venv")]
            for f in files:
                path = os.path.join(dirpath, f)
                if not f.endswith((".go", ".py")) or is_test_file(path):
                    continue
                with open(path, encoding="utf-8", errors="replace") as fh:
                    names.update(DEFINE_RE.findall(fh.read()))
    return names


def external_patterns(monitoring):
    pats = []
    path = os.path.join(monitoring, "external-metrics.txt")
    if os.path.exists(path):
        for line in open(path, encoding="utf-8"):
            line = line.split("#", 1)[0].strip()
            if line:
                pats.append(line)
    return pats


def _strip_braces(s):
    """Remove {...} label matchers (they may contain quoted strings with braces, so strings go first)."""
    out, depth = [], 0
    for ch in s:
        if ch == "{":
            depth += 1
        elif ch == "}":
            depth = max(0, depth - 1)
        elif depth == 0:
            out.append(ch)
    return "".join(out)


def metric_names(expr):
    """Metric-name candidates in one PromQL/Grafana expression."""
    s = re.sub(r'"(?:\\.|[^"\\])*"|\'(?:\\.|[^\'\\])*\'|`[^`]*`', '""', expr)
    s = re.sub(r"\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|\[\[[^\]]*\]\]", " ", s)  # Grafana variables
    s = _strip_braces(s)
    s = re.sub(r"\[[^\]]*\]", " ", s)  # range / subquery selectors
    s = re.sub(r"\b(?:by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)", " ", s)
    m = re.match(r"\s*(?:label_values|query_result)\((.*)\)\s*$", s, re.S)
    if m:  # label_values(metric, label): the last argument is a label, not a metric
        s = m.group(1).rsplit(",", 1)[0] if "," in m.group(1) else m.group(1)
    found = []
    for mt in re.finditer(r"[A-Za-z_:][A-Za-z0-9_:]*", s):
        word, rest = mt.group(0), s[mt.end():].lstrip()
        if rest.startswith("("):  # function
            continue
        if mt.start() > 0 and (s[mt.start() - 1].isdigit() or s[mt.start() - 1] == "."):
            continue  # exponent/number suffix like 1e5
        if word.lower() in KEYWORDS:
            continue
        found.append(word)
    return found


def iter_exprs(obj):
    if isinstance(obj, dict):
        for k, v in obj.items():
            if k in ("expr", "definition") and isinstance(v, str):
                yield v
            elif k == "query" and isinstance(v, str):
                if obj.get("type") != "datasource":  # a datasource variable's query is the plugin id, not PromQL
                    yield v
            elif k == "query" and isinstance(v, dict) and isinstance(v.get("query"), str):
                yield v["query"]
            else:
                yield from iter_exprs(v)
    elif isinstance(obj, list):
        for v in obj:
            yield from iter_exprs(v)


def load_docs(path):
    with open(path, encoding="utf-8") as fh:
        if path.endswith(".json"):
            return [json.load(fh)]
        return [d for d in yaml.safe_load_all(fh) if d is not None]


def recorded(docs):
    out = set()

    def walk(o):
        if isinstance(o, dict):
            if isinstance(o.get("record"), str):
                out.add(o["record"])
            for v in o.values():
                walk(v)
        elif isinstance(o, list):
            for v in o:
                walk(v)

    for d in docs:
        walk(d)
    return out


def check(root, monitoring):
    defined = defined_metrics(root)
    patterns = external_patterns(monitoring)
    files = []
    for dirpath, _, names in os.walk(monitoring):
        files += [os.path.join(dirpath, n) for n in names if n.endswith((".json", ".yaml", ".yml"))]
    files.sort()
    loaded = {f: load_docs(f) for f in files}
    for docs in loaded.values():
        defined |= recorded(docs)

    def known(name):
        base = name
        for suf in SUFFIXES:
            if name.endswith(suf):
                base = name[: -len(suf)]
                break
        return (name in defined or base in defined or any(fnmatch.fnmatchcase(name, p) for p in patterns))

    unknown = {}
    for f, docs in loaded.items():
        for doc in docs:
            for expr in iter_exprs(doc):
                for name in metric_names(expr):
                    if not known(name):
                        unknown.setdefault(name, set()).add(os.path.relpath(f, monitoring))
    return unknown, len(files)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    ap.add_argument("--root", default=here)
    ap.add_argument("--monitoring", default=None)
    a = ap.parse_args(argv)
    monitoring = a.monitoring or os.path.join(a.root, "monitoring")
    unknown, nfiles = check(a.root, monitoring)
    if unknown:
        print("Metrics referenced under monitoring/ that no exporter defines and the allow-list does not cover:")
        for name in sorted(unknown):
            print(f"  {name}    ({', '.join(sorted(unknown[name]))})")
        print("Fix the query, export the metric, or (for third-party metrics) add it to monitoring/external-metrics.txt.")
        return 1
    print(f"ok: every metric referenced in {nfiles} monitoring files is exported by Gryvia or allow-listed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
