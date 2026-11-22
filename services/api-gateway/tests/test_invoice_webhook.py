# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Invoice webhook: signing, headers, SSRF guards, admin-only, feature off by default."""
import hashlib
import hmac
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from routers import invoice_webhook


class Recorder(BaseHTTPRequestHandler):
    hits = []
    status = 200

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        Recorder.hits.append((self.path, dict(self.headers), self.rfile.read(n)))
        if self.path == "/redirect":
            self.send_response(302)
            self.send_header("Location", "/followed")
        else:
            self.send_response(Recorder.status)
        self.end_headers()

    def log_message(self, *a):
        pass


@pytest.fixture
def server():
    Recorder.hits, Recorder.status = [], 200
    srv = HTTPServer(("127.0.0.1", 0), Recorder)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


@pytest.fixture
def seeded(fake_k8s):
    fake_k8s.add("gryviausagerecords", {"metadata": {"name": "r1"}, "spec": {
        "tenant": "alpha", "job": "j", "sku": "a100", "gpuType": "A100", "gpus": 1, "start": "2026-03-10T10:00:00Z",
        "gpuHours": 2, "rate": 4, "cost": 8, "currency": "USD", "final": True}}, "tenant-alpha")


URL = "/api/invoices/alpha/2026-03/send"


def test_unset_url_is_503(make_client, seeded, monkeypatch):
    monkeypatch.delenv("GRYVIA_INVOICE_WEBHOOK_URL", raising=False)
    assert make_client("invoices").post(URL).status_code == 503


def test_admin_only(make_client, seeded, monkeypatch, server):
    monkeypatch.setenv("GRYVIA_INVOICE_WEBHOOK_URL", server)
    assert make_client("invoices", role="tenant", tenants=["alpha"]).post(URL).status_code == 403
    assert Recorder.hits == []


def test_delivery_is_signed(make_client, seeded, monkeypatch, server):
    monkeypatch.setenv("GRYVIA_INVOICE_WEBHOOK_URL", server + "/hook")
    monkeypatch.setenv("GRYVIA_INVOICE_WEBHOOK_SECRET", "s3cret")
    r = make_client("invoices").post(URL)
    assert r.json() == {"delivered": True, "status": 200, "error": None}
    path, headers, body = Recorder.hits[0]
    assert path == "/hook" and headers["Content-Type"] == "application/json"
    ts = headers["X-Gryvia-Timestamp"]
    want = "sha256=" + hmac.new(b"s3cret", ts.encode() + b"." + body, hashlib.sha256).hexdigest()
    assert headers["X-Gryvia-Signature"] == want
    assert json.loads(body)["number"] == "INV-alpha-202603"


def test_no_secret_means_unsigned(make_client, seeded, monkeypatch, server):
    monkeypatch.setenv("GRYVIA_INVOICE_WEBHOOK_URL", server)
    monkeypatch.delenv("GRYVIA_INVOICE_WEBHOOK_SECRET", raising=False)
    make_client("invoices").post(URL)
    assert "X-Gryvia-Signature" not in Recorder.hits[0][1]


def test_receiver_error_and_no_redirects(make_client, seeded, monkeypatch, server):
    monkeypatch.setenv("GRYVIA_INVOICE_WEBHOOK_URL", server + "/redirect")
    r = make_client("invoices").post(URL).json()
    assert r["delivered"] is False and r["status"] == 302
    assert [h[0] for h in Recorder.hits] == ["/redirect"]
    Recorder.status = 500
    monkeypatch.setenv("GRYVIA_INVOICE_WEBHOOK_URL", server)
    assert make_client("invoices").post(URL).json()["status"] == 500


def test_unknown_tenant_404(make_client, seeded, monkeypatch, server):
    monkeypatch.setenv("GRYVIA_INVOICE_WEBHOOK_URL", server)
    assert make_client("invoices").post("/api/invoices/nobody/2026-03/send").status_code == 404


@pytest.mark.parametrize("url", [
    "http://example.com/hook",                       # plain http to a non-loopback host
    "https://169.254.169.254/latest/meta-data",      # cloud metadata
    "https://[fe80::1]/hook",                        # IPv6 link-local
    "https://0.0.0.0/hook",
    "https://user:pw@127.0.0.1/hook",                # embedded credentials
    "ftp://127.0.0.1/hook",
    "https://127.0.0.1/hook",                        # loopback only allowed for plain-http localhost? see below
])
def test_check_url_blocks(url, monkeypatch):
    monkeypatch.setattr(invoice_webhook, "_resolve", lambda h, p: [h.strip("[]")] if h != "example.com"
                        else ["93.184.216.34"])
    if url == "https://127.0.0.1/hook":
        assert invoice_webhook.check_url(url) == ""  # https loopback is fine (local receiver)
    else:
        assert invoice_webhook.check_url(url) != ""


def test_check_url_blocks_hostname_resolving_to_metadata(monkeypatch):
    monkeypatch.setattr(invoice_webhook, "_resolve", lambda h, p: ["93.184.216.34", "169.254.169.254"])
    assert "blocked" in invoice_webhook.check_url("https://hooks.example.com/x")


def test_check_url_allows_public_https(monkeypatch):
    monkeypatch.setattr(invoice_webhook, "_resolve", lambda h, p: ["93.184.216.34"])
    assert invoice_webhook.check_url("https://hooks.example.com/x") == ""
    monkeypatch.setattr(invoice_webhook, "_resolve", lambda h, p: ["127.0.0.1"])
    assert invoice_webhook.check_url("https://hooks.example.com/x") != ""  # public name -> loopback: blocked
