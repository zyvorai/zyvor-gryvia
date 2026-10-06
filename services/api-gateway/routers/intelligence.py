"""Workload intelligence: typed analyses plus namespace-filtered live job evidence."""

from datetime import datetime, timezone
import math

from fastapi import APIRouter, Depends, HTTPException, Path, Request

from intelligence.engine import ANALYZERS, economics
from intelligence.models import Economics
from .common import Deps, list_items, run, require_admin
from .uiutil import NAME_MAX, NAME_PATTERN, find_one
from .intelligence_http import BoundedRoute


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter(route_class=BoundedRoute)

    def endpoint(area, model, function):
        async def analyze(request: Request, body: model, _=Depends(deps.verify_auth)):
            try:
                # Capacity simulations are bounded CPU work; do not block other gateway requests.
                return await run(function, body)
            except ValueError as exc:
                raise HTTPException(status_code=422, detail=str(exc)) from exc

        analyze.__name__ = f"intelligence_{area}"
        return deps.limiter.limit("20/minute")(analyze)

    for area, (model, function) in ANALYZERS.items():
        router.add_api_route(
            f"/api/intelligence/{area}",
            endpoint(area, model, function),
            methods=["POST"],
            tags=["intelligence"],
            summary=f"Analyze {area} from explicit observations",
        )

    @router.get("/api/intelligence/schemas")
    @deps.limiter.limit("30/minute")
    async def schemas(request: Request, _=Depends(deps.verify_auth)):
        return {"items": {area: model.model_json_schema() for area, (model, _) in ANALYZERS.items()}}

    @router.get("/api/intelligence/capabilities")
    @deps.limiter.limit("30/minute")
    async def capabilities(request: Request, _=Depends(deps.verify_auth)):
        return {
            "analyses": list(ANALYZERS),
            "actionsEnabled": deps.intelligence_actions,
            "analysesExecuteWorkloads": False,
            "samplesRequireTimezone": True,
            "limits": {"requestBytes": 524288, "capacityGPULanes": 65536},
            "actionKinds": (
                ["inference-replicas", "quota-max-gpus", "node-quarantine"] if deps.intelligence_actions else []
            ),
            "limitations": [
                "Caller-supplied observations are not independently verified by analysis endpoints.",
                "Multi-cluster analysis does not dispatch or fail over workloads.",
                "Live GPU, NCCL and storage qualification remains required.",
            ],
        }

    @router.get("/api/intelligence/jobs/{name}/explain")
    @deps.limiter.limit("30/minute")
    async def explain(
        request: Request,
        name: str = Path(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN),
        _=Depends(deps.verify_auth),
    ):
        job, ns = await find_one(request, deps, "gryviaaijobs", name)
        st = job.get("status") or {}
        spec = job.get("spec") or {}
        pods = await run(deps.k8s_core.list_namespaced_pod, ns, label_selector=f"gryvia.io/job={name}")
        conditions = [
            {
                k: c[k]
                for k in ("type", "status", "reason", "message", "lastTransitionTime", "observedGeneration")
                if k in c
            }
            for c in st.get("conditions", [])
        ]
        pending = []
        for pod in pods.items or []:
            for c in getattr(pod.status, "conditions", None) or []:
                if c.type == "PodScheduled" and c.status != "True":
                    pending.append({"pod": pod.metadata.name, "reason": c.reason, "message": c.message})
        distributed = spec.get("distributed") or {}
        gpus = (
            distributed.get("nodes", 1) * distributed.get("gpusPerNode", 1)
            if distributed.get("enabled")
            else spec.get("gpus", 0)
        )
        return {
            "mode": "cluster-observation",
            "job": name,
            "namespace": ns,
            "observedAt": datetime.now(timezone.utc).isoformat(),
            "phase": st.get("phase", "Unknown"),
            "requestedGPUs": gpus,
            "conditions": conditions,
            "unscheduledPods": pending,
            "selectedNodes": st.get("nodesAllocated", []),
            "caveat": "Recorded node selection and quota admission are not proof of pod placement or GPU execution.",
        }

    @router.get("/api/intelligence/jobs/{name}/economics")
    @deps.limiter.limit("30/minute")
    async def job_economics(
        request: Request,
        name: str = Path(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN),
        _=Depends(deps.verify_auth),
    ):
        job, ns = await find_one(request, deps, "gryviaaijobs", name)
        uid = (job.get("metadata") or {}).get("uid")
        if not uid:
            raise HTTPException(status_code=409, detail="job UID unavailable; cannot attribute usage safely")
        records = [
            r
            for r in await list_items(deps, "gryviausagerecords", namespace=ns)
            if (r.get("spec") or {}).get("jobUID") == uid
        ]
        if not records:
            return {"mode": "metered-estimate", "state": "Unknown", "reason": "no UID-attributed usage records"}
        ids = [(r.get("metadata") or {}).get("uid") for r in records]
        if None in ids or len(set(ids)) != len(ids):
            raise HTTPException(status_code=409, detail="usage record identity missing or duplicated")
        specs = [r.get("spec") or {} for r in records]
        currencies = {s.get("currency", "USD") for s in specs}
        if len(currencies) != 1:
            raise HTTPException(status_code=409, detail="mixed currencies cannot be aggregated")
        values = [s.get("cost") for s in specs]
        if any(isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v) or v < 0 for v in values):
            raise HTTPException(status_code=409, detail="invalid metered cost")
        phase = (job.get("status") or {}).get("phase")
        final = all(s.get("final") is True for s in specs)
        cost = sum(values)
        result = economics(
            Economics(
                currency=currencies.pop(),
                cost=cost,
                failedRunCost=cost if final and phase == "Failed" else 0,
                completedRuns=1 if final and phase == "Succeeded" else 0,
                costBasis="metered-estimate",
            )
        )
        return {
            **result,
            "jobUID": uid,
            "records": len(records),
            "final": final,
            "unknown": ["committed checkpoint count", "evaluation outcomes", "delivered token attribution"],
        }

    @router.get("/api/intelligence/inventory")
    @deps.limiter.limit("30/minute")
    async def inventory(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        nodes = await list_items(deps, "gryviagpunodes")
        return {
            "mode": "cluster-observation",
            "items": [
                {
                    "name": (n.get("metadata") or {}).get("name"),
                    "spec": n.get("spec", {}),
                    "phase": (n.get("status") or {}).get("phase", "Unknown"),
                    "lastHealthCheck": (n.get("status") or {}).get("lastHealthCheck"),
                    "freeGPUs": None,
                }
                for n in nodes
            ],
            "caveat": "Registered capacity is not free capacity. No freshness or admission guarantee is inferred.",
        }

    return router
