"""Browser access to GryviaWorkspaces through the gateway: /ws/<namespace>/<name>/...

``POST /api/workspaces/{name}/open`` (normal gateway auth, tenant-scoped) returns a short-lived launch link. The
first request with that link sets a cookie limited to the workspace path and redirects; later HTTP and WebSocket
requests carrying the cookie are forwarded to the workspace Service. Jupyter gets the token the ai-operator derived
from the shared key (GRYVIA_WORKSPACE_PROXY_KEY, the operator's --workspace-proxy-key-file), so the user never sees
it; the default code-server image runs without its own password behind this proxy.
"""
import base64
import hashlib
import hmac
import json
import logging
import time
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import urlencode, parse_qsl

import anyio
import httpx
from fastapi import APIRouter, Depends, HTTPException, Request, WebSocket
from fastapi.responses import HTMLResponse, RedirectResponse, Response, StreamingResponse
from starlette.background import BackgroundTask

from .common import Deps, get_item
from .uiutil import find_one

logger = logging.getLogger(__name__)

PLURAL = "gryviaworkspaces"
COOKIE = "gryvia_ws"
LAUNCH_PARAM = "gryvia_launch"
LAUNCH_TTL = 120
SESSION_TTL = 8 * 3600
CACHE_TTL = 5.0
TOKEN_CONTEXT = "gryvia-workspace-token-v1"  # must match the ai-operator's workspaceTokenContext
OPEN_PHASES = ("Running", "Idle")

HOP_BY_HOP = frozenset({
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "trailers",
    "transfer-encoding", "upgrade", "host", "content-length", "authorization",
})
WS_SKIP = HOP_BY_HOP | frozenset({
    "origin", "cookie", "sec-websocket-key", "sec-websocket-version", "sec-websocket-extensions",
    "sec-websocket-protocol",
})


def prefix(ns: str, name: str) -> str:
    return f"/ws/{ns}/{name}/"


def jupyter_token(key: bytes, ns: str, name: str, uid: str) -> str:
    msg = "\x00".join((TOKEN_CONTEXT, ns, name, uid)).encode()
    return hmac.new(key, msg, hashlib.sha256).hexdigest()


def _b64(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def _unb64(s: str) -> bytes:
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


def _signing_key(key: bytes) -> bytes:
    return hmac.new(key, b"gryvia-workspace-proxy-session-v1", hashlib.sha256).digest()


def sign(key: bytes, kind: str, ns: str, name: str, uid: str, ttl: int, now: Optional[float] = None) -> str:
    body = json.dumps({"k": kind, "ns": ns, "n": name, "u": uid, "exp": int((now or time.time()) + ttl)},
                      separators=(",", ":")).encode()
    sig = hmac.new(_signing_key(key), body, hashlib.sha256).digest()
    return f"{_b64(body)}.{_b64(sig)}"


def verify(key: bytes, token: str, kind: str, ns: str, name: str, uid: str, now: Optional[float] = None) -> bool:
    try:
        body_b64, sig_b64 = token.split(".", 1)
        body = _unb64(body_b64)
        want = hmac.new(_signing_key(key), body, hashlib.sha256).digest()
        if not hmac.compare_digest(want, _unb64(sig_b64)):
            return False
        c = json.loads(body)
    except (ValueError, TypeError):
        return False
    return (c.get("k") == kind and c.get("ns") == ns and c.get("n") == name and c.get("u") == uid
            and isinstance(c.get("exp"), int) and c["exp"] > (now or time.time()))


def upstream_base(obj: Dict[str, Any], ns: str, name: str) -> str:
    spec, st = obj.get("spec") or {}, obj.get("status") or {}
    svc = st.get("serviceName") or f"{name}-workspace"
    port = 8080 if spec.get("type") == "vscode" else 8888
    return f"http://{svc}.{ns}.svc.cluster.local:{port}"


def upstream_path(obj: Dict[str, Any], ns: str, name: str, rest: str) -> str:
    # Jupyter serves under the gateway prefix (base_url); code-server uses relative URLs and is served from /.
    if (obj.get("spec") or {}).get("type") == "vscode":
        return "/" + rest
    return prefix(ns, name) + rest


def _strip_cookie(header: str) -> str:
    parts = [p.strip() for p in header.split(";") if p.strip()]
    return "; ".join(p for p in parts if not p.startswith(COOKIE + "="))


def _uid(obj: Dict[str, Any]) -> str:
    return str((obj.get("metadata") or {}).get("uid") or "")


DENIED_PAGE = (
    "<!doctype html><title>Workspace</title><p>Open this workspace from the Gryvia dashboard "
    "(Workspaces, Open) or with <code>gryvia workspace open NAME</code>.</p>"
)


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()
    cache: Dict[Tuple[str, str], Tuple[float, Dict[str, Any]]] = {}

    def key() -> bytes:
        if not deps.workspace_proxy_key:
            raise HTTPException(status_code=503, detail="Workspace proxy is not configured "
                                "(apiGateway.workspaceProxy in the Helm chart)")
        return deps.workspace_proxy_key

    async def workspace(ns: str, name: str) -> Optional[Dict[str, Any]]:
        now = time.monotonic()
        hit = cache.get((ns, name))
        if hit and hit[0] > now:
            return hit[1]
        try:
            obj = await get_item(deps, PLURAL, name, namespace=ns)
        except HTTPException as exc:
            if exc.status_code == 404:
                cache.pop((ns, name), None)
                return None
            raise
        cache[(ns, name)] = (now + CACHE_TTL, obj)
        return obj

    @router.post("/api/workspaces/{name}/open")
    @deps.limiter.limit("30/minute")
    async def open_workspace(request: Request, name: str, _=Depends(deps.verify_auth)):
        k = key()
        obj, ns = await find_one(request, deps, PLURAL, name)
        phase = (obj.get("status") or {}).get("phase")
        if phase not in OPEN_PHASES:
            raise HTTPException(status_code=409, detail=f"Workspace is {phase or 'not ready'}; "
                                "open it once it is Running")
        token = sign(k, "launch", ns, name, _uid(obj), LAUNCH_TTL)
        base = (obj.get("status") or {}).get("url") or ""
        if not (base.startswith(("https://", "http://")) and base.endswith(prefix(ns, name))):
            base = prefix(ns, name)
        return {"url": f"{base}?{urlencode({LAUNCH_PARAM: token})}", "expiresIn": LAUNCH_TTL}

    async def authorize(ns: str, name: str, cookies: Dict[str, str]) -> Optional[Dict[str, Any]]:
        if not deps.workspace_proxy_key:
            return None
        obj = await workspace(ns, name)
        if obj is None or (obj.get("status") or {}).get("phase") not in OPEN_PHASES:
            return None
        if not verify(deps.workspace_proxy_key, cookies.get(COOKIE, ""), "session", ns, name, _uid(obj)):
            return None
        return obj

    def upstream_headers(obj: Dict[str, Any], ns: str, name: str, headers: Any, skip: frozenset) -> Dict[str, str]:
        out: Dict[str, str] = {}
        for k, v in headers.items():
            lk = k.lower()
            if lk in skip:
                continue
            if lk == "cookie":
                v = _strip_cookie(v)
                if not v:
                    continue
            if lk == "origin":
                v = upstream_base(obj, ns, name)
            out[k] = v
        if (obj.get("spec") or {}).get("type") != "vscode":
            out["Authorization"] = "token " + jupyter_token(deps.workspace_proxy_key, ns, name, _uid(obj))
        return out

    @router.get("/ws/{ns}/{name}", include_in_schema=False)
    async def add_slash(request: Request, ns: str, name: str):
        query = request.url.query
        return RedirectResponse(prefix(ns, name) + (f"?{query}" if query else ""), status_code=307)

    @router.api_route("/ws/{ns}/{name}/{rest:path}",
                      methods=["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"],
                      include_in_schema=False)
    async def proxy_http(request: Request, ns: str, name: str, rest: str):
        launch = request.query_params.get(LAUNCH_PARAM)
        if launch is not None:
            if not deps.workspace_proxy_key:
                return HTMLResponse(DENIED_PAGE, status_code=503)
            obj = await workspace(ns, name)
            if obj is None or not verify(deps.workspace_proxy_key, launch, "launch", ns, name, _uid(obj)):
                return HTMLResponse(DENIED_PAGE, status_code=401)
            query = urlencode([(k, v) for k, v in parse_qsl(request.url.query, keep_blank_values=True)
                               if k != LAUNCH_PARAM])
            resp = RedirectResponse(request.url.path + (f"?{query}" if query else ""), status_code=303)
            resp.set_cookie(COOKIE, sign(deps.workspace_proxy_key, "session", ns, name, _uid(obj), SESSION_TTL),
                            max_age=SESSION_TTL, path=prefix(ns, name), secure=True, httponly=True,
                            samesite="strict")
            return resp

        obj = await authorize(ns, name, request.cookies)
        if obj is None:
            return HTMLResponse(DENIED_PAGE, status_code=401)

        base = upstream_base(obj, ns, name)
        url = base + upstream_path(obj, ns, name, rest)
        has_body = "content-length" in request.headers or "transfer-encoding" in request.headers
        client = httpx.AsyncClient(transport=deps.workspace_transport, timeout=httpx.Timeout(300.0, connect=5.0))
        req = client.build_request(request.method, url, params=request.query_params.multi_items(),
                                   headers=upstream_headers(obj, ns, name, request.headers, HOP_BY_HOP),
                                   content=request.stream() if has_body else None)
        try:
            up = await client.send(req, stream=True)
        except httpx.HTTPError as exc:
            await client.aclose()
            logger.info("workspace %s/%s upstream error: %s", ns, name, exc)
            return Response("Workspace is not reachable yet; try again in a moment.", status_code=502)

        headers: List[Tuple[str, str]] = []
        for k, v in up.headers.multi_items():
            lk = k.lower()
            if lk in ("transfer-encoding", "connection", "keep-alive"):
                continue
            if lk == "location" and v.startswith(base):
                v = v[len(base):] or "/"
                if (obj.get("spec") or {}).get("type") == "vscode":
                    v = prefix(ns, name) + v.lstrip("/")
            headers.append((k, v))

        async def close() -> None:
            await up.aclose()
            await client.aclose()

        if up.is_stream_consumed:  # an already-buffered response (e.g. a mock transport)
            body: Any = iter([up.content])
        else:
            body = up.aiter_raw()
        resp = StreamingResponse(body, status_code=up.status_code, background=BackgroundTask(close))
        resp.raw_headers = [(k.lower().encode("latin-1"), v.encode("latin-1")) for k, v in headers]
        return resp

    @router.websocket("/ws/{ns}/{name}/{rest:path}")
    async def proxy_ws(websocket: WebSocket, ns: str, name: str, rest: str):
        obj = await authorize(ns, name, websocket.cookies)
        if obj is None:
            await websocket.close(code=4401)
            return
        base = upstream_base(obj, ns, name)
        url = "ws" + base[len("http"):] + upstream_path(obj, ns, name, rest)
        if websocket.url.query:
            url += "?" + websocket.url.query
        headers = upstream_headers(obj, ns, name, websocket.headers, WS_SKIP)
        cookie = _strip_cookie(websocket.headers.get("cookie", ""))
        if cookie:
            headers["Cookie"] = cookie
        protocols = [p.strip() for p in websocket.headers.get("sec-websocket-protocol", "").split(",") if p.strip()]
        connect = deps.workspace_ws_connect or _connect
        try:
            upstream = await connect(url, headers, protocols, base)
        except Exception as exc:  # noqa: BLE001 - any upstream failure closes the client side
            logger.info("workspace %s/%s websocket upstream error: %s", ns, name, exc)
            await websocket.close(code=1011)
            return
        await websocket.accept(subprotocol=getattr(upstream, "subprotocol", None))

        async def client_to_upstream(scope: anyio.CancelScope) -> None:
            try:
                while True:
                    msg = await websocket.receive()
                    if msg["type"] == "websocket.disconnect":
                        return
                    data = msg.get("bytes") if msg.get("bytes") is not None else msg.get("text")
                    if data is not None:
                        await upstream.send(data)
            finally:
                scope.cancel()

        async def upstream_to_client(scope: anyio.CancelScope) -> None:
            try:
                async for data in upstream:
                    if isinstance(data, bytes):
                        await websocket.send_bytes(data)
                    else:
                        await websocket.send_text(data)
                await websocket.close()
            finally:
                scope.cancel()

        try:
            async with anyio.create_task_group() as tg:
                tg.start_soon(client_to_upstream, tg.cancel_scope)
                tg.start_soon(upstream_to_client, tg.cancel_scope)
        except Exception as exc:  # noqa: BLE001 - either side going away ends the bridge
            logger.debug("workspace %s/%s websocket ended: %s", ns, name, exc)
        finally:
            with anyio.CancelScope(shield=True):
                await upstream.close()

    return router


async def _connect(url: str, headers: Dict[str, str], protocols: List[str], origin: str) -> Any:
    from websockets.asyncio.client import connect

    return await connect(url, additional_headers=headers, subprotocols=protocols or None, origin=origin,
                         max_size=None, open_timeout=10)
