# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""GPU reservations (GryviaReservation, cluster-scoped).

Admins see and manage every reservation. A tenant user sees only reservations owned by one of its tenants
(``spec.owner`` name equals the tenant, or its ``tenant-<name>`` namespace), may create reservations only for
its own tenant (the owner is forced), and may delete only its own. Deleting the object is the cancel
operation: the quota operator's finalizer removes the node taints and labels.
"""
import secrets
from typing import Any, Dict, List, Literal, Optional

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, ConfigDict, Field

from .common import Deps, create_item, delete_item, get_item, list_items
from .tenancy import caller_tenant_names, is_admin, namespace_of
from .uiutil import NAME_MAX, NAME_PATTERN, parse_ts

PLURAL = "gryviareservations"
KIND = "GryviaReservation"


class Owner(BaseModel):
    model_config = ConfigDict(extra="forbid")

    type: Literal["user", "team", "tenant", "project", "namespace"] = "tenant"
    name: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)


class CreateReservation(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: Optional[str] = Field(default=None, min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)
    owner: Optional[Owner] = None
    gpuType: str = Field(min_length=1, max_length=64)
    gpuCount: int = Field(ge=1, le=100_000)
    scheduleType: Literal["immediate", "scheduled", "recurring"] = "immediate"
    startTime: Optional[str] = Field(default=None, max_length=40)
    endTime: Optional[str] = Field(default=None, max_length=40)
    cron: Optional[str] = Field(default=None, max_length=100)
    duration: Optional[str] = Field(default=None, max_length=20)
    exclusive: bool = True


def owned_by(obj: Dict[str, Any], tenants: List[str]) -> bool:
    name = ((obj.get("spec") or {}).get("owner") or {}).get("name", "")
    return bool(name) and any(name in (t, namespace_of(t)) for t in tenants)


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    s, st = obj.get("spec") or {}, obj.get("status") or {}
    res, sch = s.get("resources") or {}, s.get("schedule") or {}
    rec = sch.get("recurrence")
    owner = s.get("owner") or {}
    return {
        "name": (obj.get("metadata") or {}).get("name", ""),
        "owner": {"type": owner.get("type", ""), "name": owner.get("name", "")},
        "gpuType": res.get("gpuType", ""),
        "gpuCount": res.get("gpuCount", 0),
        "schedule": {
            "type": sch.get("type", ""), "startTime": sch.get("startTime"), "endTime": sch.get("endTime"),
            "recurrence": ({"cron": rec.get("cron", ""), "duration": rec.get("duration", "")} if rec else None),
        },
        "exclusive": bool((s.get("guarantees") or {}).get("exclusive", False)),
        "state": st.get("state") or "pending",
        "allocatedNodes": list(st.get("allocatedNodes") or []),
        "allocatedGPUs": st.get("allocatedGPUs", 0),
        "actualStartTime": st.get("actualStartTime"),
        "actualEndTime": st.get("actualEndTime"),
    }


def _ts(value: Optional[str], name: str):
    if value is None:
        return None
    dt = parse_ts(value)
    if dt is None:
        raise HTTPException(status_code=400, detail=f"{name} must be an RFC3339 timestamp")
    return dt


def build_spec(body: CreateReservation, owner: Dict[str, str]) -> Dict[str, Any]:
    sch: Dict[str, Any] = {"type": body.scheduleType}
    start, end = _ts(body.startTime, "startTime"), _ts(body.endTime, "endTime")
    if body.scheduleType == "recurring":
        if not body.cron or not body.duration:
            raise HTTPException(status_code=400, detail="recurring reservations need cron and duration")
        sch["recurrence"] = {"cron": body.cron, "duration": body.duration}
    else:
        if end is None:
            raise HTTPException(status_code=400, detail="endTime is required")
        if body.scheduleType == "scheduled" and start is None:
            raise HTTPException(status_code=400, detail="startTime is required for scheduled reservations")
        if start is not None and end <= start:
            raise HTTPException(status_code=400, detail="endTime must be after startTime")
    if body.startTime:
        sch["startTime"] = body.startTime
    if body.endTime:
        sch["endTime"] = body.endTime
    return {"owner": owner, "resources": {"gpuType": body.gpuType, "gpuCount": body.gpuCount},
            "schedule": sch, "guarantees": {"exclusive": body.exclusive}}


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    def _tenant_or_403(request: Request) -> List[str]:
        tenants = caller_tenant_names(request)
        if not tenants:
            raise HTTPException(status_code=403, detail="No tenant is associated with this caller")
        return tenants

    @router.get("/api/reservations")
    @deps.limiter.limit("30/minute")
    async def list_reservations(request: Request, _=Depends(deps.verify_auth)):
        items = await list_items(deps, PLURAL)
        if not is_admin(request):
            mine = caller_tenant_names(request)
            items = [o for o in items if owned_by(o, mine)]
        items.sort(key=lambda o: (o.get("metadata") or {}).get("name", ""))
        return {"items": [to_ui(o) for o in items]}

    @router.post("/api/reservations", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_reservation(request: Request, body: CreateReservation, _=Depends(deps.verify_auth)):
        if is_admin(request):
            if body.owner is None:
                raise HTTPException(status_code=400, detail="owner is required")
            owner = {"type": body.owner.type, "name": body.owner.name}
        else:
            tenants = _tenant_or_403(request)
            wanted = body.owner.name if body.owner else tenants[0]
            if wanted not in tenants:
                raise HTTPException(status_code=403, detail="Reservations can only be made for your own tenant")
            owner = {"type": "tenant", "name": wanted}  # forced: never trust a caller-chosen owner type
        spec = build_spec(body, owner)
        name = body.name or f"res-{secrets.token_hex(4)}"
        return to_ui(await create_item(deps, PLURAL, KIND, name, spec))

    @router.delete("/api/reservations/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_reservation(request: Request, name: str, _=Depends(deps.verify_auth)):
        if not is_admin(request):
            obj = await get_item(deps, PLURAL, name)
            if not owned_by(obj, caller_tenant_names(request)):
                raise HTTPException(status_code=404, detail=f"{PLURAL}/{name}: Not Found")
        await delete_item(deps, PLURAL, name)
        return {"status": "deleted", "name": name}

    return router
