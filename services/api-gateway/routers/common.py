"""Shared helpers for the gateway's route modules.

Each module under routers/ exposes ``build_router(deps) -> APIRouter`` and is
registered by ``routers.register_routers``. Modules must not import ``main``;
everything they need comes from ``Deps`` so they can be tested with a fake
Kubernetes client (see tests/conftest.py).
"""
import asyncio
from dataclasses import dataclass
from typing import Any, Awaitable, Callable, Dict, List, Optional, Union

from fastapi import HTTPException, Request
from kubernetes.client.exceptions import ApiException

GROUP = "gryvia.io"
VERSION = "v1alpha1"

# Plurals of the cluster-scoped Gryvia CRDs (from crds/*.yaml); everything else is namespaced.
CLUSTER_SCOPED = frozenset({
    "gryviabudgets",
    "gryviachargebacks",
    "gryviacostpredictors",
    "gryviadatasets",
    "gryviafederations",
    "gryviagpumemoryoptimizers",
    "gryviagpunodes",
    "gryviagpuskus",
    "gryvianetworkrates",
    "gryviagpusharingpolicies",
    "gryviahealthchecks",
    "gryviainvoices",
    "gryvialedgerentries",
    "gryvianetworks",
    "gryviapriorities",
    "gryviaquotas",
    "gryviareservations",
    "gryviastorages",
    "gryviatemplates",
    "gryviatenants",
})


@dataclass
class Deps:
    verify_auth: Callable[..., Any]   # FastAPI dependency: raises 401/403
    k8s_custom: Any                   # kubernetes.client.CustomObjectsApi (or a fake)
    k8s_core: Any                     # kubernetes.client.CoreV1Api (or a fake)
    limiter: Any                      # slowapi Limiter (or a no-op fake)
    job_namespace: str = "default"
    collector_urls: Optional[List[str]] = None                                 # GRYVIA_COLLECTOR_URLS
    flight_token: Optional[str] = None                                           # GRYVIA_FLIGHT_TOKEN
    flight_collector_namespace: str = "gryvia-network"
    netra_url: Optional[str] = None                                            # GRYVIA_NETRA_URL
    netra_token: Optional[str] = None                                          # GRYVIA_NETRA_TOKEN
    netra_verify_tls: Union[bool, str] = True                                  # GRYVIA_NETRA_INSECURE=1 turns off; a CA file path
    netra_fetch: Optional[Callable[[], Awaitable[Any]]] = None                 # tests: returns Netra records
    llm_key_namespace: Optional[str] = None                                    # GRYVIA_LLM_KEY_NAMESPACE
    llm_gateway_url: Optional[str] = None                                      # GRYVIA_LLM_GATEWAY_URL
    zyntra_url: Optional[str] = None                                           # GRYVIA_ZYNTRA_URL
    zyntra_token: Optional[str] = None                                         # GRYVIA_ZYNTRA_TOKEN (viewer)
    zyntra_console_url: Optional[str] = None                                   # GRYVIA_ZYNTRA_CONSOLE_URL
    netra_console_url: Optional[str] = None                                    # GRYVIA_NETRA_CONSOLE_URL
    sovereign_ca_file: Optional[str] = None                                    # GRYVIA_SOVEREIGN_CA_FILE
    sovereign_client: Optional[Callable[[Any], Any]] = None                    # tests: verify -> an httpx.AsyncClient
    require_prod_approval: bool = False                                        # GRYVIA_REQUIRE_PROD_APPROVAL=1
    intelligence_actions: bool = False                                        # GRYVIA_INTELLIGENCE_ACTIONS=1
    intelligence_retention_days: int = 30                                     # GRYVIA_INTELLIGENCE_RETENTION_DAYS (0 keeps all)
    submission_admission: bool = False                                        # GRYVIA_SUBMISSION_ADMISSION_ENFORCE=1
    # tests inject this: returns a list of bodies, or (bodies, total_collectors)
    collector_fetch: Optional[Callable[[str], Awaitable[Any]]] = None
    # tests inject this: (agent chat URL, body) -> (status, JSON body)
    agent_chat: Optional[Callable[[str, Dict[str, Any]], Awaitable[Any]]] = None
    # tests inject this: the transport of streamed agent chats (httpx.MockTransport)
    agent_transport: Optional[Any] = None
    # tests inject this: the transport of playground chats sent to the LLM gateway (httpx.MockTransport)
    llm_transport: Optional[Any] = None


async def require_admin(request: Request) -> None:
    """Dependency for provider-only routes. List it AFTER ``Depends(deps.verify_auth)`` (dependencies are
    resolved in order) so the role is already set. A missing role is treated as not admin."""
    if getattr(request.state, "role", None) != "admin":
        raise HTTPException(status_code=403, detail="This action requires a provider administrator")


async def run(fn: Callable[..., Any], *args: Any, **kwargs: Any) -> Any:
    """Run a blocking Kubernetes client call off the event loop."""
    loop = asyncio.get_running_loop()
    return await loop.run_in_executor(None, lambda: fn(*args, **kwargs))


def is_cluster_scoped(plural: str) -> bool:
    return plural in CLUSTER_SCOPED


def http_error(exc: Exception, what: str) -> HTTPException:
    """Map a Kubernetes ApiException to an HTTP error without leaking internals."""
    if isinstance(exc, ApiException):
        if exc.status in (400, 404, 409, 422):
            return HTTPException(status_code=exc.status, detail=f"{what}: {exc.reason}")
        if exc.status in (401, 403):
            return HTTPException(status_code=502, detail=f"{what}: gateway is not permitted to do this")
    return HTTPException(status_code=500, detail=f"Failed to {what}")


async def list_items(deps: Deps, plural: str, namespace: Optional[str] = None) -> List[Dict[str, Any]]:
    """List all objects of a kind (cluster-wide for namespaced kinds unless namespace is given)."""
    try:
        if is_cluster_scoped(plural) or namespace is None:
            res = await run(deps.k8s_custom.list_cluster_custom_object, group=GROUP, version=VERSION, plural=plural)
        else:
            res = await run(deps.k8s_custom.list_namespaced_custom_object, group=GROUP, version=VERSION,
                            namespace=namespace, plural=plural)
    except Exception as exc:  # noqa: BLE001
        raise http_error(exc, f"list {plural}") from exc
    return res.get("items", [])


async def get_item(deps: Deps, plural: str, name: str, namespace: Optional[str] = None) -> Dict[str, Any]:
    ns = namespace or deps.job_namespace
    try:
        if is_cluster_scoped(plural):
            return await run(deps.k8s_custom.get_cluster_custom_object, group=GROUP, version=VERSION,
                             plural=plural, name=name)
        return await run(deps.k8s_custom.get_namespaced_custom_object, group=GROUP, version=VERSION,
                         namespace=ns, plural=plural, name=name)
    except Exception as exc:  # noqa: BLE001
        raise http_error(exc, f"get {plural}/{name}") from exc


async def create_item(deps: Deps, plural: str, kind: str, name: str, spec: Dict[str, Any],
                      namespace: Optional[str] = None, labels: Optional[Dict[str, str]] = None) -> Dict[str, Any]:
    body: Dict[str, Any] = {
        "apiVersion": f"{GROUP}/{VERSION}",
        "kind": kind,
        "metadata": {"name": name, **({"labels": labels} if labels else {})},
        "spec": spec,
    }
    try:
        if is_cluster_scoped(plural):
            return await run(deps.k8s_custom.create_cluster_custom_object, group=GROUP, version=VERSION,
                             plural=plural, body=body)
        ns = namespace or deps.job_namespace
        body["metadata"]["namespace"] = ns
        return await run(deps.k8s_custom.create_namespaced_custom_object, group=GROUP, version=VERSION,
                         namespace=ns, plural=plural, body=body)
    except Exception as exc:  # noqa: BLE001
        raise http_error(exc, f"create {plural}/{name}") from exc


async def delete_item(deps: Deps, plural: str, name: str, namespace: Optional[str] = None) -> None:
    ns = namespace or deps.job_namespace
    try:
        if is_cluster_scoped(plural):
            await run(deps.k8s_custom.delete_cluster_custom_object, group=GROUP, version=VERSION,
                      plural=plural, name=name)
        else:
            await run(deps.k8s_custom.delete_namespaced_custom_object, group=GROUP, version=VERSION,
                      namespace=ns, plural=plural, name=name)
    except Exception as exc:  # noqa: BLE001
        raise http_error(exc, f"delete {plural}/{name}") from exc


async def patch_item(deps: Deps, plural: str, name: str, patch: Dict[str, Any],
                     namespace: Optional[str] = None) -> Dict[str, Any]:
    ns = namespace or deps.job_namespace
    try:
        if is_cluster_scoped(plural):
            return await run(deps.k8s_custom.patch_cluster_custom_object, group=GROUP, version=VERSION,
                             plural=plural, name=name, body=patch)
        return await run(deps.k8s_custom.patch_namespaced_custom_object, group=GROUP, version=VERSION,
                         namespace=ns, plural=plural, name=name, body=patch)
    except Exception as exc:  # noqa: BLE001
        raise http_error(exc, f"update {plural}/{name}") from exc
