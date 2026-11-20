# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""OIDC login end to end against a fake identity provider. Keys are generated here, discovery and JWKS
are served by a fake httpx.AsyncClient, so no network is used."""

import base64
import json
import os
import sys
import time
import types

import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from fastapi.testclient import TestClient
from jose import jwk, jwt

sys.path.insert(0, os.path.dirname(os.path.dirname(__file__)))

ISSUER = "https://idp.example.com/realms/gryvia"
CLIENT_ID = "gryvia-web"
AUDIENCE = "gryvia-api"
JWKS_URI = ISSUER + "/certs"
DISCOVERY_URL = ISSUER + "/.well-known/openid-configuration"
API_KEY = "s3cret-key"


class Key:
    def __init__(self, kid):
        self.kid = kid
        self.private = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        self.private_pem = self.private.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.PKCS8,
            serialization.NoEncryption(),
        ).decode()
        self.public_pem = (
            self.private.public_key()
            .public_bytes(
                serialization.Encoding.PEM,
                serialization.PublicFormat.SubjectPublicKeyInfo,
            )
            .decode()
        )

    def jwk(self):
        d = jwk.construct(self.public_pem, "RS256").to_dict()
        return {**d, "kid": self.kid, "use": "sig", "alg": "RS256"}

    def sign(self, claims, kid=None, alg="RS256"):
        return jwt.encode(
            claims, self.private_pem, algorithm=alg, headers={"kid": kid or self.kid}
        )


@pytest.fixture(scope="module")
def keys():
    return Key("key-1"), Key("key-2"), Key("rogue")


class FakeResponse:
    def __init__(self, data):
        self._data = data

    def raise_for_status(self):
        pass

    def json(self):
        return self._data


class FakeIdP:
    def __init__(self):
        self.jwks = {"keys": []}
        self.discovery = {
            "issuer": ISSUER,
            "jwks_uri": JWKS_URI,
            "authorization_endpoint": ISSUER + "/auth",
            "token_endpoint": ISSUER + "/token",
        }
        self.calls = []
        self.down = False

    def client_class(self):
        idp = self

        class FakeAsyncClient:
            def __init__(self, *a, **kw):
                pass

            async def __aenter__(self):
                return self

            async def __aexit__(self, *a):
                return False

            async def get(self, url, **kw):
                idp.calls.append(url)
                if idp.down:
                    raise ConnectionError("idp down")
                if url == DISCOVERY_URL:
                    return FakeResponse(idp.discovery)
                if url == JWKS_URI:
                    return FakeResponse(idp.jwks)
                raise AssertionError(f"unexpected fetch {url}")

        return FakeAsyncClient

    def count(self, url):
        return self.calls.count(url)


@pytest.fixture
def env(fake_k8s, monkeypatch, keys):
    from kubernetes import config

    monkeypatch.setattr(config, "load_incluster_config", lambda: None)
    monkeypatch.setattr(config, "load_kube_config", lambda: None)
    import main

    idp = FakeIdP()
    idp.jwks = {"keys": [keys[0].jwk()]}
    monkeypatch.setattr(
        main, "httpx", types.SimpleNamespace(AsyncClient=idp.client_class())
    )
    monkeypatch.setattr(main, "k8s_custom", fake_k8s)
    for tenant in ("acme", "t1", "t2"):
        fake_k8s.add("gryviatenants", {"metadata": {"name": tenant}, "spec": {}})
    monkeypatch.setattr(main, "API_KEY", API_KEY)
    monkeypatch.setattr(main, "OIDC_ENABLED", True)
    monkeypatch.setattr(main, "OIDC_ISSUER_URL", ISSUER)
    monkeypatch.setattr(main, "OIDC_CLIENT_ID", CLIENT_ID)
    monkeypatch.setattr(main, "OIDC_AUDIENCE", AUDIENCE)
    monkeypatch.setattr(main, "_jwks_cache", {})
    monkeypatch.setattr(main, "_jwks_cache_time", 0)
    monkeypatch.setattr(main, "_oidc_discovery", None)
    main.limiter.enabled = True
    main.limiter.reset()
    yield TestClient(main.app), main, idp
    main.limiter.reset()


def claims(**over):
    now = int(time.time())
    c = {
        "iss": ISSUER,
        "aud": AUDIENCE,
        "sub": "user-1",
        "email": "ada@example.com",
        "name": "Ada",
        "org": "acme",
        "iat": now,
        "exp": now + 600,
    }
    c.update(over)
    return {k: v for k, v in c.items() if v is not None}


def me(c, token):
    return c.get("/api/auth/me", headers={"Authorization": f"Bearer {token}"})


def b64(obj):
    raw = obj if isinstance(obj, bytes) else json.dumps(obj).encode()
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


# -- accepted tokens ---------------------------------------------------------


def test_valid_token_is_accepted_and_me_returns_identity(env, keys):
    c, _, _ = env
    tok = keys[0].sign(claims(groups=["team-a", "team-b"], org="acme"))
    r = me(c, tok)
    assert r.status_code == 200
    assert r.json() == {
        "authenticated": True,
        "method": "oidc",
        "sub": "user-1",
        "email": "ada@example.com",
        "name": "Ada",
        "groups": ["team-a", "team-b"],
        "org": "acme",
        "role": "tenant",
        "tenant": "acme",
        "tenants": ["acme"],
        "tenantNamespaces": ["tenant-acme"],
    }


def test_name_falls_back_to_preferred_username(env, keys):
    c, _, _ = env
    tok = keys[0].sign(claims(name=None, preferred_username="ada"))
    assert me(c, tok).json()["name"] == "ada"


def test_audience_list_containing_expected_audience_is_accepted(env, keys):
    c, _, _ = env
    assert me(c, keys[0].sign(claims(aud=["other", AUDIENCE]))).status_code == 200


# -- rejected tokens ---------------------------------------------------------


def test_expired_token_is_rejected(env, keys):
    c, _, _ = env
    assert me(c, keys[0].sign(claims(exp=int(time.time()) - 60))).status_code == 403


def test_wrong_issuer_is_rejected(env, keys):
    c, _, _ = env
    assert me(c, keys[0].sign(claims(iss="https://evil.example"))).status_code == 403


def test_wrong_audience_is_rejected(env, keys):
    c, _, _ = env
    assert me(c, keys[0].sign(claims(aud="someone-else"))).status_code == 403


def test_token_signed_by_a_different_key_is_rejected(env, keys):
    c, _, _ = env
    # Rogue key claims to be key-1.
    assert me(c, keys[2].sign(claims(), kid="key-1")).status_code == 403


@pytest.mark.parametrize("missing", ["exp", "iss", "aud", "sub"])
def test_token_without_required_claims_is_rejected(env, keys, missing):
    c, _, _ = env
    tok = keys[0].sign(claims(**{missing: None}))
    assert me(c, tok).status_code == 403


def age_jwks(main, seconds):
    """Pretend the cached JWKS was fetched `seconds` ago."""
    main._jwks_cache_time -= seconds


def test_unknown_kid_does_not_force_a_fetch_per_request(env, keys):
    c, main, idp = env
    # key-2 is not published. The first request fetches the keys (count 1); a refresh would be pointless
    # moments later, so repeated unknown-kid tokens must not hammer the identity provider.
    for _ in range(5):
        assert me(c, keys[1].sign(claims())).status_code == 403
    assert idp.count(JWKS_URI) == 1


def test_unknown_kid_refreshes_once_after_the_cooldown_then_rejects(env, keys):
    c, main, idp = env
    assert me(c, keys[1].sign(claims())).status_code == 403
    assert idp.count(JWKS_URI) == 1
    age_jwks(main, main._JWKS_MIN_REFRESH_SECONDS + 1)
    assert me(c, keys[1].sign(claims())).status_code == 403
    assert idp.count(JWKS_URI) == 2  # exactly one refresh


def test_key_rotation_is_picked_up_once_the_cooldown_has_passed(env, keys):
    c, main, idp = env
    assert me(c, keys[0].sign(claims())).status_code == 200
    idp.jwks = {"keys": [keys[0].jwk(), keys[1].jwk()]}
    # Keys were fetched moments ago, so the new key is not fetched yet ...
    assert me(c, keys[1].sign(claims())).status_code == 403
    assert idp.count(JWKS_URI) == 1
    # ... and is picked up by the first unknown-kid token after the cooldown.
    age_jwks(main, main._JWKS_MIN_REFRESH_SECONDS + 1)
    assert me(c, keys[1].sign(claims())).status_code == 200
    assert idp.count(JWKS_URI) == 2


def test_alg_none_is_rejected(env, keys):
    c, _, _ = env
    tok = f"{b64({'alg': 'none', 'typ': 'JWT', 'kid': 'key-1'})}.{b64(claims())}."
    assert me(c, tok).status_code == 403


def test_hs256_with_public_key_confusion_is_rejected(env, keys):
    import hashlib
    import hmac

    c, _, _ = env
    header = b64({"alg": "HS256", "typ": "JWT", "kid": "key-1"})
    body = b64(claims())
    for secret in (keys[0].public_pem.encode(), json.dumps(keys[0].jwk()).encode()):
        sig = b64(
            hmac.new(secret, f"{header}.{body}".encode(), hashlib.sha256).digest()
        )
        assert me(c, f"{header}.{body}.{sig}").status_code == 403


@pytest.mark.parametrize(
    "token",
    [
        "a.b.c",
        "..",
        "!!!.???.***",
        b64({"alg": "RS256"}) + ".e30.sig",
        b64({"alg": "RS256", "kid": ["x"]}) + "." + b64({}) + ".sig",
        b64([1, 2]) + ".e30.sig",
    ],
)
def test_malformed_jwt_looking_tokens_get_401_or_403_not_500(env, token):
    c, _, _ = env
    assert me(c, token).status_code in (401, 403)


def test_key_type_mismatch_does_not_crash(env, keys):
    """A JWKS entry the header alg cannot use must reject, not 500."""
    c, _, idp = env
    ec = {"kty": "EC", "crv": "P-256", "x": "AAAA", "y": "AAAA", "kid": "key-1"}
    idp.jwks = {"keys": [ec]}
    assert me(c, keys[0].sign(claims())).status_code in (401, 403)


def test_idp_outage_during_token_check_is_not_a_500(env, keys):
    c, _, idp = env
    idp.down = True
    assert me(c, keys[0].sign(claims())).status_code in (401, 403)


def test_discovery_without_jwks_uri_is_not_a_500(env, keys):
    c, _, idp = env
    idp.discovery = {"issuer": ISSUER}
    assert me(c, keys[0].sign(claims())).status_code in (401, 403)


def test_no_authorization_header_is_401(env):
    c, _, _ = env
    assert c.get("/api/auth/me").status_code == 401


# -- JWKS caching ------------------------------------------------------------


def test_jwks_is_cached_across_requests(env, keys):
    c, _, idp = env
    tok = keys[0].sign(claims())
    assert me(c, tok).status_code == 200
    assert me(c, tok).status_code == 200
    assert idp.count(JWKS_URI) == 1
    assert idp.count(DISCOVERY_URL) == 1


def test_jwks_is_refreshed_after_ttl(env, keys):
    c, main, idp = env
    tok = keys[0].sign(claims())
    assert me(c, tok).status_code == 200
    main._jwks_cache_time -= main._JWKS_CACHE_TTL + 1
    assert me(c, tok).status_code == 200
    assert idp.count(JWKS_URI) == 2


# -- /api/auth/config --------------------------------------------------------


def test_auth_config_when_enabled(env):
    c, _, _ = env
    body = c.get("/api/auth/config").json()
    assert body["oidcEnabled"] is True and body["apiKeyEnabled"] is True
    assert body["issuer"] == ISSUER and body["clientId"] == CLIENT_ID
    assert body["authorizationEndpoint"] == ISSUER + "/auth"
    assert body["tokenEndpoint"] == ISSUER + "/token"


def test_auth_config_discovery_failure(env):
    c, _, idp = env
    idp.down = True
    body = c.get("/api/auth/config").json()
    assert body["oidcEnabled"] is False
    assert body["error"] == "OIDC provider unreachable"
    assert body["apiKeyEnabled"] is True


def test_auth_config_disabled(env, monkeypatch):
    c, main, idp = env
    monkeypatch.setattr(main, "OIDC_ENABLED", False)
    body = c.get("/api/auth/config").json()
    assert body["oidcEnabled"] is False and body["apiKeyEnabled"] is True
    assert "issuer" not in body
    assert idp.calls == []


def test_auth_config_says_where_the_key_lives(env, monkeypatch):
    c, main, _ = env
    monkeypatch.setattr(main, "OIDC_ENABLED", False)
    monkeypatch.setenv("GRYVIA_JOB_NAMESPACE", "ai-system")
    monkeypatch.setenv("GRYVIA_API_KEY_SECRET", "my-key")
    body = c.get("/api/auth/config").json()
    assert body["credentials"] == {"username": "admin", "secret": "my-key", "key": "GRYVIA_API_KEY",
                                   "namespace": "ai-system"}
    assert body["instance"] == {"product": "Gryvia", "version": main.app.version, "namespace": "ai-system"}
    assert main.API_KEY not in str(body)


def test_auth_config_defaults_and_oidc_branch(env, monkeypatch):
    c, main, _ = env
    monkeypatch.delenv("GRYVIA_API_KEY_SECRET", raising=False)
    monkeypatch.delenv("GRYVIA_JOB_NAMESPACE", raising=False)
    body = c.get("/api/auth/config").json()
    assert body["oidcEnabled"] is True
    assert body["credentials"]["secret"] == "gryvia-api-key"
    assert body["credentials"]["namespace"] == "gryvia-system"


def test_auth_config_no_credentials_without_api_key(env, monkeypatch):
    c, main, _ = env
    monkeypatch.setattr(main, "OIDC_ENABLED", False)
    monkeypatch.setattr(main, "API_KEY", "")
    body = c.get("/api/auth/config").json()
    assert body["apiKeyEnabled"] is False and "credentials" not in body
    assert body["instance"]["product"] == "Gryvia"


# -- OIDC disabled / other credential types ----------------------------------


def test_disabled_oidc_never_contacts_idp_and_rejects_jwt(env, keys, monkeypatch):
    c, main, idp = env
    monkeypatch.setattr(main, "OIDC_ENABLED", False)
    assert me(c, keys[0].sign(claims())).status_code == 403
    assert idp.calls == []


def test_api_key_still_works_when_oidc_enabled(env):
    c, _, idp = env
    r = me(c, API_KEY)
    assert r.status_code == 200 and r.json()["method"] == "api_key"
    assert idp.calls == []


def test_dotted_api_key_is_not_lost_to_oidc(env, monkeypatch):
    c, main, _ = env
    monkeypatch.setattr(main, "API_KEY", "a.b.c")
    assert me(c, "a.b.c").json()["method"] == "api_key"


def test_session_token_is_not_confused_with_a_jwt(env, keys):
    c, main, idp = env
    tok = main.issue_session_token("admin")["token"]
    assert tok.count(".") == 2
    r = me(c, tok)
    assert r.status_code == 200 and r.json()["method"] == "api_key"
    assert idp.calls == []


def test_forged_session_token_is_rejected(env, keys):
    c, _, _ = env
    # A well formed OIDC JWT dressed with the session prefix must not pass as OIDC.
    assert me(c, "gs1." + keys[0].sign(claims())).status_code == 401
    assert me(c, "gs1.abc.def").status_code == 401


def test_oidc_jwt_does_not_pass_as_session_or_api_key(env, keys):
    c, _, _ = env
    r = me(c, keys[2].sign(claims(), kid="key-1"))
    assert r.status_code == 403


# -- tenant namespaces -------------------------------------------------------


def test_tenants_from_org(env):
    _, main, _ = env
    assert main._extract_tenant_namespaces({"org": "acme"}) == ["acme"]


def test_org_takes_precedence_over_groups(env):
    _, main, _ = env
    assert main._extract_tenant_namespaces({"org": "acme", "groups": ["a"]}) == ["acme"]


def test_tenants_from_groups_ignore_non_strings(env):
    _, main, _ = env
    assert main._extract_tenant_namespaces({"groups": ["a", 1, None, "b"]}) == [
        "a",
        "b",
    ]


@pytest.mark.parametrize(
    "cl",
    [
        {},
        {"org": ""},
        {"org": 5},
        {"groups": []},
        {"groups": "a"},
        {"groups": {"a": 1}},
    ],
)
def test_no_tenants_when_claims_are_absent_or_odd(env, cl):
    _, main, _ = env
    assert main._extract_tenant_namespaces(cl) is None


def test_me_reports_groups_as_tenants_when_no_org(env, keys):
    c, _, _ = env
    body = me(c, keys[0].sign(claims(org=None, groups=["t1", "t2"]))).json()
    assert body["tenantNamespaces"] == ["tenant-t1", "tenant-t2"] and body["org"] == ""


def test_token_without_tenant_claims_is_refused(env, keys):
    c, _, _ = env
    r = me(c, keys[0].sign(claims(org=None)))
    assert r.status_code == 403 and "tenant" in r.json()["detail"]


def test_key_rotation_refresh_works_on_a_freshly_booted_host(env, keys, monkeypatch):
    # A host up for less than the cache TTL has a small monotonic clock; the forced refresh must not depend on it.
    import time
    monkeypatch.setattr(time, "monotonic", lambda: time.perf_counter() - time.perf_counter() + 100.0)
    c, main, idp = env
    main._jwks_cache, main._jwks_cache_time = {}, 0
    assert me(c, keys[0].sign(claims())).status_code == 200
    idp.jwks = {"keys": [keys[0].jwk(), keys[1].jwk()]}
    age_jwks(main, main._JWKS_MIN_REFRESH_SECONDS + 1)
    assert me(c, keys[1].sign(claims())).status_code == 200
