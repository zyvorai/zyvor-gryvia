# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Client for the Gryvia collector (the per-node eBPF DaemonSet, collector/main.go).

Each collector pod serves JSON on :9090 (/api/v1/gpu/nccl, /api/v1/gpu/memory, ...). The gateway
reads every collector it can find and merges the answers, so one dashboard query covers all nodes.
Collectors are found from GRYVIA_COLLECTOR_URLS (comma separated) or, by default, from pods labelled
app.kubernetes.io/component=collector. No collector reachable -> no data (never invented).
"""
import asyncio
import logging
import ssl
from typing import Any, Dict, List, Tuple

import httpx

from .collector_transport import client_kwargs, collector_scheme, request_extensions, signed_headers
from .common import Deps, run
from .observability import record_fanout

logger = logging.getLogger(__name__)

COLLECTOR_SELECTOR = "app.kubernetes.io/component=collector"
COLLECTOR_PORT = 9090
TIMEOUT_SECONDS = 3.0


async def collector_urls(deps: Deps) -> List[str]:
    if deps.collector_urls:
        return list(deps.collector_urls)
    try:
        pods = await run(deps.k8s_core.list_pod_for_all_namespaces, label_selector=COLLECTOR_SELECTOR)
    except Exception:  # noqa: BLE001 - discovery is best effort
        logger.warning("could not list collector pods", exc_info=True)
        return []
    urls = []
    for pod in getattr(pods, "items", []) or []:
        status = getattr(pod, "status", None)
        ip = getattr(status, "pod_ip", None)
        if ip and getattr(status, "phase", "") == "Running":
            urls.append(f"{collector_scheme()}://{ip}:{COLLECTOR_PORT}")
    return urls


async def _get(client: httpx.AsyncClient, url: str, path: str = "") -> Any:
    try:
        # Signed with the shared token when GRYVIA_COLLECTOR_TOKEN(_FILE) is set; else as before.
        resp = await client.get(url, headers=signed_headers(path) if path else {}, **request_extensions())
        resp.raise_for_status()
        return resp.json()
    except Exception as exc:  # noqa: BLE001 - one node down must not fail the page
        logger.info("collector %s unavailable: %s", url, exc)
        return None


async def fetch_all_with_stats(deps: Deps, path: str) -> Tuple[List[Any], Dict[str, int]]:
    """(bodies, {"reachable": n, "total": m}) for `path` across every discovered collector.

    The ``Deps.collector_fetch`` test hook may return either a list of bodies (total is then
    the number of bodies) or a ``(bodies, total)`` tuple to simulate unreachable collectors.
    """
    if deps.collector_fetch is not None:
        res = await deps.collector_fetch(path)
        if isinstance(res, tuple):
            bodies, total = list(res[0]), int(res[1])
        else:
            bodies, total = list(res), len(res)
        return bodies, {"reachable": len(bodies), "total": max(total, len(bodies))}
    urls = await collector_urls(deps)
    if not urls:
        record_fanout(0, 0)
        return [], {"reachable": 0, "total": 0}
    try:
        kwargs = client_kwargs()
    except (OSError, ssl.SSLError) as exc:  # unreadable CA/client cert: fail closed, never fall back to http/unverified
        logger.error("collector TLS configuration unusable: %s", type(exc).__name__)
        record_fanout(0, len(urls))
        return [], {"reachable": 0, "total": len(urls)}
    async with httpx.AsyncClient(timeout=TIMEOUT_SECONDS, **kwargs) as client:
        results = await asyncio.gather(*[_get(client, u + path, path) for u in urls])
    bodies = [b for b in results if b is not None]
    record_fanout(len(bodies), len(urls))
    return bodies, {"reachable": len(bodies), "total": len(urls)}


async def fetch_all(deps: Deps, path: str) -> List[Any]:
    """JSON bodies from every reachable collector for `path` (e.g. /api/v1/gpu/nccl)."""
    return (await fetch_all_with_stats(deps, path))[0]


def _num(v: Any) -> float:
    return float(v) if isinstance(v, (int, float)) and not isinstance(v, bool) else 0.0


def merge_nccl(bodies: List[Dict[str, Any]]) -> Dict[str, Any]:
    """Merge per-node op_stats: counts and bytes add, average latency is count-weighted, p99 is the worst."""
    ops: Dict[str, Dict[str, float]] = {}
    for body in bodies:
        for name, s in ((body or {}).get("op_stats") or {}).items():
            if not isinstance(s, dict):
                continue
            op = ops.setdefault(str(s.get("OpType") or name), {"count": 0, "bytes": 0, "lat": 0, "p99": 0})
            count = _num(s.get("Count"))
            op["count"] += count
            op["bytes"] += _num(s.get("TotalBytes"))
            op["lat"] += _num(s.get("AvgLatencyNs")) * count
            op["p99"] = max(op["p99"], _num(s.get("P99LatencyNs")))
    return {
        "operations": [
            {
                "opType": name,
                "count": int(o["count"]),
                "avgLatencyNs": (o["lat"] / o["count"]) if o["count"] else 0,
                "p99LatencyNs": o["p99"],
                "totalBytes": int(o["bytes"]),
            }
            for name, o in sorted(ops.items())
        ]
    }


def merge_gpu_memory(bodies: List[Dict[str, Any]]) -> Dict[str, int]:
    """Sum per-direction transfer bytes and counts across nodes (keys: H2D, D2H, D2D)."""
    out = {"h2dBytes": 0, "d2hBytes": 0, "d2dBytes": 0, "h2dCount": 0, "d2hCount": 0, "d2dCount": 0}
    keys = {"H2D": "h2d", "D2H": "d2h", "D2D": "d2d"}
    for body in bodies:
        for direction, s in (body or {}).items():
            prefix = keys.get(str(direction).upper())
            if prefix and isinstance(s, dict):
                out[prefix + "Bytes"] += int(_num(s.get("TotalBytes")))
                out[prefix + "Count"] += int(_num(s.get("TransferCount")))
    return out
