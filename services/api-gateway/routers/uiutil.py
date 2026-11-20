# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Helpers shared by the dashboard-facing route modules (workspaces, models, ...).

Namespace handling mirrors /api/jobs in main.py: with OIDC tenants the request's
``tenant_namespaces`` are used, otherwise the gateway's job namespace.
"""
from datetime import datetime, timezone
from typing import Any, Dict, List, Literal, Optional, Tuple

from fastapi import HTTPException, Request
from pydantic import BaseModel, ConfigDict, Field

from .common import Deps, get_item, list_items
from .markings import can_see, marking_of

# Kubernetes DNS-1123 label (what the UI enforces too), max 63 chars.
NAME_PATTERN = r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
NAME_MAX = 63


def namespaces(request: Request, deps: Deps) -> List[str]:
    tenant_ns = getattr(request.state, "tenant_namespaces", None)
    return list(tenant_ns) if tenant_ns else [deps.job_namespace]


async def list_all(request: Request, deps: Deps, plural: str) -> List[Dict[str, Any]]:
    items: List[Dict[str, Any]] = []
    for ns in namespaces(request, deps):
        items.extend(await list_items(deps, plural, namespace=ns))
    return [o for o in items if can_see(request, o)]


async def find_one(request: Request, deps: Deps, plural: str, name: str) -> Tuple[Dict[str, Any], str]:
    """Return (object, namespace) for ``name`` in the caller's namespaces, or raise 404."""
    nss = namespaces(request, deps)
    for ns in nss:
        try:
            obj = await get_item(deps, plural, name, namespace=ns)
            if not can_see(request, obj):  # same answer as a missing object
                raise HTTPException(status_code=404, detail=f"{plural}/{name}: Not Found")
            return obj, ns
        except HTTPException as exc:
            if exc.status_code != 404 or ns == nss[-1]:
                raise
    raise HTTPException(status_code=404, detail=f"{plural}/{name}: Not Found")  # pragma: no cover


def meta(obj: Dict[str, Any]) -> Dict[str, Any]:
    m = obj.get("metadata") or {}
    out = {k: m[k] for k in ("name", "namespace", "creationTimestamp") if m.get(k)}
    marking = marking_of(obj)
    if marking:
        out["marking"] = marking
    return out


def parse_ts(value: Any) -> Optional[datetime]:
    if not isinstance(value, str):
        return None
    try:
        dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return dt if dt.tzinfo else dt.replace(tzinfo=timezone.utc)


def fmt_duration(start: Any, end: Any = None) -> Optional[str]:
    """Human duration between two RFC3339 timestamps (end defaults to now); None if start is unknown."""
    s = parse_ts(start)
    if s is None:
        return None
    e = parse_ts(end) if end else datetime.now(timezone.utc)
    if e is None:
        return None
    secs = max(0, int((e - s).total_seconds()))
    days, rem = divmod(secs, 86400)
    hours, rem = divmod(rem, 3600)
    mins, sec = divmod(rem, 60)
    if days:
        return f"{days}d {hours}h"
    if hours:
        return f"{hours}h {mins}m"
    if mins:
        return f"{mins}m {sec}s"
    return f"{sec}s"


def prune(d: Dict[str, Any]) -> Dict[str, Any]:
    """Drop keys whose value is None (unknown), keeping 0/False/empty-but-real values."""
    return {k: v for k, v in d.items() if v is not None}


class JobTemplate(BaseModel):
    """Subset of GryviaAIJobSpec accepted from the dashboard (required: type, gpus, image)."""
    model_config = ConfigDict(extra="forbid")

    type: Literal["training", "inference", "fine-tuning", "evaluation"] = "training"
    image: str = Field(min_length=1, max_length=512, pattern=r"^[^\s]+$")
    gpus: int = Field(ge=0, le=1024)
    gpuType: str = Field(default="", max_length=63, pattern=r"^([A-Za-z0-9][A-Za-z0-9._-]*)?$")
    model: str = Field(default="", max_length=253)
    command: list[str] = Field(default_factory=list, max_length=32)
    args: list[str] = Field(default_factory=list, max_length=64)

    def to_spec(self) -> dict:
        spec = {"type": self.type, "image": self.image, "gpus": self.gpus}
        for key in ("gpuType", "model"):
            if getattr(self, key):
                spec[key] = getattr(self, key)
        for key in ("command", "args"):
            if getattr(self, key):
                spec[key] = list(getattr(self, key))
        return spec
