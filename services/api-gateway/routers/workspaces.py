# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Workspace routes (GryviaWorkspace, namespaced) for the dashboard."""
import re
from typing import Any, Dict, Literal

from fastapi import APIRouter, Depends, Request
from pydantic import BaseModel, ConfigDict, Field

from .common import Deps, create_item, delete_item, patch_item
from .uiutil import NAME_MAX, NAME_PATTERN, find_one, fmt_duration, list_all, meta, namespaces, prune

PLURAL = "gryviaworkspaces"
KIND = "GryviaWorkspace"
_TIMEOUT_RE = re.compile(r"^([1-9][0-9]{0,4})([mh])$")


class CreateWorkspace(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)
    type: Literal["jupyter", "vscode"]
    gpuCount: int = Field(default=0, ge=0, le=64)
    gpuType: str = Field(default="", max_length=63, pattern=r"^([A-Za-z0-9][A-Za-z0-9._-]*)?$")
    storageSize: str = Field(default="", pattern=r"^([1-9][0-9]{0,5}(Mi|Gi|Ti))?$")
    idleTimeout: str = Field(default="", max_length=8, pattern=r"^(([1-9][0-9]{0,4})[mh])?$")


def _idle_minutes(value: str) -> int:
    m = _TIMEOUT_RE.match(value)
    assert m
    n = int(m.group(1))
    return n * 60 if m.group(2) == "h" else n


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    idle = spec.get("idleTimeoutMinutes")
    running = st.get("phase") in ("Running", "Idle")
    return {
        "metadata": meta(obj),
        "spec": prune({
            "type": spec.get("type"),
            "gpuCount": spec.get("gpuCount"),
            "gpuType": spec.get("gpuType"),
            "storageSize": spec.get("storage"),
            "idleTimeout": f"{idle}m" if idle else None,
        }),
        "status": prune({
            "phase": st.get("phase"),
            "message": st.get("message") if isinstance(st.get("message"), str) and st.get("message") else None,
            "url": st.get("url"),
            "uptime": fmt_duration(st.get("startTime")) if running else None,
            "lastActivity": st.get("lastActivity"),
        }),
    }


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/workspaces")
    @deps.limiter.limit("30/minute")
    async def list_workspaces(request: Request, _=Depends(deps.verify_auth)):
        return {"items": [to_ui(o) for o in await list_all(request, deps, PLURAL)]}

    @router.get("/api/workspaces/{name}")
    @deps.limiter.limit("30/minute")
    async def get_workspace(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, _ns = await find_one(request, deps, PLURAL, name)
        return to_ui(obj)

    @router.post("/api/workspaces", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_workspace(request: Request, body: CreateWorkspace, _=Depends(deps.verify_auth)):
        spec: Dict[str, Any] = {"type": body.type}
        if body.gpuCount:
            spec["gpuCount"] = body.gpuCount
        if body.gpuType:
            spec["gpuType"] = body.gpuType
        if body.storageSize:
            spec["storage"] = body.storageSize
        if body.idleTimeout:
            spec["idleTimeoutMinutes"] = _idle_minutes(body.idleTimeout)
        ns = namespaces(request, deps)[0]
        return to_ui(await create_item(deps, PLURAL, KIND, body.name, spec, namespace=ns))

    @router.delete("/api/workspaces/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_workspace(request: Request, name: str, _=Depends(deps.verify_auth)):
        _obj, ns = await find_one(request, deps, PLURAL, name)
        await delete_item(deps, PLURAL, name, namespace=ns)
        return {"status": "deleted", "name": name}

    async def _set_paused(request: Request, name: str, paused: bool) -> Dict[str, Any]:
        _obj, ns = await find_one(request, deps, PLURAL, name)
        await patch_item(deps, PLURAL, name, {"spec": {"paused": paused}}, namespace=ns)
        return {"status": "paused" if paused else "resumed", "name": name}

    @router.post("/api/workspaces/{name}/pause")
    @deps.limiter.limit("10/minute")
    async def pause_workspace(request: Request, name: str, _=Depends(deps.verify_auth)):
        return await _set_paused(request, name, True)

    @router.post("/api/workspaces/{name}/resume")
    @deps.limiter.limit("10/minute")
    async def resume_workspace(request: Request, name: str, _=Depends(deps.verify_auth)):
        return await _set_paused(request, name, False)

    return router
