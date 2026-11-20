# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Tenant directory: GryviaTenant lookup (cached briefly) and claim -> tenant matching.

A tenant's namespace is ``tenant-<name>`` (the GryviaTenant controller creates it). The gateway never
trusts a token to name a namespace: token claims are matched against GryviaTenant objects and the
namespaces come from the matched tenants.
"""
import time
from typing import Any, Dict, Iterable, List, Optional

from fastapi import HTTPException, Request

from .common import GROUP, VERSION, run

PLURAL = "gryviatenants"
CACHE_TTL_SECONDS = 30.0

_cache: Dict[str, Any] = {"at": None, "items": []}


def clear_cache() -> None:
    """Forget the cached tenant list (tests, and after this gateway creates/deletes a tenant)."""
    _cache["at"] = None
    _cache["items"] = []


def namespace_of(name: str) -> str:
    return f"tenant-{name}"


async def list_tenants(custom: Any, force: bool = False) -> List[Dict[str, Any]]:
    now = time.monotonic()
    at = _cache["at"]
    if not force and at is not None and 0 <= now - at < CACHE_TTL_SECONDS:
        return list(_cache["items"])
    try:
        res = await run(custom.list_cluster_custom_object, group=GROUP, version=VERSION, plural=PLURAL)
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=503, detail="Tenant directory unavailable") from exc
    items = list(res.get("items", []))
    _cache["at"], _cache["items"] = now, items
    return list(items)


def tenant_name(obj: Dict[str, Any]) -> str:
    return (obj.get("metadata") or {}).get("name", "")


def match_tenants(items: Iterable[Dict[str, Any]], candidates: Iterable[str]) -> List[Dict[str, Any]]:
    """Tenants whose name or namespace equals one of the claim values (sorted by name, no duplicates)."""
    wanted = {c for c in candidates if isinstance(c, str) and c}
    out = [t for t in items if tenant_name(t) and (tenant_name(t) in wanted or namespace_of(tenant_name(t)) in wanted)]
    return sorted(out, key=tenant_name)


def role_of(request: Request) -> Optional[str]:
    return getattr(request.state, "role", None)


def is_admin(request: Request) -> bool:
    return role_of(request) == "admin"


def caller_tenant_names(request: Request) -> List[str]:
    return list(getattr(request.state, "tenants", None) or [])


async def caller_tenants(request: Request, custom: Any) -> List[Dict[str, Any]]:
    names = set(caller_tenant_names(request))
    return [t for t in await list_tenants(custom) if tenant_name(t) in names]
