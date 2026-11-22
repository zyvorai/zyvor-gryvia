# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Dataset routes (GryviaDataset, cluster-scoped) for the dashboard and CLI.

A dataset is materialized into spec.namespace; a tenant sees, creates and deletes only datasets whose namespace is
one of theirs, and a tenant's new dataset always goes to their first namespace.
"""
from typing import Any, Dict, List, Literal, Optional

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, ConfigDict, Field, model_validator

from .common import Deps, create_item, delete_item, get_item, list_items
from .markings import can_see
from .uiutil import NAME_MAX, NAME_PATTERN, meta, prune

PLURAL = "gryviadatasets"
KIND = "GryviaDataset"

_URL = r"^https?://[^\s]+$"
_DNS = r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"


class HTTPSource(BaseModel):
    model_config = ConfigDict(extra="forbid")
    url: str = Field(max_length=2048, pattern=_URL)
    checksumURL: str = Field(default="", max_length=2048, pattern=r"^(https?://[^\s]+)?$")


class S3Source(BaseModel):
    model_config = ConfigDict(extra="forbid")
    bucket: str = Field(min_length=3, max_length=63, pattern=r"^[a-z0-9][a-z0-9.-]+$")
    prefix: str = Field(default="", max_length=1024)
    region: str = Field(default="", max_length=32, pattern=r"^[a-z0-9-]*$")
    endpoint: str = Field(default="", max_length=2048, pattern=r"^(https?://[^\s/]+(/[^\s]*)?)?$")
    credentialsSecret: str = Field(default="", max_length=253, pattern=r"^([a-z0-9]([-a-z0-9.]*[a-z0-9])?)?$")


class NFSSource(BaseModel):
    model_config = ConfigDict(extra="forbid")
    server: str = Field(min_length=1, max_length=253)
    path: str = Field(max_length=1024, pattern=r"^/")


class Source(BaseModel):
    model_config = ConfigDict(extra="forbid")
    type: Literal["http", "s3", "nfs"]
    http: Optional[HTTPSource] = None
    s3: Optional[S3Source] = None
    nfs: Optional[NFSSource] = None

    @model_validator(mode="after")
    def _matching_block(self) -> "Source":
        if getattr(self, self.type) is None:
            raise ValueError(f"source.type {self.type} needs source.{self.type}")
        return self


class Retention(BaseModel):
    model_config = ConfigDict(extra="forbid")
    keepLast: int = Field(default=0, ge=0, le=1000)


class CreateDataset(BaseModel):
    model_config = ConfigDict(extra="forbid")
    name: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)
    source: Source
    type: Literal["", "image", "text", "audio", "video", "tabular", "multimodal"] = ""
    description: str = Field(default="", max_length=1024)
    license: str = Field(default="", max_length=64)
    version: str = Field(default="", max_length=63, pattern=r"^([A-Za-z0-9][A-Za-z0-9._-]*)?$")
    namespace: str = Field(default="", max_length=63, pattern=r"^(" + _DNS[1:-1] + r")?$")
    size: str = Field(default="", max_length=16, pattern=r"^([0-9]+(Ki|Mi|Gi|Ti|Pi)?)?$")
    storageClass: str = Field(default="", max_length=253, pattern=r"^([a-z0-9]([-a-z0-9.]*[a-z0-9])?)?$")
    retention: Optional[Retention] = None


def dataset_namespace(obj: Dict[str, Any], default: str) -> str:
    return (obj.get("spec") or {}).get("namespace") or (obj.get("status") or {}).get("namespace") or default


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    src = spec.get("source") or {}
    where = (src.get("http") or {}).get("url") or ""
    if src.get("s3"):
        where = f"s3://{src['s3'].get('bucket', '')}/{src['s3'].get('prefix', '')}"
    if src.get("nfs"):
        where = f"{src['nfs'].get('server', '')}:{src['nfs'].get('path', '')}"
    return {
        "metadata": meta(obj),
        "spec": prune({
            "type": spec.get("type"),
            "description": spec.get("description"),
            "license": spec.get("license"),
            "version": spec.get("version"),
            "namespace": spec.get("namespace"),
            "source": prune({"type": src.get("type"), "location": where}),
        }),
        "status": prune({
            "state": st.get("state"),
            "message": st.get("message"),
            "namespace": st.get("namespace"),
            "pvcName": st.get("pvcName"),
            "subPath": st.get("subPath"),
            "currentVersion": st.get("currentVersion"),
            "fileCount": st.get("fileCount"),
            "totalSizeBytes": st.get("totalSizeBytes"),
            "versions": [prune(v) for v in st.get("versions") or []] or None,
        }),
    }


def build_spec(body: CreateDataset, namespace: str) -> Dict[str, Any]:
    src = body.source
    spec: Dict[str, Any] = {"source": {"type": src.type, src.type: getattr(src, src.type).model_dump(exclude_defaults=True)}}
    for key in ("type", "description", "license", "version"):
        if getattr(body, key):
            spec[key] = getattr(body, key)
    if namespace:
        spec["namespace"] = namespace
    cache = {k: v for k, v in (("size", body.size), ("storageClass", body.storageClass)) if v}
    if cache:
        spec["cache"] = cache
    if body.retention and body.retention.keepLast:
        spec["versioning"] = {"enabled": True, "retentionPolicy": {"keepLast": body.retention.keepLast}}
    return spec


async def visible_datasets(request: Request, deps: Deps) -> List[Dict[str, Any]]:
    """Datasets the caller may see: admins all, a tenant its namespaces', minus those marked beyond its clearance."""
    allowed = None if getattr(request.state, "role", None) == "admin" else set(
        getattr(request.state, "tenant_namespaces", None) or [deps.job_namespace])
    return [o for o in await list_items(deps, PLURAL) if can_see(request, o)
            and (allowed is None or dataset_namespace(o, deps.job_namespace) in allowed)]


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    def tenant_namespaces(request: Request) -> Optional[List[str]]:
        if getattr(request.state, "role", None) == "admin":
            return None
        return list(getattr(request.state, "tenant_namespaces", None) or [deps.job_namespace])

    def visible(request: Request, obj: Dict[str, Any]) -> bool:
        allowed = tenant_namespaces(request)
        return can_see(request, obj) and (allowed is None or dataset_namespace(obj, deps.job_namespace) in allowed)

    async def get_visible(request: Request, name: str) -> Dict[str, Any]:
        obj = await get_item(deps, PLURAL, name)
        if not visible(request, obj):
            raise HTTPException(status_code=404, detail=f"{PLURAL}/{name}: Not Found")
        return obj

    @router.get("/api/datasets")
    @deps.limiter.limit("30/minute")
    async def list_datasets(request: Request, _=Depends(deps.verify_auth)):
        return {"items": [to_ui(o) for o in await list_items(deps, PLURAL) if visible(request, o)]}

    @router.get("/api/datasets/{name}")
    @deps.limiter.limit("30/minute")
    async def get_dataset(request: Request, name: str, _=Depends(deps.verify_auth)):
        return to_ui(await get_visible(request, name))

    @router.post("/api/datasets", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_dataset(request: Request, body: CreateDataset, _=Depends(deps.verify_auth)):
        allowed = tenant_namespaces(request)
        ns = body.namespace
        if allowed is not None:
            if ns and ns not in allowed:
                raise HTTPException(status_code=403, detail=f"namespace {ns} is not one of yours")
            ns = ns or allowed[0]
        return to_ui(await create_item(deps, PLURAL, KIND, body.name, build_spec(body, ns)))

    @router.delete("/api/datasets/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_dataset(request: Request, name: str, _=Depends(deps.verify_auth)):
        await get_visible(request, name)
        await delete_item(deps, PLURAL, name)
        return {"status": "deleted", "name": name}

    return router
