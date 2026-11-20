# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Gateway /metrics: disabled by default, bearer-token guarded, bounded labels."""

import os
import sys

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

sys.path.insert(0, os.path.dirname(os.path.dirname(__file__)))

from routers import observability  # noqa: E402

TOKEN = "t" * 40


def build(monkeypatch, token=None):
    if token is None:
        monkeypatch.delenv("GRYVIA_METRICS_TOKEN", raising=False)
    else:
        monkeypatch.setenv("GRYVIA_METRICS_TOKEN", token)
    app = FastAPI()
    observability.install(app)

    @app.get("/api/jobs/{name}")
    async def job(name: str):
        return {"name": name}

    return TestClient(app, raise_server_exceptions=False)


def sample(name, **labels):
    return observability.REGISTRY.get_sample_value(name, labels) or 0.0


def test_disabled_without_token(monkeypatch):
    assert build(monkeypatch).get("/metrics").status_code == 404


def test_short_token_keeps_metrics_disabled(monkeypatch):
    assert build(monkeypatch, "short").get("/metrics").status_code == 404


def test_requires_bearer_token(monkeypatch):
    c = build(monkeypatch, TOKEN)
    assert c.get("/metrics").status_code == 401
    assert (
        c.get("/metrics", headers={"Authorization": "Bearer wrong"}).status_code == 401
    )
    r = c.get("/metrics", headers={"Authorization": f"Bearer {TOKEN}"})
    assert r.status_code == 200
    assert "gryvia_gateway_http_requests_total" in r.text


def test_route_template_not_raw_path_and_unmatched_bucket(monkeypatch):
    c = build(monkeypatch, TOKEN)
    before = sample(
        "gryvia_gateway_http_requests_total",
        method="GET",
        route="/api/jobs/{name}",
        status="200",
    )
    c.get("/api/jobs/user-secret-name")
    c.get("/no/such/path/alice")
    assert (
        sample(
            "gryvia_gateway_http_requests_total",
            method="GET",
            route="/api/jobs/{name}",
            status="200",
        )
        == before + 1
    )
    assert (
        sample(
            "gryvia_gateway_http_requests_total",
            method="GET",
            route="unmatched",
            status="404",
        )
        >= 1
    )
    text = c.get("/metrics", headers={"Authorization": f"Bearer {TOKEN}"}).text
    assert "user-secret-name" not in text and "alice" not in text
    assert "gryvia_gateway_http_request_duration_seconds_bucket" in text


def test_auth_and_fanout_helpers():
    b = sample("gryvia_gateway_auth_attempts_total", method="api_key", result="failure")
    observability.record_auth("api_key", False)
    observability.record_auth(
        "bogus-user-input", True
    )  # unknown method collapses to "none"
    assert (
        sample("gryvia_gateway_auth_attempts_total", method="api_key", result="failure")
        == b + 1
    )
    assert (
        sample("gryvia_gateway_auth_attempts_total", method="none", result="success")
        >= 1
    )
    observability.record_fanout(1, 3)
    assert sample("gryvia_gateway_collector_reachable") == 1
    assert sample("gryvia_gateway_collector_targets") == 3
    assert sample("gryvia_gateway_collector_fanout_total", outcome="partial") >= 1
    observability.record_fanout(0, 0)
    assert sample("gryvia_gateway_collector_fanout_total", outcome="no_collectors") >= 1


@pytest.fixture
def main_client(fake_k8s, monkeypatch):
    from kubernetes import config

    monkeypatch.setattr(config, "load_incluster_config", lambda: None)
    monkeypatch.setattr(config, "load_kube_config", lambda: None)
    import main

    monkeypatch.setattr(main, "API_KEY", "s3cret-key")
    return TestClient(main.app)


def test_main_counts_auth_failures_and_success(main_client):
    b = sample("gryvia_gateway_auth_attempts_total", method="api_key", result="failure")
    s = sample("gryvia_gateway_auth_attempts_total", method="api_key", result="success")
    assert (
        main_client.get(
            "/api/auth/me", headers={"Authorization": "Bearer nope"}
        ).status_code
        == 403
    )
    main_client.get("/api/auth/me", headers={"Authorization": "Bearer s3cret-key"})
    assert (
        sample("gryvia_gateway_auth_attempts_total", method="api_key", result="failure")
        == b + 1
    )
    assert (
        sample("gryvia_gateway_auth_attempts_total", method="api_key", result="success")
        == s + 1
    )
