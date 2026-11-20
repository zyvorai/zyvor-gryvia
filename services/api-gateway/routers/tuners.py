# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Auto-tuner routes (GryviaAutoTuner, namespaced) for the dashboard."""
import json
import math
import re
from typing import Any, Dict, List, Literal, Optional, Union

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

from .common import Deps, create_item, delete_item
from .uiutil import NAME_MAX, NAME_PATTERN, JobTemplate, find_one, fmt_duration, list_all, meta, namespaces, prune

PLURAL = "gryviaautotuners"
KIND = "GryviaAutoTuner"

ALGORITHMS = {"grid", "random", "bayesian", "asha"}
_PARAM_NAME = r"^[A-Za-z_][A-Za-z0-9_.-]{0,62}$"
_MAX_PARAMS = 32


class AshaConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")
    maxEpochs: int = Field(ge=1, le=100000)
    reductionFactor: int = Field(default=0, ge=0, le=100)
    minResource: int = Field(default=0, ge=0, le=100000)


class CreateTuner(BaseModel):
    """UI body plus optional fields the CRD requires but the current form lacks.

    ``jobTemplate`` is required by the CRD (image/gpus/type); it has no sensible default,
    so a create without it is rejected with 422.
    """
    model_config = ConfigDict(extra="forbid")

    name: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)
    algorithm: str = Field(min_length=1, max_length=32)
    objectiveMetric: str = Field(min_length=1, max_length=128, pattern=r"^[A-Za-z0-9_./-]+$")
    maxTrials: int = Field(ge=1, le=10000)
    parameterSpace: Union[str, Dict[str, Any]]
    direction: Literal["minimize", "maximize"] = "maximize"
    parallelism: int = Field(default=0, ge=0, le=256)
    jobTemplate: Optional[JobTemplate] = None
    ashaConfig: Optional[AshaConfig] = None

    @field_validator("algorithm")
    @classmethod
    def _algo(cls, v: str) -> str:
        v = v.lower()
        if v not in ALGORITHMS:
            raise ValueError("algorithm must be one of: Grid, Random, Bayesian, ASHA")
        return v

    @model_validator(mode="after")
    def _check(self):
        if self.jobTemplate is None:
            raise ValueError("jobTemplate (image, gpus) is required: the tuner needs a job to run per trial")
        if self.algorithm == "asha" and self.ashaConfig is None:
            raise ValueError("ashaConfig.maxEpochs is required for the ASHA algorithm")
        return self


def parse_parameter_space(raw: Union[str, Dict[str, Any]]) -> List[Dict[str, Any]]:
    """UI parameterSpace ({name: {type,min,max,values}}) -> CRD ParameterSpec list."""
    if isinstance(raw, str):
        if len(raw) > 16384:
            raise ValueError("parameterSpace is too large")
        try:
            raw = json.loads(raw)
        except ValueError as exc:
            raise ValueError("parameterSpace must be valid JSON") from exc
    if not isinstance(raw, dict) or not raw:
        raise ValueError("parameterSpace must be a non-empty JSON object")
    if len(raw) > _MAX_PARAMS:
        raise ValueError(f"parameterSpace supports at most {_MAX_PARAMS} parameters")
    out: List[Dict[str, Any]] = []
    for name, p in raw.items():
        if not re.match(_PARAM_NAME, str(name)):
            raise ValueError(f"invalid parameter name '{str(name)[:64]}'")
        if not isinstance(p, dict):
            raise ValueError(f"parameter '{name}' must be an object")
        unknown = set(p) - {"type", "min", "max", "values", "scale", "step"}
        if unknown:
            raise ValueError(f"parameter '{name}' has unknown fields")
        ptype = {"choice": "categorical"}.get(p.get("type"), p.get("type"))
        if ptype in ("float", "int"):
            lo, hi = p.get("min"), p.get("max")
            for v in (lo, hi):
                if isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v):
                    raise ValueError(f"parameter '{name}' needs numeric min and max")
            if lo >= hi:
                raise ValueError(f"parameter '{name}': min must be < max")
            spec: Dict[str, Any] = {"name": name, "type": ptype, "min": float(lo), "max": float(hi)}
            if p.get("scale") is not None:
                if p["scale"] not in ("linear", "log"):
                    raise ValueError(f"parameter '{name}': scale must be linear or log")
                spec["scale"] = p["scale"]
            if p.get("step") is not None:
                step = p["step"]
                if isinstance(step, bool) or not isinstance(step, int) or step < 1:
                    raise ValueError(f"parameter '{name}': step must be a positive integer")
                spec["step"] = step
        elif ptype == "categorical":
            vals = p.get("values")
            if not isinstance(vals, list) or not (1 <= len(vals) <= 64):
                raise ValueError(f"parameter '{name}' needs 1-64 values")
            if any(isinstance(v, (dict, list)) or v is None for v in vals):
                raise ValueError(f"parameter '{name}' values must be scalars")
            strs = [str(v).lower() if isinstance(v, bool) else str(v) for v in vals]
            if any(len(v) > 128 for v in strs):
                raise ValueError(f"parameter '{name}' values are too long")
            spec = {"name": name, "type": "categorical", "values": strs}
        else:
            raise ValueError(f"parameter '{name}': type must be float, int or choice")
        out.append(spec)
    return out


def _param_space_ui(params: List[Dict[str, Any]]) -> Dict[str, Any]:
    out: Dict[str, Any] = {}
    for p in params:
        d = {k: v for k, v in p.items() if k != "name" and v is not None}
        if d.get("type") == "categorical":
            d["type"] = "choice"
        out[p.get("name", "")] = d
    return out


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    best = st.get("bestTrial") or {}
    objective = spec.get("objective") or {}
    direction = objective.get("direction")
    return {
        "metadata": meta(obj),
        "spec": prune({
            "algorithm": spec.get("searchAlgorithm"),
            "objectiveMetric": objective.get("metricName"),
            "metricName": objective.get("metricName"),
            "direction": direction if direction in ("maximize", "minimize") else "maximize",
            "maxTrials": spec.get("maxTrials"),
            "parameterSpace": _param_space_ui(spec.get("parameterSpace") or []),
        }),
        "status": prune({
            "phase": st.get("phase"),
            "trialsCompleted": st.get("trialsCompleted"),
            "trialsRunning": st.get("trialsRunning"),
            "bestMetricValue": best.get("metricValue"),
            "bestTrialId": best.get("name"),
        }),
    }


def trial_ui(t: Dict[str, Any]) -> Dict[str, Any]:
    return prune({
        "trialId": t.get("name"),
        "parameters": t.get("parameters") or {},
        "metricValue": t.get("metricValue"),
        "status": t.get("phase"),
        "duration": fmt_duration(t.get("startTime"), t.get("completionTime")),
    })


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/tuners")
    @deps.limiter.limit("30/minute")
    async def list_tuners(request: Request, _=Depends(deps.verify_auth)):
        return {"items": [to_ui(o) for o in await list_all(request, deps, PLURAL)]}

    @router.get("/api/tuners/{name}")
    @deps.limiter.limit("30/minute")
    async def get_tuner(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, _ns = await find_one(request, deps, PLURAL, name)
        return to_ui(obj)

    @router.get("/api/tuners/{name}/trials")
    @deps.limiter.limit("30/minute")
    async def get_trials(request: Request, name: str, _=Depends(deps.verify_auth)):
        obj, _ns = await find_one(request, deps, PLURAL, name)
        return {"items": [trial_ui(t) for t in (obj.get("status") or {}).get("trials") or []]}

    @router.post("/api/tuners", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_tuner(request: Request, body: CreateTuner, _=Depends(deps.verify_auth)):
        try:
            params = parse_parameter_space(body.parameterSpace)
        except ValueError as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc
        assert body.jobTemplate is not None
        spec: Dict[str, Any] = {
            "searchAlgorithm": body.algorithm,
            "parameterSpace": params,
            "objective": {"metricName": body.objectiveMetric, "direction": body.direction},
            "maxTrials": body.maxTrials,
            "jobTemplate": body.jobTemplate.to_spec(),
        }
        if body.parallelism:
            spec["parallelism"] = body.parallelism
        if body.ashaConfig:
            spec["ashaConfig"] = {k: v for k, v in body.ashaConfig.model_dump().items() if v}
        ns = namespaces(request, deps)[0]
        return to_ui(await create_item(deps, PLURAL, KIND, body.name, spec, namespace=ns))

    @router.delete("/api/tuners/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_tuner(request: Request, name: str, _=Depends(deps.verify_auth)):
        _obj, ns = await find_one(request, deps, PLURAL, name)
        await delete_item(deps, PLURAL, name, namespace=ns)
        return {"status": "deleted", "name": name}

    return router
