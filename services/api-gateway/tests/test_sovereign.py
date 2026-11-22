# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import httpx

ZYNTRA = "http://zyntra:8080"
NETRA = "https://netra:30870"


def products(make_client, handler=None, **deps):
    if handler is not None:
        deps["sovereign_client"] = lambda verify: httpx.AsyncClient(transport=httpx.MockTransport(handler))
    body = make_client("sovereign", **deps).get("/api/sovereign").json()
    return body, {p["name"]: p for p in body["products"]}


def fake(request):
    path = request.url.path
    if request.url.host == "netra":
        return httpx.Response(200, text="ok")
    if path == "/api/v1/meta":
        return httpx.Response(200, json={"version": "0.4.0", "execute_mode": "apply", "pack": {"id": "sovereign-aios"},
                                         "sources": {"total": 6, "healthy": 6}})
    if request.headers.get("authorization") != "Bearer zst_viewer":
        return httpx.Response(401, json={"error": "authentication required"})
    if path == "/api/v1/gaps":
        return httpx.Response(200, json={"gaps": [{"kpi": "gpu_utilization"}, {"kpi": "gryvia_pending_jobs"}]})
    if path == "/api/v1/proposals":
        return httpx.Response(200, json={"proposals": [{"status": "pending"}, {"status": "executed"}, {"status": "pending"}]})
    return httpx.Response(404)


def test_not_installed(make_client):
    body, p = products(make_client)
    assert body["enabled"] is False
    assert p["Zyntra"]["state"] == "not installed" and p["Netra"]["state"] == "not installed"


def test_healthy(make_client):
    body, p = products(make_client, fake, zyntra_url=ZYNTRA, zyntra_token="zst_viewer", netra_url=NETRA,
                       zyntra_console_url="https://zyntra.example", netra_console_url="https://netra.example")
    z, n = p["Zyntra"], p["Netra"]
    assert body["enabled"] is True
    assert z["state"] == "healthy" and z["version"] == "0.4.0" and z["pack"] == "sovereign-aios"
    assert z["openGaps"] == 2 and z["pendingProposals"] == 2 and z["executeMode"] == "apply"
    assert z["sources"] == {"total": 6, "healthy": 6} and z["console"] == "https://zyntra.example"
    assert n["state"] == "healthy" and n["console"] == "https://netra.example"
    assert "zst_viewer" not in str(body)


def test_without_token_only_meta(make_client):
    _, p = products(make_client, fake, zyntra_url=ZYNTRA)
    assert p["Zyntra"]["state"] == "healthy" and "openGaps" not in p["Zyntra"] and "pendingProposals" not in p["Zyntra"]


def test_unreachable_and_unhealthy(make_client):
    def down(request):
        if request.url.host == "netra":
            return httpx.Response(503)
        raise httpx.ConnectError("connection refused")

    _, p = products(make_client, down, zyntra_url=ZYNTRA, netra_url=NETRA)
    assert p["Zyntra"]["state"] == "unreachable" and "refused" in p["Zyntra"]["error"]
    assert p["Netra"]["state"] == "unhealthy (503)"


def test_netra_uses_its_own_tls_setting(make_client):
    seen = {}

    def client(verify):
        def handler(request):
            seen[request.url.host] = verify
            return httpx.Response(200, json={"version": "0.4.0"})
        return httpx.AsyncClient(transport=httpx.MockTransport(handler))

    make_client("sovereign", sovereign_client=client, zyntra_url=ZYNTRA, netra_url=NETRA, netra_verify_tls=False,
                sovereign_ca_file="/tls/tls.crt").get("/api/sovereign")
    assert seen == {"zyntra": "/tls/tls.crt", "netra": False}


def test_zyntra_unhealthy(make_client):
    _, p = products(make_client, lambda r: httpx.Response(502), zyntra_url=ZYNTRA)
    assert p["Zyntra"]["state"] == "unhealthy (502)" and "version" not in p["Zyntra"]
