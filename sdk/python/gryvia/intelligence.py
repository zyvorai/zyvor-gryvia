"""Workload analysis and explicit operation lifecycle; no automatic execution."""
from urllib.parse import quote

AREAS = frozenset({"preflight", "training", "economics", "serving", "laboratory", "recovery",
                   "locality", "fabric", "capacity", "federation"})


class Intelligence:
    def __init__(self, client):
        self._client = client

    async def analyze(self, area, inputs):
        if area not in AREAS:
            raise ValueError("unknown intelligence area")
        return await self._client._post(f"/api/intelligence/{area}", json=inputs)

    async def explain(self, job):
        return await self._client._get(f"/api/intelligence/jobs/{quote(job, safe='')}/explain")

    async def economics(self, job):
        return await self._client._get(f"/api/intelligence/jobs/{quote(job, safe='')}/economics")

    async def capabilities(self):
        return await self._client._get("/api/intelligence/capabilities")

    async def propose(self, proposal):
        return await self._client._post("/api/intelligence/actions", json=proposal)

    async def actions(self):
        return await self._client._get("/api/intelligence/actions")

    async def transition(self, operation_id, transition):
        if transition not in {"approve", "reject", "execute", "rollback"}:
            raise ValueError("unsupported operation transition")
        return await self._client._post(
            f"/api/intelligence/actions/{quote(operation_id, safe='')}/{transition}")
