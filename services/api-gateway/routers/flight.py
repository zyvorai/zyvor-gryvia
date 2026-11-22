# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Authenticated, bounded cluster view of the per-node Flight Recorder.

Node reports are independent observations. Missing nodes and nodes with no
events are reported separately; neither condition is silently called healthy.
"""
import asyncio
import hashlib
import hmac
import ipaddress
import json
import logging
import re
import ssl
import statistics
import time
from typing import Any, Callable, Dict, List, Optional, Tuple
from urllib.parse import urlencode

import httpx
from fastapi import APIRouter, Depends, HTTPException, Request

from .collector import COLLECTOR_PORT, COLLECTOR_SELECTOR
from .collector_transport import client_kwargs, collector_scheme, request_extensions
from .collectives import compare_collectives
from .common import Deps, list_items, run
from .uiutil import namespaces

logger = logging.getLogger(__name__)
NAME = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
MAX_EVENTS = 500
NODE_EVENT_LIMIT = 200          # the collector returns at most this many events per node
NODE_COLLECTIVE_LIMIT = 200     # ... and this many identified collective spans
MIN_TOKEN_LENGTH = 32           # same floor the collector enforces
NODE_TIMEOUT = 3.0              # per collector request
TOTAL_DEADLINE = 10.0           # whole fan-out; collectors still pending count as unreachable
MAX_CONCURRENCY = 16            # simultaneous collector requests
MAX_COLLECTORS = 512            # discovery cap
MAX_RESPONSE_BYTES = 2 * 1024 * 1024


def valid_name(value: str) -> bool:
    return len(value) <= 63 and bool(NAME.fullmatch(value))


async def flight_collector_urls(deps: Deps) -> List[str]:
    """Keep the shared token inside the trusted collector namespace.

    Explicit URLs (GRYVIA_COLLECTOR_URLS) are an administrator-set override for nonstandard
    deployments and are trusted to the same degree as the token itself. No request parameter ever
    reaches a collector URL: only the fixed path and validated, urlencoded job/namespace do.
    """
    if deps.collector_urls:
        return list(deps.collector_urls)[:MAX_COLLECTORS]
    try:
        pods = await run(deps.k8s_core.list_namespaced_pod, namespace=deps.flight_collector_namespace,
                         label_selector=COLLECTOR_SELECTOR)
    except Exception:  # noqa: BLE001
        logger.warning("could not list Flight Recorder collectors", exc_info=True)
        return []
    urls = []
    for pod in getattr(pods, "items", []) or []:
        status = getattr(pod, "status", None)
        ip = getattr(status, "pod_ip", None)
        if not ip or getattr(status, "phase", "") != "Running":
            continue
        try:
            address = ipaddress.ip_address(ip)
        except ValueError:
            continue
        host = f"[{address}]" if address.version == 6 else str(address)
        urls.append(f"{collector_scheme()}://{host}:{COLLECTOR_PORT}")
    return urls[:MAX_COLLECTORS]


def signature_headers(path: str, token: str, when: int) -> Dict[str, str]:
    stamp = str(when)
    message = f"GET\n{path}\n{stamp}".encode()
    return {"X-Gryvia-Flight-Time": stamp,
            "X-Gryvia-Flight-Signature": hmac.new(token.encode(), message, hashlib.sha256).hexdigest()}


async def _read_node(client: httpx.AsyncClient, url: str, path: str, token: str,
                     soft: Tuple[int, ...] = (404,)) -> Tuple[bool, Any]:
    """(reachable, body). 404 means the node answered but attributed no events to the job.
    ``soft`` lists other statuses that also mean "answered, nothing to report" (compare uses 400: only
    the node that holds the incident can resolve its id)."""
    try:
        headers = signature_headers(path, token, int(time.time()))
        async with client.stream("GET", url + path, headers=headers, **request_extensions()) as response:
            if response.status_code in soft:
                return True, None
            response.raise_for_status()
            chunks, size = [], 0
            async for chunk in response.aiter_bytes():
                size += len(chunk)
                if size > MAX_RESPONSE_BYTES:
                    raise ValueError("response too large")
                chunks.append(chunk)
        body = json.loads(b"".join(chunks))
        return True, body if isinstance(body, dict) else None
    except (httpx.HTTPError, ValueError) as exc:
        # Type only: never log headers, the signature or the token.
        logger.info("Flight Recorder collector unavailable: %s", type(exc).__name__)
        return False, None


async def _fan_out(urls: List[str], path: str, token: str, soft: Tuple[int, ...] = (404,)) -> List[Tuple[bool, Any]]:
    semaphore = asyncio.Semaphore(MAX_CONCURRENCY)
    limits = httpx.Limits(max_connections=MAX_CONCURRENCY)
    # No redirects and no proxy variables: the signed request goes to the discovered pod only.
    try:
        kwargs = client_kwargs()
    except (OSError, ssl.SSLError) as exc:  # unreadable CA/client cert: fail closed
        logger.error("collector TLS configuration unusable: %s", type(exc).__name__)
        return [(False, None)] * len(urls)
    async with httpx.AsyncClient(timeout=NODE_TIMEOUT, follow_redirects=False, trust_env=False,
                                 limits=limits, **kwargs) as client:
        async def one(url: str) -> Tuple[bool, Any]:
            async with semaphore:
                if soft == (404,):
                    return await _read_node(client, url, path, token)
                return await _read_node(client, url, path, token, soft)

        tasks = [asyncio.ensure_future(one(url)) for url in urls]
        _, pending = await asyncio.wait(tasks, timeout=TOTAL_DEADLINE)
        for task in pending:
            task.cancel()
        results: List[Tuple[bool, Any]] = []
        for task in tasks:
            results.append(task.result() if task.done() and not task.cancelled() and not task.exception()
                           else (False, None))
        return results


def token_usable(token: Any) -> bool:
    return isinstance(token, str) and len(token) >= MIN_TOKEN_LENGTH


async def collect(deps: Deps, path: str, soft: Tuple[int, ...] = (404,)) -> Tuple[List[Tuple[bool, Any]], int]:
    """Ask every collector for ``path``: ([(reachable, body)], number of discovered collectors).
    ``soft`` lists statuses that mean "answered, nothing to report" (see _read_node)."""
    if deps.collector_fetch is not None:
        result = await deps.collector_fetch(path)
        bodies, total = (list(result[0]), int(result[1])) if isinstance(result, tuple) else (list(result), len(result))
        # The test hook models responsive collectors. None means a responsive node without events.
        replies = [(True, body) for body in bodies]
        return replies, max(total, len(replies))
    urls = await flight_collector_urls(deps)
    if not token_usable(deps.flight_token):
        return [], len(urls)
    return (await _fan_out(urls, path, deps.flight_token, soft) if urls else []), len(urls)


async def gather(deps: Deps, namespace: str, job: str, path: str = "",
                 soft: Tuple[int, ...] = (404,)) -> Tuple[List[Dict[str, Any]], Dict[str, int]]:
    """Fan out one signed GET. With the default path this is the per-job flight report and bodies must
    name this namespace and job; a custom ``path`` (diagnosis, incidents) keeps only dict bodies that name
    a node, and the collector already scopes them by the query string."""
    custom = bool(path)
    path = path or "/api/v1/flight/diagnose?" + urlencode({"namespace": namespace, "job": job})
    replies, total = await collect(deps, path, soft)
    if custom:
        valid = [body for ok, body in replies if ok and isinstance(body, dict)
                 and isinstance(body.get("node"), str) and body.get("node")]
    else:
        valid = [body for ok, body in replies if ok and isinstance(body, dict)
                 and body.get("namespace") == namespace and body.get("job") == job]
    return valid, {"total": total, "reachable": sum(ok for ok, _ in replies), "reporting": len(valid)}


def merge(namespace: str, job: str, bodies: List[Dict[str, Any]], coverage: Dict[str, int]) -> Dict[str, Any]:
    events: List[Dict[str, Any]] = []
    collectives: List[Dict[str, Any]] = []
    counts: Dict[str, int] = {}
    findings: List[Dict[str, str]] = []
    nodes: List[str] = []
    truncated = False
    for body in bodies:
        node = body.get("node")
        if not isinstance(node, str) or not node:
            continue
        nodes.append(node)
        raw_counts = body.get("counts")
        for kind, value in (raw_counts if isinstance(raw_counts, dict) else {}).items():
            if isinstance(kind, str) and isinstance(value, int) and not isinstance(value, bool) and value >= 0:
                counts[kind] = counts.get(kind, 0) + value
        raw_events = body.get("events")
        if isinstance(raw_events, list) and len(raw_events) >= NODE_EVENT_LIMIT:
            truncated = True  # the node keeps more than it returns
        for item in (raw_events if isinstance(raw_events, list) else [])[:NODE_EVENT_LIMIT]:
            if not isinstance(item, dict):
                continue
            identity = item.get("identity") or {}
            if isinstance(identity, dict) and identity.get("namespace") == namespace and identity.get("job") == job:
                events.append({**item, "node": node})
        raw_collectives = body.get("collectives")
        for item in (raw_collectives if isinstance(raw_collectives, list) else [])[:NODE_COLLECTIVE_LIMIT]:
            identity = item.get("identity") if isinstance(item, dict) else None
            if isinstance(identity, dict) and identity.get("namespace") == namespace and identity.get("job") == job:
                collectives.append({**item, "node": node})
        raw_findings = body.get("findings")
        for item in (raw_findings if isinstance(raw_findings, list) else [])[:20]:
            if isinstance(item, dict) and isinstance(item.get("code"), str) and isinstance(item.get("evidence"), str):
                findings.append({"node": node, "code": item["code"], "evidence": item["evidence"][:500]})
    events.sort(key=lambda event: str(event.get("time") or ""))
    rank_observations = []
    by_op: Dict[str, Dict[str, List[int]]] = {}
    for event in events:
        if event.get("kind") != "nccl_collective":
            continue
        identity = event.get("identity") or {}
        rank, op, duration = identity.get("rank"), event.get("operation"), event.get("duration_ns")
        if isinstance(rank, str) and rank and len(rank) <= 32 and isinstance(op, str) and op \
                and isinstance(duration, int) and not isinstance(duration, bool) and 0 < duration < 10**12:
            by_op.setdefault(op, {}).setdefault(rank, []).append(duration)
    for op, ranks in sorted(by_op.items()):
        medians = {rank: int(statistics.median(samples)) for rank, samples in ranks.items() if len(samples) >= 3}
        if len(medians) < 2:
            continue
        baseline = statistics.median(medians.values())
        for rank, median_ns in sorted(medians.items()):
            rank_observations.append({"operation": op, "rank": rank, "samples": len(ranks[rank]),
                                      "medianDurationNs": median_ns})
            if baseline > 0 and median_ns > baseline * 1.5 and median_ns - baseline > 100_000:
                findings.append({"node": "cluster", "code": "nccl_api_duration_outlier",
                                 "evidence": f"Rank {rank} observed median {op} API duration {median_ns} ns; "
                                             f"rank median baseline {int(baseline)} ns. This does not establish GPU or network cause."})
    truncated = truncated or len(events) > MAX_EVENTS
    comparison = compare_collectives(collectives)
    findings.extend(comparison.pop("findings"))
    return {
        "namespace": namespace, "job": job, "scope": "cluster observations; observed events only",
        # complete = every discovered collector answered. Nodes without a collector, or whose
        # collector pod is not Running, are not counted; `truncated` says events were dropped.
        "coverage": {**coverage, "complete": coverage["total"] > 0 and coverage["reachable"] == coverage["total"]},
        "truncated": truncated,
        "nodes": sorted(set(nodes)), "counts": counts, "findings": findings[:100],
        "rankObservations": rank_observations[:100],
        # Identified collectives compared op-for-op across ranks/nodes (see routers/collectives.py).
        "collectiveComparison": comparison,
        "events": events[-MAX_EVENTS:],
    }


# ---- request tracing -------------------------------------------------------------------------
# A trace id is user data. It is validated, never echoed into an error message and never logged;
# the access log line of uvicorn would carry it in the path, so it is redacted below.

TRACE_ID = re.compile(r"^[0-9a-fA-F]{32}$")
TRACE_PATH = re.compile(r"(/api/flight/trace/)[0-9A-Za-z%-]{1,64}")
MAX_TRACE_OBSERVATIONS = 100
MAX_TRACE_EVENTS = 500
TRACE_EVENT_KEYS = ("time", "source", "kind", "operation", "bytes", "retransmits", "duration_ns", "traceMatch")


def valid_trace_id(value: str) -> bool:
    return bool(TRACE_ID.fullmatch(value)) and int(value, 16) != 0


class RedactTraceIds(logging.Filter):
    """Replaces the trace id in access-log request lines (``GET /api/flight/trace/<id> HTTP/1.1``)."""

    def filter(self, record: logging.LogRecord) -> bool:
        if isinstance(record.args, tuple):
            record.args = tuple(TRACE_PATH.sub(r"\1<redacted>", a) if isinstance(a, str) else a for a in record.args)
        if isinstance(record.msg, str):
            record.msg = TRACE_PATH.sub(r"\1<redacted>", record.msg)
        return True


logging.getLogger("uvicorn.access").addFilter(RedactTraceIds())


def _identity(item: Any) -> Optional[Dict[str, str]]:
    ident = item.get("identity") if isinstance(item, dict) else None
    if not isinstance(ident, dict) or not isinstance(ident.get("namespace"), str):
        return None
    return {k: ident[k][:253] for k in ("namespace", "job", "pod", "node", "rank")
            if isinstance(ident.get(k), str) and ident[k]}


def merge_trace(trace_id: str, bodies: List[Dict[str, Any]], coverage: Dict[str, int],
                in_scope: Callable[[str], bool]) -> Dict[str, Any]:
    """Merge node answers, keeping only what ``in_scope(namespace)`` allows.

    Tenant scoping happens here, on the identity the collector attached to every observation and
    event: anything without a namespace, or in another namespace, is dropped before it can be
    counted or returned. Only known fields are copied out.
    """
    observations: List[Dict[str, Any]] = []
    events: List[Dict[str, Any]] = []
    nodes: List[str] = []
    for body in bodies:
        node = body.get("node")
        if not isinstance(node, str) or not node or body.get("traceId") != trace_id:
            continue
        kept = False
        raw_obs = body.get("observations")
        for item in (raw_obs if isinstance(raw_obs, list) else [])[:MAX_TRACE_OBSERVATIONS]:
            ident = _identity(item)
            if ident is None or not in_scope(ident["namespace"]):
                continue
            kept = True
            observations.append({"node": node, "identity": ident,
                                 **{k: item[k][:64] for k in ("time", "spanId", "client", "server")
                                    if isinstance(item.get(k), str)}})
        raw_events = body.get("events")
        for item in (raw_events if isinstance(raw_events, list) else [])[:NODE_EVENT_LIMIT]:
            ident = _identity(item)
            if ident is None or not in_scope(ident["namespace"]) or item.get("traceId") != trace_id:
                continue
            kept = True
            events.append({"node": node, "identity": ident, "traceId": trace_id,
                           **{k: item[k] for k in TRACE_EVENT_KEYS
                              if isinstance(item.get(k), (str, int, float)) and not isinstance(item.get(k), bool)}})
        if kept:
            nodes.append(node)
    observations.sort(key=lambda o: str(o.get("time") or ""))
    events.sort(key=lambda e: str(e.get("time") or ""))
    return {
        "traceId": trace_id,
        "scope": "cluster observations; node-local, plaintext HTTP/1.x with a traceparent header, IPv4; "
                 "events joined by TCP 4-tuple or by pod, port and time (see traceMatch)",
        "coverage": {**coverage, "reporting": len(set(nodes)),
                     "complete": coverage["total"] > 0 and coverage["reachable"] == coverage["total"]},
        "nodes": sorted(set(nodes)),
        "observations": observations[:MAX_TRACE_OBSERVATIONS],
        "events": events[-MAX_TRACE_EVENTS:],
    }


# ---- serving-engine latency (read from GryviaFabricSignal.status) ------------------------------

INFERENCE_FIELDS = ("ttftP99ms", "itlP99ms", "queueTimeP99ms", "e2eP99ms", "queueTimeMeanMs", "e2eMeanMs",
                    "requestsWaiting", "kvCacheUsage", "inferWaitP99ms")
ENGINE = re.compile(r"^[a-z]{2,16}$")


def inference_view(namespace: str, job: str, items: List[Dict[str, Any]]) -> Dict[str, Any]:
    """The newest engine figures among the job's GryviaFabricSignals. ``available`` is false unless
    the collector's metrics scraper (-infer-metrics) has published an engine name."""
    best: Optional[Dict[str, Any]] = None
    for item in items:
        spec, status = item.get("spec") or {}, item.get("status") or {}
        engine = status.get("engine")
        if spec.get("jobRef") != job or not isinstance(engine, str) or not ENGINE.fullmatch(engine):
            continue
        if best is None or str(status.get("updatedAt") or "") > str(best.get("updatedAt") or ""):
            best = status
    if best is None:
        return {"namespace": namespace, "job": job, "available": False}
    out: Dict[str, Any] = {"namespace": namespace, "job": job, "available": True, "engine": best["engine"]}
    if isinstance(best.get("updatedAt"), str):
        out["updatedAt"] = best["updatedAt"][:40]
    for key in INFERENCE_FIELDS:
        value = best.get(key)
        if isinstance(value, (int, float)) and not isinstance(value, bool):
            out[key] = value
    return out


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/flight/jobs/{job}")
    @deps.limiter.limit("30/minute")
    async def diagnose(request: Request, job: str, namespace: str = "", _=Depends(deps.verify_auth)):
        allowed = namespaces(request, deps)
        admin = getattr(request.state, "role", None) == "admin"
        ns = namespace or (deps.job_namespace if admin else allowed[0])
        if not valid_name(job) or not valid_name(ns):
            raise HTTPException(status_code=400, detail="Invalid job or namespace")
        # Tenant users only see their tenant namespaces; the admin (API key) sees any namespace.
        if not admin and ns not in allowed:
            raise HTTPException(status_code=403, detail="Namespace is not available to this user")
        if not token_usable(deps.flight_token):
            raise HTTPException(status_code=503, detail="Flight Recorder token is not configured")
        bodies, coverage = await gather(deps, ns, job)
        if coverage["total"] == 0 or coverage["reachable"] == 0:
            raise HTTPException(status_code=503, detail="No Flight Recorder collector is reachable")
        return merge(ns, job, bodies, coverage)

    from . import flight_diagnosis  # noqa: E402  (imports this module's helpers)
    flight_diagnosis.add_routes(router, deps)

    @router.get("/api/flight/trace/{trace_id}")
    @deps.limiter.limit("30/minute")
    async def trace(request: Request, trace_id: str, namespace: str = "", _=Depends(deps.verify_auth)):
        """Node-local observations and Flight Recorder events correlated with one W3C trace id."""
        if not valid_trace_id(trace_id):
            raise HTTPException(status_code=400, detail="Invalid trace id")  # the id is not echoed
        trace_id = trace_id.lower()
        allowed = namespaces(request, deps)
        admin = getattr(request.state, "role", None) == "admin"
        if namespace and not valid_name(namespace):
            raise HTTPException(status_code=400, detail="Invalid namespace")
        if namespace and not admin and namespace not in allowed:
            raise HTTPException(status_code=403, detail="Namespace is not available to this user")
        if namespace:
            def in_scope(ns: str) -> bool:
                return ns == namespace
        elif admin:
            def in_scope(ns: str) -> bool:
                return True
        else:
            def in_scope(ns: str) -> bool:
                return ns in allowed
        if not token_usable(deps.flight_token):
            raise HTTPException(status_code=503, detail="Flight Recorder token is not configured")
        replies, total = await collect(deps, "/api/v1/flight/trace?" + urlencode({"traceId": trace_id}))
        reachable = sum(ok for ok, _ in replies)
        if total == 0 or reachable == 0:
            raise HTTPException(status_code=503, detail="No Flight Recorder collector is reachable")
        bodies = [body for ok, body in replies if ok and isinstance(body, dict)]
        result = merge_trace(trace_id, bodies, {"total": total, "reachable": reachable}, in_scope)
        if not result["observations"]:
            # Same answer whether the trace does not exist or belongs to someone else.
            raise HTTPException(status_code=404, detail="Trace not observed in the namespaces you can read")
        return result

    @router.get("/api/flight/inference/{job}")
    @deps.limiter.limit("60/minute")
    async def inference(request: Request, job: str, namespace: str = "", _=Depends(deps.verify_auth)):
        """Serving-engine latency of a job, from the status the collector publishes on its
        GryviaFabricSignal. Reads the cluster API only (no collector fan-out)."""
        allowed = namespaces(request, deps)
        admin = getattr(request.state, "role", None) == "admin"
        ns = namespace or (deps.job_namespace if admin else allowed[0])
        if not valid_name(job) or not valid_name(ns):
            raise HTTPException(status_code=400, detail="Invalid job or namespace")
        if not admin and ns not in allowed:
            raise HTTPException(status_code=403, detail="Namespace is not available to this user")
        return inference_view(ns, job, await list_items(deps, "gryviafabricsignals", namespace=ns))

    return router
