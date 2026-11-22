# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Data catalog over GryviaDataset: find a dataset by name, tag or owner, see its schema, how fresh it is and what
depends on it.

The catalog metadata is stored as annotations on the dataset (no CRD change): gryvia.io/owner, gryvia.io/tags
(comma separated) and gryvia.io/schema (JSON list of columns). The description is the dataset's own
spec.description. "Used by" comes from the lineage graph: the jobs that mount the dataset's volume and the models
and services downstream of them. Visibility is the dataset routes' (tenant namespaces, markings).
"""
import json
import re
from datetime import datetime
from typing import Any, Dict, List, Optional

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from pydantic import BaseModel, ConfigDict, Field

from .common import Deps, list_items, patch_item
from .datasets import dataset_namespace, visible_datasets
from .lineage import build_graph
from .markings import can_see, marking_of
from .uiutil import namespaces, parse_ts

OWNER = "gryvia.io/owner"
TAGS = "gryvia.io/tags"
SCHEMA = "gryvia.io/schema"
PLURAL = "gryviadatasets"
MAX_TAGS = 10
MAX_COLUMNS = 200
_TAG = r"^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$"


class Column(BaseModel):
    model_config = ConfigDict(extra="forbid")
    name: str = Field(min_length=1, max_length=63, pattern=r"^[A-Za-z0-9_][A-Za-z0-9_.-]*$")
    type: str = Field(default="", max_length=32, pattern=r"^[A-Za-z0-9_<>(), ]*$")
    description: str = Field(default="", max_length=200)


class CatalogEdit(BaseModel):
    """Everything is replaced: send the full set. Empty values clear a field."""
    model_config = ConfigDict(extra="forbid")
    owner: str = Field(default="", max_length=100)
    description: str = Field(default="", max_length=1024)
    tags: List[str] = Field(default_factory=list, max_length=MAX_TAGS)
    columns: List[Column] = Field(default_factory=list, max_length=MAX_COLUMNS)


def _tags(obj: Dict[str, Any]) -> List[str]:
    raw = ((obj.get("metadata") or {}).get("annotations") or {}).get(TAGS) or ""
    return [t for t in raw.split(",") if t]


def _columns(obj: Dict[str, Any]) -> List[Dict[str, Any]]:
    raw = ((obj.get("metadata") or {}).get("annotations") or {}).get(SCHEMA)
    try:
        cols = json.loads(raw) if raw else []
    except ValueError:
        return []
    return [c for c in cols if isinstance(c, dict) and c.get("name")] if isinstance(cols, list) else []


def _updated(obj: Dict[str, Any]) -> Optional[str]:
    """When the newest version was created (falls back to the object's creation time)."""
    stamps = [v.get("createdAt") for v in ((obj.get("status") or {}).get("versions") or []) if isinstance(v, dict)]
    stamps = [s for s in stamps if parse_ts(s)]
    if stamps:
        return max(stamps, key=lambda s: parse_ts(s) or datetime.min)
    return (obj.get("metadata") or {}).get("creationTimestamp")


def entry(obj: Dict[str, Any], default_ns: str, used_by: Dict[str, List[str]]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    name = (obj.get("metadata") or {}).get("name", "")
    ns = dataset_namespace(obj, default_ns)
    cols = _columns(obj)
    return {
        "name": name, "namespace": ns,
        "description": spec.get("description") or "", "type": spec.get("type") or "",
        "license": spec.get("license") or "",
        "owner": ((obj.get("metadata") or {}).get("annotations") or {}).get(OWNER) or "",
        "tags": _tags(obj), "columns": cols,
        "marking": marking_of(obj),
        "state": st.get("state"), "version": st.get("currentVersion"),
        "files": st.get("fileCount"), "bytes": st.get("totalSizeBytes"),
        "updated": _updated(obj),
        "usedBy": used_by.get(f"dataset/{ns}/{name}", []),
    }


def downstream(graph: Dict[str, Any]) -> Dict[str, List[str]]:
    """For each dataset node, the names ('kind/name') of everything downstream of it."""
    nodes = {n["id"]: n for n in graph["nodes"]}
    nxt: Dict[str, List[str]] = {}
    for e in graph["edges"]:
        nxt.setdefault(e["from"], []).append(e["to"])
    out: Dict[str, List[str]] = {}
    for nid, n in nodes.items():
        if n["kind"] != "dataset":
            continue
        seen, queue = {nid}, [nid]
        while queue:
            for far in nxt.get(queue.pop(0), []):
                if far not in seen:
                    seen.add(far)
                    queue.append(far)
        seen.discard(nid)
        out[nid] = sorted(f"{nodes[i]['kind']}/{nodes[i]['name']}" for i in seen if i in nodes)
    return out


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    async def scoped(request: Request, plural: str) -> List[Dict[str, Any]]:
        """Admins: every namespace. Tenants: their own."""
        if getattr(request.state, "role", None) == "admin":
            return [o for o in await list_items(deps, plural) if can_see(request, o)]
        out: List[Dict[str, Any]] = []
        for ns in namespaces(request, deps):
            out.extend(o for o in await list_items(deps, plural, namespace=ns) if can_see(request, o))
        return out

    @router.get("/api/data-catalog")
    @deps.limiter.limit("30/minute")
    async def catalog(request: Request, q: str = Query("", max_length=100), tag: str = Query("", max_length=40),
                      owner: str = Query("", max_length=100), _=Depends(deps.verify_auth)):
        datasets = await visible_datasets(request, deps)
        used = downstream(build_graph(datasets, await scoped(request, "gryviaaijobs"),
                                      await scoped(request, "gryviamodelregistries"),
                                      await scoped(request, "gryviainferenceservices"), deps.job_namespace))
        items = [entry(d, deps.job_namespace, used) for d in datasets]
        facets: Dict[str, Dict[str, int]] = {"tags": {}, "owners": {}}
        for e in items:
            for t in e["tags"]:
                facets["tags"][t] = facets["tags"].get(t, 0) + 1
            if e["owner"]:
                facets["owners"][e["owner"]] = facets["owners"].get(e["owner"], 0) + 1
        needle = q.strip().lower()

        def match(e: Dict[str, Any]) -> bool:
            if tag and tag not in e["tags"]:
                return False
            if owner and e["owner"] != owner:
                return False
            hay = " ".join([e["name"], e["description"], e["owner"], *e["tags"], *(c["name"] for c in e["columns"])])
            return not needle or needle in hay.lower()

        items = sorted((e for e in items if match(e)), key=lambda e: e["name"])
        return {"items": items, "facets": facets}

    @router.put("/api/data-catalog/{name}")
    @deps.limiter.limit("20/minute")
    async def edit(request: Request, name: str, body: CatalogEdit, _=Depends(deps.verify_auth)):
        match = next((d for d in await visible_datasets(request, deps) if (d.get("metadata") or {}).get("name") == name), None)
        if match is None:
            raise HTTPException(status_code=404, detail=f"{PLURAL}/{name}: Not Found")
        tags = sorted({t.strip().lower() for t in body.tags if t.strip()})
        if any(len(t) > 32 or not re.match(_TAG, t) for t in tags):
            raise HTTPException(status_code=422, detail="tags are lowercase letters, digits, '-', '_' or '.', at most 32 characters")
        if len({c.name for c in body.columns}) != len(body.columns):
            raise HTTPException(status_code=422, detail="column names must be unique")
        schema = json.dumps([c.model_dump(exclude_defaults=True) for c in body.columns], separators=(",", ":"))
        patch = {"metadata": {"annotations": {OWNER: body.owner.strip() or None, TAGS: ",".join(tags) or None,
                                              SCHEMA: schema if body.columns else None}},
                 "spec": {"description": body.description}}
        await patch_item(deps, PLURAL, name, patch)
        fresh = next(d for d in await visible_datasets(request, deps) if (d.get("metadata") or {}).get("name") == name)
        return entry(fresh, deps.job_namespace, {})

    return router
