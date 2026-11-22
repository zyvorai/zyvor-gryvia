# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Model registry routes (GryviaModelRegistry, namespaced) for the dashboard."""
import json
from datetime import datetime, timezone
from typing import Any, Dict, List, Literal, Optional

from fastapi import APIRouter, Depends, HTTPException, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel, ConfigDict, Field, model_validator

from .common import Deps, create_item, patch_item, require_admin
from .uiutil import NAME_MAX, NAME_PATTERN, find_one, list_all, meta, namespaces, prune

PLURAL = "gryviamodelregistries"
KIND = "GryviaModelRegistry"
ROLLBACK_ANNOTATION = "gryvia.io/rollback-requested"
# A tenant's request to promote to production, awaiting an administrator (GRYVIA_REQUIRE_PROD_APPROVAL=1):
# JSON {"target", "by", "at"}. Removed on approval or rejection.
PROMOTION_ANNOTATION = "gryvia.io/promotion-request"

# Same promotion ladder the UI offers.
NEXT_STAGE = {"dev": ["staging"], "staging": ["production"], "production": ["archived"]}


class PromoteRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    targetStage: Literal["dev", "staging", "production", "archived"]


class Artifacts(BaseModel):
    model_config = ConfigDict(extra="forbid")

    s3Path: str = Field(default="", max_length=1024, pattern=r"^([a-z0-9]+://[^\s]+)?$")
    pvcName: str = Field(default="", max_length=NAME_MAX, pattern=r"^(" + NAME_PATTERN[1:-1] + r")?$")
    subPath: str = Field(default="", max_length=512, pattern=r"^([A-Za-z0-9._/-]*)?$")
    format: str = Field(default="", max_length=64, pattern=r"^([A-Za-z0-9._-]*)?$")

    @model_validator(mode="after")
    def _check(self):
        if not (self.s3Path or self.pvcName):
            raise ValueError("artifacts needs an s3Path or a pvcName")
        if ".." in self.subPath.split("/"):
            raise ValueError("artifacts.subPath must not contain '..'")
        return self


class ServingConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")

    backend: Literal["triton", "vllm", "tensorrt-llm", "torchserve"] = "vllm"
    replicas: int = Field(default=1, ge=1, le=100)
    gpuCount: int = Field(default=0, ge=0, le=64)
    gpuType: str = Field(default="", max_length=63, pattern=r"^([A-Za-z0-9][A-Za-z0-9._-]*)?$")


class CreateModel(BaseModel):
    """Register a model version. ``modelName`` defaults to ``name``.

    The registry entry is metadata; only ``autoServe`` on a ``production`` entry makes the operator create a
    serving GryviaInferenceService for it.
    """
    model_config = ConfigDict(extra="forbid")

    name: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)
    modelName: Optional[str] = Field(default=None, min_length=1, max_length=128,
                                     pattern=r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
    version: str = Field(min_length=1, max_length=63, pattern=r"^[A-Za-z0-9][A-Za-z0-9._+-]*$")
    stage: Literal["dev", "staging", "production"] = "dev"
    description: str = Field(default="", max_length=1024)
    sourceJob: str = Field(default="", max_length=NAME_MAX, pattern=r"^(" + NAME_PATTERN[1:-1] + r")?$")
    artifacts: Artifacts
    autoServe: bool = False
    servingConfig: Optional[ServingConfig] = None


def _artifacts(art: Dict[str, Any]) -> List[str]:
    out: List[str] = []
    if art.get("s3Path"):
        out.append(art["s3Path"])
    if art.get("pvcName"):
        out.append(f"pvc://{art['pvcName']}/{art.get('subPath', '').lstrip('/')}".rstrip("/"))
    return out


def pending_request(obj: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    raw = ((obj.get("metadata") or {}).get("annotations") or {}).get(PROMOTION_ANNOTATION)
    if not raw:
        return None
    try:
        req = json.loads(raw)
    except ValueError:
        return None
    return req if isinstance(req, dict) and req.get("target") else None


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    src = spec.get("source") or {}
    return {
        "metadata": meta(obj),
        "pendingPromotion": pending_request(obj),
        "spec": prune({
            "version": spec.get("version"),
            "stage": spec.get("stage"),
            "sourceJob": src.get("jobRef") or None,
            "artifacts": _artifacts(spec.get("artifacts") or {}),
        }),
        "status": prune({
            "servingEndpoint": st.get("servingEndpoint") or None,
            "phase": st.get("phase") or None,
            "previousVersion": st.get("previousVersion") or None,
            "message": st.get("message") or None,
        }),
    }


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/models")
    @deps.limiter.limit("30/minute")
    async def list_models(request: Request, _=Depends(deps.verify_auth)):
        return {"items": [to_ui(o) for o in await list_all(request, deps, PLURAL)]}

    @router.get("/api/models/{name}")
    @deps.limiter.limit("30/minute")
    async def get_model(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, _ns = await find_one(request, deps, PLURAL, name)
        return to_ui(obj)

    @router.post("/api/models", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_model(request: Request, body: CreateModel, _=Depends(deps.verify_auth)):
        arts = {k: v for k, v in body.artifacts.model_dump().items() if v}
        spec: Dict[str, Any] = {
            "modelName": body.modelName or body.name,
            "version": body.version,
            "stage": body.stage,
            "artifacts": arts,
        }
        if body.description:
            spec["description"] = body.description
        if body.sourceJob:
            spec["source"] = {"jobRef": body.sourceJob}
        if body.autoServe:
            spec["autoServe"] = True
        if body.servingConfig:
            sc = {k: v for k, v in body.servingConfig.model_dump().items() if v not in ("", 0)}
            sc["backend"] = body.servingConfig.backend
            spec["servingConfig"] = sc
        ns = namespaces(request, deps)[0]
        return to_ui(await create_item(deps, PLURAL, KIND, body.name, spec, namespace=ns))

    def check_transition(obj: Dict[str, Any], target: str) -> None:
        current = (obj.get("spec") or {}).get("stage") or "dev"
        if target not in NEXT_STAGE.get(current, []):
            raise HTTPException(status_code=409, detail=f"Cannot promote model from '{current}' to '{target}'")

    @router.post("/api/models/{name}/promote")
    @deps.limiter.limit("10/minute")
    async def promote_model(request: Request, name: str, body: PromoteRequest, _=Depends(deps.verify_auth)):
        obj, ns = await find_one(request, deps, PLURAL, name)
        check_transition(obj, body.targetStage)
        if (deps.require_prod_approval and body.targetStage == "production"
                and getattr(request.state, "role", None) != "admin"):
            # Not applied: record the request for an administrator (POST .../approve or .../reject).
            when = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
            by = getattr(request.state, "tenant", None) or "unknown"
            req = json.dumps({"target": "production", "by": by, "at": when}, separators=(",", ":"))
            patch = {"metadata": {"annotations": {PROMOTION_ANNOTATION: req}}}
            return JSONResponse(to_ui(await patch_item(deps, PLURAL, name, patch, namespace=ns)), status_code=202)
        patch: Dict[str, Any] = {"spec": {"stage": body.targetStage}}
        if pending_request(obj):  # an administrator promoting directly settles any open request
            patch["metadata"] = {"annotations": {PROMOTION_ANNOTATION: None}}
        return to_ui(await patch_item(deps, PLURAL, name, patch, namespace=ns))

    @router.post("/api/models/{name}/approve")
    @deps.limiter.limit("10/minute")
    async def approve_promotion(request: Request, name: str, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        obj, ns = await find_one(request, deps, PLURAL, name)
        req = pending_request(obj)
        if not req:
            raise HTTPException(status_code=409, detail=f"Model '{name}' has no pending promotion request")
        check_transition(obj, req["target"])  # the entry may have moved since the request
        patch = {"spec": {"stage": req["target"]}, "metadata": {"annotations": {PROMOTION_ANNOTATION: None}}}
        return to_ui(await patch_item(deps, PLURAL, name, patch, namespace=ns))

    @router.post("/api/models/{name}/reject")
    @deps.limiter.limit("10/minute")
    async def reject_promotion(request: Request, name: str, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        obj, ns = await find_one(request, deps, PLURAL, name)
        if not pending_request(obj):
            raise HTTPException(status_code=409, detail=f"Model '{name}' has no pending promotion request")
        patch = {"metadata": {"annotations": {PROMOTION_ANNOTATION: None}}}
        return to_ui(await patch_item(deps, PLURAL, name, patch, namespace=ns))

    @router.post("/api/models/{name}/rollback", status_code=202)
    @deps.limiter.limit("10/minute")
    async def rollback_model(request: Request, name: str, _=Depends(deps.verify_auth)):
        """Ask the operator to put status.previousVersion back on the entry's shared service and archive this
        entry. The operator acts on the annotation (see the RolledBack condition)."""
        obj, ns = await find_one(request, deps, PLURAL, name)
        spec, st = obj.get("spec") or {}, obj.get("status") or {}
        if spec.get("stage") != "production" or not (spec.get("servingConfig") or {}).get("serviceName"):
            raise HTTPException(status_code=409,
                                detail="Only a production entry with servingConfig.serviceName can be rolled back")
        if not st.get("previousVersion"):
            raise HTTPException(status_code=409, detail=f"Model '{name}' has no previous version to roll back to")
        when = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        patch = {"metadata": {"annotations": {ROLLBACK_ANNOTATION: f"api at {when}"}}}
        return to_ui(await patch_item(deps, PLURAL, name, patch, namespace=ns))

    return router
