# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Inference service routes (GryviaInferenceService, namespaced) for the dashboard."""
from typing import Any, Dict

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, ConfigDict, Field, model_validator

from .common import Deps, create_item, delete_item, get_item
from .uiutil import NAME_MAX, NAME_PATTERN, find_one, list_all, meta, namespaces, prune

PLURAL = "gryviainferenceservices"
KIND = "GryviaInferenceService"
MODELS_PLURAL = "gryviamodelregistries"

# UI display names -> CRD enum values.
BACKENDS = {
    "triton": "triton",
    "vllm": "vllm",
    "tensorrt-llm": "tensorrt-llm",
    "torchserve": "torchserve",
}


class CreateInference(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)
    modelRef: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)
    backend: str = Field(min_length=1, max_length=32)
    replicas: int = Field(ge=1, le=100)
    minReplicas: int = Field(ge=1, le=100)
    maxReplicas: int = Field(ge=1, le=100)
    targetUtilization: int = Field(ge=1, le=100)

    @model_validator(mode="after")
    def _check(self):
        if self.backend.lower() not in BACKENDS:
            raise ValueError("backend must be one of: Triton, vLLM, TensorRT-LLM, TorchServe")
        if self.minReplicas > self.maxReplicas:
            raise ValueError("minReplicas must be <= maxReplicas")
        return self


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    canary = spec.get("canary") or {}
    auto = spec.get("autoscaling") or {}
    out_spec = prune({
        "modelRef": spec.get("modelRef"),
        "backend": spec.get("backend"),
        "replicas": spec.get("replicas"),
    })
    if canary.get("enabled"):
        out_spec["canary"] = prune({"trafficPercent": canary.get("weight")})
    if auto:
        out_spec["autoscaling"] = prune({
            "minReplicas": auto.get("minReplicas"),
            "maxReplicas": auto.get("maxReplicas"),
            "targetUtilization": auto.get("targetGPUUtilization"),
        })
    return {
        "metadata": meta(obj),
        "spec": out_spec,
        # latencyMs is not exposed by the CRD status, so it is omitted rather than invented.
        "status": prune({
            "phase": st.get("phase"),
            "readyReplicas": st.get("readyReplicas"),
            "endpoint": st.get("endpoint") or None,
        }),
    }


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/inference")
    @deps.limiter.limit("30/minute")
    async def list_inference(request: Request, _=Depends(deps.verify_auth)):
        return {"items": [to_ui(o) for o in await list_all(request, deps, PLURAL)]}

    @router.get("/api/inference/{name}")
    @deps.limiter.limit("30/minute")
    async def get_inference(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, _ns = await find_one(request, deps, PLURAL, name)
        return to_ui(obj)

    @router.post("/api/inference", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_inference(request: Request, body: CreateInference, _=Depends(deps.verify_auth)):
        ns = namespaces(request, deps)[0]
        try:
            await get_item(deps, MODELS_PLURAL, body.modelRef, namespace=ns)
        except HTTPException as exc:
            if exc.status_code == 404:
                raise HTTPException(status_code=422, detail=f"modelRef '{body.modelRef}' does not exist") from exc
            raise
        spec: Dict[str, Any] = {
            "modelRef": body.modelRef,
            "backend": BACKENDS[body.backend.lower()],
            "replicas": body.replicas,
            "autoscaling": {
                "enabled": True,
                "minReplicas": body.minReplicas,
                "maxReplicas": body.maxReplicas,
                "targetGPUUtilization": body.targetUtilization,
            },
        }
        return to_ui(await create_item(deps, PLURAL, KIND, body.name, spec, namespace=ns))

    @router.delete("/api/inference/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_inference(request: Request, name: str, _=Depends(deps.verify_auth)):
        _obj, ns = await find_one(request, deps, PLURAL, name)
        await delete_item(deps, PLURAL, name, namespace=ns)
        return {"status": "deleted", "name": name}

    return router
