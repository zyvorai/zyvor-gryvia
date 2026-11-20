# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""GPU SKU catalog (GryviaGpuSku, cluster-scoped price sheet).

Any authenticated user can read the catalog; a tenant sees only enabled SKUs and, when its
GryviaTenant sets ``spec.allowedSkus``, only those (matched by SKU name or gpuType). Writes are admin only.
"""
from typing import Any, Dict, List, Optional

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, ConfigDict, Field

from .common import Deps, create_item, delete_item, get_item, list_items, patch_item, require_admin
from .pricing import SKU_PLURAL
from .tenancy import caller_tenants, is_admin
from .uiutil import NAME_MAX, NAME_PATTERN, meta, prune

KIND = "GryviaGpuSku"
GPU_TYPE = r"^[A-Za-z0-9][A-Za-z0-9._-]*$"


class SkuFields(BaseModel):
    model_config = ConfigDict(extra="forbid")

    gpuType: str = Field(min_length=1, max_length=63, pattern=GPU_TYPE)
    gpusPerUnit: int = Field(default=1, ge=1, le=1024)
    hourlyRate: float = Field(ge=0, le=1_000_000)  # per GPU-hour
    currency: str = Field(default="USD", pattern=r"^[A-Z]{3}$")
    spotDiscount: int = Field(default=0, ge=0, le=100)
    description: str = Field(default="", max_length=512)
    enabled: bool = True

    def to_spec(self) -> Dict[str, Any]:
        return self.model_dump()


class CreateSku(SkuFields):
    name: str = Field(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)


def to_ui(obj: Dict[str, Any]) -> Dict[str, Any]:
    s = obj.get("spec") or {}
    return {
        "metadata": meta(obj),
        "spec": prune({
            "gpuType": s.get("gpuType"),
            "gpusPerUnit": s.get("gpusPerUnit", 1),
            "hourlyRate": s.get("hourlyRate", 0),
            "currency": s.get("currency") or "USD",
            "spotDiscount": s.get("spotDiscount", 0),
            "description": s.get("description", ""),
            "enabled": s.get("enabled", True) is not False,
        }),
    }


def allowed_for(tenants: List[Dict[str, Any]]) -> Optional[set]:
    """Union of allowedSkus over the caller's tenants; None (no restriction) if any tenant lists none."""
    allowed: set = set()
    for t in tenants:
        lst = (t.get("spec") or {}).get("allowedSkus") or []
        if not lst:
            return None
        allowed.update(x for x in lst if isinstance(x, str))
    return allowed


def visible(sku: Dict[str, Any], allowed: Optional[set]) -> bool:
    spec = sku.get("spec") or {}
    if spec.get("enabled", True) is False:
        return False
    if allowed is None:
        return True
    return (sku.get("metadata") or {}).get("name") in allowed or spec.get("gpuType") in allowed


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    async def _visible_filter(request: Request):
        """None for admins (see everything), else a predicate for the tenant's view."""
        if is_admin(request):
            return None
        allowed = allowed_for(await caller_tenants(request, deps.k8s_custom))
        return lambda sku: visible(sku, allowed)

    @router.get("/api/skus")
    @deps.limiter.limit("30/minute")
    async def list_skus(request: Request, _=Depends(deps.verify_auth)):
        pred = await _visible_filter(request)
        items = await list_items(deps, SKU_PLURAL)
        if pred:
            items = [o for o in items if pred(o)]
        items.sort(key=lambda o: (o.get("metadata") or {}).get("name", ""))
        return {"items": [to_ui(o) for o in items]}

    @router.get("/api/skus/{name}")
    @deps.limiter.limit("30/minute")
    async def get_sku(request: Request, name: str, _=Depends(deps.verify_auth)):
        pred = await _visible_filter(request)
        obj = await get_item(deps, SKU_PLURAL, name)
        if pred and not pred(obj):
            raise HTTPException(status_code=404, detail=f"{SKU_PLURAL}/{name}: Not Found")
        return to_ui(obj)

    @router.post("/api/skus", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_sku(request: Request, body: CreateSku, _=Depends(deps.verify_auth),
                         __=Depends(require_admin)):
        spec = body.to_spec()
        spec.pop("name", None)
        return to_ui(await create_item(deps, SKU_PLURAL, KIND, body.name, spec))

    @router.put("/api/skus/{name}")
    @deps.limiter.limit("10/minute")
    async def update_sku(request: Request, body: SkuFields, name: str, _=Depends(deps.verify_auth),
                         __=Depends(require_admin)):
        await get_item(deps, SKU_PLURAL, name)  # 404 when missing
        return to_ui(await patch_item(deps, SKU_PLURAL, name, {"spec": body.to_spec()}))

    @router.delete("/api/skus/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_sku(request: Request, name: str, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        await delete_item(deps, SKU_PLURAL, name)
        return {"status": "deleted", "name": name}

    return router
