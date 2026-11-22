# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Flight Recorder cluster aggregation, coverage, and namespace isolation."""
import asyncio

import httpx

import routers.flight as flight_module
from routers.flight import _read_node, flight_collector_urls, gather, merge, signature_headers

TOKEN = "0123456789abcdef0123456789abcdef"


def report(node, namespace="ml", job="train", count=2):
    return {"node": node, "namespace": namespace, "job": job,
            "counts": {"tcp_retransmit": count},
            "events": [{"time": f"2026-01-01T00:00:0{count}Z", "kind": "tcp_retransmit",
                        "identity": {"namespace": namespace, "job": job, "node": node}}],
            "findings": [{"code": "tcp_retransmits_observed", "evidence": "observed"}]}


def test_merge_preserves_partial_coverage_and_time_order():
    result = merge("ml", "train", [report("node-b", count=3), report("node-a", count=1)],
                   {"total": 3, "reachable": 2, "reporting": 2})
    assert result["coverage"]["complete"] is False
    assert result["counts"]["tcp_retransmit"] == 4
    assert result["nodes"] == ["node-a", "node-b"]
    assert [event["node"] for event in result["events"]] == ["node-a", "node-b"]


def test_admin_route_and_no_data(make_client):
    async def fetch(path):
        assert "namespace=ml" in path and "job=train" in path
        return [report("node-a"), None], 2
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetch)
    result = client.get("/api/flight/jobs/train?namespace=ml")
    assert result.status_code == 200
    assert result.json()["coverage"] == {"total": 2, "reachable": 2, "reporting": 1, "complete": True}


def test_tenant_cannot_query_other_namespace(make_client):
    calls = []
    async def fetch(_path):
        calls.append(_path)
        return [report("node-a", namespace="tenant-alpha")]
    client = make_client("flight", role="tenant", tenants=["alpha"],
                         flight_token=TOKEN, collector_fetch=fetch)
    assert client.get("/api/flight/jobs/train?namespace=tenant-beta").status_code == 403
    assert calls == []
    assert client.get("/api/flight/jobs/train?namespace=tenant-alpha").status_code == 200


def test_missing_token_fails_closed(make_client):
    client = make_client("flight")
    assert client.get("/api/flight/jobs/train?namespace=ml").status_code == 503


def test_all_collectors_unreachable_returns_503(make_client):
    async def fetch(_path):
        return [], 2
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetch)
    assert client.get("/api/flight/jobs/train?namespace=ml").status_code == 503


def test_mismatched_report_does_not_leak_events(make_client):
    async def fetch(_path):
        return [report("node-a", namespace="other")]
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetch)
    body = client.get("/api/flight/jobs/train?namespace=ml").json()
    assert body["events"] == []
    assert body["coverage"]["reporting"] == 0


def test_collector_token_header_and_404_coverage():
    def respond(request):
        assert "X-Gryvia-Flight-Token" not in request.headers
        assert len(request.headers["X-Gryvia-Flight-Signature"]) == 64
        return httpx.Response(404)
    async def check():
        async with httpx.AsyncClient(transport=httpx.MockTransport(respond)) as client:
            return await _read_node(client, "http://collector", "/api/v1/flight/diagnose?job=train", "secret")
    assert asyncio.run(check()) == (True, None)


def test_signature_covers_path_and_timestamp():
    a = signature_headers("/api/v1/flight/diagnose?job=one", "secret", 123)
    b = signature_headers("/api/v1/flight/diagnose?job=two", "secret", 123)
    assert a["X-Gryvia-Flight-Signature"] != b["X-Gryvia-Flight-Signature"]


def test_collector_discovery_is_scoped_to_trusted_namespace():
    from types import SimpleNamespace
    class Core:
        def list_namespaced_pod(self, namespace, label_selector):
            assert namespace == "gryvia-network"
            assert label_selector == "app.kubernetes.io/component=collector"
            return SimpleNamespace(items=[SimpleNamespace(status=SimpleNamespace(phase="Running", pod_ip="10.2.3.4"))])
    deps = SimpleNamespace(collector_urls=None, flight_collector_namespace="gryvia-network", k8s_core=Core())
    assert asyncio.run(flight_collector_urls(deps)) == ["http://10.2.3.4:9090"]


def test_rank_api_duration_requires_samples_and_names_the_limit():
    events = []
    for rank, duration in (("0", 100_000), ("1", 500_000)):
        for n in range(3):
            events.append({"kind": "nccl_collective", "operation": "AllReduce", "duration_ns": duration,
                           "time": f"2026-01-01T00:00:0{n}Z",
                           "identity": {"namespace": "ml", "job": "train", "rank": rank}})
    result = merge("ml", "train", [{"node": "gpu-1", "namespace": "ml", "job": "train", "events": events}],
                   {"total": 1, "reachable": 1, "reporting": 1})
    assert len(result["rankObservations"]) == 2
    assert result["findings"][0]["code"] == "nccl_api_duration_outlier"
    assert "does not establish GPU or network cause" in result["findings"][0]["evidence"]


def test_signature_cross_language_vector():
    """Same constants as TestFlightSignatureCrossLanguageVector in collector/flight_auth_test.go.

    The Go verifier accepts exactly this signature; if this test or that one changes alone, the
    signer and verifier have drifted.
    """
    path = "/api/v1/flight/diagnose?namespace=ml&job=train"
    headers = signature_headers(path, TOKEN, 1700000000)
    assert headers == {"X-Gryvia-Flight-Time": "1700000000",
                       "X-Gryvia-Flight-Signature":
                       "1f2b78d5323fef0d77f9f1426ab3f23ef50b33dd60b244c298bdaefae1b5d46f"}


def test_request_sent_matches_signed_path_exactly():
    """httpx must put the signed string on the wire unchanged (the collector verifies RequestURI)."""
    seen = {}

    def respond(request):
        seen["target"] = request.url.raw_path.decode()
        seen["sig"] = request.headers["X-Gryvia-Flight-Signature"]
        seen["time"] = request.headers["X-Gryvia-Flight-Time"]
        return httpx.Response(200, json={"node": "n"})

    path = "/api/v1/flight/diagnose?namespace=ml&job=train"

    async def check():
        async with httpx.AsyncClient(transport=httpx.MockTransport(respond)) as client:
            return await _read_node(client, "http://10.0.0.1:9090", path, TOKEN)

    assert asyncio.run(check()) == (True, {"node": "n"})
    assert seen["target"] == path
    assert seen["sig"] == signature_headers(path, TOKEN, int(seen["time"]))["X-Gryvia-Flight-Signature"]


def test_collector_errors_and_oversize_are_unreachable_and_not_logged(caplog):
    async def check(handler):
        async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
            return await _read_node(client, "http://c", "/p", TOKEN)

    caplog.set_level("DEBUG")
    assert asyncio.run(check(lambda r: httpx.Response(401))) == (False, None)
    assert asyncio.run(check(lambda r: httpx.Response(503))) == (False, None)
    assert asyncio.run(check(lambda r: httpx.Response(200, content=b"not json"))) == (False, None)
    big = b"{" + b" " * (flight_module.MAX_RESPONSE_BYTES + 1) + b"}"
    assert asyncio.run(check(lambda r: httpx.Response(200, content=big))) == (False, None)
    assert asyncio.run(check(lambda r: httpx.Response(200, json=[1]))) == (True, None)
    assert TOKEN not in caplog.text and "Signature" not in caplog.text


def test_tenant_scoping(make_client):
    calls = []

    async def fetch(path):
        calls.append(path)
        return [report("node-a", namespace="tenant-alpha")]

    client = make_client("flight", role="tenant", tenants=["alpha", "gamma"], flight_token=TOKEN,
                         collector_fetch=fetch)
    for other in ("tenant-beta", "default", "kube-system", "gryvia-network"):
        assert client.get(f"/api/flight/jobs/train?namespace={other}").status_code == 403
    assert calls == []
    assert client.get("/api/flight/jobs/train?namespace=tenant-gamma").status_code == 200
    # No namespace given: the tenant's own first namespace, never the admin default.
    assert client.get("/api/flight/jobs/train").status_code == 200
    assert "namespace=tenant-alpha" in calls[-1]


def test_admin_may_query_any_namespace_and_defaults_to_job_namespace(make_client):
    calls = []

    async def fetch(path):
        calls.append(path)
        return []

    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetch)
    assert client.get("/api/flight/jobs/train?namespace=tenant-beta").status_code == 503  # no collector answered
    client.get("/api/flight/jobs/train")
    assert "namespace=tenant-beta" in calls[0] and "namespace=default" in calls[1]


def test_invalid_names_are_rejected_before_any_fetch(make_client):
    calls = []

    async def fetch(path):
        calls.append(path)
        return [report("node-a")]

    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetch)
    for bad in ("../x", "Train", "a%0d%0ab", "x" * 64, "a b"):
        assert client.get("/api/flight/jobs/train", params={"namespace": bad}).status_code == 400
    assert client.get("/api/flight/jobs/" + "x" * 64, params={"namespace": "ml"}).status_code == 400
    assert calls == []


def test_short_token_fails_closed(make_client):
    client = make_client("flight", flight_token="too-short", collector_fetch=lambda p: None)
    assert client.get("/api/flight/jobs/train?namespace=ml").status_code == 503


def test_partial_coverage_is_never_complete_and_truncation_is_flagged():
    body = report("node-a")
    body["events"] = body["events"] * flight_module.NODE_EVENT_LIMIT
    result = merge("ml", "train", [body], {"total": 3, "reachable": 1, "reporting": 1})
    assert result["coverage"]["complete"] is False and result["truncated"] is True
    assert merge("ml", "train", [], {"total": 0, "reachable": 0, "reporting": 0})["coverage"]["complete"] is False
    small = merge("ml", "train", [report("node-a")], {"total": 1, "reachable": 1, "reporting": 1})
    assert small["coverage"]["complete"] is True and small["truncated"] is False


def test_discovery_skips_non_ip_and_non_running_and_brackets_ipv6():
    from types import SimpleNamespace as NS

    def pod(ip, phase="Running"):
        return NS(status=NS(phase=phase, pod_ip=ip))

    class Core:
        def list_namespaced_pod(self, namespace, label_selector):
            return NS(items=[pod("10.0.0.1"), pod("evil.example.com"), pod("10.0.0.2", "Pending"),
                             pod("http://169.254.169.254/#"), pod("fd00::1"), pod(None)])

    deps = NS(collector_urls=None, flight_collector_namespace="gryvia-network", k8s_core=Core())
    assert asyncio.run(flight_collector_urls(deps)) == ["http://10.0.0.1:9090", "http://[fd00::1]:9090"]


def test_fan_out_is_bounded_and_deadline_marks_slow_nodes_unreachable(monkeypatch):
    import types

    active, peak = 0, 0

    async def fake_read(client, url, path, token):
        nonlocal active, peak
        active += 1
        peak = max(peak, active)
        await asyncio.sleep(0.01 if "fast" in url else 5)
        active -= 1
        return True, {"node": url, "namespace": "ml", "job": "train"}

    monkeypatch.setattr(flight_module, "_read_node", fake_read)
    monkeypatch.setattr(flight_module, "TOTAL_DEADLINE", 0.3)
    urls = [f"http://fast{i}" for i in range(40)] + ["http://slow"]
    deps = types.SimpleNamespace(collector_fetch=None, collector_urls=urls, flight_token=TOKEN)
    bodies, coverage = asyncio.run(gather(deps, "ml", "train"))
    assert peak <= flight_module.MAX_CONCURRENCY
    assert coverage == {"total": 41, "reachable": 40, "reporting": 40}
