"""
Gryvia API Gateway
Provides REST API for Web UI with aggregated metrics and cluster data
"""

import asyncio
import base64
import functools
import hashlib
import hmac
import ipaddress
import json
import os
import time
import urllib.parse
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional
from fastapi import FastAPI, HTTPException, Query, Depends, Header, Request
from fastapi.middleware.cors import CORSMiddleware
from pydantic import BaseModel, Field
from kubernetes import client, config
from slowapi import Limiter, _rate_limit_exceeded_handler
from slowapi.util import get_remote_address
from slowapi.errors import RateLimitExceeded
import logging
from collections import defaultdict

from routers import submission
from routers import tenancy
from routers.phases import count_phases, is_billable, normalize as normalize_phase
from routers.pricing import load_rates
from routers.common import require_admin
from routers.usage import fetch_records
from routers import observability

import httpx
from jose import jwt, JWTError
from jose.exceptions import JOSEError

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


def _client_ip(request: Request) -> str:
    """Rate-limit key: the real client address.

    Behind the dashboard's nginx every request arrives from the proxy's pod IP, so one shared
    bucket would throttle all users together. Trust X-Forwarded-For only when the direct peer is
    a private or loopback address (the in-cluster proxy); a peer on a public address is used as-is,
    so the header cannot be spoofed from outside the cluster.
    """
    peer = get_remote_address(request)
    forwarded = request.headers.get("x-forwarded-for", "")
    if forwarded:
        try:
            addr = ipaddress.ip_address(peer)
            if addr.is_private or addr.is_loopback:
                return forwarded.split(",")[0].strip() or peer
        except ValueError:
            pass
    return peer


limiter = Limiter(key_func=_client_ip)

app = FastAPI(
    title="Gryvia API Gateway",
    description="REST API for Gryvia Web UI",
    version="1.0.0",
)
app.state.limiter = limiter
app.add_exception_handler(RateLimitExceeded, _rate_limit_exceeded_handler)
# Request metrics; /metrics itself exists only when GRYVIA_METRICS_TOKEN is set (routers/observability.py).
observability.install(app)
from routers import audit  # noqa: E402

audit.install(app)  # who changed what; see routers/audit.py

# CORS middleware - restrict origins via environment variable
ALLOWED_ORIGINS = os.environ.get("CORS_ALLOWED_ORIGINS", "").split(",")
if ALLOWED_ORIGINS == [""]:
    ALLOWED_ORIGINS = ["http://localhost:3000", "http://localhost:5173"]

app.add_middleware(
    CORSMiddleware,
    allow_origins=ALLOWED_ORIGINS,
    allow_credentials=True,
    allow_methods=["GET", "POST", "PUT", "DELETE"],
    allow_headers=["Authorization", "Content-Type"],
)

# API key for authentication (from environment or mounted secret)
API_KEY = os.environ.get("GRYVIA_API_KEY", "").strip()
# The well-known lab key shipped as the install default. The dashboard warns while it is in use.
DEFAULT_LAB_KEY = "Admin@321"
LOGIN_USERNAME = "admin"


# Browser sessions. Login exchanges the API key for a signed, expiring token so the long-lived key is
# never kept in the browser. Tokens are stateless (HMAC-SHA256), tied to the API key (rotating the key
# ends every session), and end at expiry.
SESSION_PREFIX = "gs1."
SESSION_TTL_SECONDS = int(os.environ.get("GRYVIA_SESSION_TTL_SECONDS", "28800"))


def _b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def _b64url_decode(text: str) -> bytes:
    return base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))


@functools.lru_cache(maxsize=4)
def _derive_session_key(material: str) -> bytes:
    # The signing key is derived from the API key with a slow KDF; cached so each request does not pay for it.
    return hashlib.pbkdf2_hmac(
        "sha256", material.encode(), b"gryvia-session-v1", 200_000
    )


def _session_key() -> bytes:
    material = os.environ.get("GRYVIA_SESSION_SECRET", "").strip() or API_KEY
    return _derive_session_key(material)


def issue_session_token(
    subject: str = "admin", ttl: Optional[int] = None
) -> Dict[str, Any]:
    now = int(time.time())
    exp = now + (SESSION_TTL_SECONDS if ttl is None else ttl)
    payload = _b64url(
        json.dumps(
            {"sub": subject, "iat": now, "exp": exp}, separators=(",", ":")
        ).encode()
    )
    sig = _b64url(hmac.new(_session_key(), payload.encode(), hashlib.sha256).digest())
    return {"token": f"{SESSION_PREFIX}{payload}.{sig}", "expiresAt": exp}


def verify_session_token(token: str) -> Optional[Dict[str, Any]]:
    """Return the claims of a valid, unexpired session token, else None."""
    if not token.startswith(SESSION_PREFIX) or not API_KEY:
        return None
    try:
        payload, sig = token[len(SESSION_PREFIX) :].split(".", 1)
        expected = _b64url(
            hmac.new(_session_key(), payload.encode(), hashlib.sha256).digest()
        )
        if not hmac.compare_digest(sig.encode(), expected.encode()):
            return None
        claims = json.loads(_b64url_decode(payload))
        if not isinstance(claims, dict) or int(claims.get("exp", 0)) <= int(
            time.time()
        ):
            return None
        return claims
    except (ValueError, TypeError, UnicodeError, json.JSONDecodeError):
        return None


def using_default_key() -> bool:
    return bool(API_KEY) and hmac.compare_digest(
        API_KEY.encode(), DEFAULT_LAB_KEY.encode()
    )


# OIDC configuration
OIDC_ENABLED = os.environ.get("OIDC_ENABLED", "false").lower() in ("true", "1", "yes")
OIDC_ISSUER_URL = os.environ.get("OIDC_ISSUER_URL", "").strip().rstrip("/")
OIDC_CLIENT_ID = os.environ.get("OIDC_CLIENT_ID", "").strip()
OIDC_AUDIENCE = os.environ.get("OIDC_AUDIENCE", "").strip() or OIDC_CLIENT_ID

# JWKS cache for OIDC token validation
_jwks_cache: Dict[str, Any] = {}
_jwks_cache_time: float = 0
_JWKS_CACHE_TTL = 3600  # 1 hour
# A token naming an unknown key forces a refresh (the provider may have rotated keys). Unauthenticated
# callers can send such tokens, so refresh at most once per interval to avoid hammering the provider.
_JWKS_MIN_REFRESH_SECONDS = 30
_oidc_discovery: Optional[Dict[str, Any]] = None


async def _fetch_oidc_discovery() -> Dict[str, Any]:
    """Fetch and cache the OIDC discovery document."""
    global _oidc_discovery
    if _oidc_discovery is not None:
        return _oidc_discovery
    discovery_url = f"{OIDC_ISSUER_URL}/.well-known/openid-configuration"
    async with httpx.AsyncClient(timeout=10) as http_client:
        resp = await http_client.get(discovery_url)
        resp.raise_for_status()
        _oidc_discovery = resp.json()
        return _oidc_discovery


async def _get_jwks(force: bool = False) -> Dict[str, Any]:
    """Fetch and cache JWKS from the OIDC provider. `force` skips the cache (a key rotation was detected)."""
    global _jwks_cache, _jwks_cache_time

    now = time.monotonic()
    if not force and _jwks_cache and (now - _jwks_cache_time) < _JWKS_CACHE_TTL:
        return _jwks_cache

    discovery = await _fetch_oidc_discovery()
    jwks_uri = discovery.get("jwks_uri")
    if not jwks_uri:
        raise HTTPException(status_code=500, detail="OIDC provider has no jwks_uri")

    async with httpx.AsyncClient(timeout=10) as http_client:
        resp = await http_client.get(jwks_uri)
        resp.raise_for_status()
        _jwks_cache = resp.json()
        _jwks_cache_time = now
        return _jwks_cache


async def _fetch_jwks_or_503(force: bool = False) -> Dict[str, Any]:
    """_get_jwks, with an unreachable or broken identity provider reported as 503, not a crash."""
    try:
        return await _get_jwks(force)
    except HTTPException:
        raise
    except Exception:
        logger.warning("Failed to fetch OIDC signing keys")
        raise HTTPException(status_code=503, detail="OIDC provider unreachable")


async def _validate_jwt_token(token: str) -> Dict[str, Any]:
    """Validate a JWT token against the OIDC provider's JWKS."""
    try:
        # Decode header without verification to get the key id
        unverified_header = jwt.get_unverified_header(token)
    except JWTError:
        raise HTTPException(status_code=401, detail="Invalid token header")

    jwks_data = await _fetch_jwks_or_503()
    kid = unverified_header.get("kid")

    # Find the matching key
    rsa_key: Dict[str, Any] = {}
    for key in jwks_data.get("keys", []):
        if key.get("kid") == kid:
            rsa_key = key
            break

    if not rsa_key:
        # Key not found - maybe keys rotated, refresh the cache and retry once. Skipped when the keys
        # were fetched moments ago, so unknown-kid tokens cannot force a fetch per request.
        if time.monotonic() - _jwks_cache_time >= _JWKS_MIN_REFRESH_SECONDS:
            jwks_data = await _fetch_jwks_or_503(force=True)
            for key in jwks_data.get("keys", []):
                if key.get("kid") == kid:
                    rsa_key = key
                    break

    if not rsa_key:
        raise HTTPException(
            status_code=401, detail="Unable to find matching signing key"
        )

    try:
        payload = jwt.decode(
            token,
            rsa_key,
            algorithms=["RS256", "RS384", "RS512", "ES256", "ES384"],
            audience=OIDC_AUDIENCE,
            issuer=OIDC_ISSUER_URL,
            options={
                "require_exp": True,
                "require_iss": True,
                "require_aud": True,
                "require_sub": True,
            },
        )
        return payload
    except jwt.ExpiredSignatureError:
        raise HTTPException(status_code=401, detail="Token has expired")
    except jwt.JWTClaimsError as e:
        raise HTTPException(status_code=401, detail=f"Invalid token claims: {e}")
    except (JWTError, JOSEError):
        raise HTTPException(status_code=401, detail="Invalid token")


def _extract_tenant_namespaces(claims: Dict[str, Any]) -> Optional[List[str]]:
    """Extract tenant namespaces from JWT claims (org or groups)."""
    # Check for org claim (single tenant)
    org = claims.get("org")
    if org and isinstance(org, str):
        return [org]

    # Check for groups claim (multi-tenant)
    groups = claims.get("groups")
    if groups and isinstance(groups, list):
        return [g for g in groups if isinstance(g, str)]

    return None


# Role model. API key and browser-session callers are the provider administrator ("admin"). An OIDC user
# is a tenant user ("tenant") unless its token carries a group listed in GRYVIA_OIDC_ADMIN_GROUPS
# (comma separated, matched against the `groups` claim; default none, so nobody is admin via OIDC).
# A tenant user's namespaces come from the GryviaTenant objects its `org`/`groups` claim matches (by tenant
# name or namespace `tenant-<name>`); a token can never name an arbitrary namespace, and a claim that
# matches no tenant is refused with 403.
# Compatibility escape hatch: with GRYVIA_OIDC_LEGACY_NAMESPACES=1 AND no GryviaTenant objects at all, the
# old behaviour (claim values used as namespaces) still applies, so existing OIDC installs keep working
# until tenants are created. Off by default.
OIDC_ADMIN_GROUPS = {
    g.strip()
    for g in os.environ.get("GRYVIA_OIDC_ADMIN_GROUPS", "").split(",")
    if g.strip()
}
OIDC_LEGACY_NAMESPACES = os.environ.get("GRYVIA_OIDC_LEGACY_NAMESPACES", "") in (
    "1",
    "true",
    "yes",
)


def _set_admin_state(request: Optional[Request]) -> None:
    if request is not None:
        request.state.user_claims = None
        request.state.auth_method = "api_key"
        request.state.role = "admin"
        request.state.tenant = None
        request.state.tenants = []
        request.state.tenant_namespaces = None


async def _resolve_oidc_identity(request: Request, claims: Dict[str, Any]) -> None:
    """Set role/tenant/tenant_namespaces on the request for a validated OIDC token (raises 403/503)."""
    request.state.user_claims = claims
    request.state.auth_method = "oidc"
    groups = claims.get("groups")
    if (
        OIDC_ADMIN_GROUPS
        and isinstance(groups, list)
        and OIDC_ADMIN_GROUPS.intersection(g for g in groups if isinstance(g, str))
    ):
        request.state.role = "admin"
        request.state.tenant = None
        request.state.tenants = []
        request.state.tenant_namespaces = None
        return

    request.state.role = "tenant"
    candidates = _extract_tenant_namespaces(claims) or []
    items = await tenancy.list_tenants(k8s_custom)
    matched = tenancy.match_tenants(items, candidates)
    if matched:
        names = [tenancy.tenant_name(t) for t in matched]
        request.state.tenants = names
        request.state.tenant = names[0]
        request.state.tenant_namespaces = [tenancy.namespace_of(n) for n in names]
        return
    if not items and OIDC_LEGACY_NAMESPACES:
        request.state.tenants = []
        request.state.tenant = None
        request.state.tenant_namespaces = candidates or None
        return
    raise HTTPException(
        status_code=403,
        detail="Your account is not mapped to a Gryvia tenant. Ask the provider to create a tenant "
        "matching your organization or group.",
    )


async def verify_auth(
    authorization: Optional[str] = Header(None), request: Request = None
):
    """Authenticate, counting the outcome by method (never by identity) for /metrics."""
    method = ["none"]
    try:
        await _verify_auth(authorization, request, method)
    except HTTPException:
        observability.record_auth(method[0], False)
        raise
    observability.record_auth(method[0], True)


async def _verify_auth(
    authorization: Optional[str], request: Optional[Request], method: List[str]
):
    """Verify API key or OIDC JWT token for all protected endpoints.

    Authentication priority:
    1. OIDC JWT token (when OIDC_ENABLED=true and Bearer token is a JWT)
    2. API key (Bearer token matched against GRYVIA_API_KEY)

    Sets request.state.role ("admin" | "tenant"), .tenant, .tenants, .tenant_namespaces and the OIDC claims.
    """
    if not authorization:
        raise HTTPException(status_code=401, detail="Authorization header required")

    token = authorization.removeprefix("Bearer ").strip()

    if not token:
        raise HTTPException(status_code=401, detail="Authorization token required")

    # Signed browser session issued by /api/auth/login
    if token.startswith(SESSION_PREFIX):
        method[0] = "session"
        if verify_session_token(token) is None:
            raise HTTPException(status_code=401, detail="Session expired or invalid")
        _set_admin_state(request)
        return

    # Try OIDC validation first when enabled
    if OIDC_ENABLED and OIDC_ISSUER_URL:
        # Heuristic: JWTs have 3 dot-separated parts
        if token.count(".") == 2:
            claims = None
            method[0] = "oidc"
            try:
                claims = await _validate_jwt_token(token)
            except HTTPException:
                # If OIDC validation fails, fall through to API key check
                pass
            if claims is not None:
                # A valid token is never re-tried as an API key; tenant problems are final (403/503).
                if request is not None:
                    await _resolve_oidc_identity(request, claims)
                return

    # Fall back to API key authentication
    method[0] = "api_key"
    if not API_KEY:
        raise HTTPException(status_code=401, detail="Authentication required")
    if not hmac.compare_digest(token, API_KEY):
        raise HTTPException(status_code=403, detail="Invalid credentials")

    _set_admin_state(request)


# Initialize Kubernetes client
try:
    config.load_incluster_config()
    logger.info("Loaded in-cluster Kubernetes config")
except config.ConfigException:
    try:
        config.load_kube_config()
        logger.info("Loaded local kubeconfig")
    except config.ConfigException as e:
        logger.error("Failed to load Kubernetes config: %s", e)
        raise

k8s_custom = client.CustomObjectsApi()
k8s_core = client.CoreV1Api()


def _validate_prometheus_url(url: str) -> str:
    parsed = urllib.parse.urlparse(url)
    hostname = parsed.hostname or ""
    # Block common SSRF targets
    blocked = ["169.254.169.254", "metadata.google.internal", "metadata.aws"]
    for b in blocked:
        if hostname == b or hostname.endswith("." + b):
            raise ValueError(f"Blocked Prometheus URL pointing to {hostname}")
    return url


# Prometheus client (optional - used for historical metrics when available)
PROMETHEUS_URL = _validate_prometheus_url(
    os.environ.get("PROMETHEUS_URL", "http://prometheus-operated.gryvia-system:9090")
)
HTTP_TIMEOUT_SECONDS = int(os.environ.get("HTTP_TIMEOUT_SECONDS", "10"))
prom = None
try:
    from prometheus_api_client import PrometheusConnect
    import requests as _requests

    class _TimeoutSession(_requests.Session):
        """requests.Session subclass that enforces a default timeout."""

        def __init__(self, timeout: int = HTTP_TIMEOUT_SECONDS):
            super().__init__()
            self._default_timeout = timeout

        def request(self, *args, **kwargs):
            kwargs.setdefault("timeout", self._default_timeout)
            return super().request(*args, **kwargs)

    if PROMETHEUS_URL:
        prom = PrometheusConnect(
            url=PROMETHEUS_URL, disable_ssl=PROMETHEUS_URL.startswith("http://")
        )
        prom._session = _TimeoutSession(timeout=HTTP_TIMEOUT_SECONDS)
        logger.info(
            "Connected to Prometheus at %s (timeout=%ds)",
            PROMETHEUS_URL,
            HTTP_TIMEOUT_SECONDS,
        )
except Exception:
    logger.warning(
        "Failed to connect to Prometheus at %s - historical metrics unavailable",
        PROMETHEUS_URL,
    )

# Namespace for job queries (configurable)
JOB_NAMESPACE = os.environ.get("GRYVIA_JOB_NAMESPACE", "default")

from routers import Deps, register_routers  # noqa: E402

deps = Deps(
    verify_auth=verify_auth,
    k8s_custom=k8s_custom,
    k8s_core=k8s_core,
    limiter=limiter,
    job_namespace=JOB_NAMESPACE,
    collector_urls=[
        u.strip().rstrip("/")
        for u in os.environ.get("GRYVIA_COLLECTOR_URLS", "").split(",")
        if u.strip()
    ]
    or None,
    flight_token=os.environ.get("GRYVIA_FLIGHT_TOKEN", "").strip() or None,
    flight_collector_namespace=os.environ.get(
        "GRYVIA_FLIGHT_COLLECTOR_NAMESPACE", "gryvia-network"
    ),
    netra_url=os.environ.get("GRYVIA_NETRA_URL", "").strip().rstrip("/") or None,
    netra_token=os.environ.get("GRYVIA_NETRA_TOKEN", "").strip() or None,
    netra_verify_tls=(
        os.environ.get("GRYVIA_NETRA_INSECURE", "") != "1"
        and (os.environ.get("GRYVIA_SOVEREIGN_CA_FILE", "").strip() or True)
    ),
    llm_key_namespace=os.environ.get("GRYVIA_LLM_KEY_NAMESPACE", "").strip() or None,
    llm_gateway_url=os.environ.get("GRYVIA_LLM_GATEWAY_URL", "").strip().rstrip("/")
    or None,
    zyntra_url=os.environ.get("GRYVIA_ZYNTRA_URL", "").strip().rstrip("/") or None,
    zyntra_token=os.environ.get("GRYVIA_ZYNTRA_TOKEN", "").strip() or None,
    zyntra_console_url=os.environ.get("GRYVIA_ZYNTRA_CONSOLE_URL", "")
    .strip()
    .rstrip("/")
    or None,
    netra_console_url=os.environ.get("GRYVIA_NETRA_CONSOLE_URL", "").strip().rstrip("/")
    or None,
    sovereign_ca_file=os.environ.get("GRYVIA_SOVEREIGN_CA_FILE", "").strip() or None,
    require_prod_approval=os.environ.get("GRYVIA_REQUIRE_PROD_APPROVAL", "") == "1",
    intelligence_actions=os.environ.get("GRYVIA_INTELLIGENCE_ACTIONS", "") == "1",
    submission_admission=os.environ.get("GRYVIA_SUBMISSION_ADMISSION_ENFORCE", "")
    == "1",
    intelligence_retention_days=max(
        0,
        min(
            3650, int(os.environ.get("GRYVIA_INTELLIGENCE_RETENTION_DAYS", "30") or 30)
        ),
    ),
)
register_routers(app, deps)


def _query_namespaces(request: Request) -> List[str]:
    """Namespaces the caller may read jobs from: its tenant namespaces, else the gateway's job namespace."""
    tenant_ns = getattr(request.state, "tenant_namespaces", None)
    return list(tenant_ns) if tenant_ns else [JOB_NAMESPACE]


def _quota_visible(request: Request, quota: Dict[str, Any]) -> bool:
    """Admins see every quota; a tenant only quotas whose spec.namespaces intersect its namespaces."""
    if getattr(request.state, "role", None) == "admin":
        return True
    mine = set(_query_namespaces(request))
    return bool(mine.intersection((quota.get("spec") or {}).get("namespaces") or []))


async def _list_jobs(request: Request) -> Dict[str, Any]:
    """GryviaAIJobs from the caller's namespaces, as {"items": [...]}."""
    loop = asyncio.get_running_loop()
    items: List[dict] = []
    for ns in _query_namespaces(request):
        res = await loop.run_in_executor(
            None,
            lambda ns=ns: k8s_custom.list_namespaced_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                namespace=ns,
                plural="gryviaaijobs",
            ),
        )
        items.extend(res.get("items", []))
    return {"items": items}


def _node_gpu_status(status: Dict[str, Any]) -> List[Dict[str, Any]]:
    """Per-GPU status of a GryviaGpuNode: ``status.gpuStatus`` (older objects used ``gpus``)."""
    return status.get("gpuStatus") or status.get("gpus") or []


@app.get("/")
async def root():
    return {"status": "healthy", "service": "gryvia-api-gateway"}


@app.get("/health")
async def health():
    return {"status": "ok"}


@app.get("/api/cluster/stats")
@limiter.limit("30/minute")
async def get_cluster_stats(request: Request, _=Depends(verify_auth)):
    """Get overall cluster statistics"""
    try:
        loop = asyncio.get_running_loop()

        # Nodes are cluster-wide (admin only); jobs come from the caller's namespaces. A tenant user
        # gets its own job counts and zero node/GPU capacity figures.
        is_admin = getattr(request.state, "role", None) == "admin"

        async def _nodes() -> Dict[str, Any]:
            if not is_admin:
                return {"items": []}
            return await loop.run_in_executor(
                None,
                lambda: k8s_custom.list_cluster_custom_object(
                    group="gryvia.io", version="v1alpha1", plural="gryviagpunodes"
                ),
            )

        nodes, jobs = await asyncio.gather(_nodes(), _list_jobs(request))

        total_gpus = 0
        available_gpus = 0
        allocated_gpus = 0
        gpu_utilization_sum = 0
        gpu_count = 0

        for node in nodes.get("items", []):
            node_gpus = node.get("spec", {}).get("gpuCount", 0)
            total_gpus += node_gpus

            # Get GPU metrics from status
            status = node.get("status", {})
            for gpu in _node_gpu_status(status):
                gpu_utilization_sum += gpu.get("utilization", 0)
                gpu_count += 1

        # Count allocated GPUs from running jobs
        for job in jobs.get("items", []):
            status = job.get("status", {})
            if normalize_phase(status.get("phase")) == "running":
                allocated_gpus += job.get("spec", {}).get(
                    "gpus", job.get("spec", {}).get("resources", {}).get("gpuCount", 0)
                )

        available_gpus = max(0, total_gpus - allocated_gpus)
        avg_utilization = gpu_utilization_sum / gpu_count if gpu_count > 0 else 0

        # Count jobs by status (case-insensitive; see phases.py)
        job_counts = count_phases(
            job.get("status", {}).get("phase") for job in jobs.get("items", [])
        )

        return {
            "totalGPUs": total_gpus,
            "availableGPUs": available_gpus,
            "allocatedGPUs": allocated_gpus,
            "utilizationPercent": round(avg_utilization, 1),
            "totalJobs": len(jobs.get("items", [])),
            "runningJobs": job_counts["running"],
            "pendingJobs": job_counts["pending"],
            "completedJobs": job_counts["completed"],
            "failedJobs": job_counts["failed"],
            "totalNodes": len(nodes.get("items", [])),
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
    except Exception as e:
        logger.error("Error getting cluster stats: %s", e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to retrieve cluster stats")


@app.get("/api/metrics/gpu")
@limiter.limit("30/minute")
async def get_gpu_metrics(
    request: Request,
    time_range: str = Query(
        "1h",
        description="Time range (1h, 6h, 24h, 7d) - reserved for Prometheus integration",
    ),
    _=Depends(verify_auth),
    __=Depends(require_admin),
):
    """Get GPU utilization metrics over time"""
    allowed_ranges = {"1h", "6h", "24h", "7d", "30d"}
    if time_range not in allowed_ranges:
        raise HTTPException(
            status_code=400,
            detail=f"Invalid time_range. Must be one of: {', '.join(sorted(allowed_ranges))}",
        )
    try:
        loop = asyncio.get_running_loop()

        # Query GPU node status for current metrics
        nodes = await loop.run_in_executor(
            None,
            lambda: k8s_custom.list_cluster_custom_object(
                group="gryvia.io", version="v1alpha1", plural="gryviagpunodes"
            ),
        )

        metrics = []
        for node in nodes.get("items", []):
            spec = node.get("spec", {})
            status = node.get("status", {})
            for gpu in _node_gpu_status(status):
                metrics.append(
                    {
                        "node": spec.get("nodeName", "unknown"),
                        "gpuIndex": gpu.get("index", 0),
                        "utilization": gpu.get("utilization", 0),
                        "temperature": gpu.get("temperature", 0),
                        "memoryUsed": gpu.get("memoryUsed", 0),
                        "memoryTotal": gpu.get("memoryTotal", 0),
                        "timestamp": datetime.now(timezone.utc).isoformat(),
                    }
                )

        return {"timeRange": time_range, "metrics": metrics}
    except Exception as e:
        logger.error("Error getting GPU metrics: %s", e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to retrieve GPU metrics")


@app.get("/api/metrics/costs")
@limiter.limit("30/minute")
async def get_cost_metrics(request: Request, _=Depends(verify_auth)):
    """Cost metrics for the caller's scope (admin: everything, tenant: its own namespaces).

    Metered GryviaUsageRecords are preferred when any exist; otherwise costs are computed on the fly from
    job wall-clock time, priced from the GryviaGpuSku catalog (built-in table when there are no SKUs).
    """
    try:
        loop = asyncio.get_running_loop()
        is_admin = getattr(request.state, "role", None) == "admin"

        # Get all quotas with budget info (only used to map namespaces to team names)
        quotas = await loop.run_in_executor(
            None,
            lambda: k8s_custom.list_cluster_custom_object(
                group="gryvia.io", version="v1alpha1", plural="gryviaquotas"
            ),
        )

        # namespace -> team from GryviaQuota (spec.namespaces), used to group costs
        ns_team: Dict[str, str] = {}
        for q in quotas.get("items", []):
            qspec = q.get("spec") or {}
            if qspec.get("team"):
                for qns in qspec.get("namespaces") or []:
                    ns_team.setdefault(qns, qspec["team"])

        team_costs = defaultdict(float)
        gpu_type_costs = defaultdict(lambda: {"cost": 0.0, "hours": 0.0})
        source = "jobs"

        try:
            records = await fetch_records(
                k8s_custom, None if is_admin else _query_namespaces(request)
            )
        except (
            Exception
        ):  # noqa: BLE001 - CRD not installed / not readable: compute from jobs
            records = []

        if records:
            source = "usage-records"
            for rec in records:
                rspec = rec.get("spec") or {}
                rns = (rec.get("metadata") or {}).get("namespace")
                team = ns_team.get(rns) or rspec.get("tenant") or "unassigned"
                cost = float(rspec.get("cost") or 0)
                hours = float(rspec.get("gpuHours") or 0)
                team_costs[team] += cost
                gtype = rspec.get("gpuType") or rspec.get("sku") or "unknown"
                gpu_type_costs[gtype]["cost"] += cost
                gpu_type_costs[gtype]["hours"] += hours
        else:
            rates = await load_rates(k8s_custom)
            jobs = await _list_jobs(request)
            for job in jobs.get("items", []):
                status = job.get("status", {})
                spec = job.get("spec", {})

                if is_billable(status.get("phase")):
                    gpu_type = spec.get(
                        "gpuType", spec.get("resources", {}).get("gpuType", "unknown")
                    )
                    gpu_count = spec.get(
                        "gpus", spec.get("resources", {}).get("gpuCount", 0)
                    )

                    # Calculate hours
                    start_time = status.get("startTime")
                    end_time = (
                        status.get("completionTime")
                        or datetime.now(timezone.utc).isoformat()
                    )

                    if start_time:
                        try:
                            start = datetime.fromisoformat(
                                start_time.replace("Z", "+00:00")
                            )
                            end = datetime.fromisoformat(
                                end_time.replace("Z", "+00:00")
                            )
                        except (ValueError, TypeError):
                            continue
                        hours = (end - start).total_seconds() / 3600

                        cost = hours * gpu_count * rates.get(gpu_type, 1.0)

                        # Add to team costs (use namespace or label as team identifier)
                        jmeta = job.get("metadata") or {}
                        team = (
                            ns_team.get(jmeta.get("namespace"))
                            or (jmeta.get("labels") or {}).get("gryvia.io/team")
                            or "unassigned"
                        )
                        team_costs[team] += cost

                        # Add to GPU type costs
                        gpu_type_costs[gpu_type]["cost"] += cost
                        gpu_type_costs[gpu_type]["hours"] += hours * gpu_count

        current_month_cost = sum(team_costs.values())

        # Historical monthly data requires Prometheus integration
        monthly_data = []
        has_historical_data = False

        # Format team costs
        by_team = [
            {"team": team, "cost": round(cost, 2)} for team, cost in team_costs.items()
        ]

        # Format GPU type costs
        by_gpu_type = [
            {
                "type": gpu_type,
                "cost": round(data["cost"], 2),
                "hours": round(data["hours"], 1),
            }
            for gpu_type, data in gpu_type_costs.items()
        ]

        return {
            "monthly": monthly_data,
            "hasHistoricalData": has_historical_data,
            "byTeam": by_team,
            "byGPUType": by_gpu_type,
            "totalCost": round(current_month_cost, 2),
            "scope": "all-time",
            "source": source,
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
    except Exception as e:
        logger.error("Error getting cost metrics: %s", e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to retrieve cost metrics")


@app.get("/api/metrics/jobs")
@limiter.limit("30/minute")
async def get_job_metrics(
    request: Request,
    time_range: str = Query("24h", description="Time range"),
    _=Depends(verify_auth),
):
    """Get job metrics over time"""
    try:
        jobs = await _list_jobs(request)

        # Calculate job statistics
        total_jobs = len(jobs.get("items", []))
        by_status = defaultdict(int)
        by_framework = defaultdict(int)
        avg_duration = 0
        duration_count = 0

        for job in jobs.get("items", []):
            status = job.get("status", {})
            spec = job.get("spec", {})

            phase = normalize_phase(status.get("phase")).capitalize() or "Unknown"
            by_status[phase] += 1

            framework = spec.get("framework", spec.get("type", "unknown"))
            by_framework[framework] += 1

            # Calculate duration for completed jobs
            if status.get("startTime") and status.get("completionTime"):
                try:
                    start = datetime.fromisoformat(
                        status["startTime"].replace("Z", "+00:00")
                    )
                    end = datetime.fromisoformat(
                        status["completionTime"].replace("Z", "+00:00")
                    )
                except (ValueError, TypeError):
                    continue
                duration = (end - start).total_seconds() / 3600
                avg_duration += duration
                duration_count += 1

        avg_duration = avg_duration / duration_count if duration_count > 0 else 0

        return {
            "totalJobs": total_jobs,
            "byStatus": dict(by_status),
            "byFramework": dict(by_framework),
            "averageDurationHours": round(avg_duration, 2),
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
    except Exception as e:
        logger.error("Error getting job metrics: %s", e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to retrieve job metrics")


@app.get("/api/jobs")
@limiter.limit("30/minute")
async def list_jobs(
    request: Request,
    limit: int = Query(500, ge=1, le=1000),
    offset: int = Query(0, ge=0),
    _=Depends(verify_auth),
):
    """List all jobs with pagination, filtered by tenant when using OIDC."""
    try:
        loop = asyncio.get_running_loop()

        # Determine which namespaces to query based on tenant
        query_namespaces = _query_namespaces(request)

        # Fetch jobs from all tenant namespaces
        all_items: List[dict] = []
        for ns in query_namespaces:
            jobs = await loop.run_in_executor(
                None,
                lambda ns=ns: k8s_custom.list_namespaced_custom_object(
                    group="gryvia.io",
                    version="v1alpha1",
                    namespace=ns,
                    plural="gryviaaijobs",
                ),
            )
            all_items.extend(jobs.get("items", []))

        total = len(all_items)
        items = all_items[offset : offset + limit]

        return {
            "items": items,
            "total": total,
            "limit": limit,
            "offset": offset,
        }
    except Exception as e:
        logger.error("Error listing jobs: %s", e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to list jobs")


@app.get("/api/jobs/{name}")
@limiter.limit("30/minute")
async def get_job(request: Request, name: str, _=Depends(verify_auth)):
    """Get a specific job by name"""
    try:
        loop = asyncio.get_running_loop()

        # Only the caller's namespaces are searched, so another tenant's job is a 404.
        job = None
        for ns in _query_namespaces(request):
            try:
                job = await loop.run_in_executor(
                    None,
                    lambda ns=ns: k8s_custom.get_namespaced_custom_object(
                        group="gryvia.io",
                        version="v1alpha1",
                        namespace=ns,
                        plural="gryviaaijobs",
                        name=name,
                    ),
                )
                break
            except client.ApiException as e:
                if e.status != 404:
                    raise
        if job is None:
            raise HTTPException(status_code=404, detail=f"Job '{name}' not found")
        return job
    except HTTPException:
        raise
    except client.ApiException as e:
        if e.status == 404:
            raise HTTPException(status_code=404, detail=f"Job '{name}' not found")
        raise HTTPException(status_code=500, detail="Failed to get job")
    except Exception as e:
        logger.error("Error getting job %s: %s", name, e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to get job")


@app.post("/api/jobs/preflight")
@limiter.limit("10/minute")
async def preflight_job(request: Request, _=Depends(verify_auth)):
    """Preview real Kubernetes admission without persisting a job."""
    target_ns = _query_namespaces(request)[0]
    body = await submission.read_job(request, target_ns)
    return await submission.admit_job(
        k8s_custom, body, target_ns, enforced=deps.submission_admission
    )


@app.post("/api/jobs")
@limiter.limit("10/minute")
async def create_job(request: Request, _=Depends(verify_auth)):
    """Create a new job. With apiGateway.submissionAdmission.enforce every create rechecks admission first."""
    if not deps.submission_admission:
        return await _create_job_unchecked(request)
    target_ns = _query_namespaces(request)[0]
    body = await submission.read_job(request, target_ns)
    await submission.admit_job(k8s_custom, body, target_ns)
    try:
        return await submission.run(
            k8s_custom.create_namespaced_custom_object,
            group="gryvia.io",
            version="v1alpha1",
            namespace=target_ns,
            plural="gryviaaijobs",
            body=body,
            field_validation="Strict",
        )
    except client.ApiException as exc:
        raise HTTPException(
            status_code=exc.status or 500, detail="Failed to create job"
        ) from None
    except Exception:
        logger.exception("Error creating job")
        raise HTTPException(status_code=503, detail="Failed to create job") from None


async def _create_job_unchecked(request: Request):
    try:
        loop = asyncio.get_running_loop()
        body = await request.json()

        # Validate required fields
        if not isinstance(body, dict):
            raise HTTPException(
                status_code=400, detail="Request body must be a JSON object"
            )
        if body.get("apiVersion") != "gryvia.io/v1alpha1":
            raise HTTPException(
                status_code=400, detail="apiVersion must be gryvia.io/v1alpha1"
            )
        if body.get("kind") != "GryviaAIJob":
            raise HTTPException(status_code=400, detail="kind must be GryviaAIJob")

        # Validate spec contains required fields
        spec = body.get("spec", {})
        if not isinstance(spec, dict):
            raise HTTPException(status_code=400, detail="spec must be a JSON object")
        if not spec.get("image"):
            raise HTTPException(status_code=400, detail="spec.image is required")
        if not spec.get("gpus") and not spec.get("gpuCount"):
            raise HTTPException(status_code=400, detail="spec.gpus is required")

        # Enforce namespace server-side to prevent namespace bypass
        # When using OIDC with tenant namespaces, use the first tenant namespace
        target_ns = _query_namespaces(request)[0]
        if not isinstance(body.get("metadata"), dict):
            body["metadata"] = {}
        body["metadata"]["namespace"] = target_ns

        job = await loop.run_in_executor(
            None,
            lambda: k8s_custom.create_namespaced_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                namespace=target_ns,
                plural="gryviaaijobs",
                body=body,
            ),
        )
        return job
    except HTTPException:
        raise
    except client.ApiException as e:
        raise HTTPException(status_code=e.status or 500, detail="Failed to create job")
    except Exception as e:
        logger.error("Error creating job: %s", e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to create job")


@app.delete("/api/jobs/{name}")
@limiter.limit("10/minute")
async def delete_job(request: Request, name: str, _=Depends(verify_auth)):
    """Delete a job by name"""
    try:
        loop = asyncio.get_running_loop()

        deleted = False
        for ns in _query_namespaces(request):
            try:
                await loop.run_in_executor(
                    None,
                    lambda ns=ns: k8s_custom.delete_namespaced_custom_object(
                        group="gryvia.io",
                        version="v1alpha1",
                        namespace=ns,
                        plural="gryviaaijobs",
                        name=name,
                    ),
                )
                deleted = True
                break
            except client.ApiException as e:
                if e.status != 404:
                    raise
        if not deleted:
            raise HTTPException(status_code=404, detail=f"Job '{name}' not found")
        return {"status": "deleted", "name": name}
    except HTTPException:
        raise
    except client.ApiException as e:
        if e.status == 404:
            raise HTTPException(status_code=404, detail=f"Job '{name}' not found")
        raise HTTPException(status_code=500, detail="Failed to delete job")
    except Exception as e:
        logger.error("Error deleting job %s: %s", name, e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to delete job")


@app.get("/api/quotas")
@limiter.limit("30/minute")
async def list_quotas(
    request: Request,
    limit: int = Query(500, ge=1, le=1000),
    offset: int = Query(0, ge=0),
    _=Depends(verify_auth),
):
    """List all quotas with pagination"""
    try:
        loop = asyncio.get_running_loop()

        quotas = await loop.run_in_executor(
            None,
            lambda: k8s_custom.list_cluster_custom_object(
                group="gryvia.io", version="v1alpha1", plural="gryviaquotas"
            ),
        )

        all_items = [q for q in quotas.get("items", []) if _quota_visible(request, q)]
        total = len(all_items)
        items = all_items[offset : offset + limit]

        return {
            "items": items,
            "total": total,
            "limit": limit,
            "offset": offset,
        }
    except Exception as e:
        logger.error("Error listing quotas: %s", e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to list quotas")


@app.get("/api/quotas/{name}")
@limiter.limit("30/minute")
async def get_quota(request: Request, name: str, _=Depends(verify_auth)):
    """Get a specific quota by name"""
    try:
        loop = asyncio.get_running_loop()

        quota = await loop.run_in_executor(
            None,
            lambda: k8s_custom.get_cluster_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                plural="gryviaquotas",
                name=name,
            ),
        )
        if not _quota_visible(request, quota):
            raise HTTPException(status_code=404, detail=f"Quota '{name}' not found")
        return quota
    except HTTPException:
        raise
    except client.ApiException as e:
        if e.status == 404:
            raise HTTPException(status_code=404, detail=f"Quota '{name}' not found")
        raise HTTPException(status_code=500, detail="Failed to get quota")
    except Exception as e:
        logger.error("Error getting quota %s: %s", name, e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to get quota")


@app.get("/api/nodes")
@limiter.limit("30/minute")
async def list_nodes(
    request: Request,
    limit: int = Query(500, ge=1, le=1000),
    offset: int = Query(0, ge=0),
    _=Depends(verify_auth),
    __=Depends(require_admin),
):
    """List all GPU nodes with pagination"""
    try:
        loop = asyncio.get_running_loop()

        nodes = await loop.run_in_executor(
            None,
            lambda: k8s_custom.list_cluster_custom_object(
                group="gryvia.io", version="v1alpha1", plural="gryviagpunodes"
            ),
        )

        all_items = nodes.get("items", [])
        total = len(all_items)
        items = all_items[offset : offset + limit]

        return {
            "items": items,
            "total": total,
            "limit": limit,
            "offset": offset,
        }
    except Exception as e:
        logger.error("Error listing nodes: %s", e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to list nodes")


@app.get("/api/nodes/health")
@limiter.limit("30/minute")
async def get_node_health(
    request: Request,
    limit: int = Query(100, ge=1, le=1000),
    offset: int = Query(0, ge=0),
    _=Depends(verify_auth),
    __=Depends(require_admin),
):
    """Get GPU node health status"""
    try:
        loop = asyncio.get_running_loop()

        nodes = await loop.run_in_executor(
            None,
            lambda: k8s_custom.list_cluster_custom_object(
                group="gryvia.io", version="v1alpha1", plural="gryviagpunodes"
            ),
        )

        health_data = []
        for node in nodes.get("items", []):
            spec = node.get("spec", {})
            status = node.get("status", {})

            # Calculate node health based on GPU metrics
            gpu_health = "Healthy"
            issues = []

            for gpu in _node_gpu_status(status):
                temp = gpu.get("temperature", 0)
                if temp > 90:
                    gpu_health = "Critical"
                    issues.append(
                        f"GPU {gpu.get('index', '?')} critical temperature: {temp}C"
                    )
                elif temp > 85:
                    gpu_health = "Warning"
                    issues.append(
                        f"GPU {gpu.get('index', '?')} high temperature: {temp}C"
                    )

            health_data.append(
                {
                    "nodeName": spec.get("nodeName", "unknown"),
                    "gpuType": spec.get("gpuType", "unknown"),
                    "gpuCount": spec.get("gpuCount", 0),
                    "phase": status.get("phase", "Unknown"),
                    "health": gpu_health,
                    "issues": issues,
                    "rdmaEnabled": bool(
                        spec.get("rdma", spec.get("rdmaEnabled", False))
                    ),
                }
            )

        total = len(health_data)
        health_data = health_data[offset : offset + limit]

        return {
            "nodes": health_data,
            "total": total,
            "limit": limit,
            "offset": offset,
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
    except Exception as e:
        logger.error("Error getting node health: %s", e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to retrieve node health")


# ── Auth endpoints ──────────────────────────────────────────────────


@app.get("/api/nodes/{name}")
@limiter.limit("30/minute")
async def get_node(
    request: Request, name: str, _=Depends(verify_auth), __=Depends(require_admin)
):
    """Get a specific GPU node by name"""
    try:
        loop = asyncio.get_running_loop()

        node = await loop.run_in_executor(
            None,
            lambda: k8s_custom.get_cluster_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                plural="gryviagpunodes",
                name=name,
            ),
        )
        return node
    except client.ApiException as e:
        if e.status == 404:
            raise HTTPException(status_code=404, detail=f"Node '{name}' not found")
        raise HTTPException(status_code=500, detail="Failed to get node")
    except Exception as e:
        logger.error("Error getting node %s: %s", name, e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to get node")


@app.get("/api/quota/usage")
@limiter.limit("30/minute")
async def get_quota_usage(
    request: Request,
    limit: int = Query(100, ge=1, le=1000),
    offset: int = Query(0, ge=0),
    _=Depends(verify_auth),
):
    """Get quota usage across all teams (a tenant sees only its own quotas)"""
    try:
        loop = asyncio.get_running_loop()

        quotas = await loop.run_in_executor(
            None,
            lambda: k8s_custom.list_cluster_custom_object(
                group="gryvia.io", version="v1alpha1", plural="gryviaquotas"
            ),
        )

        usage_data = []
        for quota in quotas.get("items", []):
            if not _quota_visible(request, quota):
                continue
            spec = quota.get("spec", {})
            status = quota.get("status", {})
            current_usage = status.get("currentUsage", {})
            budget_status = status.get("budgetStatus", {})
            gpu_quota = spec.get("gpuQuota", {})
            max_gpus = gpu_quota.get("maxGPUs", 0)

            usage_data.append(
                {
                    "team": spec.get("team", "unknown"),
                    "maxGPUs": max_gpus,
                    "allocatedGPUs": current_usage.get("allocatedGPUs", 0),
                    "utilizationPercent": (
                        round(
                            (current_usage.get("allocatedGPUs", 0) / max_gpus) * 100, 1
                        )
                        if max_gpus > 0
                        else 0
                    ),
                    "runningJobs": current_usage.get("runningJobs", 0),
                    "queuedJobs": current_usage.get("queuedJobs", 0),
                    "monthlyBudget": spec.get("budget", {}).get("monthlyBudget", 0),
                    "spentThisMonth": budget_status.get("spentThisMonth", 0),
                    "remainingBudget": budget_status.get("remainingBudget", 0),
                }
            )

        total = len(usage_data)
        usage_data = usage_data[offset : offset + limit]

        return {
            "quotas": usage_data,
            "total": total,
            "limit": limit,
            "offset": offset,
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
    except Exception as e:
        logger.error("Error getting quota usage: %s", e, exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to retrieve quota usage")


def _login_info() -> dict:
    """What the sign-in page shows: the instance, and where an operator reads the admin key.

    Never the key itself: this is served to anyone who can reach the login page.
    """
    namespace = os.environ.get("GRYVIA_JOB_NAMESPACE", "gryvia-system")
    info: dict = {
        "instance": {
            "product": "Gryvia",
            "version": app.version,
            "namespace": namespace,
        }
    }
    if API_KEY:
        info["credentials"] = {
            "username": "admin",
            "secret": os.environ.get("GRYVIA_API_KEY_SECRET", "").strip()
            or "gryvia-api-key",
            "key": "GRYVIA_API_KEY",
            "namespace": namespace,
        }
    return info


@app.get("/api/auth/config")
async def get_auth_config():
    """Return OIDC provider configuration for the frontend.

    This endpoint is unauthenticated so the login page can fetch it.
    """
    if not OIDC_ENABLED or not OIDC_ISSUER_URL:
        return {
            "oidcEnabled": False,
            "apiKeyEnabled": bool(API_KEY),
            **_login_info(),
        }

    try:
        discovery = await _fetch_oidc_discovery()
    except Exception:
        logger.warning("Failed to fetch OIDC discovery document")
        return {
            "oidcEnabled": False,
            "apiKeyEnabled": bool(API_KEY),
            "error": "OIDC provider unreachable",
            **_login_info(),
        }

    return {
        **_login_info(),
        "oidcEnabled": True,
        "apiKeyEnabled": bool(API_KEY),
        "issuer": OIDC_ISSUER_URL,
        "clientId": OIDC_CLIENT_ID,
        "authorizationEndpoint": discovery.get("authorization_endpoint", ""),
        "tokenEndpoint": discovery.get("token_endpoint", ""),
        "scopes": "openid profile email groups",
    }


class LoginRequest(BaseModel):
    username: str = Field(max_length=128)
    password: str = Field(max_length=512)


@app.post("/api/auth/login")
@limiter.limit("10/minute")
async def login(request: Request, body: LoginRequest):
    """Exchange the dashboard credentials (admin / the API key) for a short-lived session token.

    The password is validated here, on the server, so the web UI carries no credential of its own and
    never stores the API key: it keeps only the expiring signed token. Failed attempts are slowed down
    and rate limited. Scripts can still send the API key itself as the bearer.
    """
    if not API_KEY:
        observability.record_auth("login", False)
        raise HTTPException(status_code=401, detail="Authentication is not configured")
    # Evaluate both comparisons before branching so timing does not reveal which one failed.
    user_ok = hmac.compare_digest(body.username.encode(), LOGIN_USERNAME.encode())
    pass_ok = hmac.compare_digest(body.password.encode(), API_KEY.encode())
    if not (user_ok and pass_ok):
        observability.record_auth("login", False)
        await asyncio.sleep(0.5)
        raise HTTPException(status_code=401, detail="Wrong username or password.")
    observability.record_auth("login", True)
    session = issue_session_token(LOGIN_USERNAME)
    return {
        "token": session["token"],
        "expiresAt": session["expiresAt"],
        "method": "api_key",
        "name": LOGIN_USERNAME,
        "usingDefaultKey": using_default_key(),
    }


@app.get("/api/auth/me")
@limiter.limit("60/minute")
async def get_current_user(request: Request, _=Depends(verify_auth)):
    """Return current user info from JWT claims or API key identity."""
    auth_method = getattr(request.state, "auth_method", "api_key")

    if auth_method == "oidc":
        claims = getattr(request.state, "user_claims", {}) or {}
        return {
            "authenticated": True,
            "method": "oidc",
            "sub": claims.get("sub", ""),
            "email": claims.get("email", ""),
            "name": claims.get("name", claims.get("preferred_username", "")),
            "groups": claims.get("groups", []),
            "org": claims.get("org", ""),
            "role": getattr(request.state, "role", "tenant"),
            "tenant": getattr(request.state, "tenant", None),
            "tenants": getattr(request.state, "tenants", []),
            "tenantNamespaces": getattr(request.state, "tenant_namespaces", None),
        }

    return {
        "authenticated": True,
        "method": "api_key",
        "sub": "api-key-user",
        "email": "",
        "name": "admin",
        "groups": [],
        "org": "",
        "role": "admin",
        "tenant": None,
        "tenants": [],
        "tenantNamespaces": None,
        "usingDefaultKey": using_default_key(),
    }


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=8080)
