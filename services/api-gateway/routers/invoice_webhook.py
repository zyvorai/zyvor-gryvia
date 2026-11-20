# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Provider-neutral invoice webhook: POST the invoice JSON to one operator-configured URL.

This is a delivery seam only. No payment is processed and no payment provider is integrated.

Config (environment, read per call so it can be rotated): GRYVIA_INVOICE_WEBHOOK_URL (unset = feature off) and
GRYVIA_INVOICE_WEBHOOK_SECRET (HMAC-SHA256 key; when unset the body is sent unsigned). The URL never comes
from a request. SSRF hardening: https is required unless the host is loopback (localhost, 127.0.0.1, ::1);
every address the host resolves to must be a routable one (link-local incl. the 169.254.169.254 metadata
address, multicast, unspecified and reserved ranges are refused); redirects are never followed; the timeout is bounded.
Note: the resolved-address check and the connection resolve separately, so a DNS rebinding race is possible
against a hostile DNS server; the URL is operator-supplied, not attacker-supplied.

Signature: header ``X-Gryvia-Signature: sha256=<hex HMAC(secret, "<timestamp>.<body>")>`` with the unix
seconds in ``X-Gryvia-Timestamp``. Receivers should reject old timestamps.
"""
import asyncio
import hashlib
import hmac
import ipaddress
import json
import os
import socket
import time
from typing import Any, Dict, List
from urllib.parse import urlsplit

import httpx
from fastapi import HTTPException

TIMEOUT_SECONDS = 10.0
LOOPBACK_HOSTS = ("localhost", "127.0.0.1", "::1")


def sign(secret: str, timestamp: str, body: bytes) -> str:
    mac = hmac.new(secret.encode(), timestamp.encode() + b"." + body, hashlib.sha256)
    return "sha256=" + mac.hexdigest()


def _resolve(host: str, port: int) -> List[str]:
    return sorted({ai[4][0] for ai in socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)})


def check_url(url: str) -> str:
    """Return an error message when the URL must not be called, else ''."""
    parts = urlsplit(url)
    host = parts.hostname or ""
    if parts.scheme not in ("http", "https") or not host:
        return "webhook URL must be an absolute http(s) URL"
    if parts.username or parts.password:
        return "webhook URL must not embed credentials"
    loopback = host in LOOPBACK_HOSTS
    if parts.scheme != "https" and not loopback:
        return "webhook URL must use https"
    try:
        addrs = _resolve(host, parts.port or (443 if parts.scheme == "https" else 80))
    except (socket.gaierror, UnicodeError):
        return "webhook host does not resolve"
    for a in addrs:
        ip = ipaddress.ip_address(a.split("%")[0])
        if ip.is_loopback and loopback:
            continue
        if (ip.is_link_local or ip.is_loopback or ip.is_multicast or ip.is_unspecified or ip.is_reserved
                or (ip.version == 4 and str(ip).startswith("169.254."))):
            return "webhook host resolves to a blocked address"
    return ""


def _post(url: str, body: bytes, headers: Dict[str, str]) -> Dict[str, Any]:
    try:
        with httpx.Client(timeout=TIMEOUT_SECONDS, follow_redirects=False, trust_env=False) as c:
            r = c.post(url, content=body, headers=headers)
    except httpx.HTTPError as exc:
        return {"delivered": False, "status": None, "error": f"request failed: {type(exc).__name__}"}
    ok = 200 <= r.status_code < 300
    return {"delivered": ok, "status": r.status_code,
            "error": None if ok else f"receiver answered HTTP {r.status_code}"}


async def deliver_invoice(invoice: Dict[str, Any]) -> Dict[str, Any]:
    url = os.environ.get("GRYVIA_INVOICE_WEBHOOK_URL", "").strip()
    if not url:
        raise HTTPException(status_code=503, detail="Invoice webhook is not configured (GRYVIA_INVOICE_WEBHOOK_URL)")
    err = await asyncio.get_running_loop().run_in_executor(None, check_url, url)
    if err:
        return {"delivered": False, "status": None, "error": err}
    body = json.dumps(invoice, separators=(",", ":"), sort_keys=True).encode()
    ts = str(int(time.time()))
    headers = {"Content-Type": "application/json", "X-Gryvia-Timestamp": ts}
    secret = os.environ.get("GRYVIA_INVOICE_WEBHOOK_SECRET", "")
    if secret:
        headers["X-Gryvia-Signature"] = sign(secret, ts, body)
    return await asyncio.get_running_loop().run_in_executor(None, _post, url, body, headers)
