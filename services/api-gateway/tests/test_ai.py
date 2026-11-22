# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

def test_empty_state(make_client):
    c = make_client("ai")
    r = c.get("/api/ai/training/insight")
    assert r.status_code == 200
    assert r.json() == {"rankStats": [], "commPattern": "", "commComputeRatio": 0, "stragglers": [], "bottleneck": ""}
    none = {"reachable": 0, "total": 0}
    assert c.get("/api/ai/training/nccl").json() == {"operations": [], "collectors": none}
    assert c.get("/api/gpu/memory").json() == {
        "h2dBytes": 0, "d2hBytes": 0, "d2dBytes": 0, "h2dCount": 0, "d2hCount": 0, "d2dCount": 0,
        "collectors": none}


def test_insight_without_status_is_empty(make_client, fake_k8s):
    fake_k8s.add("gryviatraininginsights", {"metadata": {"name": "a"}, "spec": {}}, namespace="default")
    assert make_client("ai").get("/api/ai/training/insight").json()["rankStats"] == []


def test_populated_insight_uses_latest(make_client, fake_k8s):
    fake_k8s.add("gryviatraininginsights", {
        "metadata": {"name": "old"},
        "status": {"bottleneck": "compute", "lastAnalysis": "2026-01-01T00:00:00Z"},
    }, namespace="default")
    fake_k8s.add("gryviatraininginsights", {
        "metadata": {"name": "new"},
        "status": {
            "bottleneck": "communication", "commPattern": "ring_allreduce", "commComputeRatio": 0.42,
            "lastAnalysis": "2026-02-01T00:00:00Z",
            "rankStats": [{"rank": 0, "avgLatencyNs": 100, "totalBytes": 2048, "isStraggler": False},
                          {"rank": 1, "avgLatencyNs": 300, "totalBytes": 2048, "isStraggler": True}],
            "stragglers": [{"rank": 1, "slowdownFactor": 3.0, "reason": "slow link"}],
        },
    }, namespace="ml")
    body = make_client("ai").get("/api/ai/training/insight").json()
    assert body["bottleneck"] == "communication"
    assert body["commPattern"] == "ring_allreduce"
    assert body["commComputeRatio"] == 0.42
    assert body["lastAnalysis"] == "2026-02-01T00:00:00Z"
    assert [r["rank"] for r in body["rankStats"]] == [0, 1]
    assert body["rankStats"][1]["isStraggler"] is True
    assert body["stragglers"] == [{"rank": 1, "slowdownFactor": 3.0, "reason": "slow link"}]


def _collector(bodies):
    async def fetch(path):
        return [b[path] for b in bodies if path in b]
    return fetch


NODE_A = {
    "/api/v1/gpu/nccl": {"op_stats": {
        "AllReduce": {"OpType": "AllReduce", "Count": 10, "TotalBytes": 1000, "AvgLatencyNs": 100.0, "P99LatencyNs": 400.0},
    }, "stragglers": []},
    "/api/v1/gpu/memory": {
        "H2D": {"Direction": "H2D", "TotalBytes": 500, "TransferCount": 5},
        "D2H": {"Direction": "D2H", "TotalBytes": 200, "TransferCount": 2},
    },
}
NODE_B = {
    "/api/v1/gpu/nccl": {"op_stats": {
        "AllReduce": {"OpType": "AllReduce", "Count": 30, "TotalBytes": 3000, "AvgLatencyNs": 200.0, "P99LatencyNs": 900.0},
        "Broadcast": {"OpType": "Broadcast", "Count": 4, "TotalBytes": 40, "AvgLatencyNs": 50.0, "P99LatencyNs": 60.0},
    }, "stragglers": []},
    "/api/v1/gpu/memory": {"H2D": {"Direction": "H2D", "TotalBytes": 100, "TransferCount": 1},
                           "D2D": {"Direction": "D2D", "TotalBytes": 900, "TransferCount": 9}},
}


def test_nccl_merges_collectors(make_client):
    c = make_client("ai", collector_fetch=_collector([NODE_A, NODE_B]))
    ops = {o["opType"]: o for o in c.get("/api/ai/training/nccl").json()["operations"]}
    assert ops["AllReduce"]["count"] == 40
    assert ops["AllReduce"]["totalBytes"] == 4000
    assert ops["AllReduce"]["avgLatencyNs"] == 175.0  # (10*100 + 30*200) / 40
    assert ops["AllReduce"]["p99LatencyNs"] == 900.0  # worst node
    assert ops["Broadcast"]["count"] == 4


def test_gpu_memory_sums_collectors(make_client):
    c = make_client("ai", collector_fetch=_collector([NODE_A, NODE_B]))
    assert c.get("/api/gpu/memory").json() == {
        "h2dBytes": 600, "d2hBytes": 200, "d2dBytes": 900,
        "h2dCount": 6, "d2hCount": 2, "d2dCount": 9,
        "collectors": {"reachable": 2, "total": 2},
    }


def test_no_collector_means_no_data(make_client):
    c = make_client("ai", collector_fetch=_collector([]))
    assert c.get("/api/ai/training/nccl").json() == {"operations": [], "collectors": {"reachable": 0, "total": 0}}
    mem = c.get("/api/gpu/memory").json()
    assert mem.pop("collectors") == {"reachable": 0, "total": 0}
    assert all(v == 0 for v in mem.values())


def test_collectors_reachable_vs_total(make_client):
    async def fetch(path):
        return [NODE_A[path]], 3  # 3 discovered, only 1 answered

    c = make_client("ai", collector_fetch=fetch)
    assert c.get("/api/ai/training/nccl").json()["collectors"] == {"reachable": 1, "total": 3}
    assert c.get("/api/gpu/memory").json()["collectors"] == {"reachable": 1, "total": 3}


def test_fetch_all_keeps_list_return(fake_k8s):
    import asyncio

    from routers.collector import fetch_all, fetch_all_with_stats
    from routers.common import Deps

    async def fetch(path):
        return [{"a": 1}], 2

    deps = Deps(verify_auth=None, k8s_custom=fake_k8s, k8s_core=None, limiter=None, collector_fetch=fetch)
    assert asyncio.run(fetch_all(deps, "/x")) == [{"a": 1}]
    assert asyncio.run(fetch_all_with_stats(deps, "/x")) == ([{"a": 1}], {"reachable": 1, "total": 2})


def test_insight_source(make_client, fake_k8s):
    fake_k8s.add("gryviatraininginsights", {
        "metadata": {"name": "run-1"},
        "status": {"bottleneck": "compute", "lastAnalysis": "2026-02-01T00:00:00Z"},
    }, namespace="ml")
    body = make_client("ai").get("/api/ai/training/insight").json()
    assert body["source"] == {"name": "run-1", "namespace": "ml"}
    assert body["lastAnalysis"] == "2026-02-01T00:00:00Z"


def test_insight_source_omitted_when_empty(make_client):
    assert "source" not in make_client("ai").get("/api/ai/training/insight").json()
