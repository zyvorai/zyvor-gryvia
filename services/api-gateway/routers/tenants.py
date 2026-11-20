# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Tenant routes (GryviaTenant, cluster-scoped). Admin: full CRUD-lite; a tenant user sees only its own tenant."""
from typing import Any, Dict, List, Optional

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, ConfigDict, Field

from . import tenancy
from .common import Deps, create_item, delete_item, require_admin
from .tenancy import PLURAL, is_admin, namespace_of
from .uiutil import NAME_MAX, NAME_PATTERN, meta, prune

KIND = "GryviaTenant"
# The controller derives namespace "tenant-<name>" (63 chars max), so leave room for the prefix.
TENANT_NAME_MAX = NAME_MAX - len("tenant-")


class CreateTenant(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str = Field(min_length=1, max_length=TENANT_NAME_MAX, pattern=NAME_PATTERN)
    displayName: str = Field(default="", max_length=128)
    allowedSkus: List[str] = Field(default_factory=list, max_length=64)
    maxGPUs: Optional[int] = Field(default=None, ge=0, le=100_000)
    isolated: bool = True


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    name = (obj.get("metadata") or {}).get("name", "")
    s, st = obj.get("spec") or {}, obj.get("status") or {}
    quotas = s.get("quotas") or {}
    return {
        "metadata": meta(obj),
        "spec": prune({
            "displayName": s.get("displayName"),
            "allowedSkus": s.get("allowedSkus") or [],
            "namespace": namespace_of(name),
            "quotas": quotas or None,
            "networkPolicy": s.get("networkPolicy"),
        }),
        "status": prune({
            "health": st.get("health"),
            "memberCount": st.get("memberCount"),
            "namespacesManaged": st.get("namespacesManaged"),
            "currentUsage": st.get("currentUsage"),
        }),
    }


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    async def _visible(request: Request) -> List[Dict[str, Any]]:
        items = await tenancy.list_tenants(deps.k8s_custom, force=True)
        if not is_admin(request):
            mine = set(tenancy.caller_tenant_names(request))
            items = [t for t in items if tenancy.tenant_name(t) in mine]
        return sorted(items, key=tenancy.tenant_name)

    @router.get("/api/tenants")
    @deps.limiter.limit("30/minute")
    async def list_tenants(request: Request, _=Depends(deps.verify_auth)):
        return {"items": [to_ui(o) for o in await _visible(request)]}

    @router.get("/api/tenants/{name}")
    @deps.limiter.limit("30/minute")
    async def get_tenant(request: Request, name: str, _=Depends(deps.verify_auth)):
        for o in await _visible(request):
            if tenancy.tenant_name(o) == name:
                return to_ui(o)
        raise HTTPException(status_code=404, detail=f"{PLURAL}/{name}: Not Found")

    @router.post("/api/tenants", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_tenant(request: Request, body: CreateTenant, _=Depends(deps.verify_auth),
                            __=Depends(require_admin)):
        spec: Dict[str, Any] = {"networkPolicy": {"isolated": body.isolated}}
        if body.displayName:
            spec["displayName"] = body.displayName
        if body.allowedSkus:
            spec["allowedSkus"] = list(body.allowedSkus)
        if body.maxGPUs is not None:
            spec["quotas"] = {"concurrentGPUs": body.maxGPUs}
        created = await create_item(deps, PLURAL, KIND, body.name, spec)
        tenancy.clear_cache()
        return to_ui(created)

    @router.delete("/api/tenants/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_tenant(request: Request, name: str, _=Depends(deps.verify_auth),
                            __=Depends(require_admin)):
        await delete_item(deps, PLURAL, name)
        tenancy.clear_cache()
        return {"status": "deleted", "name": name}

    return router
