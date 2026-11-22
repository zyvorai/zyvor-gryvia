#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Validate every gryvia.io manifest under examples/ and in fenced ```yaml blocks of the Markdown docs against the CRD schemas in crds/.

Structural check: known kind, no unknown fields (unless the schema preserves unknown fields), required fields
present, and basic types. Not a full OpenAPI validator, but it catches renamed or invented fields.
A doc snippet that sketches a design the CRDs do not support must not be fenced as yaml (use a text fence and say so).

Usage: python3 scripts/check-examples.py            (exit 1 if any manifest is invalid)
"""
import glob
import os
import re
import sys

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def load_schemas():
    out = {}
    for path in glob.glob(os.path.join(ROOT, "crds", "*.yaml")):
        crd = yaml.safe_load(open(path))
        ver = crd["spec"]["versions"][0]
        out[crd["spec"]["names"]["kind"]] = ver["schema"]["openAPIV3Schema"]
    return out


TYPES = {"string": str, "integer": int, "number": (int, float), "boolean": bool, "array": list, "object": dict}


def check(value, schema, path, errs):
    if schema is None:
        return
    t = schema.get("type")
    if t in TYPES:
        ok = isinstance(value, TYPES[t]) and not (t in ("integer", "number") and isinstance(value, bool))
        if not ok:
            if not (schema.get("x-kubernetes-int-or-string") or schema.get("format") == "int-or-string"):
                errs.append(f"{path}: expected {t}, got {type(value).__name__}")
            return
    if schema.get("x-kubernetes-int-or-string") and isinstance(value, (int, str)):
        return
    if isinstance(value, dict):
        props = schema.get("properties")
        if props is not None:
            for k, v in value.items():
                if k in props:
                    check(v, props[k], f"{path}.{k}", errs)
                elif not schema.get("x-kubernetes-preserve-unknown-fields") and not schema.get("additionalProperties"):
                    errs.append(f"{path}.{k}: unknown field")
            for r in schema.get("required", []):
                if r not in value:
                    errs.append(f"{path}.{r}: required field missing")
        elif isinstance(schema.get("additionalProperties"), dict):
            for k, v in value.items():
                check(v, schema["additionalProperties"], f"{path}.{k}", errs)
    elif isinstance(value, list) and isinstance(schema.get("items"), dict):
        for i, v in enumerate(value):
            check(v, schema["items"], f"{path}[{i}]", errs)


FENCE = re.compile(r"^```ya?ml[^\n]*\n(.*?)^```", re.S | re.M)
SKIP_DOCS = ("node_modules", "CHANGELOG.md")


def documents():
    for path in glob.glob(os.path.join(ROOT, "examples", "**", "*.y*ml"), recursive=True):
        yield os.path.relpath(path, ROOT), open(path, errors="ignore").read(), False
    for path in glob.glob(os.path.join(ROOT, "**", "*.md"), recursive=True):
        rel = os.path.relpath(path, ROOT)
        if any(part in rel for part in SKIP_DOCS):
            continue
        text = open(path, errors="ignore").read()
        for m in FENCE.finditer(text):
            line = text[: m.start()].count("\n") + 2
            yield f"{rel}:{line}", m.group(1), True


def main() -> int:
    schemas = load_schemas()
    bad, total = [], 0
    for rel, text, in_doc in documents():
        try:
            docs = list(yaml.safe_load_all(text))
        except yaml.YAMLError as e:
            if not in_doc:  # doc snippets may hold placeholders such as "..."
                bad.append((rel, "-", [f"YAML parse error: {str(e).splitlines()[0]}"]))
            continue
        for d in docs:
            if not isinstance(d, dict) or not re.match(r"^gryvia\.io/", str(d.get("apiVersion", ""))):
                continue
            if "spec" not in d and "status" in d:
                continue  # illustrative status output, not a manifest
            total += 1
            name = f"{d.get('kind')}/{(d.get('metadata') or {}).get('name', '?')}"
            if d.get("kind") not in schemas:
                bad.append((rel, name, ["unknown kind"]))
                continue
            errs = []
            if in_doc and d.get("apiVersion") != "gryvia.io/v1alpha1":
                errs.append(f"apiVersion is {d.get('apiVersion')}, expected gryvia.io/v1alpha1")
            check(d.get("spec", {}), schemas[d["kind"]]["properties"]["spec"], "spec", errs)
            if errs:
                bad.append((rel, name, errs))
    print(f"{total} gryvia.io manifests checked, {len(bad)} invalid")
    for rel, name, errs in bad:
        print(f"{rel}: {name}")
        for e in errs[:6]:
            print(f"    {e}")
        if len(errs) > 6:
            print(f"    ... {len(errs) - 6} more")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
