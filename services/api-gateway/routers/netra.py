# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Optional flow source: a Netra controller (netrad).

Netra (https://github.com/zyvorai/netra) is a separate, standalone eBPF network observability product. Gryvia does
not carry its own kernel programs; when GRYVIA_NETRA_URL points at a netrad, the network pages read real flows from
its documented HTTP API (GET /api/v1/flows/history) instead of only the service-graph edges. Nothing is invented:
an unreachable or unset Netra means no Netra flows.
"""
import logging
from typing import Any, Dict, List, Optional

import httpx

from .common import Deps

logger = logging.getLogger(__name__)

HISTORY_PATH = "/api/v1/flows/history"
TIMEOUT_SECONDS = 4.0
WINDOW = "15m"
LIMIT = 500


def _endpoint(record: Dict[str, Any]) -> str:
    ns, pod = record.get("namespace") or "", record.get("pod") or ""
    if ns and pod:
        return f"{ns}/{pod}"
    if pod or record.get("workloadName"):
        return pod or record["workloadName"]
    # host processes have no pod: name the process and node instead
    comm, node = record.get("comm") or "", record.get("node") or ""
    return f"{comm}@{node}" if comm and node else comm or node


def _latency(record: Dict[str, Any]) -> str:
    us = record.get("srttUs") or 0
    if not us:
        return ""
    return f"{us / 1000:.1f}ms" if us >= 1000 else f"{us}us"


def to_flow(record: Dict[str, Any], index: int) -> Dict[str, Any]:
    """One Netra flow record as a dashboard NetworkFlow item. `blocked` counts packets Netra dropped."""
    return {
        "metadata": {"name": f"netra-{index}",
                     **({"namespace": record["namespace"]} if record.get("namespace") else {})},
        "spec": {
            "timestamp": record.get("observedAt", ""),
            "source": _endpoint(record),
            "destination": record.get("peer", ""),
            "protocol": (record.get("protocol") or "").upper(),
            "port": record.get("port", 0),
            "bytes": str(record.get("bytes", 0)),
            "latency": _latency(record),
            "verdict": "DROP" if record.get("blocked") else "FORWARDED",
        },
    }


async def fetch_flows(deps: Deps) -> Optional[List[Dict[str, Any]]]:
    """Recent flows from Netra as dashboard items, or None when Netra is not configured or unreachable."""
    if deps.netra_fetch is not None:
        records = await deps.netra_fetch()
    elif deps.netra_url:
        headers = {"Authorization": f"Bearer {deps.netra_token}"} if deps.netra_token else {}
        try:
            async with httpx.AsyncClient(timeout=TIMEOUT_SECONDS, verify=deps.netra_verify_tls) as client:
                resp = await client.get(deps.netra_url + HISTORY_PATH, params={"since": WINDOW, "limit": LIMIT},
                                        headers=headers)
                resp.raise_for_status()
                records = (resp.json() or {}).get("records") or []
        except Exception as exc:  # noqa: BLE001 - a missing Netra must not break the page
            logger.info("netra %s unavailable: %s", deps.netra_url, exc)
            return None
    else:
        return None
    if records is None:
        return None
    return [to_flow(r, i) for i, r in enumerate(records) if isinstance(r, dict)]


HISTORY_MAX = 5000


async def fetch_history(deps: Deps, since_hours: int, limit: int = HISTORY_MAX) -> Optional[List[Dict[str, Any]]]:
    """Raw Netra flow-history records of the last ``since_hours`` hours (at most ``limit``), or None when Netra
    is not configured or unreachable. Used by the network cost reconciliation only. The ``since``/``limit``
    parameters follow the documented history API; the retention and record semantics are Netra's (unverified
    here against a live netrad)."""
    if deps.netra_fetch is not None:
        records = await deps.netra_fetch()
    elif deps.netra_url:
        headers = {"Authorization": f"Bearer {deps.netra_token}"} if deps.netra_token else {}
        try:
            async with httpx.AsyncClient(timeout=TIMEOUT_SECONDS * 4, verify=deps.netra_verify_tls) as client:
                resp = await client.get(deps.netra_url + HISTORY_PATH,
                                        params={"since": f"{max(1, since_hours)}h", "limit": limit}, headers=headers)
                resp.raise_for_status()
                records = (resp.json() or {}).get("records") or []
        except Exception as exc:  # noqa: BLE001
            logger.info("netra %s unavailable: %s", deps.netra_url, exc)
            return None
    else:
        return None
    return [r for r in (records or []) if isinstance(r, dict)]
