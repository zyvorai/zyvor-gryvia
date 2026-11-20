# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Gateway metrics (Prometheus) and the guarded /metrics endpoint.

Security: /metrics is DISABLED unless GRYVIA_METRICS_TOKEN is set (>= 32 characters); it is then served only to
callers presenting ``Authorization: Bearer <token>`` (constant-time compare), the same model as the collector's
/metrics. With no token the route is not registered at all (404). The gateway API key is deliberately not reused.

Cardinality: labels are bounded enums or route templates ("/api/jobs/{name}"), never raw paths, user names, tenants,
tokens or collector addresses. Requests that match no route are all labelled route="unmatched".
"""

import hmac
import logging
import os
import time
from typing import Optional

from fastapi import FastAPI, Request
from fastapi.responses import PlainTextResponse
from prometheus_client import (
    CONTENT_TYPE_LATEST,
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    generate_latest,
)

logger = logging.getLogger(__name__)

MIN_TOKEN_LENGTH = 32
REGISTRY = CollectorRegistry()

HTTP_REQUESTS = Counter(
    "gryvia_gateway_http_requests_total",
    "HTTP requests handled by the gateway.",
    ["method", "route", "status"],
    registry=REGISTRY,
)
HTTP_LATENCY = Histogram(
    "gryvia_gateway_http_request_duration_seconds",
    "HTTP request latency by route template.",
    ["method", "route"],
    registry=REGISTRY,
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
)
AUTH_RESULTS = Counter(
    "gryvia_gateway_auth_attempts_total",
    "Authentication attempts by method (session, oidc, api_key, login, none) and result.",
    ["method", "result"],
    registry=REGISTRY,
)
AUDIT_EVENTS = Counter(
    "gryvia_gateway_audit_events_total",
    "State-changing requests recorded in the audit trail, by HTTP method and outcome (success, denied, error).",
    ["method", "outcome"],
    registry=REGISTRY,
)
RATE_LIMIT_HITS = Counter(
    "gryvia_gateway_rate_limit_hits_total",
    "Requests rejected with 429 by the rate limiter.",
    ["route"],
    registry=REGISTRY,
)
COLLECTOR_TARGETS = Gauge(
    "gryvia_gateway_collector_targets",
    "Collectors addressed by the last fan-out request.",
    registry=REGISTRY,
)
COLLECTOR_REACHABLE = Gauge(
    "gryvia_gateway_collector_reachable",
    "Collectors that answered the last fan-out request.",
    registry=REGISTRY,
)
COLLECTOR_FANOUT = Counter(
    "gryvia_gateway_collector_fanout_total",
    "Collector fan-out requests by outcome (all_reachable, partial, none_reachable, no_collectors).",
    ["outcome"],
    registry=REGISTRY,
)

_AUTH_METHODS = {"session", "oidc", "api_key", "login", "none"}


def route_template(request: Request) -> str:
    route = request.scope.get("route")
    path = getattr(route, "path", None)
    return path if isinstance(path, str) else "unmatched"


def record_audit(method: str, outcome: str) -> None:
    AUDIT_EVENTS.labels(method, outcome).inc()


def record_auth(method: str, ok: bool) -> None:
    AUTH_RESULTS.labels(
        method if method in _AUTH_METHODS else "none", "success" if ok else "failure"
    ).inc()


def record_fanout(reachable: int, total: int) -> None:
    COLLECTOR_TARGETS.set(total)
    COLLECTOR_REACHABLE.set(reachable)
    if total == 0:
        outcome = "no_collectors"
    elif reachable >= total:
        outcome = "all_reachable"
    elif reachable == 0:
        outcome = "none_reachable"
    else:
        outcome = "partial"
    COLLECTOR_FANOUT.labels(outcome).inc()


def metrics_token() -> Optional[str]:
    token = os.environ.get("GRYVIA_METRICS_TOKEN", "").strip()
    if token and len(token) < MIN_TOKEN_LENGTH:
        logger.error(
            "GRYVIA_METRICS_TOKEN is shorter than %d characters: /metrics stays disabled",
            MIN_TOKEN_LENGTH,
        )
        return None
    return token or None


def install(app: FastAPI) -> None:
    """Attach the request-metrics middleware and, only when a token is configured, the /metrics route."""

    @app.middleware("http")
    async def _observe(request: Request, call_next):
        start = time.perf_counter()
        status = 500
        try:
            response = await call_next(request)
            status = response.status_code
            return response
        finally:
            route = route_template(request)
            HTTP_REQUESTS.labels(request.method, route, str(status)).inc()
            HTTP_LATENCY.labels(request.method, route).observe(
                time.perf_counter() - start
            )
            if status == 429:
                RATE_LIMIT_HITS.labels(route).inc()

    token = metrics_token()
    if token is None:
        return

    @app.get("/metrics", include_in_schema=False)
    async def _metrics(request: Request):
        header = request.headers.get("authorization", "")
        presented = header[7:].strip() if header.lower().startswith("bearer ") else ""
        if not hmac.compare_digest(presented.encode(), token.encode()):
            return PlainTextResponse(
                "unauthorized\n",
                status_code=401,
                headers={"WWW-Authenticate": "Bearer"},
            )
        return PlainTextResponse(
            generate_latest(REGISTRY), media_type=CONTENT_TYPE_LATEST
        )
