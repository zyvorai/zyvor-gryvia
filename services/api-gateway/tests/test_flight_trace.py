# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Trace lookup and engine-latency routes: tenant scoping, validation, and no trace id in logs."""
import logging

from routers.flight import RedactTraceIds, inference_view, merge_trace, valid_trace_id

TOKEN = "0123456789abcdef0123456789abcdef"
TRACE = "0af7651916cd43dd8448eb211c80319c"


def node_body(node, namespace="tenant-alpha", job="serve", trace=TRACE, extra_obs=(), extra_events=()):
    ident = {"namespace": namespace, "job": job, "pod": f"{job}-0", "node": node}
    obs = [{"time": "2026-01-01T00:00:01Z", "spanId": "b7ad6b7169203331", "client": "203.0.113.9:51000",
            "server": "10.1.2.3:8000", "identity": ident}, *extra_obs]
    events = [{"time": "2026-01-01T00:00:02Z", "kind": "flow", "source": "ebpf", "bytes": 1200,
               "traceId": trace, "traceMatch": "tuple", "identity": ident}, *extra_events]
    return {"node": node, "traceId": trace, "scope": "node-local", "observations": obs, "events": events}


def fetcher(*bodies, total=None, record=None):
    async def fetch(path):
        if record is not None:
            record.append(path)
        return list(bodies), total if total is not None else len(bodies)
    return fetch


def test_valid_trace_id():
    assert valid_trace_id(TRACE) and valid_trace_id(TRACE.upper())
    for bad in ("", TRACE[:31], TRACE + "0", "0" * 32, "g" * 32, TRACE[:-1] + "\n"):
        assert not valid_trace_id(bad)


def test_admin_sees_every_namespace(make_client):
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetcher(
        node_body("node-a"), node_body("node-b", namespace="tenant-beta")))
    body = client.get(f"/api/flight/trace/{TRACE.upper()}").json()
    assert body["traceId"] == TRACE
    assert {o["identity"]["namespace"] for o in body["observations"]} == {"tenant-alpha", "tenant-beta"}
    assert body["nodes"] == ["node-a", "node-b"]
    assert body["coverage"] == {"total": 2, "reachable": 2, "reporting": 2, "complete": True}
    assert body["events"][0]["traceMatch"] == "tuple"


def test_tenant_only_sees_own_namespaces(make_client):
    client = make_client("flight", role="tenant", tenants=["alpha"], flight_token=TOKEN, collector_fetch=fetcher(
        node_body("node-a"), node_body("node-b", namespace="tenant-beta", job="secret")))
    resp = client.get(f"/api/flight/trace/{TRACE}")
    assert resp.status_code == 200
    body = resp.json()
    assert [o["identity"]["namespace"] for o in body["observations"]] == ["tenant-alpha"]
    assert [e["identity"]["namespace"] for e in body["events"]] == ["tenant-alpha"]
    assert body["nodes"] == ["node-a"]
    # Nothing about the other tenant leaks, not even through counts.
    assert body["coverage"]["reporting"] == 1
    assert "tenant-beta" not in resp.text and "secret" not in resp.text


def test_tenant_gets_404_for_someone_elses_trace_and_403_for_their_namespace(make_client):
    client = make_client("flight", role="tenant", tenants=["alpha"], flight_token=TOKEN,
                         collector_fetch=fetcher(node_body("node-b", namespace="tenant-beta")))
    resp = client.get(f"/api/flight/trace/{TRACE}")
    assert resp.status_code == 404
    assert TRACE not in resp.text
    assert client.get(f"/api/flight/trace/{TRACE}?namespace=tenant-beta").status_code == 403


def test_events_of_other_namespaces_inside_an_allowed_observation_are_dropped(make_client):
    other = {"time": "2026-01-01T00:00:03Z", "kind": "flow", "traceId": TRACE,
             "identity": {"namespace": "tenant-beta", "job": "x"}}
    no_ident = {"time": "2026-01-01T00:00:03Z", "kind": "flow", "traceId": TRACE}
    wrong_trace = {"time": "2026-01-01T00:00:03Z", "kind": "flow", "traceId": "1" * 32,
                   "identity": {"namespace": "tenant-alpha", "job": "serve"}}
    client = make_client("flight", role="tenant", tenants=["alpha"], flight_token=TOKEN,
                         collector_fetch=fetcher(node_body("node-a", extra_events=(other, no_ident, wrong_trace))))
    events = client.get(f"/api/flight/trace/{TRACE}").json()["events"]
    assert len(events) == 1 and events[0]["identity"]["namespace"] == "tenant-alpha"


def test_reply_for_another_trace_id_is_ignored(make_client):
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetcher(node_body("node-a", trace="2" * 32)))
    assert client.get(f"/api/flight/trace/{TRACE}").status_code == 404


def test_unknown_fields_are_not_forwarded(make_client):
    body = node_body("node-a")
    body["events"][0]["password"] = "hunter2"
    body["observations"][0]["identity"]["secret"] = "x"
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetcher(body))
    text = client.get(f"/api/flight/trace/{TRACE}").text
    assert "hunter2" not in text and "secret" not in text


def test_invalid_ids_and_namespaces(make_client):
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetcher(node_body("node-a")))
    for bad in ("nothex", "0" * 32, TRACE + "0"):
        resp = client.get(f"/api/flight/trace/{bad}")
        assert resp.status_code == 400 and bad not in resp.text
    assert client.get(f"/api/flight/trace/{TRACE}?namespace=Bad_NS").status_code == 400


def test_missing_token_and_unreachable_collectors(make_client):
    assert make_client("flight").get(f"/api/flight/trace/{TRACE}").status_code == 503
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetcher(total=3))
    assert client.get(f"/api/flight/trace/{TRACE}").status_code == 503


def test_collector_is_asked_with_the_normalised_id(make_client):
    paths = []
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetcher(node_body("node-a"), record=paths))
    client.get(f"/api/flight/trace/{TRACE.upper()}")
    assert paths == [f"/api/v1/flight/trace?traceId={TRACE}"]


def test_merge_trace_partial_coverage():
    result = merge_trace(TRACE, [node_body("node-a")], {"total": 3, "reachable": 2}, lambda ns: True)
    assert result["coverage"] == {"total": 3, "reachable": 2, "reporting": 1, "complete": False}


def test_access_log_redacts_trace_ids():
    args = ("10.0.0.1:5", "GET", f"/api/flight/trace/{TRACE}?namespace=ml", "1.1", 200)
    record = logging.LogRecord("uvicorn.access", logging.INFO, "", 0, '%s - "%s %s HTTP/%s" %d', args, None)
    assert RedactTraceIds().filter(record)
    line = record.getMessage()
    assert TRACE not in line and "/api/flight/trace/<redacted>?namespace=ml" in line
    other = logging.LogRecord("uvicorn.access", logging.INFO, "", 0, '%s "%s %s"',
                              ("a", "GET", "/api/flight/jobs/train"), None)
    RedactTraceIds().filter(other)
    assert "/api/flight/jobs/train" in other.getMessage()


# ---- engine latency ---------------------------------------------------------------------------

def signal(name, job, status, ns="tenant-alpha"):
    return {"metadata": {"name": name}, "spec": {"jobRef": job}, "status": status}


def test_inference_view_absent_without_scraper():
    view = inference_view("ml", "serve", [signal("a", "serve", {"inferWaitP99ms": 4, "scoreDelta": 0.1})])
    assert view == {"namespace": "ml", "job": "serve", "available": False}


def test_inference_view_picks_newest_and_only_known_numeric_fields():
    older = signal("a", "serve", {"engine": "tgi", "updatedAt": "2026-01-01T00:00:00Z", "queueTimeP99ms": 9})
    newer = signal("b", "serve", {"engine": "vllm", "updatedAt": "2026-01-02T00:00:00Z", "ttftP99ms": 475,
                                  "itlP99ms": 72.5, "kvCacheUsage": 0.42, "requestsWaiting": 3,
                                  "inferWaitP99ms": 4, "scoreDelta": 0.9, "e2eP99ms": True, "queueTimeP99ms": "1"})
    view = inference_view("ml", "serve", [older, newer, signal("c", "other", {"engine": "vllm"})])
    assert view["engine"] == "vllm" and view["ttftP99ms"] == 475 and view["kvCacheUsage"] == 0.42
    assert view["inferWaitP99ms"] == 4
    for absent in ("scoreDelta", "e2eP99ms", "queueTimeP99ms", "itlP99ms_"):
        assert absent not in view


def test_inference_route_scoping(make_client, fake_k8s):
    fake_k8s.add("gryviafabricsignals", signal("serve-fabric", "serve", {"engine": "vllm", "ttftP99ms": 475}),
                 namespace="tenant-alpha")
    fake_k8s.add("gryviafabricsignals", signal("serve-fabric", "serve", {"engine": "tgi", "ttftP99ms": 1}),
                 namespace="tenant-beta")
    tenant = make_client("flight", role="tenant", tenants=["alpha"])
    body = tenant.get("/api/flight/inference/serve").json()
    assert body["available"] is True and body["engine"] == "vllm" and body["ttftP99ms"] == 475
    assert tenant.get("/api/flight/inference/serve?namespace=tenant-beta").status_code == 403
    assert tenant.get("/api/flight/inference/Bad_Job").status_code == 400
    assert tenant.get("/api/flight/inference/nojob").json()["available"] is False
    admin = make_client("flight")
    assert admin.get("/api/flight/inference/serve?namespace=tenant-beta").json()["engine"] == "tgi"
