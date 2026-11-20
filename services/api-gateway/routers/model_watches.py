# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Model watch routes (GryviaModelWatch, namespaced) for the dashboard and CLI."""
import json
import re
from typing import Any, Dict, List, Literal, Optional

from fastapi import APIRouter, Depends, Request
from pydantic import BaseModel, ConfigDict, Field, field_validator

from .common import Deps, create_item, delete_item, patch_item
from .uiutil import NAME_MAX, NAME_PATTERN, find_one, list_all, meta, namespaces, prune

PLURAL = "gryviamodelwatches"
KIND = "GryviaModelWatch"

_MAX_TEMPLATE_BYTES = 65536
_DURATION = r"^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$"


class Source(BaseModel):
    model_config = ConfigDict(extra="forbid")
    provider: Literal["huggingface", "ngc"] = "huggingface"
    author: str = Field(default="", max_length=96, pattern=r"^[A-Za-z0-9_.-]*$")
    nameRegex: str = Field(default="", max_length=256)
    pipelineTag: str = Field(default="", max_length=64, pattern=r"^[a-z0-9-]*$")
    minDownloads: int = Field(default=0, ge=0)
    limit: int = Field(default=0, ge=0, le=500)

    @field_validator("nameRegex")
    @classmethod
    def _regex(cls, v: str) -> str:
        try:
            re.compile(v)
        except re.error as exc:
            raise ValueError(f"nameRegex is not a valid regular expression: {exc}") from exc
        return v


class Sizing(BaseModel):
    model_config = ConfigDict(extra="forbid")
    gpuMemoryGB: int = Field(default=0, ge=0, le=1024)
    overheadPercent: int = Field(default=0, ge=0, le=1000)
    maxGPUs: int = Field(default=0, ge=0, le=1024)
    autoSizeSteps: List[str] = Field(default_factory=list, max_length=32)


class CreateModelWatch(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)
    sources: List[Source] = Field(min_length=1, max_length=16)
    workflowTemplate: Dict[str, Any]
    licenseAllowlist: List[str] = Field(default_factory=list, max_length=64)
    maxParamsB: int = Field(default=0, ge=0, le=10000)
    tokenSecretName: str = Field(default="", max_length=253, pattern=r"^([a-z0-9]([-a-z0-9.]*[a-z0-9])?)?$")
    pollInterval: str = Field(default="", max_length=32)
    includeExisting: bool = False
    retrainOnNewRevision: bool = False
    maxConcurrentRuns: int = Field(default=0, ge=0, le=64)
    sizing: Optional[Sizing] = None
    suspend: bool = False

    @field_validator("pollInterval")
    @classmethod
    def _duration(cls, v: str) -> str:
        if v and not re.match(_DURATION, v):
            raise ValueError("pollInterval must be a duration such as 30m or 1h")
        return v

    @field_validator("workflowTemplate")
    @classmethod
    def _template(cls, v: Dict[str, Any]) -> Dict[str, Any]:
        if len(json.dumps(v)) > _MAX_TEMPLATE_BYTES:
            raise ValueError("workflowTemplate is too large")
        steps = v.get("steps")
        if not isinstance(steps, list) or not steps:
            raise ValueError("workflowTemplate.steps must be a non-empty list")
        return v


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    cands = st.get("candidates") or []
    counts: Dict[str, int] = {}
    for c in cands:
        counts[c.get("phase") or "Unknown"] = counts.get(c.get("phase") or "Unknown", 0) + 1
    return {
        "metadata": meta(obj),
        "spec": prune({
            "sources": spec.get("sources") or [],
            "licenseAllowlist": spec.get("licenseAllowlist") or None,
            "maxParamsB": spec.get("maxParamsB"),
            "pollInterval": spec.get("pollInterval"),
            "maxConcurrentRuns": spec.get("maxConcurrentRuns"),
            "suspend": bool(spec.get("suspend")),
            "steps": [s.get("name") for s in (spec.get("workflowTemplate") or {}).get("steps") or []],
        }),
        "status": prune({
            "phase": st.get("phase"),
            "message": st.get("message"),
            "lastPollTime": st.get("lastPollTime"),
            "nextPollTime": st.get("nextPollTime"),
            "activeRuns": st.get("activeRuns"),
            "candidates": counts or None,
        }),
    }


def run_ui(c: Dict[str, Any]) -> Dict[str, Any]:
    return prune({
        "model": c.get("id"),
        "revision": c.get("revision"),
        "paramsB": c.get("paramsB"),
        "license": c.get("license"),
        "gpus": c.get("gpus"),
        "phase": c.get("phase"),
        "workflow": c.get("workflow"),
        "firstSeen": c.get("firstSeen"),
        "message": c.get("message"),
    })


def build_spec(body: CreateModelWatch) -> Dict[str, Any]:
    spec: Dict[str, Any] = {
        "sources": [s.model_dump(exclude_defaults=True) | {"provider": s.provider} for s in body.sources],
        "workflowTemplate": body.workflowTemplate,
    }
    for key in ("licenseAllowlist", "maxParamsB", "pollInterval", "includeExisting",
                "retrainOnNewRevision", "maxConcurrentRuns", "suspend"):
        value = getattr(body, key)
        if value:
            spec[key] = value
    if body.tokenSecretName:
        spec["tokenSecretRef"] = {"name": body.tokenSecretName}
    if body.sizing:
        sizing = body.sizing.model_dump(exclude_defaults=True)
        if sizing:
            spec["sizing"] = sizing
    return spec


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/model-watches")
    @deps.limiter.limit("30/minute")
    async def list_watches(request: Request, _=Depends(deps.verify_auth)):
        return {"items": [to_ui(o) for o in await list_all(request, deps, PLURAL)]}

    @router.get("/api/model-watches/{name}")
    @deps.limiter.limit("30/minute")
    async def get_watch(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, _ns = await find_one(request, deps, PLURAL, name)
        return to_ui(obj)

    @router.get("/api/model-watches/{name}/runs")
    @deps.limiter.limit("30/minute")
    async def get_runs(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, _ns = await find_one(request, deps, PLURAL, name)
        cands = (obj.get("status") or {}).get("candidates") or []
        return {"items": [run_ui(c) for c in reversed(cands)]}

    @router.post("/api/model-watches", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_watch(request: Request, body: CreateModelWatch, _=Depends(deps.verify_auth)):
        ns = namespaces(request, deps)[0]
        return to_ui(await create_item(deps, PLURAL, KIND, body.name, build_spec(body), namespace=ns))

    async def _set_suspend(request: Request, name: str, suspend: bool) -> Dict[str, Any]:
        _obj, ns = await find_one(request, deps, PLURAL, name)
        await patch_item(deps, PLURAL, name, {"spec": {"suspend": suspend}}, namespace=ns)
        return {"status": "suspended" if suspend else "resumed", "name": name}

    @router.post("/api/model-watches/{name}/suspend")
    @deps.limiter.limit("10/minute")
    async def suspend_watch(request: Request, name: str, _=Depends(deps.verify_auth)):
        return await _set_suspend(request, name, True)

    @router.post("/api/model-watches/{name}/resume")
    @deps.limiter.limit("10/minute")
    async def resume_watch(request: Request, name: str, _=Depends(deps.verify_auth)):
        return await _set_suspend(request, name, False)

    @router.delete("/api/model-watches/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_watch(request: Request, name: str, _=Depends(deps.verify_auth)):
        _obj, ns = await find_one(request, deps, PLURAL, name)
        await delete_item(deps, PLURAL, name, namespace=ns)
        return {"status": "deleted", "name": name}

    return router
