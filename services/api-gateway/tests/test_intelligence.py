"""Numerical behavior, evidence gaps, payload limits and real-object namespace scoping."""

from datetime import datetime, timedelta, timezone

import pytest

from intelligence.engine import (
    capacity,
    fabric,
    federation,
    laboratory,
    locality,
    preflight,
    recovery,
    serving,
    training,
)
from intelligence.models import (
    Capacity,
    Fabric,
    Federation,
    Laboratory,
    Locality,
    Preflight,
    Recovery,
    Serving,
    Training,
)

NOW = datetime.now(timezone.utc)
DIGEST = "a" * 64
OBS = {"observedAt": NOW.isoformat(), "windowSeconds": 60, "source": "test"}
POOL = {"name": "pool-a", "gpuType": "A100", "nodes": 2, "gpusPerNode": 4, "memoryGiB": 80, "hourlyRate": 2}


def test_preflight_memory_and_budget():
    body = Preflight(
        gpuType="A100",
        parametersBillions=70,
        extraMemoryGiBPerGPU=10,
        tensorParallel=2,
        gpusPerNode=2,
        durationHours=2,
        quotaGPUs=2,
        storageReady=True,
        budget=7,
        pools=[POOL],
    )
    result = preflight(body)
    assert 75 < result["estimatedMemoryGiBPerGPU"] < 76
    assert result["candidates"][0]["estimatedCost"] == 8
    assert result["state"] == "Blocked"
    assert "estimated spend exceeds supplied budget" in result["candidates"][0]["reasons"]


def test_preflight_missing_is_incomplete():
    b = Preflight(gpuType="A100", parametersBillions=1, extraMemoryGiBPerGPU=1, durationHours=1, pools=[POOL])
    assert preflight(b)["state"] == "Incomplete"


def test_training_stale_missing_and_bottlenecks():
    ranks = [
        {**OBS, "rank": n, "stepSeconds": sec, "dataWaitSeconds": wait, "collectiveSeconds": 0, "gpuUtilization": 90}
        for n, sec, wait in [(0, 1, 0.4), (1, 1, 0), (2, 2, 0)]
    ]
    result = training(Training(expectedRanks=4, ranks=ranks), NOW)
    assert result["missingRanks"] == [3]
    assert {f["kind"] for f in result["findings"]} == {"data-loading", "straggler"}
    stale = [{**ranks[0], "observedAt": (NOW - timedelta(hours=1)).isoformat()}]
    assert training(Training(expectedRanks=1, ranks=stale), NOW)["medianStepSeconds"] is None


def test_serving_missing_metrics_does_not_scale():
    sample = {**OBS, "requests": 100, "errors": 0, "ttftP95Ms": 3000, "interTokenP95Ms": 100, "queueDepth": 10}
    body = dict(sample=sample, replicas=1, minReplicas=1, maxReplicas=4, maxTTFTMs=1000, maxInterTokenMs=50)
    result = serving(Serving(**body), NOW)
    assert result["action"] == "Hold" and result["state"] == "Unknown"
    body["sample"]["kvCachePercent"] = 95
    assert serving(Serving(**body), NOW)["recommendedReplicas"] == 2
    body["sample"]["observedAt"] = (NOW + timedelta(seconds=1)).isoformat()
    assert serving(Serving(**body), NOW)["state"] == "Unknown"


def test_laboratory_rejects_incomparable_and_keeps_pareto():
    base = {
        **OBS,
        "name": "cheap",
        "model": "llama",
        "gpuType": "A100",
        "engine": "vllm",
        "precision": "fp16",
        "concurrency": 4,
        "workloadDigest": DIGEST,
        "qualitySuite": "exact-match-v1",
        "qualityScore": 0.9,
        "tokensPerSecond": 100,
        "ttftP95Ms": 800,
        "hourlyCost": 2,
        "successfulRequests": 100,
    }
    b = Laboratory(
        benchmarks=[
            base,
            {**base, "name": "fast", "hourlyCost": 4, "ttftP95Ms": 400},
            {**base, "name": "wrong-workload", "workloadDigest": "b" * 64},
        ],
        model="llama",
        workloadDigest=DIGEST,
        qualitySuite="exact-match-v1",
        minQuality=0.8,
        maxTTFTMs=1000,
    )
    result = laboratory(b, NOW)
    assert result["recommended"] == "cheap"
    assert set(result["paretoFrontier"]) == {"cheap", "fast"}
    assert result["rejected"][0]["name"] == "wrong-workload"
    assert result["ranked"][0]["costPerMillionTokens"] == pytest.approx(5.55555555)


def test_recovery_requires_complete_compatible_commit():
    checkpoint = {
        "step": 10,
        "modelDigest": DIGEST,
        "datasetDigest": DIGEST,
        "framework": "pytorch",
        "worldSize": 2,
        "ranks": [0, 1],
        "integrityVerified": True,
        "globallyCommitted": True,
        "durableStorage": True,
        "committedAt": NOW.isoformat(),
    }
    b = dict(
        checkpoint=checkpoint,
        modelDigest=DIGEST,
        datasetDigest=DIGEST,
        framework="pytorch",
        targetWorldSize=2,
        currentStep=15,
        maxLostSteps=5,
        rendezvousReady=True,
    )
    assert recovery(Recovery(**b), NOW)["recoverable"]
    b["targetWorldSize"] = 4
    assert not recovery(Recovery(**b), NOW)["recoverable"]
    b["targetWorldSize"] = 2
    b["checkpoint"]["ranks"] = [0]
    assert not recovery(Recovery(**b), NOW)["recoverable"]


def test_locality_requires_digest_verification():
    b = Locality(
        datasetDigest=DIGEST,
        datasetGiB=1,
        requestedGPUs=2,
        replicas=[{"cluster": "india", "pool": "pool-a", "digest": DIGEST, "verified": False, "ready": True}],
        candidates=[{"cluster": "india", "pool": "pool-a", "freeGPUs": 4}],
    )
    assert locality(b)["recommended"] is None
    b.replicas[0].verified = True
    assert locality(b)["recommended"]["estimatedTransferSeconds"] == 0


def test_fabric_empty_and_stale_are_unknown():
    assert fabric(Fabric(links=[]), NOW)["state"] == "Unknown"
    link = {
        **OBS,
        "sourceNode": "node-a",
        "destinationNode": "node-b",
        "transport": "rdma",
        "expectedGbps": 400,
        "measuredGbps": 100,
        "errors": 0,
    }
    assert fabric(Fabric(links=[link]), NOW)["state"] == "Degraded"
    link["observedAt"] = (NOW - timedelta(hours=1)).isoformat()
    assert fabric(Fabric(links=[link]), NOW)["state"] == "Unknown"


def test_capacity_queue_cost_and_node_shape():
    demand = [
        {"name": name, "gpuType": "A100", "arrivalSeconds": 0, "durationSeconds": 3600, "nodes": 2, "gpusPerNode": 4}
        for name in ("job-a", "job-b")
    ]
    result = capacity(Capacity(pools=[POOL], demand=demand))
    assert result["runs"][1]["queueSeconds"] == 3600
    assert result["estimatedCost"] == 32
    assert result["utilization"] == 1
    demand[0]["gpusPerNode"] = 8  # enough total GPUs, wrong shape
    assert capacity(Capacity(pools=[POOL], demand=demand))["blocked"][0]["name"] == "job-a"


def test_capacity_reservations_do_not_overallocate():
    pool = {**POOL, "reservedGPUs": 1}
    demand = [
        {"name": "gang", "gpuType": "A100", "arrivalSeconds": 0, "durationSeconds": 1, "nodes": 2, "gpusPerNode": 4}
    ]
    assert capacity(Capacity(pools=[pool], demand=demand))["blocked"]


def test_federation_residency_currency_and_data():
    cluster = {
        **OBS,
        "name": "india",
        "region": "in",
        "ready": True,
        "gpuType": "A100",
        "freeGPUs": 8,
        "hourlyRate": 2,
        "datasetDigests": [DIGEST],
        "credentialConfigured": True,
    }
    b = Federation(
        allowedRegions=["in"],
        gpuType="A100",
        gpus=4,
        datasetDigest=DIGEST,
        clusters=[cluster, {**cluster, "name": "us", "region": "us", "hourlyRate": 1}],
    )
    assert federation(b, NOW)["selected"] == "india"
    b.clusters[0].datasetDigests = []
    assert federation(b, NOW)["selected"] is None


def test_api_validation_and_payload_limit(make_client):
    client = make_client("intelligence")
    assert client.post("/api/intelligence/economics", json={"currency": "USD", "cost": -1}).status_code == 422
    assert (
        client.post("/api/intelligence/economics", json={"currency": "USD", "cost": 1, "url": "x"}).status_code == 422
    )
    assert client.post("/api/intelligence/economics", content=b" " * 524289).status_code == 413
    assert client.get("/api/intelligence/capabilities").json()["actionsEnabled"] is False
    result = client.post("/api/intelligence/economics", json={"currency": "USD", "cost": 10}).json()
    assert result["costPerMillionDeliveredTokens"] is None


def test_namespaced_live_reads_no_cross_tenant(make_client, fake_k8s):
    fake_k8s.add("gryviaaijobs", {"metadata": {"name": "secret", "uid": "u"}, "spec": {}, "status": {}}, "tenant-beta")
    client = make_client("intelligence", role="tenant", tenants=["alpha"])
    assert client.get("/api/intelligence/jobs/secret/explain").status_code == 404
    assert client.get("/api/intelligence/inventory").status_code == 403


def test_metered_economics_uid_attribution(make_client, fake_k8s):
    fake_k8s.add("gryviaaijobs", {"metadata": {"name": "job", "uid": "current"}, "status": {"phase": "Succeeded"}})
    for name, uid, cost in [("run", "current", 8), ("old-run", "deleted", 999)]:
        fake_k8s.add(
            "gryviausagerecords",
            {
                "metadata": {"name": name, "uid": name},
                "spec": {"jobUID": uid, "cost": cost, "currency": "USD", "final": True},
            },
            "default",
        )
    # A real namespaced job is addressed through the namespace; seed accordingly.
    fake_k8s.add(
        "gryviaaijobs", {"metadata": {"name": "job", "uid": "current"}, "status": {"phase": "Succeeded"}}, "default"
    )
    result = make_client("intelligence").get("/api/intelligence/jobs/job/economics").json()
    assert result["totalCost"] == 8 and result["costPerCompletedRun"] == 8


@pytest.mark.parametrize("value", [float("nan"), float("inf"), -1])
def test_nonfinite_and_negative_rejected(value):
    from intelligence.models import Economics

    with pytest.raises(ValueError):
        Economics(currency="USD", cost=value)
