# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Security routes backed by GryviaSecurityPolicy CRs."""
import re
from typing import Any, Dict, List, Literal, Optional
from urllib.parse import urlparse

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, Field, field_validator

from .common import Deps, create_item, list_items, require_admin

K8S_NAME = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
POLICIES = "gryviasecuritypolicies"


def _valid_name(v: str) -> str:
    if len(v) > 63 or not K8S_NAME.match(v):
        raise ValueError("must be a valid Kubernetes name (lowercase alphanumerics and '-', max 63)")
    return v


class DetectionRule(BaseModel):
    type: Literal["escape", "mining", "exfiltration", "privesc", "driver_fim"]
    enabled: bool
    sensitivity: Literal["low", "medium", "high"]


class SecurityPolicyRequest(BaseModel):
    name: str = Field(min_length=1, max_length=63)
    targetNamespaces: List[str] = Field(min_length=1, max_length=50)
    detectionRules: List[DetectionRule] = Field(min_length=1, max_length=20)
    autoBlock: bool = False
    alertWebhook: Optional[str] = Field(default=None, max_length=2048)

    @field_validator("name")
    @classmethod
    def _name(cls, v: str) -> str:
        return _valid_name(v)

    @field_validator("targetNamespaces")
    @classmethod
    def _namespaces(cls, v: List[str]) -> List[str]:
        for ns in v:
            _valid_name(ns)
        return v

    @field_validator("alertWebhook")
    @classmethod
    def _webhook(cls, v: Optional[str]) -> Optional[str]:
        if not v:
            return None
        u = urlparse(v)
        if u.scheme not in ("http", "https") or not u.hostname:
            raise ValueError("alertWebhook must be an http(s) URL")
        return v


def _policy_view(obj: Dict[str, Any]) -> Dict[str, Any]:
    m = obj.get("metadata", {})
    meta = {"name": m.get("name", "")}
    for k in ("namespace", "creationTimestamp"):
        if m.get(k):
            meta[k] = m[k]
    s = obj.get("spec") or {}
    out: Dict[str, Any] = {"metadata": meta, "spec": {
        "targetNamespaces": s.get("targetNamespaces") or [],
        "detectionRules": s.get("detectionRules") or [],
        "autoBlock": bool(s.get("autoBlock", False)),
    }}
    if s.get("alertWebhook"):
        out["spec"]["alertWebhook"] = s["alertWebhook"]
    st = obj.get("status") or {}
    if st:
        out["status"] = {k: st[k] for k in ("phase", "activeDetections", "alertsTriggered", "lastAlert",
                                            "detectionCounts") if k in st}
    return out


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/security/alerts")
    @deps.limiter.limit("30/minute")
    async def alerts(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        # GryviaSecurityPolicy status only carries counters (alertsTriggered, detectionCounts),
        # not per-event records (process/path/sourceIP), so there is no source for individual
        # alerts. Verify the backing kind is readable, then return no items rather than invent any.
        # A missing/unreadable CRD must not turn into a 500: the answer is the same either way.
        # eventSource=false tells the UI no per-event source is wired ("no event source", not "all clear").
        try:
            await list_items(deps, POLICIES)
        except HTTPException:
            pass
        return {"items": [], "eventSource": False}

    @router.get("/api/security/policies")
    @deps.limiter.limit("30/minute")
    async def list_policies(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        return {"items": [_policy_view(o) for o in await list_items(deps, POLICIES)]}

    @router.post("/api/security/policies", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_policy(request: Request, body: SecurityPolicyRequest, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        spec: Dict[str, Any] = {
            "targetNamespaces": body.targetNamespaces,
            "detectionRules": [r.model_dump() for r in body.detectionRules],
            "autoBlock": body.autoBlock,
        }
        if body.alertWebhook:
            spec["alertWebhook"] = body.alertWebhook
        created = await create_item(deps, POLICIES, "GryviaSecurityPolicy", body.name, spec)
        return _policy_view(created)

    return router
