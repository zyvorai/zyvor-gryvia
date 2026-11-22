# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Transport security for talking to collectors (TLS verification, mTLS, request signing).

Everything is read from the environment on each call, and every variable is optional: with none of
them set the gateway behaves exactly as before (plain http, unsigned, except the Flight endpoint's
own signature). See docs/collector-security.md.

  GRYVIA_COLLECTOR_CA_FILE          CA bundle that must have signed the collector certificate. Setting it
                                    switches discovered collector URLs to https.
  GRYVIA_COLLECTOR_TLS=1            use https with the system trust store when no CA file is set.
  GRYVIA_COLLECTOR_CERT_FILE/KEY_FILE   client certificate for collectors that enable mTLS.
  GRYVIA_COLLECTOR_SERVER_NAME      certificate name to verify (SNI). Needed because collectors are
                                    reached by pod IP; the collector certificate must carry this DNS name.
  GRYVIA_COLLECTOR_TOKEN / GRYVIA_COLLECTOR_TOKEN_FILE   shared token; every non-flight request is then
                                    signed (X-Gryvia-Time / X-Gryvia-Signature).
"""
import hashlib
import hmac
import os
import ssl
import time
from typing import Any, Dict, Optional

API_TIME_HEADER = "X-Gryvia-Time"
API_SIGNATURE_HEADER = "X-Gryvia-Signature"
API_SIGNATURE_PREFIX = "GRYVIA-API-V1\n"
MIN_TOKEN_LENGTH = 32  # same floor the collector enforces


def _env(name: str) -> str:
    return os.environ.get(name, "").strip()


def collector_scheme() -> str:
    return "https" if _env("GRYVIA_COLLECTOR_CA_FILE") or _env("GRYVIA_COLLECTOR_TLS") == "1" else "http"


def collector_token() -> Optional[str]:
    """The shared API token, or None (unsigned requests). A too-short token is treated as unset."""
    token = _env("GRYVIA_COLLECTOR_TOKEN")
    path = _env("GRYVIA_COLLECTOR_TOKEN_FILE")
    if not token and path:
        try:
            with open(path, "r", encoding="utf-8") as handle:
                token = handle.read(4097).strip()
        except OSError:
            return None
    return token if len(token) >= MIN_TOKEN_LENGTH else None


def api_signature_headers(request_uri: str, token: str, when: int, method: str = "GET") -> Dict[str, str]:
    """hex(HMAC-SHA256(token, "GRYVIA-API-V1\\n" METHOD "\\n" REQUEST-URI "\\n" UNIX-SECONDS)).

    Mirrors signAPI in collector/listener_security.go; the vector test in tests/test_collector_transport.py
    and TestAPISignatureCrossLanguageVector pin both sides.
    """
    stamp = str(when)
    message = f"{API_SIGNATURE_PREFIX}{method}\n{request_uri}\n{stamp}".encode()
    return {API_TIME_HEADER: stamp,
            API_SIGNATURE_HEADER: hmac.new(token.encode(), message, hashlib.sha256).hexdigest()}


def signed_headers(request_uri: str) -> Dict[str, str]:
    token = collector_token()
    return api_signature_headers(request_uri, token, int(time.time())) if token else {}


def ssl_context() -> Optional[ssl.SSLContext]:
    """Verifying context (CA bundle and optional client certificate) or None for plain http/defaults."""
    ca, cert = _env("GRYVIA_COLLECTOR_CA_FILE"), _env("GRYVIA_COLLECTOR_CERT_FILE")
    key = _env("GRYVIA_COLLECTOR_KEY_FILE")
    if not (ca or cert or _env("GRYVIA_COLLECTOR_TLS") == "1"):
        return None
    ctx = ssl.create_default_context(cafile=ca or None)  # verify + hostname checking stay on
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    if cert:
        ctx.load_cert_chain(cert, key or None)
    return ctx


def client_kwargs() -> Dict[str, Any]:
    """Extra httpx.AsyncClient arguments; empty when TLS is not configured."""
    ctx = ssl_context()
    return {"verify": ctx} if ctx is not None else {}


def request_extensions() -> Dict[str, Any]:
    """Per-request extensions: the certificate name to verify instead of the pod IP."""
    name = _env("GRYVIA_COLLECTOR_SERVER_NAME")
    return {"extensions": {"sni_hostname": name}} if name else {}
