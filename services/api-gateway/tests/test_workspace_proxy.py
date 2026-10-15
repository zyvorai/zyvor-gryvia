import asyncio
import time
from urllib.parse import parse_qs, urlparse

import httpx
import pytest
from starlette.websockets import WebSocketDisconnect

from routers import workspace_proxy as wp

NS = "default"
KEY = b"0123456789abcdef0123456789abcdef"


def seed(fake_k8s, name="nb", type_="jupyter", phase="Running", uid="uid-1", url=None):
    fake_k8s.add("gryviaworkspaces", {
        "metadata": {"name": name, "uid": uid},
        "spec": {"type": type_},
        "status": {"phase": phase, "serviceName": f"{name}-workspace",
                   "url": url or f"https://gryvia.example.com/ws/{NS}/{name}/"},
    }, namespace=NS)


def test_token_matches_the_operator():
    # Same golden value as TestWorkspaceToken_StableAndScoped in the ai-operator.
    assert wp.jupyter_token(KEY, "ns", "dev", "uid-1") == \
        "a5398f1ed0ccc9b98d605eb8c65143e92dd2923c9093192bb0909108b6d68e3c"


def test_sign_verify_scope_and_expiry():
    t = wp.sign(KEY, "session", "ns", "a", "u", 60)
    assert wp.verify(KEY, t, "session", "ns", "a", "u")
    assert not wp.verify(KEY, t, "launch", "ns", "a", "u")
    assert not wp.verify(KEY, t, "session", "ns", "b", "u")
    assert not wp.verify(KEY, t, "session", "ns", "a", "other-uid")
    assert not wp.verify(b"x" * 32, t, "session", "ns", "a", "u")
    assert not wp.verify(KEY, t, "session", "ns", "a", "u", now=time.time() + 120)
    assert not wp.verify(KEY, "garbage", "session", "ns", "a", "u")
    body, sig = t.split(".")
    assert not wp.verify(KEY, body + "x." + sig, "session", "ns", "a", "u")


def test_open_requires_configuration(make_client, fake_k8s):
    seed(fake_k8s)
    assert make_client("workspace_proxy").post("/api/workspaces/nb/open").status_code == 503


def test_open_needs_a_running_workspace(make_client, fake_k8s):
    seed(fake_k8s, phase="Pending")
    c = make_client("workspace_proxy", workspace_proxy_key=KEY)
    assert c.post("/api/workspaces/nb/open").status_code == 409
    assert c.post("/api/workspaces/missing/open").status_code == 404


def test_open_is_tenant_scoped(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("workspace_proxy", role="tenant", tenants=["alpha"], workspace_proxy_key=KEY)
    assert c.post("/api/workspaces/nb/open").status_code == 404


def launch(c):
    r = c.post("/api/workspaces/nb/open")
    assert r.status_code == 200, r.text
    url = urlparse(r.json()["url"])
    assert url.scheme == "https" and url.path == f"/ws/{NS}/nb/"
    return url.path + "?" + url.query


def upstream(seen):
    def handler(req: httpx.Request) -> httpx.Response:
        seen.append(req)
        if req.url.path.endswith("/redirect"):
            return httpx.Response(302, headers={"location": f"http://{req.url.host}:{req.url.port}/elsewhere"})
        return httpx.Response(200, headers={"content-type": "text/plain", "x-up": "1"}, content=b"hello")
    return httpx.MockTransport(handler)


def test_launch_sets_cookie_and_proxies_jupyter(make_client, fake_k8s):
    seed(fake_k8s)
    seen = []
    c = make_client("workspace_proxy", workspace_proxy_key=KEY, workspace_transport=upstream(seen))
    c.base_url = "https://testserver"
    first = c.get(launch(c) + "&tab=1", follow_redirects=False)
    assert first.status_code == 303
    assert first.headers["location"] == f"/ws/{NS}/nb/?tab=1"
    cookie = first.headers["set-cookie"]
    assert "gryvia_ws=" in cookie and f"Path=/ws/{NS}/nb/" in cookie
    assert "HttpOnly" in cookie and "Secure" in cookie and "samesite=strict" in cookie.lower()

    session = c.cookies.get("gryvia_ws")
    r = c.get(f"/ws/{NS}/nb/lab?x=1", headers={
        "Authorization": "Bearer user-key",
        "Origin": "https://gryvia.example.com",
        "Cookie": f"gryvia_ws={session}; _xsrf=abc",
    })
    assert r.status_code == 200 and r.text == "hello" and r.headers["x-up"] == "1"
    req = seen[-1]
    assert str(req.url) == f"http://nb-workspace.{NS}.svc.cluster.local:8888/ws/{NS}/nb/lab?x=1"
    assert req.headers["authorization"] == "token " + wp.jupyter_token(KEY, NS, "nb", "uid-1")
    assert req.headers["origin"] == f"http://nb-workspace.{NS}.svc.cluster.local:8888"
    assert req.headers["cookie"] == "_xsrf=abc"


def test_vscode_path_is_stripped_and_redirects_rewritten(make_client, fake_k8s):
    seed(fake_k8s, name="nb", type_="vscode")
    seen = []
    c = make_client("workspace_proxy", workspace_proxy_key=KEY, workspace_transport=upstream(seen))
    c.base_url = "https://testserver"
    c.get(launch(c), follow_redirects=False)
    r = c.get(f"/ws/{NS}/nb/static/app.js")
    assert r.status_code == 200
    assert seen[-1].url.path == "/static/app.js"
    assert "authorization" not in seen[-1].headers
    r = c.get(f"/ws/{NS}/nb/redirect", follow_redirects=False)
    assert r.status_code == 302 and r.headers["location"] == f"/ws/{NS}/nb/elsewhere"


def test_without_cookie_or_with_a_bad_launch_token(make_client, fake_k8s):
    seed(fake_k8s)
    seen = []
    c = make_client("workspace_proxy", workspace_proxy_key=KEY, workspace_transport=upstream(seen))
    c.base_url = "https://testserver"
    assert c.get(f"/ws/{NS}/nb/lab").status_code == 401
    assert c.get(f"/ws/{NS}/nb/?gryvia_launch=forged", follow_redirects=False).status_code == 401
    # A launch link for one workspace does not open another.
    seed(fake_k8s, name="other", uid="uid-2")
    q = parse_qs(urlparse(launch(c)).query)["gryvia_launch"][0]
    assert c.get(f"/ws/{NS}/other/?gryvia_launch={q}", follow_redirects=False).status_code == 401
    assert not seen


def test_recreated_workspace_invalidates_the_cookie(make_client, fake_k8s):
    seed(fake_k8s)
    seen = []
    c = make_client("workspace_proxy", workspace_proxy_key=KEY, workspace_transport=upstream(seen))
    c.base_url = "https://testserver"
    c.get(launch(c), follow_redirects=False)
    fake_k8s.store.pop(("gryviaworkspaces", NS, "nb"))
    seed(fake_k8s, uid="uid-new")
    # A fresh client: the router caches the workspace object for a few seconds.
    c2 = make_client("workspace_proxy", workspace_proxy_key=KEY, workspace_transport=upstream(seen))
    c2.base_url = "https://testserver"
    c2.cookies = c.cookies
    assert c2.get(f"/ws/{NS}/nb/lab").status_code == 401


def test_paused_workspace_is_not_proxied(make_client, fake_k8s):
    seed(fake_k8s)
    seen = []
    c = make_client("workspace_proxy", workspace_proxy_key=KEY, workspace_transport=upstream(seen))
    c.base_url = "https://testserver"
    c.get(launch(c), follow_redirects=False)
    fake_k8s.store[("gryviaworkspaces", NS, "nb")]["status"]["phase"] = "Paused"
    c2 = make_client("workspace_proxy", workspace_proxy_key=KEY, workspace_transport=upstream(seen))
    c2.base_url = "https://testserver"
    c2.cookies = c.cookies
    assert c2.get(f"/ws/{NS}/nb/lab").status_code == 401


def test_upstream_down_is_502(make_client, fake_k8s):
    seed(fake_k8s)

    def boom(req):
        raise httpx.ConnectError("refused")
    c = make_client("workspace_proxy", workspace_proxy_key=KEY, workspace_transport=httpx.MockTransport(boom))
    c.base_url = "https://testserver"
    c.get(launch(c), follow_redirects=False)
    assert c.get(f"/ws/{NS}/nb/lab").status_code == 502


def test_missing_slash_redirects(make_client):
    c = make_client("workspace_proxy", workspace_proxy_key=KEY)
    r = c.get(f"/ws/{NS}/nb?a=1", follow_redirects=False)
    assert r.status_code == 307 and r.headers["location"] == f"/ws/{NS}/nb/?a=1"


class FakeUpstreamWS:
    def __init__(self, replies):
        self.subprotocol = "v1.kernel.websocket.jupyter.org"
        self.sent = []
        self.replies = list(replies)
        self.closed = False

    async def send(self, data):
        self.sent.append(data)

    def __aiter__(self):
        return self

    async def __anext__(self):
        while not self.replies:
            if self.closed:
                raise StopAsyncIteration
            await asyncio.sleep(0.01)
        return self.replies.pop(0)

    async def close(self):
        self.closed = True


def test_websocket_bridge(make_client, fake_k8s):
    seed(fake_k8s)
    up = FakeUpstreamWS(["hello", b"\x01\x02"])
    calls = []

    async def connect(url, headers, protocols, origin):
        calls.append((url, headers, protocols, origin))
        return up

    c = make_client("workspace_proxy", workspace_proxy_key=KEY, workspace_ws_connect=connect)
    c.base_url = "https://testserver"
    c.get(launch(c), follow_redirects=False)
    cookie = c.cookies.get("gryvia_ws")
    with c.websocket_connect(f"/ws/{NS}/nb/api/kernels/k1/channels?session_id=s",
                             subprotocols=["v1.kernel.websocket.jupyter.org"],
                             headers={"cookie": f"gryvia_ws={cookie}; _xsrf=abc"}) as ws:
        assert ws.receive_text() == "hello"
        assert ws.receive_bytes() == b"\x01\x02"
        ws.send_text("ping")
        for _ in range(100):
            if up.sent:
                break
            time.sleep(0.01)
    url, headers, protocols, origin = calls[0]
    assert url == f"ws://nb-workspace.{NS}.svc.cluster.local:8888/ws/{NS}/nb/api/kernels/k1/channels?session_id=s"
    assert headers["Authorization"] == "token " + wp.jupyter_token(KEY, NS, "nb", "uid-1")
    assert headers["Cookie"] == "_xsrf=abc"
    assert protocols == ["v1.kernel.websocket.jupyter.org"]
    assert origin == f"http://nb-workspace.{NS}.svc.cluster.local:8888"
    assert up.sent == ["ping"]


def test_websocket_without_cookie_is_refused(make_client, fake_k8s):
    seed(fake_k8s)

    async def connect(*a):
        raise AssertionError("must not connect upstream")

    c = make_client("workspace_proxy", workspace_proxy_key=KEY, workspace_ws_connect=connect)
    with pytest.raises(WebSocketDisconnect) as exc:
        with c.websocket_connect(f"/ws/{NS}/nb/api/kernels/k1/channels"):
            pass
    assert exc.value.code == 4401
