# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Cluster diagnosis merge, expected-node coverage, incident history routes and tenant scoping."""
import csv
import io

from routers.flight_diagnosis import (incidents_csv, merge_compare, merge_diagnosis, merge_incidents,
                                      safe_cell)

TOKEN = "0123456789abcdef0123456789abcdef"
CLUSTER_ONLY = "cluster coverage is not evaluated in a node-local report (nodesExpected unknown); the gateway computes it"


def completeness(reasons=None, dropped=0, skipped=None):
    return {"probesAttached": 5, "probesSkipped": skipped or [], "droppedEvents": {"total": dropped, "counts": []},
            "sampling": {"ratio": 1, "note": "no sampling"}, "nodesExpected": "unknown", "nodesReporting": 1,
            "missingNodes": [], "complete": False, "reasons": (reasons or []) + [CLUSTER_ONLY]}


def node_diag(node, findings=None, unavailable=None, measured=("cgroup.cpu.stat",), ns="ml", job="train", **mc):
    return {"node": node, "namespace": ns, "job": job, "summary": "s", "findings": findings or [],
            "unavailable": unavailable or [], "measured": list(measured),
            "measurementCompleteness": completeness(**mc)}


def finding(kind="cpu", severity="warning", confidence="medium", value=0.4, metric="cpu_throttle_ratio"):
    return {"kind": kind, "severity": severity, "confidence": confidence, "summary": f"{kind} problem",
            "evidence": [{"source": "cgroup", "metric": metric, "value": value, "window": "5m0s"}],
            "whatWasNotMeasured": ["host CPU"]}


COV = {"total": 2, "reachable": 2, "reporting": 2}


def test_merge_combines_kinds_keeps_per_node_evidence_and_raises_confidence():
    bodies = [node_diag("a", [finding(value=0.3)]), node_diag("b", [finding(severity="critical", value=0.9),
                                                                      finding("memory", "critical", "high", 1, "mem_oom_kills")])]
    out = merge_diagnosis("ml", "train", bodies, COV, ["a", "b"])
    cpu = next(f for f in out["findings"] if f["kind"] == "cpu")
    assert cpu["severity"] == "critical" and cpu["confidence"] == "high"   # medium on two nodes -> high
    assert cpu["nodes"] == ["a", "b"]
    assert {(e["node"], e["value"]) for e in cpu["evidence"]} == {("a", 0.3), ("b", 0.9)}
    assert [f["kind"] for f in out["findings"]] == ["cpu", "memory"]   # equal severity and confidence: by kind
    assert cpu["whatWasNotMeasured"] == ["host CPU"]


def test_expected_nodes_and_missing_nodes_named_and_never_complete_when_partial():
    out = merge_diagnosis("ml", "train", [node_diag("a")], {"total": 3, "reachable": 2, "reporting": 1}, ["a", "b", "c"])
    mc = out["measurementCompleteness"]
    assert mc["nodesExpected"] == 3 and mc["nodesReporting"] == 1 and mc["missingNodes"] == ["b", "c"]
    assert mc["complete"] is False and out["partial"] is True
    assert any("b, c" in r for r in mc["reasons"])
    assert out["summary"].startswith("no bottleneck detected in measured signals on 1 of 3 node(s)")
    assert "not a statement about the whole job" in out["summary"]


def test_unknown_expected_nodes_is_never_complete():
    out = merge_diagnosis("ml", "train", [node_diag("a")], {"total": 1, "reachable": 1, "reporting": 1}, None)
    mc = out["measurementCompleteness"]
    assert mc["nodesExpected"] == "unknown" and mc["complete"] is False
    assert "expected nodes unknown" in mc["reasons"][0]
    assert out["partial"] is True and "expected nodes unknown" in out["summary"]


def test_complete_only_when_every_signal_and_node_is_covered():
    # A node whose only reason is the node-local coverage note does not block completeness.
    body = node_diag("a")
    body["measurementCompleteness"]["reasons"] = [CLUSTER_ONLY]
    out = merge_diagnosis("ml", "train", [body], {"total": 1, "reachable": 1, "reporting": 1}, ["a"])
    assert out["measurementCompleteness"]["complete"] is True and out["partial"] is False
    assert out["summary"] == "no bottleneck detected in measured signals"
    # Unavailable telemetry on the node keeps it incomplete and is said in the summary.
    body = node_diag("a", unavailable=[{"signal": "cgroup.io.pressure", "reason": "PSI disabled"}],
                     reasons=["1 signal(s) unavailable: cgroup.io.pressure"])
    out = merge_diagnosis("ml", "train", [body], {"total": 1, "reachable": 1, "reporting": 1}, ["a"])
    assert out["measurementCompleteness"]["complete"] is False
    assert out["unavailable"] == [{"node": "a", "signal": "cgroup.io.pressure", "reason": "PSI disabled"}]
    assert any(r.startswith("a: 1 signal(s) unavailable") for r in out["measurementCompleteness"]["reasons"])


def test_dropped_events_probes_and_sampling_are_aggregated():
    bodies = [node_diag("a", dropped=5, skipped=[{"object": "straggler.o", "program": "u", "reason": "skipped: no libnccl"}]),
              node_diag("b", dropped=7)]
    out = merge_diagnosis("ml", "train", bodies, COV, ["a", "b"])
    mc = out["measurementCompleteness"]
    assert mc["droppedEvents"] == {"total": 12, "byNode": {"a": 5, "b": 7}}
    assert mc["probesAttached"] == 10 and mc["probesSkipped"][0]["node"] == "a"
    assert mc["sampling"]["ratio"] == 1.0


def test_nothing_reporting_is_not_healthy():
    out = merge_diagnosis("ml", "train", [], {"total": 2, "reachable": 2, "reporting": 0}, ["a"])
    assert "no node reported" in out["summary"] and out["measurementCompleteness"]["complete"] is False
    out = merge_diagnosis("ml", "train", [node_diag("a", measured=())], {"total": 1, "reachable": 1, "reporting": 1}, ["a"])
    assert "nothing could be measured" in out["summary"]


def test_untrusted_body_content_is_bounded_and_mismatches_dropped():
    evil = node_diag("a", [{"kind": "nonsense", "severity": "critical"},
                           {"kind": "cpu", "severity": "warning", "summary": "x" * 5000,
                            "evidence": [{"metric": "m", "value": float("inf")}, {"metric": "ok", "value": 1}]}])
    other = node_diag("b", [finding()], ns="other")
    out = merge_diagnosis("ml", "train", [evil, other], COV, ["a", "b"])
    assert out["nodes"] == ["a"]
    assert [f["kind"] for f in out["findings"]] == ["cpu"]
    assert len(out["findings"][0]["summary"]) <= 2000 and len(out["findings"][0]["evidence"]) == 1


def test_diagnosis_route_end_to_end(make_client, monkeypatch):
    import routers.flight_diagnosis as mod

    async def fake_expected(deps, ns, job):
        assert (ns, job) == ("ml", "train")
        return ["a", "b"], 1

    async def fetch(_path):
        return [node_diag("a", [finding()])], 2
    monkeypatch.setattr(mod, "expected_nodes", fake_expected)
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetch)
    body = client.get("/api/flight/jobs/train/diagnosis?namespace=ml").json()
    mc = body["measurementCompleteness"]
    assert mc["nodesExpected"] == 2 and mc["missingNodes"] == ["b"] and mc["complete"] is False
    assert any("not scheduled" in r for r in mc["reasons"])
    assert body["findings"][0]["kind"] == "cpu"


def test_expected_nodes_from_fake_pods(make_client):
    import asyncio
    from types import SimpleNamespace

    import routers.flight_diagnosis as mod

    class Core:
        def list_namespaced_pod(self, namespace, label_selector):
            assert namespace == "ml" and label_selector == "gryvia.io/job=train"
            def pod(node, phase):
                return SimpleNamespace(spec=SimpleNamespace(node_name=node), status=SimpleNamespace(phase=phase))
            return SimpleNamespace(items=[pod("a", "Running"), pod("b", "Pending"), pod(None, "Pending"), pod("c", "Succeeded")])

    nodes, unscheduled = asyncio.run(mod.expected_nodes(SimpleNamespace(k8s_core=Core()), "ml", "train"))
    assert nodes == ["a", "b"] and unscheduled == 1

    class Broken:
        def list_namespaced_pod(self, **kw):
            raise RuntimeError("forbidden")
    assert asyncio.run(mod.expected_nodes(SimpleNamespace(k8s_core=Broken()), "ml", "train")) == (None, 0)


def test_diagnosis_route_when_pods_cannot_be_listed_says_unknown(make_client, monkeypatch):
    import routers.flight_diagnosis as mod

    async def no_pods(*_a):
        return None, 0

    async def fetch(_path):
        return [node_diag("a")], 1
    monkeypatch.setattr(mod, "expected_nodes", no_pods)
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetch)
    body = client.get("/api/flight/jobs/train/diagnosis?namespace=ml").json()
    assert body["measurementCompleteness"]["nodesExpected"] == "unknown"
    assert body["measurementCompleteness"]["complete"] is False and body["partial"] is True


def test_diagnosis_route_with_a_job_that_has_no_pods_is_not_complete(make_client):
    async def fetch(_path):
        return [node_diag("a")], 1
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetch)
    body = client.get("/api/flight/jobs/train/diagnosis?namespace=ml").json()
    assert body["measurementCompleteness"]["nodesExpected"] == 0 and body["measurementCompleteness"]["complete"] is False


def test_diagnosis_scoping_and_token(make_client):
    calls = []

    async def fetch(path):
        calls.append(path)
        return [node_diag("a", ns="tenant-alpha")], 1
    client = make_client("flight", role="tenant", tenants=["alpha"], flight_token=TOKEN, collector_fetch=fetch)
    assert client.get("/api/flight/jobs/train/diagnosis?namespace=tenant-beta").status_code == 403
    assert client.get("/api/flight/incidents?namespace=tenant-beta").status_code == 403
    assert client.get("/api/flight/compare?namespace=tenant-beta&job=train&a=now&b=now").status_code == 403
    assert calls == []
    assert client.get("/api/flight/jobs/train/diagnosis?namespace=tenant-alpha").status_code == 200
    assert make_client("flight").get("/api/flight/jobs/train/diagnosis?namespace=ml").status_code == 503
    assert client.get("/api/flight/jobs/Bad_Name/diagnosis?namespace=tenant-alpha").status_code == 400

    async def none(_path):
        return [], 2
    assert make_client("flight", flight_token=TOKEN, collector_fetch=none).get(
        "/api/flight/jobs/train/diagnosis?namespace=ml").status_code == 503


def inc(node_ns="ml", job="train", id="inc-1", **kw):
    return {"id": id, "namespace": node_ns, "job": job, "kind": "cpu", "severity": "warning", "confidence": "medium",
            "start": "2026-01-01T00:00:00Z", "end": "2026-01-01T00:05:00Z", "open": False, "summary": kw.pop("summary", "s"),
            "evidence": [], "unavailable": ["fabric.pfc"], **kw}


def test_incidents_merge_enforces_namespace_and_reports_disabled_nodes():
    bodies = [{"node": "a", "enabled": True, "incidents": [inc(), inc("other", id="inc-2"), inc(job="x", id="inc-3")]},
              {"node": "b", "enabled": False, "incidents": []}]
    out = merge_incidents("ml", "train", bodies, {"total": 2, "reachable": 2, "reporting": 2})
    assert [i["id"] for i in out["incidents"]] == ["inc-1"] and out["incidents"][0]["node"] == "a"
    assert out["history"] == {"enabledNodes": ["a"], "disabledNodes": ["b"], "note": out["history"]["note"]}
    assert out["coverage"]["complete"] is False   # a node without history is not complete coverage


def test_incident_routes(make_client):
    seen = []

    async def fetch(path):
        seen.append(path)
        return [{"node": "a", "enabled": True, "incidents": [inc(summary="=HYPERLINK(1)")]}], 1
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetch)
    r = client.get("/api/flight/incidents?namespace=ml&job=train&since=6h")
    assert r.status_code == 200 and r.json()["incidents"][0]["id"] == "inc-1"
    assert seen[-1].startswith("/api/v1/flight/incidents?") and "since=6h" in seen[-1] and "namespace=ml" in seen[-1]
    assert client.get("/api/flight/incidents?namespace=ml&since=bad").status_code == 400
    assert client.get("/api/flight/incidents?namespace=ml&job=UP").status_code == 400
    r = client.get("/api/flight/incidents/export?namespace=ml&job=train&format=csv")
    assert r.status_code == 200 and r.headers["content-type"].startswith("text/csv")
    rows = list(csv.reader(io.StringIO(r.text)))
    assert rows[0][0] == "node" and rows[1][11] == "'=HYPERLINK(1)"
    r = client.get("/api/flight/incidents/export?namespace=ml&job=train")
    assert r.status_code == 200 and r.json()["incidents"][0]["id"] == "inc-1"
    assert client.get("/api/flight/incidents/export?namespace=ml&format=xml").status_code == 400


def test_csv_safety():
    assert safe_cell("=cmd") == "'=cmd" and safe_cell("@x") == "'@x" and safe_cell("-1") == "'-1"
    assert safe_cell("a\x1bb") == "a b" and safe_cell("ok") == "ok" and safe_cell("l1\nl2") == "l1\nl2"
    text = incidents_csv([inc(node="=x", summary='a,"b"\n+c')])
    rows = list(csv.reader(io.StringIO(text)))
    assert len(rows) == 2 and rows[1][11] == 'a,"b"\n+c'.replace("+c", "+c")


def test_compare_route_and_merge(make_client):
    seen = []

    async def fetch(path):
        seen.append(path)
        return [{"node": "a", "enabled": True, "comparison": {"a": {}, "b": {}, "metrics": [{"metric": "m", "a": 1, "b": 2, "delta": 1}],
                                                                "notes": ["n"]}},
                {"node": "b", "enabled": False}], 2
    client = make_client("flight", flight_token=TOKEN, collector_fetch=fetch)
    r = client.get("/api/flight/compare?namespace=ml&job=train&a=inc-0123456789abcdef&b=2026-01-01T00:00:00Z&window=30m")
    body = r.json()
    assert r.status_code == 200 and body["nodes"][0]["node"] == "a" and body["history"]["disabledNodes"] == ["b"]
    assert "a=inc-0123456789abcdef" in seen[0] and "window=30m" in seen[0]
    for bad in ("a=x&b=now", "a=now", "a=now&b=now&window=9d", "a=inc-zzz&b=now"):
        assert client.get(f"/api/flight/compare?namespace=ml&job=train&{bad}").status_code == 400
    assert client.get("/api/flight/compare?namespace=ml&a=now&b=now").status_code == 400
    assert merge_compare("ml", "train", [], {"total": 1, "reachable": 1, "reporting": 0})["nodes"] == []
