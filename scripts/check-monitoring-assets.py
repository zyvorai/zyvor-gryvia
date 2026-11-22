#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Structural lint of monitoring/ without promtool or Grafana.

Rules (monitoring/prometheus-rules.yaml, plain rule-file format): groups[].name, rules[] each with exactly one of
alert/record and a non-empty expr; alerts need `for`, labels.severity in {info,warning,critical}, annotations
summary/description/runbook_url, alert names unique, and runbook_url must point at an existing docs/runbooks/<Alert>.md
(and every runbook must belong to an alert). Template braces must balance.
Dashboards (monitoring/grafana-dashboards/*.json): valid JSON, raw dashboard (not wrapped in {"dashboard": ...}), keys
uid/title/panels/templating/schemaVersion, unique uid, unique panel ids, every non-row/text panel has targets with expr.

This is a local stand-in: CI additionally runs the real `promtool check rules`. It cannot validate PromQL syntax.

Usage: python3 scripts/check-monitoring-assets.py [--root DIR]
"""
import argparse
import glob
import json
import os
import re
import sys

import yaml

RUNBOOK_BASE = "https://github.com/zyvorai/gryvia/blob/main/docs/runbooks/"
SEVERITIES = {"info", "warning", "critical"}


def check_rules(path, runbook_dir):
    errs = []
    try:
        doc = yaml.safe_load(open(path, encoding="utf-8"))
    except yaml.YAMLError as e:
        return [f"{path}: invalid YAML: {e}"]
    if not isinstance(doc, dict) or not isinstance(doc.get("groups"), list) or not doc["groups"]:
        return [f"{path}: top level must be a mapping with a non-empty `groups` list (plain rule-file format)"]
    if set(doc) != {"groups"}:
        errs.append(f"{path}: unexpected top-level keys {sorted(set(doc) - {'groups'})} (not a promtool rule file)")
    alerts, group_names = set(), set()
    for g in doc["groups"]:
        gname = g.get("name") if isinstance(g, dict) else None
        if not gname or gname in group_names:
            errs.append(f"{path}: group missing or duplicate name: {gname!r}")
        group_names.add(gname)
        for r in (g.get("rules") or []) if isinstance(g, dict) else []:
            kind = [k for k in ("alert", "record") if k in r]
            if len(kind) != 1:
                errs.append(f"{path}: {gname}: rule needs exactly one of alert/record: {r}")
                continue
            name = r[kind[0]]
            if not isinstance(r.get("expr"), str) or not r["expr"].strip():
                errs.append(f"{path}: {name}: empty expr")
            elif r["expr"].count("(") != r["expr"].count(")"):
                errs.append(f"{path}: {name}: unbalanced parentheses in expr")
            if kind[0] == "record":
                continue
            if name in alerts:
                errs.append(f"{path}: duplicate alert {name}")
            alerts.add(name)
            if not re.fullmatch(r"[A-Za-z][A-Za-z0-9_]*", str(name)):
                errs.append(f"{path}: bad alert name {name!r}")
            if not re.fullmatch(r"\d+[smhd]", str(r.get("for", ""))):
                errs.append(f"{path}: {name}: `for` must be a duration such as 10m")
            if (r.get("labels") or {}).get("severity") not in SEVERITIES:
                errs.append(f"{path}: {name}: labels.severity must be one of {sorted(SEVERITIES)}")
            ann = r.get("annotations") or {}
            for k in ("summary", "description", "runbook_url"):
                if not isinstance(ann.get(k), str) or not ann[k].strip():
                    errs.append(f"{path}: {name}: annotations.{k} missing")
            for k, v in ann.items():
                if isinstance(v, str) and v.count("{{") != v.count("}}"):
                    errs.append(f"{path}: {name}: unbalanced template braces in {k}")
            url = ann.get("runbook_url", "")
            if url != f"{RUNBOOK_BASE}{name}.md":
                errs.append(f"{path}: {name}: runbook_url must be {RUNBOOK_BASE}{name}.md")
            elif not os.path.isfile(os.path.join(runbook_dir, f"{name}.md")):
                errs.append(f"{path}: {name}: runbook docs/runbooks/{name}.md does not exist")
    if os.path.isdir(runbook_dir):
        for f in os.listdir(runbook_dir):
            if f.endswith(".md") and f != "README.md" and f[:-3] not in alerts:
                errs.append(f"docs/runbooks/{f}: no alert named {f[:-3]}")
    return errs


def check_dashboards(dashdir):
    errs, uids = [], {}
    files = sorted(glob.glob(os.path.join(dashdir, "*.json")))
    if not files:
        errs.append(f"{dashdir}: no dashboards")
    for f in files:
        try:
            d = json.load(open(f, encoding="utf-8"))
        except ValueError as e:
            errs.append(f"{f}: invalid JSON: {e}")
            continue
        if not isinstance(d, dict):
            errs.append(f"{f}: not an object")
            continue
        if "dashboard" in d and "panels" not in d:
            errs.append(f"{f}: wrapped in {{\"dashboard\": ...}}; Grafana provisioning needs the raw dashboard object")
            continue
        for k in ("uid", "title", "panels", "templating", "schemaVersion"):
            if k not in d:
                errs.append(f"{f}: missing key {k}")
        if d.get("uid") in uids:
            errs.append(f"{f}: uid {d.get('uid')} also used by {uids[d['uid']]}")
        uids[d.get("uid")] = f
        ids = set()
        for p in d.get("panels", []):
            if p.get("id") in ids:
                errs.append(f"{f}: duplicate panel id {p.get('id')}")
            ids.add(p.get("id"))
            if p.get("type") in ("row", "text"):
                continue
            targets = p.get("targets") or []
            if not targets or not all(isinstance(t.get("expr"), str) and t["expr"].strip() for t in targets):
                errs.append(f"{f}: panel {p.get('title')!r} has no target with an expr")
            if "gridPos" not in p:
                errs.append(f"{f}: panel {p.get('title')!r} lacks gridPos")
    return errs


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default=os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    a = ap.parse_args(argv)
    mon = os.path.join(a.root, "monitoring")
    errs = check_rules(os.path.join(mon, "prometheus-rules.yaml"), os.path.join(a.root, "docs", "runbooks"))
    errs += check_dashboards(os.path.join(mon, "grafana-dashboards"))
    for e in errs:
        print(e)
    if errs:
        return 1
    print("ok: rules and dashboards are structurally valid")
    return 0


if __name__ == "__main__":
    sys.exit(main())
