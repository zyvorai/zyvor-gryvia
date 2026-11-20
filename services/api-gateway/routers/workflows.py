# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Workflow routes (GryviaWorkflow, namespaced) for the dashboard."""
from typing import Any, Dict, List, Literal, Optional

from fastapi import APIRouter, Depends, Request
from pydantic import BaseModel, ConfigDict, Field, model_validator

from .common import Deps, create_item, delete_item
from .uiutil import NAME_MAX, NAME_PATTERN, JobTemplate, find_one, fmt_duration, list_all, meta, namespaces, prune

PLURAL = "gryviaworkflows"
KIND = "GryviaWorkflow"

_STEP_NAME = dict(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)


class ScriptStep(BaseModel):
    model_config = ConfigDict(extra="forbid")
    image: str = Field(min_length=1, max_length=512, pattern=r"^[^\s]+$")
    command: List[str] = Field(min_length=1, max_length=32)
    args: List[str] = Field(default_factory=list, max_length=64)


class WebhookStep(BaseModel):
    model_config = ConfigDict(extra="forbid")
    url: str = Field(min_length=1, max_length=2048, pattern=r"^https?://[^\s]+$")
    method: Literal["GET", "POST", "PUT", "PATCH", "DELETE"] = "POST"
    body: str = Field(default="", max_length=16384)


class Step(BaseModel):
    model_config = ConfigDict(extra="forbid")
    name: str = Field(**_STEP_NAME)
    type: Literal["job", "script", "webhook"] = "job"
    dependsOn: List[str] = Field(default_factory=list, max_length=64)
    jobTemplate: Optional[JobTemplate] = None
    script: Optional[ScriptStep] = None
    webhook: Optional[WebhookStep] = None
    retries: int = Field(default=0, ge=0, le=10)
    timeoutSeconds: int = Field(default=0, ge=0, le=604800)

    @model_validator(mode="after")
    def _payload(self):
        need = {"job": self.jobTemplate, "script": self.script, "webhook": self.webhook}
        if need[self.type] is None:
            key = {"job": "jobTemplate", "script": "script", "webhook": "webhook"}[self.type]
            raise ValueError(f"step '{self.name}' of type {self.type} requires '{key}'")
        return self


class WorkflowMeta(BaseModel):
    model_config = ConfigDict(extra="forbid")
    name: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)


class WorkflowSpec(BaseModel):
    model_config = ConfigDict(extra="forbid")
    steps: List[Step] = Field(min_length=1, max_length=100)
    parameters: Dict[str, str] = Field(default_factory=dict, max_length=64)

    @model_validator(mode="after")
    def _dag(self):
        names = [s.name for s in self.steps]
        if len(set(names)) != len(names):
            raise ValueError("step names must be unique")
        deps = {s.name: s.dependsOn for s in self.steps}
        for n, ds in deps.items():
            for d in ds:
                if d not in deps:
                    raise ValueError(f"step '{n}' depends on unknown step '{d}'")
                if d == n:
                    raise ValueError(f"step '{n}' depends on itself")
        # cycle check (Kahn)
        remaining = {n: set(ds) for n, ds in deps.items()}
        while remaining:
            ready = [n for n, ds in remaining.items() if not ds]
            if not ready:
                raise ValueError("step dependencies contain a cycle")
            for n in ready:
                del remaining[n]
            for ds in remaining.values():
                ds.difference_update(ready)
        return self


class CreateWorkflow(BaseModel):
    model_config = ConfigDict(extra="forbid")
    metadata: WorkflowMeta
    spec: WorkflowSpec


def _spec_step(step: Dict[str, Any]) -> Dict[str, Any]:
    return prune({"name": step.get("name"), "type": step.get("type"),
                  "dependsOn": step.get("dependsOn") or None})


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    spec_steps = {s.get("name"): s for s in spec.get("steps") or []}
    out_spec: Dict[str, Any] = {"steps": [_spec_step(s) for s in spec.get("steps") or []]}
    out_status = prune({
        "phase": st.get("phase"),
        "duration": fmt_duration(st.get("startTime"), st.get("completionTime")),
        "startedAt": st.get("startTime"),
    })
    if st.get("stepStatuses"):
        steps: List[Dict[str, Any]] = []
        for ss in st["stepStatuses"]:
            base = spec_steps.get(ss.get("name"), {})
            steps.append(prune({
                "name": ss.get("name"),
                "type": base.get("type"),
                "status": ss.get("phase"),
                "duration": fmt_duration(ss.get("startTime"), ss.get("completionTime")),
                "jobRef": ss.get("jobName") or None,
                "dependsOn": base.get("dependsOn") or None,
            }))
        out_status["steps"] = steps
    return {"metadata": meta(obj), "spec": out_spec, "status": out_status}


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/workflows")
    @deps.limiter.limit("30/minute")
    async def list_workflows(request: Request, _=Depends(deps.verify_auth)):
        return {"items": [to_ui(o) for o in await list_all(request, deps, PLURAL)]}

    @router.get("/api/workflows/{name}")
    @deps.limiter.limit("30/minute")
    async def get_workflow(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, _ns = await find_one(request, deps, PLURAL, name)
        return to_ui(obj)

    @router.post("/api/workflows", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_workflow(request: Request, body: CreateWorkflow, _=Depends(deps.verify_auth)):
        steps: List[Dict[str, Any]] = []
        for s in body.spec.steps:
            d: Dict[str, Any] = {"name": s.name, "type": s.type}
            if s.dependsOn:
                d["dependsOn"] = s.dependsOn
            if s.jobTemplate:
                d["jobTemplate"] = s.jobTemplate.to_spec()
            if s.script:
                d["script"] = s.script.model_dump(exclude_defaults=False, exclude_none=True)
            if s.webhook:
                d["webhook"] = s.webhook.model_dump(exclude_none=True)
            if s.retries:
                d["retries"] = s.retries
            if s.timeoutSeconds:
                d["timeoutSeconds"] = s.timeoutSeconds
            steps.append(d)
        spec: Dict[str, Any] = {"steps": steps}
        if body.spec.parameters:
            spec["parameters"] = body.spec.parameters
        ns = namespaces(request, deps)[0]
        return to_ui(await create_item(deps, PLURAL, KIND, body.metadata.name, spec, namespace=ns))

    @router.delete("/api/workflows/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_workflow(request: Request, name: str, _=Depends(deps.verify_auth)):
        _obj, ns = await find_one(request, deps, PLURAL, name)
        await delete_item(deps, PLURAL, name, namespace=ns)
        return {"status": "deleted", "name": name}

    return router
