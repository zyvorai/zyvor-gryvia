#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Fail when a leaf key in a chart's values.yaml is not read by any template or helper.

A value nobody reads is a silent no-op for the user who sets it. For every chart under helm/ this script
flattens values.yaml into leaf paths (a scalar, a list, or an empty map counts as a leaf) and looks for a read in
templates/ (every file, including _helpers.tpl):

  .Values.a.b.c            the leaf itself
  .Values.a.b              a parent used as a whole (toYaml, range, with, index, a variable assignment):
                           the leaf is then read through it
  $x := .Values.a.b ; $x.c the same, through a variable
  dig "a" "b" "c" nil .Values.AsMap   the leaf, read nil-safe (a map missing from reused values is off)

`$ops := dict "a" .Values.x "b" .Values.y` followed by `range $n, $o := $ops` reads `x.f` for every `$o.f` (the
dict does not count as reading x whole). A leaf under a parent that is only ever read key by key must be read itself.
A chart without templates/ (an umbrella of sub-charts only) is skipped. Keys read by an included sub-chart
(`dependencies:` of Chart.yaml, by name or alias) and `global` are not checked. Documented extension points that are
deliberately read only by the user's own overrides go in ALLOW below, with the reason.

Usage: python3 scripts/check-chart-values.py [chart-dir ...]   (default: every helm/* chart)
"""
import glob
import os
import re
import sys

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# chart name -> {leaf path or parent prefix: reason}. A prefix allows everything below it.
ALLOW = {
    "gryvia": {},
    "network-intelligence": {},
}

REF = re.compile(r"\.Values((?:\.[A-Za-z_][A-Za-z0-9_-]*)+)")
INDEX = re.compile(r"index\s+\$?\.Values\s+((?:\"[^\"]+\"\s*)+)")
DICT = re.compile(r"(\$[A-Za-z_][A-Za-z0-9_]*)\s*:?=\s*dict\b([^}]*)")
RANGE = re.compile(r"range\s+\$[A-Za-z_][A-Za-z0-9_]*\s*,\s*(\$[A-Za-z_][A-Za-z0-9_]*)\s*:?=\s*(\$[A-Za-z_][A-Za-z0-9_]*)")
VAR = re.compile(r"(\$[A-Za-z_][A-Za-z0-9_]*)\s*:?=\s*\$?\.Values((?:\.[A-Za-z_][A-Za-z0-9_-]*)+)")
DIG = re.compile(r"dig\s+((?:\"[^\"]+\"\s+)+)\S+\s+\$?\.Values\.AsMap")


def leaves(node, prefix=()):
    if isinstance(node, dict) and node:
        for k, v in node.items():
            yield from leaves(v, prefix + (str(k),))
    else:
        yield prefix


def reads(text):
    """Return the set of value paths (tuples) read in a template, with variable aliases resolved."""
    out = set()
    dicts = {}
    for m in DICT.finditer(text):
        paths = [tuple(p.strip(".").split(".")) for p in REF.findall(m.group(2))]
        if paths:
            dicts[m.group(1)] = paths
            text = text.replace(m.group(0), " ")  # the dict itself reads nothing: only its elements' fields do
    for m in RANGE.finditer(text):
        elem, coll = m.group(1), m.group(2)
        for r in re.finditer(re.escape(elem) + r"((?:\.[A-Za-z_][A-Za-z0-9_-]*)+)", text):
            for base in dicts.get(coll, []):
                out.add(base + tuple(r.group(1).strip(".").split(".")))
    for m in REF.finditer(text):
        out.add(tuple(m.group(1).strip(".").split(".")))
    for m in INDEX.finditer(text):
        out.add(tuple(re.findall(r'"([^"]+)"', m.group(1))))
    for m in DIG.finditer(text):
        out.add(tuple(re.findall(r'"([^"]+)"', m.group(1))))
    for m in VAR.finditer(text):
        var, base = m.group(1), tuple(m.group(2).strip(".").split("."))
        out.add(base)
        for r in re.finditer(re.escape(var) + r"((?:\.[A-Za-z_][A-Za-z0-9_-]*)+)", text):
            out.add(base + tuple(r.group(1).strip(".").split(".")))
    return out


def check(chart):
    name = os.path.basename(chart.rstrip("/"))
    values = yaml.safe_load(open(os.path.join(chart, "values.yaml"))) or {}
    meta = yaml.safe_load(open(os.path.join(chart, "Chart.yaml"))) or {}
    skip = {"global"}
    for d in meta.get("dependencies") or []:
        skip.add(d.get("alias") or d["name"])
    used = set()
    for path in glob.glob(os.path.join(chart, "templates", "**", "*"), recursive=True):
        if os.path.isfile(path):
            used |= reads(open(path, encoding="utf-8").read())
    allow = ALLOW.get(name, {})
    dead = []
    for leaf in leaves(values):
        if not leaf or leaf[0] in skip:
            continue
        if any(leaf[: len(a.split("."))] == tuple(a.split(".")) for a in allow):
            continue
        # read if the leaf, or any parent of it, is read as a whole path
        if any(leaf[:i] in used for i in range(1, len(leaf) + 1)):
            continue
        dead.append(".".join(leaf))
    return name, dead


def main():
    charts = sys.argv[1:] or sorted(
        d for d in glob.glob(os.path.join(ROOT, "helm", "*")) if os.path.isdir(os.path.join(d, "templates")))
    rc = 0
    for c in charts:
        name, dead = check(c)
        for d in dead:
            print(f"{name}: values.yaml key `{d}` is not read by any template")
        if dead:
            rc = 1
        else:
            print(f"{name}: ok")
    if rc:
        print("Remove the key, wire it into a template, or list it in ALLOW in scripts/check-chart-values.py with a reason.")
    return rc


if __name__ == "__main__":
    sys.exit(main())
