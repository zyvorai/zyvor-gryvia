#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""A FAKE collector for the network-intelligence e2e test (.github/workflows/e2e-netintel.yml).

It serves canned JSON on the routes the operator reads (/api/v1/graph, /api/v1/anomalies,
/api/v1/security/alerts) with the collector's real JSON field names, and verifies the operator's request signature
exactly like collector/listener_security.go (HMAC-SHA256 over "GRYVIA-API-V1\\nGET\\n<request-uri>\\n<unix>", 30 s skew)
when a token file is present. It measures NOTHING: it proves the operator side (discovery by pod label, signing,
merging across pods, status/conditions/objects) and says nothing about the real eBPF collector.
"""
import hmac
import hashlib
import json
import os
import time
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TOKEN_FILE = os.environ.get("TOKEN_FILE", "/etc/collector-token/token")
def _start():
    """One detection time for every replica. Each replica computing its own made the operator see two
    anomalies (same type, detection times a second apart) and the e2e's len == 1 check flaked."""
    try:
        with open(os.environ.get("START_FILE", "/srv/start.txt"), "r", encoding="utf-8") as f:
            return f.read(64).strip()
    except OSError:
        return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


START = _start()

GRAPH = {
    "nodes": [
        {"id": "web", "service": "web", "namespace": "shop", "pod_count": 1},
        {"id": "api", "service": "api", "namespace": "shop", "pod_count": 1},
        {"id": "db", "service": "db", "namespace": "shop", "pod_count": 1},
    ],
    "edges": [
        {"source": "web", "target": "api", "protocol": "TCP", "port": 8080, "bytes_total": 4096,
         "latency_p50_ms": 12.5, "last_seen": START, "flow_count": 40},
        {"source": "api", "target": "db", "protocol": "TCP", "port": 5432, "bytes_total": 2048,
         "latency_p50_ms": 3.0, "last_seen": START, "flow_count": 25},
        {"source": "api", "target": "8.8.8.8", "protocol": "TCP", "port": 443, "bytes_total": 512,
         "latency_p50_ms": 0, "last_seen": START, "flow_count": 2},
    ],
}
ANOMALIES = [
    {"type": "latency_spike", "service": "api", "namespace": "shop", "value": 250.0, "baseline": 40.0,
     "stddev": 10.0, "threshold": 70.0, "detected_at": START, "message": "api latency 250ms vs baseline 40ms"},
]
ALERTS = {
    "alerts": [
        {"timestamp": START, "event_type": "crypto_mining", "severity": "high", "pid": 4242, "uid": 0,
         "process_name": "xmrig", "details": "connect to mining pool", "src_ip": "10.1.2.3",
         "dst_ip": "1.2.3.4", "dst_port": 3333},
    ],
    "counts": {"crypto_mining": 1},
}
ROUTES = {"/api/v1/graph": GRAPH, "/api/v1/anomalies": ANOMALIES, "/api/v1/security/alerts": ALERTS}


def token():
    try:
        with open(TOKEN_FILE, "r", encoding="utf-8") as f:
            t = f.read(4097).strip()
        return t.encode() if len(t) >= 32 else None
    except OSError:
        return None


def authorized(h):
    tok = token()
    if tok is None:
        return True
    stamp, sig = h.headers.get("X-Gryvia-Time", ""), h.headers.get("X-Gryvia-Signature", "")
    try:
        when = int(stamp)
    except ValueError:
        return False
    if abs(time.time() - when) > 30:
        return False
    msg = ("GRYVIA-API-V1\n" + "GET" + "\n" + h.path + "\n" + stamp).encode()
    return hmac.compare_digest(hmac.new(tok, msg, hashlib.sha256).hexdigest(), sig)


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802
        path = self.path.split("?")[0]
        if path in ("/healthz", "/readyz"):
            return self._send(200, b"ok", "text/plain")
        if not authorized(self):
            return self._send(401, b"unauthorized", "text/plain")
        if path not in ROUTES:
            return self._send(404, b"not found", "text/plain")
        self._send(200, json.dumps(ROUTES[path]).encode(), "application/json")

    def _send(self, code, body, ctype):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        print("%s %s" % (self.address_string(), fmt % args), flush=True)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 9090), Handler).serve_forever()
