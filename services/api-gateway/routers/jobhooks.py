# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Job hook routes (GryviaJobHook, namespaced) for the dashboard and CLI.

A hook POSTs a signed JSON body (or a Slack message) when a GryviaAIJob or GryviaWorkflow in its namespace reaches one
of its events. The ai-operator delivers it (--enable-job-hooks) and refuses private receivers unless the operator
allows their CIDR.
"""
import re
from typing import Any, Dict, List, Literal, Optional

from fastapi import APIRouter, Depends, Request
from pydantic import BaseModel, ConfigDict, Field, field_validator

from .common import Deps, create_item, delete_item, patch_item
from .uiutil import NAME_MAX, NAME_PATTERN, find_one, list_all, meta, namespaces, prune

PLURAL = "gryviajobhooks"
KIND = "GryviaJobHook"

Event = Literal["Running", "Succeeded", "Failed", "Cancelled", "Preempted", "Rejected"]
JobKind = Literal["GryviaAIJob", "GryviaWorkflow"]
_HEADER = re.compile(r"^[A-Za-z0-9-]{1,64}$")
_RESERVED = {"host", "content-type", "content-length", "user-agent", "connection", "transfer-encoding"}
_LABEL_KEY = r"^([a-z0-9]([-a-z0-9.]*[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$"
_LABEL_VALUE = re.compile(r"^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$")


class CreateJobHook(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)
    events: List[Event] = Field(min_length=1, max_length=6)
    kinds: List[JobKind] = Field(default_factory=list, max_length=2)
    matchLabels: Dict[str, str] = Field(default_factory=dict, max_length=16)
    url: str = Field(min_length=1, max_length=2048, pattern=r"^https?://[^\s/?#@]+(/\S*)?$")
    format: Literal["gryvia", "slack"] = "gryvia"
    headers: Dict[str, str] = Field(default_factory=dict, max_length=16)
    secretName: str = Field(default="", max_length=253, pattern=r"^([a-z0-9]([-a-z0-9.]*[a-z0-9])?)?$")
    timeoutSeconds: int = Field(default=0, ge=0, le=30)
    attempts: int = Field(default=0, ge=0, le=10)
    backoffSeconds: int = Field(default=0, ge=0, le=3600)
    suspend: bool = False

    @field_validator("events", "kinds")
    @classmethod
    def _unique(cls, v: List[str]) -> List[str]:
        if len(v) != len(set(v)):
            raise ValueError("values must be unique")
        return v

    @field_validator("url")
    @classmethod
    def _no_fragment(cls, v: str) -> str:
        if "#" in v:
            raise ValueError("the URL must not have a fragment")
        return v

    @field_validator("headers")
    @classmethod
    def _headers(cls, v: Dict[str, str]) -> Dict[str, str]:
        for k, val in v.items():
            lk = k.lower()
            if not _HEADER.match(k) or lk in _RESERVED or lk.startswith("x-gryvia-"):
                raise ValueError(f"header {k!r} is not allowed")
            if len(val) > 1024 or "\r" in val or "\n" in val:
                raise ValueError(f"header {k!r} has an invalid value")
        return v

    @field_validator("matchLabels")
    @classmethod
    def _labels(cls, v: Dict[str, str]) -> Dict[str, str]:
        for k, val in v.items():
            if len(k) > 316 or not re.match(_LABEL_KEY, k) or len(val) > 63 or not _LABEL_VALUE.match(val):
                raise ValueError(f"invalid label {k}={val}")
        return v


class Suspend(BaseModel):
    model_config = ConfigDict(extra="forbid")
    suspend: bool


def build_spec(body: CreateJobHook) -> Dict[str, Any]:
    webhook: Dict[str, Any] = {"url": body.url, "format": body.format}
    if body.headers:
        webhook["headers"] = body.headers
    if body.secretName:
        webhook["secretRef"] = {"name": body.secretName}
    if body.timeoutSeconds:
        webhook["timeoutSeconds"] = body.timeoutSeconds
    spec: Dict[str, Any] = {"events": body.events, "webhook": webhook}
    if body.kinds:
        spec["kinds"] = body.kinds
    if body.matchLabels:
        spec["selector"] = {"matchLabels": body.matchLabels}
    retry = prune({"attempts": body.attempts or None, "backoffSeconds": body.backoffSeconds or None})
    if retry:
        spec["retry"] = retry
    if body.suspend:
        spec["suspend"] = True
    return spec


def _ready(st: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    for c in st.get("conditions") or []:
        if c.get("type") == "Ready":
            return c
    return None


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    wh = spec.get("webhook") or {}
    ready = _ready(st) or {}
    return {
        "metadata": meta(obj),
        "spec": prune({
            "events": spec.get("events"),
            "kinds": spec.get("kinds"),
            "matchLabels": (spec.get("selector") or {}).get("matchLabels"),
            "url": wh.get("url"),
            "format": wh.get("format"),
            "headers": sorted((wh.get("headers") or {}).keys()) or None,
            "secretName": (wh.get("secretRef") or {}).get("name"),
            "suspend": spec.get("suspend"),
        }),
        "status": prune({
            "ready": ready.get("status"),
            "reason": ready.get("reason"),
            "message": ready.get("message"),
            "deliveries": st.get("deliveries"),
            "failures": st.get("failures"),
            "lastDelivery": st.get("lastDelivery"),
        }),
    }


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/job-hooks")
    @deps.limiter.limit("30/minute")
    async def list_hooks(request: Request, _=Depends(deps.verify_auth)):
        return {"items": [to_ui(o) for o in await list_all(request, deps, PLURAL)]}

    @router.get("/api/job-hooks/{name}")
    @deps.limiter.limit("30/minute")
    async def get_hook(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, _ns = await find_one(request, deps, PLURAL, name)
        return to_ui(obj)

    @router.post("/api/job-hooks", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_hook(request: Request, body: CreateJobHook, _=Depends(deps.verify_auth)):
        ns = namespaces(request, deps)[0]
        return to_ui(await create_item(deps, PLURAL, KIND, body.name, build_spec(body), namespace=ns))

    @router.post("/api/job-hooks/{name}/suspend")
    @deps.limiter.limit("10/minute")
    async def suspend_hook(request: Request, name: str, body: Suspend, _=Depends(deps.verify_auth)):
        _obj, ns = await find_one(request, deps, PLURAL, name)
        await patch_item(deps, PLURAL, name, {"spec": {"suspend": body.suspend}}, namespace=ns)
        return {"status": "suspended" if body.suspend else "resumed", "name": name}

    @router.delete("/api/job-hooks/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_hook(request: Request, name: str, _=Depends(deps.verify_auth)):
        _obj, ns = await find_one(request, deps, PLURAL, name)
        await delete_item(deps, PLURAL, name, namespace=ns)
        return {"status": "deleted", "name": name}

    return router
