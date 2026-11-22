# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Markings: label-based access on top of tenant namespaces.

A resource annotated ``gryvia.io/marking: <name>`` (GryviaDataset, GryviaModelRegistry) is visible only to a
provider administrator and to OIDC users whose ``groups`` claim contains ``gryvia-marking-<name>``. Everyone
else gets the same 404 as for a resource that does not exist, and it is left out of lists and the lineage graph.
Unmarked resources behave as before. A provider administrator sets or clears a marking with
``PUT /api/markings/{datasets|models}/{name}``.

Scope: reads, and the actions that start from reading the object (promote, delete, serve lookup). It does not
stop a cleared user's job from mounting a marked dataset's volume by hand, and the API key and dashboard login
(one admin identity) see everything.
"""
import re
from typing import Any, Dict, Optional, Set

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, ConfigDict

from .common import Deps, get_item, patch_item, require_admin

ANNOTATION = "gryvia.io/marking"
GROUP_PREFIX = "gryvia-marking-"
NAME_RE = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
KINDS = {"datasets": "gryviadatasets", "models": "gryviamodelregistries"}


def marking_of(obj: Dict[str, Any]) -> Optional[str]:
    value = ((obj.get("metadata") or {}).get("annotations") or {}).get(ANNOTATION)
    return value if isinstance(value, str) and value else None


def clearances(request: Request) -> Set[str]:
    claims = getattr(request.state, "user_claims", None) or {}
    groups = claims.get("groups") if isinstance(claims, dict) else None
    if not isinstance(groups, list):
        return set()
    return {g[len(GROUP_PREFIX):] for g in groups if isinstance(g, str) and g.startswith(GROUP_PREFIX)}


def can_see(request: Request, obj: Dict[str, Any]) -> bool:
    if getattr(request.state, "role", None) == "admin":
        return True
    marking = marking_of(obj)
    return marking is None or marking in clearances(request)


class SetMarking(BaseModel):
    model_config = ConfigDict(extra="forbid")

    marking: str = ""  # empty clears it


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.put("/api/markings/{kind}/{name}")
    @deps.limiter.limit("10/minute")
    async def set_marking(request: Request, kind: str, name: str, body: SetMarking,
                          _=Depends(deps.verify_auth), __=Depends(require_admin)):
        plural = KINDS.get(kind)
        if plural is None:
            raise HTTPException(status_code=404, detail=f"unknown kind '{kind}' (use datasets or models)")
        if body.marking and not (len(body.marking) <= 40 and NAME_RE.match(body.marking)):
            raise HTTPException(status_code=422, detail="marking must be a lowercase DNS label of at most 40 characters")
        ns = None
        if kind == "models":
            # Admin: the gateway's namespace, or the tenant namespaces an OIDC admin is scoped to.
            from .uiutil import find_one  # imported here: uiutil imports this module
            _obj, ns = await find_one(request, deps, plural, name)
        else:
            await get_item(deps, plural, name)
        patch = {"metadata": {"annotations": {ANNOTATION: body.marking or None}}}
        await patch_item(deps, plural, name, patch, namespace=ns)
        return {"name": name, "marking": body.marking or None,
                "group": f"{GROUP_PREFIX}{body.marking}" if body.marking else None}

    return router
