#!/usr/bin/env python3
"""Open running workspaces through the dashboard's /ws/ proxy, as a browser would, and check that they work.

Needs a Gryvia install with workspaceProxy.enabled and Running workspaces, e.g. the kind demo plus:
  kubectl apply -f examples/demo/workspace.yaml -f examples/demo/workspace-vscode.yaml
Then: pip install httpx websockets
      scripts/e2e-workspaces.py --url https://localhost:8443 --jupyter demo-notebook --vscode demo-vscode
The API key comes from --key or GRYVIA_API_KEY.
"""
import argparse
import asyncio
import json
import os
import re
import ssl
import sys
import uuid
from urllib.parse import urlsplit

import httpx
import websockets

ok = True


def check(name, cond, detail=""):
    global ok
    print(("PASS " if cond else "FAIL ") + name + (f"  ({detail})" if detail and not cond else ""))
    ok = ok and bool(cond)


def launch(a, name):
    """Open the workspace through the API and follow the launch link; return the cookie client and the prefix."""
    c = httpx.Client(verify=not a.insecure, follow_redirects=False, timeout=30)
    r = c.post(f"{a.url}/api/workspaces/{name}/open", headers={"Authorization": f"Bearer {a.key}"})
    check(f"{name}: open returns a launch link", r.status_code == 200 and "gryvia_launch=" in r.text,
          f"{r.status_code} {r.text[:200]}")
    if r.status_code != 200:
        return None, ""
    url = r.json()["url"]
    prefix = urlsplit(url).path
    check(f"{name}: launch link is on the dashboard URL", url.startswith(a.url + prefix), url)
    r = c.get(url)
    sc = r.headers.get("set-cookie", "")
    check(f"{name}: launch sets the session cookie and redirects", r.status_code == 303 and "gryvia_ws" in sc, str(r.status_code))
    check(f"{name}: cookie is HttpOnly, Secure, SameSite=strict, scoped to the workspace",
          "HttpOnly" in sc and "Secure" in sc and "samesite=strict" in sc.lower() and f"Path={prefix}" in sc, sc)
    check(f"{name}: redirect drops the token", "gryvia_launch" not in r.headers.get("location", ""), r.headers.get("location"))
    anon = httpx.Client(verify=not a.insecure, timeout=30)
    check(f"{name}: no cookie is refused", anon.get(f"{a.url}{prefix}").status_code == 401)
    check(f"{name}: tampered launch token is refused",
          anon.get(url.replace("gryvia_launch=", "gryvia_launch=x")).status_code == 401)
    return c, prefix


def check_assets(c, name, page):
    """Every script and stylesheet the page references must load through the proxy (not the dashboard's files)."""
    refs = [u for u in re.findall(r'(?:src|href)="([^"]+)"', page.text) if u]
    assets = [u for u in refs if urlsplit(u).path.endswith((".js", ".css"))]
    check(f"{name}: page references scripts or styles", assets, page.text[:300])
    for u in assets:
        r = c.get(str(page.url.join(u)))
        check(f"{name}: loads {u[:80]}", r.status_code == 200 and r.content, str(r.status_code))


def ssl_context(a):
    ctx = ssl.create_default_context()
    if a.insecure:
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
    return ctx


async def refused_without_cookie(a, name, ws_url):
    try:
        async with websockets.connect(ws_url, ssl=ssl_context(a), origin=a.url) as ws:
            await ws.recv()
        check(f"{name}: WebSocket without a cookie is refused", False, "connected")
    except websockets.exceptions.ConnectionClosed as e:
        check(f"{name}: WebSocket without a cookie is refused", e.rcvd is not None and e.rcvd.code == 4401, repr(e))
    except Exception:  # noqa: BLE001 - a refused handshake is a pass
        check(f"{name}: WebSocket without a cookie is refused", True)


def ws_base(a):
    return "wss://" + a.url.split("://", 1)[1]


async def jupyter(a):
    c, prefix = launch(a, a.jupyter)
    if c is None:
        return
    r = c.get(f"{a.url}{prefix}api/status")
    check("jupyter: API through the proxy", r.status_code == 200 and "last_activity" in r.text, f"{r.status_code}")
    page = c.get(f"{a.url}{prefix}lab")
    check("jupyter: JupyterLab page through the proxy", page.status_code == 200 and "JupyterLab" in page.text,
          str(page.status_code))
    check_assets(c, "jupyter", page)

    r = c.post(f"{a.url}{prefix}api/kernels", json={"name": "python3"})
    check("jupyter: start a kernel", r.status_code == 201, f"{r.status_code} {r.text[:200]}")
    if r.status_code != 201:
        return
    kid = r.json()["id"]
    cookie = "; ".join(f"{k}={v}" for k, v in c.cookies.items())
    ws_url = f"{ws_base(a)}{prefix}api/kernels/{kid}/channels"
    try:
        async with websockets.connect(ws_url, ssl=ssl_context(a), additional_headers={"Cookie": cookie}, origin=a.url) as ws:
            msg_id = uuid.uuid4().hex
            await ws.send(json.dumps({
                "header": {"msg_id": msg_id, "username": "e2e", "session": uuid.uuid4().hex,
                           "msg_type": "execute_request", "version": "5.3"},
                "parent_header": {}, "metadata": {}, "channel": "shell",
                "content": {"code": "print(6*7)", "silent": False, "store_history": False,
                            "user_expressions": {}, "allow_stdin": False},
            }))
            out = ""
            async with asyncio.timeout(30):
                while True:
                    m = json.loads(await ws.recv())
                    if m.get("parent_header", {}).get("msg_id") == msg_id and m["msg_type"] == "stream":
                        out = m["content"]["text"]
                        break
            check("jupyter: kernel runs code over the WebSocket", out.strip() == "42", repr(out))
    except Exception as e:  # noqa: BLE001
        check("jupyter: kernel runs code over the WebSocket", False, repr(e))
    await refused_without_cookie(a, "jupyter", ws_url)
    c.delete(f"{a.url}{prefix}api/kernels/{kid}")


async def vscode(a):
    c, prefix = launch(a, a.vscode)
    if c is None:
        return
    page = c.get(f"{a.url}{prefix}", follow_redirects=True)
    check("vscode: workbench page through the proxy", page.status_code == 200 and "workbench" in page.text,
          str(page.status_code))
    refs = re.findall(r'(?:src|href)="([^"]+)"', page.text)
    check("vscode: no absolute asset path (it would bypass the proxy prefix)", not [u for u in refs if u.startswith("/")],
          str(refs))
    check_assets(c, "vscode", page)
    r = c.get(f"{a.url}{prefix}healthz")
    check("vscode: /healthz through the proxy", r.status_code == 200 and "lastHeartbeat" in r.text, f"{r.status_code}")

    cookie = "; ".join(f"{k}={v}" for k, v in c.cookies.items())
    ws_url = f"{ws_base(a)}{prefix}?reconnectionToken={uuid.uuid4()}&reconnection=false&skipWebSocketFrames=false"
    try:
        async with websockets.connect(ws_url, ssl=ssl_context(a), additional_headers={"Cookie": cookie}, origin=a.url) as ws:
            check("vscode: editor WebSocket upgrades through the proxy", ws.response.status_code == 101)
    except Exception as e:  # noqa: BLE001
        check("vscode: editor WebSocket upgrades through the proxy", False, repr(e))
    await refused_without_cookie(a, "vscode", ws_url)


def main():
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--url", required=True, help="dashboard origin, e.g. https://localhost:8443")
    p.add_argument("--key", default=os.environ.get("GRYVIA_API_KEY", ""), help="API key (default: $GRYVIA_API_KEY)")
    p.add_argument("--jupyter", help="name of a Running Jupyter workspace")
    p.add_argument("--vscode", help="name of a Running code-server workspace")
    p.add_argument("--insecure", action="store_true", help="accept a self-signed certificate")
    a = p.parse_args()
    a.url = a.url.rstrip("/")
    if not a.key or not (a.jupyter or a.vscode):
        p.error("--key (or GRYVIA_API_KEY) and at least one of --jupyter/--vscode are required")
    if a.jupyter:
        asyncio.run(jupyter(a))
    if a.vscode:
        asyncio.run(vscode(a))
    print("ALL PASSED" if ok else "SOME FAILED")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
