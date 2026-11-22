# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""SDK tests against an in-process fake gateway (httpx.MockTransport); no network or cluster needed."""
import asyncio

import httpx
import pytest

from gryvia import Gryvia
from gryvia.exceptions import (
    AuthenticationError,
    AuthorizationError,
    ConfigurationError,
    GryviaError,
    NotFoundError,
    RateLimitError,
    ServerError,
    ValidationError,
)

STATS = {
    "totalGPUs": 12, "availableGPUs": 10, "allocatedGPUs": 2, "utilizationPercent": 16.5,
    "totalJobs": 3, "runningJobs": 1, "pendingJobs": 1, "completedJobs": 1, "failedJobs": 0,
    "totalNodes": 2, "timestamp": "2026-01-01T00:00:00Z",
}
JOB = {"apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaAIJob",
       "metadata": {"name": "train", "namespace": "default"},
       "spec": {"gpus": 2, "image": "busybox", "type": "training"}, "status": {"phase": "Pending"}}


def make_client(handler, token="secret"):
    client = Gryvia(api_url="https://gw.test", token=token)
    client._http = httpx.AsyncClient(base_url="https://gw.test", transport=httpx.MockTransport(handler))
    return client


def run(coro):
    return asyncio.run(coro)


def test_configuration_is_required(monkeypatch):
    monkeypatch.delenv("GRYVIA_API_URL", raising=False)
    monkeypatch.delenv("GRYVIA_TOKEN", raising=False)
    with pytest.raises(ConfigurationError):
        Gryvia(token="t")
    with pytest.raises(ConfigurationError):
        Gryvia(api_url="https://gw.test")


def test_configuration_from_environment(monkeypatch):
    monkeypatch.setenv("GRYVIA_API_URL", "https://gw.test/")
    monkeypatch.setenv("GRYVIA_TOKEN", "from-env")
    assert Gryvia()._api_url == "https://gw.test"


def test_requests_carry_the_bearer_token():
    seen = {}

    def handler(request):
        seen["auth"] = request.headers.get("authorization")
        seen["path"] = request.url.path
        return httpx.Response(200, json=STATS)

    stats = run(make_client(handler).metrics.cluster_stats())
    assert seen == {"auth": "Bearer secret", "path": "/api/cluster/stats"}
    assert stats.total_gpus == 12 and stats.pending_jobs == 1


def test_jobs_list_get_create_delete():
    calls = []

    def handler(request):
        calls.append((request.method, request.url.path, dict(request.url.params)))
        if request.method == "GET" and request.url.path == "/api/jobs":
            return httpx.Response(200, json={"items": [JOB], "total": 1, "limit": 5, "offset": 10})
        if request.method == "GET":
            return httpx.Response(200, json=JOB)
        if request.method == "POST":
            return httpx.Response(200, json=JOB)
        return httpx.Response(200, json={"status": "deleted", "name": "train"})

    async def scenario():
        c = make_client(handler)
        listed = await c.jobs.list(limit=5, offset=10)
        job = await c.jobs.get("train")
        created = await c.jobs.create({"apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaAIJob",
                                       "metadata": {"name": "train"}, "spec": {"gpus": 2}})
        deleted = await c.jobs.delete("train")
        return listed, job, created, deleted

    listed, job, created, deleted = run(scenario())
    assert listed.total == 1 and listed.items[0].metadata.name == "train"
    assert job.status.phase == "Pending" and created.metadata.name == "train"
    assert deleted["status"] == "deleted"
    assert calls[0] == ("GET", "/api/jobs", {"limit": "5", "offset": "10"})
    assert [c[:2] for c in calls[1:]] == [("GET", "/api/jobs/train"), ("POST", "/api/jobs"), ("DELETE", "/api/jobs/train")]


@pytest.mark.parametrize("status,exc", [
    (400, ValidationError), (401, AuthenticationError), (403, AuthorizationError),
    (404, NotFoundError), (429, RateLimitError), (500, ServerError), (503, ServerError),
])
def test_http_errors_map_to_sdk_exceptions(status, exc):
    client = make_client(lambda request: httpx.Response(status, json={"detail": "nope"}))
    with pytest.raises(exc) as info:
        run(client.jobs.get("missing"))
    assert isinstance(info.value, GryviaError)
    assert info.value.status_code == status


def test_health_does_not_need_auth_handling():
    client = make_client(lambda request: httpx.Response(200, json={"status": "ok"}))
    assert run(client.health()) == {"status": "ok"}
