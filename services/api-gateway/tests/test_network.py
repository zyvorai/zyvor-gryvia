# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import pytest

NS = "default"


def obj(name, spec=None, status=None):
    o = {"metadata": {"name": name}, "spec": spec or {}}
    if status is not None:
        o["status"] = status
    return o


@pytest.fixture
def client(make_client):
    return make_client("network")


GRAPH = obj("g1", {}, {
    "lastUpdated": "2026-01-02T00:00:00Z",
    "nodes": [{"name": "api", "health": "Healthy"}, {"name": "db", "health": "critical"}],
    "edges": [{"source": "api", "destination": "db", "protocol": "tcp", "port": 5432,
               "latencyP99": "3ms", "throughput": "10MB/s", "verdict": "forwarded"},
              {"source": "api", "destination": "cache", "protocol": "tcp", "port": 6379,
               "verdict": "dropped"}],
})


@pytest.mark.parametrize("path", ["flows", "policies", "anomalies", "traces"])
def test_empty_lists(client, path):
    r = client.get(f"/api/network/{path}")
    assert r.status_code == 200 and r.json() == {"items": []}


def test_empty_insights_graph_costs(client):
    assert client.get("/api/network/insights").json() == {
        "activeFlows": 0, "policiesEnforced": 0, "anomaliesDetected": 0, "traceSessions": 0}
    assert client.get("/api/network/graph").json() == {"nodes": [], "edges": []}
    assert client.get("/api/network/costs").json() == {
        "reports": [], "costPerGB": {"sameZone": 0, "crossZone": 0, "internetEgress": 0}}


def test_flows_and_graph_from_service_graph(client, fake_k8s):
    fake_k8s.add("gryviaservicegraphs", GRAPH, NS)
    flows = client.get("/api/network/flows").json()["items"]
    assert len(flows) == 2
    assert flows[0]["spec"] == {"timestamp": "2026-01-02T00:00:00Z", "source": "api", "destination": "db",
                                "protocol": "TCP", "port": 5432, "bytes": "10MB/s", "latency": "3ms",
                                "verdict": "FORWARDED"}
    assert flows[1]["spec"]["verdict"] == "DROP"
    g = client.get("/api/network/graph").json()
    by_id = {n["id"]: n for n in g["nodes"]}
    assert set(by_id) == {"api", "db", "cache"}
    assert by_id["api"]["flowCount"] == 2 and by_id["api"]["health"] == "healthy"
    assert by_id["cache"]["health"] == "unknown"
    assert g["edges"][0] == {"source": "api", "target": "db", "protocol": "tcp", "latency": "3ms",
                             "verdict": "FORWARDED"}


def test_policies_list_and_shape(client, fake_k8s):
    fake_k8s.add("gryviaflowpolicies", obj("p1", {
        "source": {"service": "a"}, "destination": {"service": "b", "port": 80},
        "protocol": "tcp", "action": "allow", "intent": "secure"},
        {"phase": "Enforced", "enforced": True, "matchedFlows": 7}), NS)
    p = client.get("/api/network/policies").json()["items"][0]
    assert p["spec"] == {"sourceService": "a", "destinationService": "b", "port": 80, "protocol": "tcp",
                         "action": "allow", "intent": "secure"}
    assert p["status"] == {"phase": "Enforced", "matchedFlows": 7}


def test_create_policy(client, fake_k8s):
    body = {"sourceService": "web", "destinationService": "api", "port": 8080, "protocol": "TCP",
            "action": "deny", "intent": "secure"}
    r = client.post("/api/network/policies", json=body)
    assert r.status_code == 201
    j = r.json()
    assert j["spec"]["sourceService"] == "web" and j["spec"]["protocol"] == "tcp" and j["spec"]["port"] == 8080
    stored = fake_k8s.store[("gryviaflowpolicies", NS, j["metadata"]["name"])]
    assert stored["spec"]["destination"] == {"service": "api", "port": 8080}
    assert client.post("/api/network/policies", json=body).status_code == 409


@pytest.mark.parametrize("patch", [
    {"sourceService": "Bad_Name"}, {"destinationService": ""}, {"port": 0}, {"port": 70000},
    {"protocol": "sctp"}, {"action": "maybe"}, {"intent": ""}, {"intent": "<script>"},
    {"name": "-x"}, {"sourceService": "a" * 64},
])
def test_create_policy_validation(client, patch):
    body = {"sourceService": "web", "destinationService": "api", "port": 80, "protocol": "tcp",
            "action": "allow", "intent": "secure", **patch}
    assert client.post("/api/network/policies", json=body).status_code == 422


def test_apply_policy(client, fake_k8s):
    fake_k8s.add("gryviaflowpolicies", obj("p1", {"source": {"service": "a"}}), NS)
    r = client.post("/api/network/policies/p1/apply")
    assert r.status_code == 200
    ann = fake_k8s.store[("gryviaflowpolicies", NS, "p1")]["metadata"]["annotations"]
    assert "gryvia.io/apply-requested-at" in ann


def test_apply_policy_404(client):
    assert client.post("/api/network/policies/missing/apply").status_code == 404
    assert client.post("/api/network/policies/Bad_Name/apply").status_code == 404


def test_anomalies_and_insights(client, fake_k8s):
    fake_k8s.add("gryvianetworkanomalies", obj("an1", {"targetService": "api"}, {"anomalies": [
        {"type": "latency", "severity": "High", "detected": "2026-01-02T00:00:00Z", "description": "slow"}]}), NS)
    fake_k8s.add("gryviatrafficinsights", obj("ti1", {"service": "db"}, {"anomalies": [
        {"type": "drops", "severity": "low", "detected": "2026-01-03T00:00:00Z"}]}), NS)
    fake_k8s.add("gryviaservicegraphs", GRAPH, NS)
    fake_k8s.add("gryviaflowpolicies", obj("p1", {}, {"enforced": True}), NS)
    fake_k8s.add("gryviaflowpolicies", obj("p2", {}, {"phase": "Pending"}), NS)
    fake_k8s.add("gryviatracesessions", obj("t1", {"service": "api"}), NS)
    items = client.get("/api/network/anomalies").json()["items"]
    assert [i["spec"]["service"] for i in items] == ["db", "api"]
    assert items[1]["spec"]["severity"] == "high" and items[1]["metadata"]["name"] == "an1-0"
    assert client.get("/api/network/insights").json() == {
        "activeFlows": 2, "policiesEnforced": 1, "anomaliesDetected": 2, "traceSessions": 1}


def test_costs(client, fake_k8s):
    fake_k8s.add("gryvianetworkcosts", obj("c1", {"costPerGB": {"sameZone": 0, "crossZone": 0.01,
                                                                 "internetEgress": 0.09}}, {"reports": [
        {"period": "2026-01-01", "namespace": "ml", "team": "t", "sameZoneBytes": 1, "crossZoneBytes": 2,
         "externalBytes": 3, "totalCostUSD": 1.5}]}), NS)
    j = client.get("/api/network/costs").json()
    assert j["costPerGB"] == {"sameZone": 0, "crossZone": 0.01, "internetEgress": 0.09}
    assert j["reports"][0]["totalCostUSD"] == 1.5 and j["reports"][0]["team"] == "t"


def test_traces_create_get_list(client, fake_k8s):
    body = {"targetService": "api", "duration": "5m", "captureLevel": "L7", "namespace": "ml"}
    r = client.post("/api/network/traces", json=body)
    assert r.status_code == 201
    name = r.json()["metadata"]["name"]
    assert r.json()["spec"] == {"targetService": "api", "duration": "5m", "captureLevel": "l7",
                                "namespace": "ml"}
    assert fake_k8s.store[("gryviatracesessions", NS, name)]["spec"]["service"] == "api"
    fake_k8s.store[("gryviatracesessions", NS, name)]["status"] = {"phase": "active", "flowsCaptured": 4}
    got = client.get(f"/api/network/traces/{name}").json()
    assert got["status"] == {"phase": "Active", "flowsCaptured": 4}
    assert len(client.get("/api/network/traces").json()["items"]) == 1


@pytest.mark.parametrize("patch", [
    {"duration": "forever"}, {"duration": "0m"}, {"captureLevel": "l9"}, {"targetService": "A B"},
    {"namespace": ""},
])
def test_trace_validation(client, patch):
    body = {"targetService": "api", "duration": "5m", "captureLevel": "l4", "namespace": "ml", **patch}
    assert client.post("/api/network/traces", json=body).status_code == 422


def test_trace_404(client):
    assert client.get("/api/network/traces/nope").status_code == 404
    assert client.get("/api/network/traces/Bad_Name").status_code == 404


@pytest.mark.parametrize("raw,expected", [
    ("forwarded", "FORWARDED"), ("Dropped", "DROP"), ("DROP", "DROP"), ("denied", "DENIED"),
    ("allow", "ALLOW"), ("error", "ERROR"), ("weird", ""), (None, ""), (5, "")])
def test_graph_verdict_vocabulary(client, fake_k8s, raw, expected):
    edge = {"source": "a", "destination": "b", "protocol": "tcp", "port": 1}
    if raw is not None:
        edge["verdict"] = raw
    fake_k8s.add("gryviaservicegraphs", {"metadata": {"name": "g"}, "status": {"edges": [edge]}}, "default")
    assert client.get("/api/network/graph").json()["edges"][0]["verdict"] == expected
    assert client.get("/api/network/flows").json()["items"][0]["spec"]["verdict"] == expected


@pytest.mark.parametrize("raw,expected", [
    ("Healthy", "healthy"), ("warning", "warning"), ("Degraded", "warning"), ("unhealthy", "critical"),
    ("bad", "critical"), ("critical", "critical"), ("???", "unknown"), (None, "unknown")])
def test_graph_node_health_vocabulary(client, fake_k8s, raw, expected):
    node = {"name": "n"}
    if raw is not None:
        node["health"] = raw
    fake_k8s.add("gryviaservicegraphs", {"metadata": {"name": "g"}, "status": {"nodes": [node]}}, "default")
    assert client.get("/api/network/graph").json()["nodes"][0]["health"] == expected


NETRA_RECORDS = [
    {"observedAt": "2026-01-02T00:00:00Z", "namespace": "ml", "pod": "trainer-0", "peer": "10.0.0.9", "port": 443,
     "protocol": "tcp", "bytes": 4096, "srttUs": 2500},
    {"observedAt": "2026-01-02T00:00:01Z", "workloadName": "gateway", "peer": "10.0.0.7", "port": 53,
     "protocol": "udp", "bytes": 10, "blocked": 3},
]


def test_flows_come_from_netra_when_configured(make_client, fake_k8s):
    fake_k8s.add("gryviaservicegraphs", GRAPH, namespace=NS)

    async def fetch():
        return NETRA_RECORDS

    body = make_client("network", netra_fetch=fetch).get("/api/network/flows").json()
    assert body["source"] == "netra"
    assert [f["spec"] for f in body["items"]] == [
        {"timestamp": "2026-01-02T00:00:00Z", "source": "ml/trainer-0", "destination": "10.0.0.9", "protocol": "TCP",
         "port": 443, "bytes": "4096", "latency": "2.5ms", "verdict": "FORWARDED"},
        {"timestamp": "2026-01-02T00:00:01Z", "source": "gateway", "destination": "10.0.0.7", "protocol": "UDP",
         "port": 53, "bytes": "10", "latency": "", "verdict": "DROP"},
    ]


def test_flows_fall_back_to_the_service_graph_without_netra(make_client, fake_k8s):
    fake_k8s.add("gryviaservicegraphs", GRAPH, namespace=NS)
    body = make_client("network").get("/api/network/flows").json()
    assert "source" not in body and len(body["items"]) == 2


def test_unreachable_netra_falls_back(make_client, fake_k8s):
    fake_k8s.add("gryviaservicegraphs", GRAPH, namespace=NS)
    # nothing listens on this port, so the request fails and the graph edges are used
    body = make_client("network", netra_url="http://127.0.0.1:9").get("/api/network/flows").json()
    assert "source" not in body and len(body["items"]) == 2


def test_netra_host_process_flow_is_named_by_process_and_node(make_client):
    async def fetch():
        return [{"node": "n1", "comm": "kubelet", "peer": "10.43.0.1", "port": 443, "protocol": "tcp"}]

    item = make_client("network", netra_fetch=fetch).get("/api/network/flows").json()["items"][0]
    assert item["spec"]["source"] == "kubelet@n1"
