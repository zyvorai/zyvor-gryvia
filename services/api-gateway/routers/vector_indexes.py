# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Vector index routes (GryviaVectorIndex, namespaced) for the dashboard and CLI.

Queries go to the LLM gateway (POST /v1/retrieve with an API key of the index's namespace); these routes only
manage the indexes.
"""
import time
from typing import Any, Dict, Literal, Optional

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, ConfigDict, Field, model_validator

from .common import Deps, create_item, delete_item, patch_item
from .uiutil import NAME_MAX, NAME_PATTERN, find_one, list_all, meta, namespaces, prune

PLURAL = "gryviavectorindexes"
KIND = "GryviaVectorIndex"
REINGEST_ANNOTATION = "gryvia.io/reingest"

_DNS = r"^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$"
_QUANTITY = r"^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|K|M|G|T|P)?$"


class Embedding(BaseModel):
    model_config = ConfigDict(extra="forbid")
    model: str = Field(min_length=1, max_length=63, pattern=r"^[A-Za-z0-9._-]+$")
    batchSize: int = Field(default=0, ge=0, le=1024)


class Chunking(BaseModel):
    model_config = ConfigDict(extra="forbid")
    size: int = Field(default=0, ge=0, le=20000)
    overlap: int = Field(default=0, ge=0, le=10000)

    @model_validator(mode="after")
    def _overlap(self) -> "Chunking":
        if self.size and self.size < 50:
            raise ValueError("chunking.size must be at least 50")
        if self.overlap >= (self.size or 1000):
            raise ValueError("chunking.overlap must be less than chunking.size")
        return self


class Store(BaseModel):
    model_config = ConfigDict(extra="forbid")
    type: Literal["managed", "external"] = "managed"
    storageSize: str = Field(default="", max_length=32, pattern=r"^(" + _QUANTITY[1:-1] + r")?$")
    storageClass: str = Field(default="", max_length=253, pattern=r"^(" + _DNS[1:-1] + r")?$")
    url: str = Field(default="", max_length=2048, pattern=r"^(https?://\S+)?$")
    apiKeySecretName: str = Field(default="", max_length=253, pattern=r"^(" + _DNS[1:-1] + r")?$")
    apiKeySecretKey: str = Field(default="", max_length=253, pattern=r"^[-._a-zA-Z0-9]*$")

    @model_validator(mode="after")
    def _by_type(self) -> "Store":
        if self.type == "external":
            if not self.url:
                raise ValueError("an external store needs url")
            if self.storageSize or self.storageClass:
                raise ValueError("storageSize and storageClass apply to managed stores")
        elif self.url or self.apiKeySecretName:
            raise ValueError("url and apiKeySecretName apply to external stores")
        return self


class CreateVectorIndex(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)
    datasetRef: str = Field(min_length=1, max_length=253, pattern=_DNS)
    embedding: Embedding
    chunking: Optional[Chunking] = None
    store: Store = Field(default_factory=Store)
    collection: str = Field(default="", max_length=63, pattern=r"^([A-Za-z0-9_-]+)?$")
    schedule: str = Field(default="", max_length=64)
    suspend: bool = False


def build_spec(body: CreateVectorIndex) -> Dict[str, Any]:
    spec: Dict[str, Any] = {
        "datasetRef": body.datasetRef,
        "embedding": body.embedding.model_dump(exclude_defaults=True),
        "store": {"type": body.store.type},
    }
    if body.chunking:
        spec["chunking"] = body.chunking.model_dump(exclude_defaults=True)
    s = body.store
    if s.type == "managed":
        managed = prune({"storageSize": s.storageSize or None, "storageClass": s.storageClass or None})
        if managed:
            spec["store"]["managed"] = managed
    else:
        ext: Dict[str, Any] = {"url": s.url}
        if s.apiKeySecretName:
            ext["apiKeySecretRef"] = prune({"name": s.apiKeySecretName, "key": s.apiKeySecretKey or None})
        spec["store"]["external"] = ext
    for key in ("collection", "schedule", "suspend"):
        value = getattr(body, key)
        if value:
            spec[key] = value
    return spec


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    store = spec.get("store") or {}
    return {
        "metadata": meta(obj),
        "spec": prune({
            "datasetRef": spec.get("datasetRef"),
            "embeddingModel": (spec.get("embedding") or {}).get("model"),
            "chunking": spec.get("chunking"),
            "store": store.get("type"),
            "storeURL": (store.get("external") or {}).get("url"),
            "collection": spec.get("collection"),
            "schedule": spec.get("schedule"),
            "suspend": bool(spec.get("suspend")),
        }),
        "status": prune({
            "phase": st.get("phase"),
            "message": st.get("message"),
            "storeURL": st.get("storeURL"),
            "collection": st.get("collection"),
            "documents": st.get("documents"),
            "chunks": st.get("chunks"),
            "dimensions": st.get("dimensions"),
            "datasetVersion": st.get("datasetVersion"),
            "lastIngested": st.get("lastIngested"),
            "ingestJob": st.get("ingestJob"),
            "nextScheduleTime": st.get("nextScheduleTime"),
            "keySecret": st.get("keySecret"),
        }),
    }


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/vector-indexes")
    @deps.limiter.limit("30/minute")
    async def list_indexes(request: Request, _=Depends(deps.verify_auth)):
        return {"items": [to_ui(o) for o in await list_all(request, deps, PLURAL)],
                "retrieveURL": (deps.llm_gateway_url.rstrip("/") + "/v1/retrieve") if deps.llm_gateway_url else ""}

    @router.get("/api/vector-indexes/{name}")
    @deps.limiter.limit("30/minute")
    async def get_index(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, _ns = await find_one(request, deps, PLURAL, name)
        return to_ui(obj)

    @router.post("/api/vector-indexes", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_index(request: Request, body: CreateVectorIndex, _=Depends(deps.verify_auth)):
        ns = namespaces(request, deps)[0]
        return to_ui(await create_item(deps, PLURAL, KIND, body.name, build_spec(body), namespace=ns))

    @router.post("/api/vector-indexes/{name}/reingest")
    @deps.limiter.limit("10/minute")
    async def reingest(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, ns = await find_one(request, deps, PLURAL, name)
        if (obj.get("spec") or {}).get("suspend"):
            raise HTTPException(status_code=409, detail="the index is suspended; resume it first")
        stamp = str(time.time_ns())
        await patch_item(deps, PLURAL, name, {"metadata": {"annotations": {REINGEST_ANNOTATION: stamp}}}, namespace=ns)
        return {"status": "reingesting", "name": name, "reingest": stamp}

    async def _set_suspend(request: Request, name: str, suspend: bool) -> Dict[str, Any]:
        _obj, ns = await find_one(request, deps, PLURAL, name)
        await patch_item(deps, PLURAL, name, {"spec": {"suspend": suspend}}, namespace=ns)
        return {"status": "suspended" if suspend else "resumed", "name": name}

    @router.post("/api/vector-indexes/{name}/suspend")
    @deps.limiter.limit("10/minute")
    async def suspend_index(request: Request, name: str, _=Depends(deps.verify_auth)):
        return await _set_suspend(request, name, True)

    @router.post("/api/vector-indexes/{name}/resume")
    @deps.limiter.limit("10/minute")
    async def resume_index(request: Request, name: str, _=Depends(deps.verify_auth)):
        return await _set_suspend(request, name, False)

    @router.delete("/api/vector-indexes/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_index(request: Request, name: str, _=Depends(deps.verify_auth)):
        _obj, ns = await find_one(request, deps, PLURAL, name)
        await delete_item(deps, PLURAL, name, namespace=ns)
        return {"status": "deleted", "name": name}

    return router
