# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Sovereign AI OS overview: the health of Zyntra and Netra next to Gryvia, for the dashboard's overview page.

GET /api/sovereign probes the products the gateway is pointed at (GRYVIA_ZYNTRA_URL, GRYVIA_NETRA_URL) from inside the
cluster, so the browser never needs their addresses or tokens. Zyntra is read with a viewer service token
(GRYVIA_ZYNTRA_TOKEN): its version, source health, open gaps and proposals waiting for approval. Netra is checked on
/healthz. GRYVIA_ZYNTRA_CONSOLE_URL and GRYVIA_NETRA_CONSOLE_URL are the links the page offers; empty means the
console is reached by port-forward. Nothing is invented: an unset product is "not installed", an unreachable one is
"unreachable" with the error.
"""
import logging
from typing import Any, Dict, Optional

import httpx
from fastapi import APIRouter, Depends

from .common import Deps

logger = logging.getLogger(__name__)

TIMEOUT_SECONDS = 4.0


def _client(deps: Deps, verify: Any = None) -> httpx.AsyncClient:
    if verify is None:
        verify = deps.sovereign_ca_file or True
    if deps.sovereign_client is not None:
        return deps.sovereign_client(verify)
    return httpx.AsyncClient(timeout=TIMEOUT_SECONDS, verify=verify, follow_redirects=False)


async def zyntra_status(deps: Deps) -> Dict[str, Any]:
    out: Dict[str, Any] = {"name": "Zyntra", "role": "Ontology, typed actions, approvals and signed audit",
                           "console": deps.zyntra_console_url or None, "installed": bool(deps.zyntra_url)}
    if not deps.zyntra_url:
        out["state"] = "not installed"
        return out
    headers = {"Authorization": f"Bearer {deps.zyntra_token}"} if deps.zyntra_token else {}
    try:
        async with _client(deps) as client:
            meta = await client.get(deps.zyntra_url + "/api/v1/meta", headers=headers)
            if meta.status_code != 200:
                out["state"] = f"unhealthy ({meta.status_code})"
                return out
            m = meta.json()
            out.update(state="healthy", version=m.get("version"), executeMode=m.get("execute_mode"),
                       pack=(m.get("pack") or {}).get("id") if isinstance(m.get("pack"), dict) else m.get("pack"),
                       sources=m.get("sources"))
            if deps.zyntra_token:
                gaps = await client.get(deps.zyntra_url + "/api/v1/gaps", headers=headers)
                if gaps.status_code == 200:
                    out["openGaps"] = len(gaps.json().get("gaps") or [])
                props = await client.get(deps.zyntra_url + "/api/v1/proposals", headers=headers)
                if props.status_code == 200:
                    items = props.json().get("proposals") or []
                    out["pendingProposals"] = sum(1 for p in items if p.get("status") == "pending")
    except (httpx.HTTPError, ValueError) as exc:
        logger.info("zyntra %s unavailable: %s", deps.zyntra_url, exc)
        out.update(state="unreachable", error=str(exc)[:200])
    return out


async def netra_status(deps: Deps) -> Dict[str, Any]:
    out: Dict[str, Any] = {"name": "Netra", "role": "eBPF network observability and enforcement",
                           "console": deps.netra_console_url or None, "installed": bool(deps.netra_url)}
    if not deps.netra_url:
        out["state"] = "not installed"
        return out
    try:
        async with _client(deps, deps.netra_verify_tls) as client:
            resp = await client.get(deps.netra_url + "/healthz")
            out["state"] = "healthy" if resp.status_code == 200 else f"unhealthy ({resp.status_code})"
    except httpx.HTTPError as exc:
        logger.info("netra %s unavailable: %s", deps.netra_url, exc)
        out.update(state="unreachable", error=str(exc)[:200])
    return out


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/sovereign")
    async def sovereign(_=Depends(deps.verify_auth)) -> Dict[str, Optional[Any]]:
        zyntra, netra = await zyntra_status(deps), await netra_status(deps)
        return {"enabled": zyntra["installed"] or netra["installed"], "products": [zyntra, netra]}

    return router
