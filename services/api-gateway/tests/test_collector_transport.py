# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Gateway -> collector transport: TLS verification, mTLS and request signing (real local TLS server)."""
import asyncio
import datetime
import http.server
import json
import ssl
import threading
import types

import pytest
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import ExtendedKeyUsageOID, NameOID

from routers import collector as collector_module
from routers import flight as flight_module
from routers.collector import fetch_all_with_stats
from routers.collector_transport import api_signature_headers, collector_scheme, collector_token, signed_headers

TOKEN = "0123456789abcdef0123456789abcdef"
NAME = "collector.test"


def test_api_signature_cross_language_vector():
    """Same constants as TestAPISignatureCrossLanguageVector in collector/listener_security_test.go."""
    headers = api_signature_headers("/api/v1/graph?x=1", TOKEN, 1700000000)
    assert headers == {"X-Gryvia-Time": "1700000000",
                       "X-Gryvia-Signature": "31bcceb2e106086a5b3623200f4b7f54c95ba27df0dbd81918ba88c898b1e501"}


def test_signature_is_domain_separated_from_flight():
    from routers.flight import signature_headers
    flight = signature_headers("/api/v1/graph", TOKEN, 5)["X-Gryvia-Flight-Signature"]
    assert api_signature_headers("/api/v1/graph", TOKEN, 5)["X-Gryvia-Signature"] != flight


def test_defaults_are_unchanged(monkeypatch):
    for var in ("GRYVIA_COLLECTOR_CA_FILE", "GRYVIA_COLLECTOR_TLS", "GRYVIA_COLLECTOR_TOKEN",
                "GRYVIA_COLLECTOR_TOKEN_FILE"):
        monkeypatch.delenv(var, raising=False)
    assert collector_scheme() == "http"
    assert collector_token() is None
    assert signed_headers("/x") == {}


def test_token_file_and_short_token(monkeypatch, tmp_path):
    f = tmp_path / "t"
    f.write_text(TOKEN + "\n")
    monkeypatch.setenv("GRYVIA_COLLECTOR_TOKEN_FILE", str(f))
    assert collector_token() == TOKEN
    monkeypatch.delenv("GRYVIA_COLLECTOR_TOKEN_FILE")
    monkeypatch.setenv("GRYVIA_COLLECTOR_TOKEN", "short")
    assert collector_token() is None


# ---- real TLS server ----

def _key():
    return ec.generate_private_key(ec.SECP256R1())


def _pem(path, obj):
    if isinstance(obj, x509.Certificate):
        path.write_bytes(obj.public_bytes(serialization.Encoding.PEM))
    else:
        path.write_bytes(obj.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
                                           serialization.NoEncryption()))


class CA:
    def __init__(self, tmp, label):
        self.key = _key()
        self.name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, label)])
        self.cert = self._sign(self.name, self.key.public_key(), ca=True)
        self.file = tmp / f"{label}.crt"
        _pem(self.file, self.cert)
        self.tmp = tmp

    def _sign(self, subject, public, ca=False, sans=None, usage=None):
        now = datetime.datetime.now(datetime.timezone.utc)
        b = (x509.CertificateBuilder().subject_name(subject).issuer_name(self.name)
             .public_key(public).serial_number(x509.random_serial_number())
             .not_valid_before(now - datetime.timedelta(hours=1)).not_valid_after(now + datetime.timedelta(hours=1))
             .add_extension(x509.BasicConstraints(ca=ca, path_length=None), critical=True))
        b = b.add_extension(x509.SubjectKeyIdentifier.from_public_key(public), critical=False)
        b = b.add_extension(x509.AuthorityKeyIdentifier.from_issuer_public_key(self.key.public_key()), critical=False)
        b = b.add_extension(x509.KeyUsage(
            digital_signature=not ca, content_commitment=False, key_encipherment=False, data_encipherment=False,
            key_agreement=False, key_cert_sign=ca, crl_sign=ca, encipher_only=False, decipher_only=False),
            critical=True)
        if sans:
            b = b.add_extension(x509.SubjectAlternativeName(sans), critical=False)
        if usage:
            b = b.add_extension(x509.ExtendedKeyUsage([usage]), critical=False)
        return b.sign(self.key, hashes.SHA256())

    def issue(self, label, usage, sans=None):
        key = _key()
        cert = self._sign(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, label)]), key.public_key(),
                          sans=sans, usage=usage)
        crt, k = self.tmp / f"{label}.crt", self.tmp / f"{label}.key"
        _pem(crt, cert)
        _pem(k, key)
        return str(crt), str(k)


class Server:
    def __init__(self, server_cert, server_key, client_ca=None):
        self.requests = []
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):  # noqa: N802
                outer.requests.append((self.path, dict(self.headers)))
                body = json.dumps({"node": "n1", "namespace": "ml", "job": "train", "op_stats": {}}).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):
                pass

        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.minimum_version = ssl.TLSVersion.TLSv1_2
        ctx.load_cert_chain(server_cert, server_key)
        if client_ca:
            ctx.verify_mode = ssl.CERT_REQUIRED
            ctx.load_verify_locations(str(client_ca))
        self.httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.httpd.socket = ctx.wrap_socket(self.httpd.socket, server_side=True)
        self.url = f"https://127.0.0.1:{self.httpd.server_address[1]}"
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


@pytest.fixture
def env(tmp_path, monkeypatch):
    for var in ("GRYVIA_COLLECTOR_CA_FILE", "GRYVIA_COLLECTOR_CERT_FILE", "GRYVIA_COLLECTOR_KEY_FILE",
                "GRYVIA_COLLECTOR_TOKEN", "GRYVIA_COLLECTOR_TOKEN_FILE", "GRYVIA_COLLECTOR_TLS"):
        monkeypatch.delenv(var, raising=False)
    monkeypatch.setenv("GRYVIA_COLLECTOR_SERVER_NAME", NAME)
    ca = CA(tmp_path, "ca")
    server_cert = ca.issue("server", ExtendedKeyUsageOID.SERVER_AUTH, [x509.DNSName(NAME)])
    return types.SimpleNamespace(tmp=tmp_path, ca=ca, server_cert=server_cert, monkeypatch=monkeypatch)


def fetch(url):
    deps = types.SimpleNamespace(collector_fetch=None, collector_urls=[url])
    return asyncio.run(fetch_all_with_stats(deps, "/api/v1/gpu/nccl"))


def test_verifies_server_certificate_against_ca(env):
    srv = Server(*env.server_cert)
    try:
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_CA_FILE", str(env.ca.file))
        bodies, stats = fetch(srv.url)
        assert stats == {"reachable": 1, "total": 1} and bodies[0]["node"] == "n1"
        # A different CA must fail verification: the collector counts as unreachable, not trusted.
        other = CA(env.tmp, "other")
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_CA_FILE", str(other.file))
        assert fetch(srv.url) == ([], {"reachable": 0, "total": 1})
        # Wrong expected name fails hostname verification.
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_CA_FILE", str(env.ca.file))
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_SERVER_NAME", "evil.test")
        assert fetch(srv.url)[1]["reachable"] == 0
        # No CA configured: verification against system roots fails (self-made CA), never silently skipped.
        env.monkeypatch.delenv("GRYVIA_COLLECTOR_CA_FILE")
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_TLS", "1")
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_SERVER_NAME", NAME)
        assert fetch(srv.url)[1]["reachable"] == 0
    finally:
        srv.close()


def test_unreadable_ca_fails_closed(env):
    env.monkeypatch.setenv("GRYVIA_COLLECTOR_CA_FILE", str(env.tmp / "missing.crt"))
    assert fetch("https://127.0.0.1:9") == ([], {"reachable": 0, "total": 1})


def test_mtls_client_certificate(env):
    srv = Server(*env.server_cert, client_ca=env.ca.file)
    try:
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_CA_FILE", str(env.ca.file))
        assert fetch(srv.url)[1]["reachable"] == 0  # no client cert: handshake refused
        crt, key = env.ca.issue("gateway", ExtendedKeyUsageOID.CLIENT_AUTH)
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_CERT_FILE", crt)
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_KEY_FILE", key)
        assert fetch(srv.url)[1] == {"reachable": 1, "total": 1}
        # Client certificate from a foreign CA is rejected by the server.
        fcrt, fkey = CA(env.tmp, "foreign").issue("gateway2", ExtendedKeyUsageOID.CLIENT_AUTH)
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_CERT_FILE", fcrt)
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_KEY_FILE", fkey)
        assert fetch(srv.url)[1]["reachable"] == 0
    finally:
        srv.close()


def test_non_flight_requests_are_signed_when_token_set(env):
    srv = Server(*env.server_cert)
    try:
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_CA_FILE", str(env.ca.file))
        fetch(srv.url)
        assert "X-Gryvia-Signature" not in srv.requests[-1][1]  # backwards compatible when unset
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_TOKEN", TOKEN)
        fetch(srv.url)
        path, headers = srv.requests[-1]
        assert path == "/api/v1/gpu/nccl"
        expected = api_signature_headers(path, TOKEN, int(headers["X-Gryvia-Time"]))
        assert headers["X-Gryvia-Signature"] == expected["X-Gryvia-Signature"]
    finally:
        srv.close()


def test_flight_uses_tls_and_keeps_its_own_signature(env):
    srv = Server(*env.server_cert, client_ca=env.ca.file)
    try:
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_CA_FILE", str(env.ca.file))
        crt, key = env.ca.issue("gateway", ExtendedKeyUsageOID.CLIENT_AUTH)
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_CERT_FILE", crt)
        env.monkeypatch.setenv("GRYVIA_COLLECTOR_KEY_FILE", key)
        path = "/api/v1/flight/diagnose?namespace=ml&job=train"
        results = asyncio.run(flight_module._fan_out([srv.url], path, TOKEN))
        assert results[0][0] is True
        _, headers = srv.requests[-1]
        assert "X-Gryvia-Flight-Signature" in headers and "X-Gryvia-Signature" not in headers
    finally:
        srv.close()


def test_discovery_uses_https_when_ca_set(env):
    env.monkeypatch.setenv("GRYVIA_COLLECTOR_CA_FILE", str(env.ca.file))

    class Core:
        def list_pod_for_all_namespaces(self, label_selector):
            return types.SimpleNamespace(items=[types.SimpleNamespace(
                status=types.SimpleNamespace(phase="Running", pod_ip="10.1.1.1"))])

    deps = types.SimpleNamespace(collector_urls=None, k8s_core=Core())
    assert asyncio.run(collector_module.collector_urls(deps)) == ["https://10.1.1.1:9090"]
