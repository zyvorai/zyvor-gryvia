# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Login endpoint, default-key flag and the proxy-aware rate-limit key. main.py loads a kubeconfig at
import time, so the loaders are stubbed before importing it (same approach as test_main_endpoints).
"""

import os
import sys

import pytest
from fastapi.testclient import TestClient

sys.path.insert(0, os.path.dirname(os.path.dirname(__file__)))


@pytest.fixture
def auth(fake_k8s, monkeypatch):
    from kubernetes import config

    monkeypatch.setattr(config, "load_incluster_config", lambda: None)
    monkeypatch.setattr(config, "load_kube_config", lambda: None)
    import main

    async def no_sleep(_):
        return None

    monkeypatch.setattr(main.asyncio, "sleep", no_sleep)
    monkeypatch.setattr(main, "API_KEY", "s3cret-key")
    main.limiter.enabled = True
    main.limiter.reset()
    yield TestClient(main.app), main
    main.limiter.reset()


def login(c, user="admin", pw="s3cret-key"):
    return c.post("/api/auth/login", json={"username": user, "password": pw})


def test_login_returns_a_session_token_not_the_api_key(auth):
    c, main = auth
    r = login(c)
    assert r.status_code == 200
    body = r.json()
    assert body["token"].startswith("gs1.") and "s3cret-key" not in r.text
    assert body["name"] == "admin" and body["usingDefaultKey"] is False
    assert body["expiresAt"] > main.time.time()


@pytest.mark.parametrize(
    "user,pw", [("admin", "wrong"), ("root", "s3cret-key"), ("", ""), ("admin", "")]
)
def test_login_rejects_bad_credentials(auth, user, pw):
    c, _ = auth
    r = login(c, user, pw)
    assert r.status_code == 401
    assert "token" not in r.text


def test_login_when_auth_is_not_configured(auth, monkeypatch):
    c, main = auth
    monkeypatch.setattr(main, "API_KEY", "")
    assert login(c, pw="anything").status_code == 401


def test_login_is_rate_limited(auth):
    c, _ = auth
    codes = [login(c, pw="wrong").status_code for _ in range(12)]
    assert codes[:10] == [401] * 10
    assert 429 in codes[10:]


def test_login_rejects_oversized_input(auth):
    c, _ = auth
    assert login(c, pw="x" * 1000).status_code == 422


def bearer(token):
    return {"Authorization": f"Bearer {token}"}


def test_session_token_authenticates_requests(auth):
    c, _ = auth
    token = login(c).json()["token"]
    me = c.get("/api/auth/me", headers=bearer(token))
    assert me.status_code == 200 and me.json()["name"] == "admin"


def test_api_key_still_works_directly_for_scripts(auth):
    c, _ = auth
    assert c.get("/api/auth/me", headers=bearer("s3cret-key")).status_code == 200


def test_expired_session_is_rejected_with_401(auth, monkeypatch):
    c, main = auth
    token = login(c).json()["token"]
    real = main.time.time
    monkeypatch.setattr(
        main.time, "time", lambda: real() + main.SESSION_TTL_SECONDS + 5
    )
    r = c.get("/api/auth/me", headers=bearer(token))
    assert r.status_code == 401 and "expired" in r.json()["detail"].lower()


def test_tampered_or_forged_session_is_rejected(auth):
    c, main = auth
    token = login(c).json()["token"]
    payload, sig = token[len("gs1.") :].split(".")
    forged_payload = main._b64url(b'{"sub":"admin","iat":1,"exp":99999999999}')
    for bad in (
        f"gs1.{forged_payload}.{sig}",
        f"gs1.{payload}.{'A' * len(sig)}",
        "gs1.",
        "gs1.x.y",
        "gs1.not-base64.!!",
    ):
        assert c.get("/api/auth/me", headers=bearer(bad)).status_code == 401


def test_rotating_the_api_key_ends_every_session(auth, monkeypatch):
    c, main = auth
    token = login(c).json()["token"]
    monkeypatch.setattr(main, "API_KEY", "a-new-key")
    assert c.get("/api/auth/me", headers=bearer(token)).status_code == 401


def test_session_secret_can_be_set_separately(auth, monkeypatch):
    c, main = auth
    token = main.issue_session_token("admin")["token"]
    monkeypatch.setenv("GRYVIA_SESSION_SECRET", "other-secret")
    assert main.verify_session_token(token) is None


def test_default_key_is_flagged(auth, monkeypatch):
    c, main = auth
    monkeypatch.setattr(main, "API_KEY", "Admin@321")
    assert login(c, pw="Admin@321").json()["usingDefaultKey"] is True
    me = c.get("/api/auth/me", headers={"Authorization": "Bearer Admin@321"}).json()
    assert me["usingDefaultKey"] is True and me["name"] == "admin"


def test_custom_key_is_not_flagged_in_me(auth):
    c, _ = auth
    me = c.get("/api/auth/me", headers={"Authorization": "Bearer s3cret-key"}).json()
    assert me["usingDefaultKey"] is False


class _Req:
    def __init__(self, peer, xff=None):
        self.client = type("C", (), {"host": peer})()
        self.headers = {"x-forwarded-for": xff} if xff else {}


def test_rate_limit_key_trusts_forwarded_for_only_from_private_peers(auth):
    _, main = auth
    assert main._client_ip(_Req("10.42.0.5", "203.0.113.9, 10.42.0.5")) == "203.0.113.9"
    assert main._client_ip(_Req("127.0.0.1", "198.51.100.7")) == "198.51.100.7"
    # A public peer cannot choose its own bucket by sending the header.
    assert main._client_ip(_Req("8.8.8.8", "1.2.3.4")) == "8.8.8.8"
    assert main._client_ip(_Req("10.0.0.1")) == "10.0.0.1"
