import asyncio
import json

import httpx

from gryvia import Gryvia


def test_preflight_manifest_and_yaml_path(tmp_path):
    body = {"apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaAIJob",
            "metadata": {"name": "train"}, "spec": {"image": "x", "gpus": 1}}
    path = tmp_path / "job.yaml"
    path.write_text(json.dumps(body))
    calls = []

    def handle(request):
        calls.append((request.url.path, json.loads(request.content)))
        return httpx.Response(200, json={"admitted": True, "persisted": False})

    async def check():
        client = Gryvia(api_url="https://test", token="token")
        client._http = httpx.AsyncClient(base_url="https://test", transport=httpx.MockTransport(handle))
        assert (await client.jobs.preflight(body))["persisted"] is False
        assert (await client.jobs.preflight(path))["admitted"] is True
        await client.close()

    asyncio.run(check())
    assert calls == [("/api/jobs/preflight", body)] * 2
