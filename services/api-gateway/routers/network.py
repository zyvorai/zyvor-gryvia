# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Network intelligence routes (flows, policies, insights, graph, anomalies, costs, traces).

Everything is derived from network-intelligence CRs; when no source object exists the
response is empty / zero, never sample data.
"""
import hashlib
import re
import uuid
from datetime import datetime, timezone
from typing import Any, Dict, List, Literal, Optional

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, Field, field_validator

from .common import Deps, create_item, get_item, list_items, patch_item, require_admin
from .netra import fetch_flows

K8S_NAME = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
DURATION = re.compile(r"^[1-9][0-9]{0,3}[smh]$")
INTENT = re.compile(r"^[\w .,:;/()+-]{1,200}$")

FLOW_POLICIES = "gryviaflowpolicies"
INSIGHTS = "gryviatrafficinsights"
GRAPHS = "gryviaservicegraphs"
ANOMALIES = "gryvianetworkanomalies"
COSTS = "gryvianetworkcosts"
TRACES = "gryviatracesessions"


# CRD verdicts (forwarded, dropped, error) -> the verdict vocabulary the UI understands.
_VERDICTS = {"forwarded": "FORWARDED", "dropped": "DROP", "drop": "DROP", "denied": "DENIED",
             "allow": "ALLOW", "allowed": "ALLOW", "error": "ERROR"}

# CRD node health -> exactly one of healthy | warning | critical | unknown.
_HEALTH = {"healthy": "healthy", "ok": "healthy", "ready": "healthy", "up": "healthy",
           "warning": "warning", "warn": "warning", "degraded": "warning",
           "critical": "critical", "unhealthy": "critical", "bad": "critical", "down": "critical",
           "error": "critical", "failed": "critical"}


def norm_verdict(value: Any) -> str:
    """Verdict vocabulary shared by /flows and /graph: FORWARDED|DROP|DENIED|ALLOW|ERROR, else ''."""
    return _VERDICTS.get(value.strip().lower(), "") if isinstance(value, str) else ""


def norm_health(value: Any) -> str:
    return _HEALTH.get(value.strip().lower(), "unknown") if isinstance(value, str) else "unknown"


def _valid_name(v: str) -> str:
    if len(v) > 63 or not K8S_NAME.match(v):
        raise ValueError("must be a valid Kubernetes name (lowercase alphanumerics and '-', max 63)")
    return v


class FlowPolicyRequest(BaseModel):
    sourceService: str = Field(min_length=1, max_length=63)
    destinationService: str = Field(min_length=1, max_length=63)
    port: int = Field(ge=1, le=65535)
    protocol: Literal["tcp", "udp", "icmp", "any", "http", "grpc"]
    action: Literal["allow", "deny", "log"]
    intent: str = Field(min_length=1, max_length=200)
    name: Optional[str] = Field(default=None, min_length=1, max_length=63)

    @field_validator("protocol", mode="before")
    @classmethod
    def _lower_protocol(cls, v: Any) -> Any:
        return v.lower() if isinstance(v, str) else v

    @field_validator("sourceService", "destinationService", "name")
    @classmethod
    def _names(cls, v: Optional[str]) -> Optional[str]:
        return v if v is None else _valid_name(v)

    @field_validator("intent")
    @classmethod
    def _intent(cls, v: str) -> str:
        v = v.strip()
        if not INTENT.match(v):
            raise ValueError("intent contains unsupported characters")
        return v


class TraceRequest(BaseModel):
    targetService: str = Field(min_length=1, max_length=63)
    duration: str = Field(min_length=2, max_length=5)
    captureLevel: Literal["l3", "l4", "l7"]
    namespace: str = Field(min_length=1, max_length=63)

    @field_validator("captureLevel", mode="before")
    @classmethod
    def _lower_level(cls, v: Any) -> Any:
        return v.lower() if isinstance(v, str) else v

    @field_validator("targetService", "namespace")
    @classmethod
    def _names(cls, v: str) -> str:
        return _valid_name(v)

    @field_validator("duration")
    @classmethod
    def _duration(cls, v: str) -> str:
        if not DURATION.match(v):
            raise ValueError("duration must look like 30s, 5m or 1h")
        return v


def _meta(obj: Dict[str, Any]) -> Dict[str, Any]:
    m = obj.get("metadata", {})
    out = {"name": m.get("name", "")}
    for k in ("namespace", "creationTimestamp", "labels"):
        if m.get(k):
            out[k] = m[k]
    return out


def _policy_view(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec = obj.get("spec") or {}
    src, dst = spec.get("source") or {}, spec.get("destination") or {}
    out: Dict[str, Any] = {
        "metadata": _meta(obj),
        "spec": {
            "sourceService": src.get("service", ""),
            "destinationService": dst.get("service", ""),
            "port": dst.get("port", 0),
            "protocol": spec.get("protocol", ""),
            "action": spec.get("action", ""),
            "intent": spec.get("intent", ""),
        },
    }
    st = obj.get("status") or {}
    if st:
        out["status"] = {"phase": st.get("phase") or ("Enforced" if st.get("enforced") else "Pending"),
                         "matchedFlows": st.get("matchedFlows", 0)}
    return out


def _trace_view(obj: Dict[str, Any]) -> Dict[str, Any]:
    spec = obj.get("spec") or {}
    out: Dict[str, Any] = {
        "metadata": _meta(obj),
        "spec": {
            "targetService": spec.get("service", ""),
            "duration": spec.get("duration", ""),
            "captureLevel": spec.get("level", ""),
            "namespace": spec.get("namespace", ""),
        },
    }
    st = obj.get("status") or {}
    if st:
        phase = st.get("phase", "")
        out["status"] = {"phase": phase[:1].upper() + phase[1:], "flowsCaptured": st.get("flowsCaptured", 0)}
    return out


def _merge_graphs(graphs: List[Dict[str, Any]]) -> Dict[str, Any]:
    nodes: Dict[str, Dict[str, Any]] = {}
    edges: List[Dict[str, Any]] = []
    seen = set()
    for g in graphs:
        st = g.get("status") or {}
        for n in st.get("nodes") or []:
            nid = n.get("name")
            if nid and nid not in nodes:
                nodes[nid] = {"id": nid, "label": nid, "health": norm_health(n.get("health")),
                              "flowCount": 0}
        for e in st.get("edges") or []:
            s, d = e.get("source"), e.get("destination")
            if not s or not d:
                continue
            key = (s, d, e.get("protocol", ""), e.get("port", 0))
            if key in seen:
                continue
            seen.add(key)
            edges.append({"source": s, "target": d, "protocol": e.get("protocol", ""),
                          "latency": e.get("latencyP99", ""), "verdict": norm_verdict(e.get("verdict")),
                          "_port": e.get("port", 0), "_throughput": e.get("throughput", ""),
                          "_updated": st.get("lastUpdated", "")})
    for e in edges:
        for end in (e["source"], e["target"]):
            nodes.setdefault(end, {"id": end, "label": end, "health": "unknown", "flowCount": 0})
            nodes[end]["flowCount"] += 1
    return {"nodes": list(nodes.values()), "edges": edges}


def _anomaly_items(anomaly_crs: List[Dict[str, Any]], insight_crs: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
    items: List[Dict[str, Any]] = []
    sources = ((anomaly_crs, "targetService", "anomalies", "detected"),
               (insight_crs, "service", "anomalies", "detected"))
    for crs, svc_key, list_key, ts_key in sources:
        for cr in crs:
            svc = (cr.get("spec") or {}).get(svc_key, "")
            for i, a in enumerate((cr.get("status") or {}).get(list_key) or []):
                meta = _meta(cr)
                meta["name"] = f"{meta['name']}-{i}"
                items.append({"metadata": meta, "spec": {
                    "severity": (a.get("severity") or "low").lower(),
                    "service": svc,
                    "type": a.get("type", ""),
                    "description": a.get("description", ""),
                    "detectedAt": a.get(ts_key, ""),
                }})
    items.sort(key=lambda x: x["spec"]["detectedAt"], reverse=True)
    return items


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/network/flows")
    @deps.limiter.limit("30/minute")
    async def list_flows(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        """Observed flows: real flows from Netra when GRYVIA_NETRA_URL is set and reachable, otherwise the
        GryviaServiceGraph edges (no separate flow CRD exists)."""
        netra = await fetch_flows(deps)
        if netra is not None:
            return {"items": netra, "source": "netra"}
        graphs = await list_items(deps, GRAPHS)
        items = []
        for g in graphs:
            st = g.get("status") or {}
            ns = g.get("metadata", {}).get("namespace")
            for i, e in enumerate(st.get("edges") or []):
                meta: Dict[str, Any] = {"name": f"{g['metadata']['name']}-{i}"}
                if ns:
                    meta["namespace"] = ns
                items.append({"metadata": meta, "spec": {
                    "timestamp": st.get("lastUpdated", ""),
                    "source": e.get("source", ""),
                    "destination": e.get("destination", ""),
                    "protocol": (e.get("protocol") or "").upper(),
                    "port": e.get("port", 0),
                    "bytes": e.get("throughput", ""),
                    "latency": e.get("latencyP99", ""),
                    "verdict": norm_verdict(e.get("verdict")),
                }})
        return {"items": items}

    @router.get("/api/network/policies")
    @deps.limiter.limit("30/minute")
    async def list_policies(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        return {"items": [_policy_view(o) for o in await list_items(deps, FLOW_POLICIES)]}

    @router.post("/api/network/policies", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_policy(request: Request, body: FlowPolicyRequest, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        name = body.name
        if not name:
            digest = hashlib.sha1(
                f"{body.sourceService}|{body.destinationService}|{body.port}|{body.protocol}".encode()
            ).hexdigest()[:8]
            name = f"{body.sourceService[:22]}-to-{body.destinationService[:22]}-{digest}"
        spec = {
            "source": {"service": body.sourceService},
            "destination": {"service": body.destinationService, "port": body.port},
            "protocol": body.protocol, "action": body.action, "intent": body.intent,
        }
        created = await create_item(deps, FLOW_POLICIES, "GryviaFlowPolicy", name, spec)
        return _policy_view(created)

    @router.post("/api/network/policies/{name}/apply")
    @deps.limiter.limit("10/minute")
    async def apply_policy(request: Request, name: str, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        """Request (re)enforcement: annotate the CR so the operator reconciles it."""
        _name_or_404(name)
        await get_item(deps, FLOW_POLICIES, name)
        stamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        patched = await patch_item(deps, FLOW_POLICIES, name,
                                   {"metadata": {"annotations": {"gryvia.io/apply-requested-at": stamp}}})
        return _policy_view(patched)

    @router.get("/api/network/insights")
    @deps.limiter.limit("30/minute")
    async def insights(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        graphs = await list_items(deps, GRAPHS)
        policies = await list_items(deps, FLOW_POLICIES)
        anomalies = await list_items(deps, ANOMALIES)
        tis = await list_items(deps, INSIGHTS)
        traces = await list_items(deps, TRACES)
        enforced = sum(1 for p in policies
                       if (p.get("status") or {}).get("enforced")
                       or (p.get("status") or {}).get("phase", "").lower() in ("enforced", "active"))
        return {
            "activeFlows": len(_merge_graphs(graphs)["edges"]),
            "policiesEnforced": enforced,
            "anomaliesDetected": len(_anomaly_items(anomalies, tis)),
            "traceSessions": len(traces),
        }

    @router.get("/api/network/graph")
    @deps.limiter.limit("30/minute")
    async def graph(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        g = _merge_graphs(await list_items(deps, GRAPHS))
        g["edges"] = [{k: v for k, v in e.items() if not k.startswith("_")} for e in g["edges"]]
        return g

    @router.get("/api/network/anomalies")
    @deps.limiter.limit("30/minute")
    async def anomalies(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        return {"items": _anomaly_items(await list_items(deps, ANOMALIES), await list_items(deps, INSIGHTS))}

    @router.get("/api/network/costs")
    @deps.limiter.limit("30/minute")
    async def costs(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        crs = sorted(await list_items(deps, COSTS), key=lambda o: o.get("metadata", {}).get("name", ""))
        rates = {"sameZone": 0, "crossZone": 0, "internetEgress": 0}
        for cr in crs:
            r = (cr.get("spec") or {}).get("costPerGB") or {}
            if any(r.get(k) for k in rates):
                rates = {k: r.get(k, 0) for k in rates}
                break
        reports = []
        for cr in crs:
            for r in (cr.get("status") or {}).get("reports") or []:
                reports.append({
                    "period": r.get("period", ""), "namespace": r.get("namespace", ""),
                    "team": r.get("team", ""), "sameZoneBytes": r.get("sameZoneBytes", 0),
                    "crossZoneBytes": r.get("crossZoneBytes", 0), "externalBytes": r.get("externalBytes", 0),
                    "totalCostUSD": r.get("totalCostUSD", 0),
                })
        return {"reports": reports, "costPerGB": rates}

    @router.get("/api/network/traces")
    @deps.limiter.limit("30/minute")
    async def list_traces(request: Request, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        return {"items": [_trace_view(o) for o in await list_items(deps, TRACES)]}

    @router.post("/api/network/traces", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_trace(request: Request, body: TraceRequest, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        name = f"trace-{body.targetService[:40]}-{uuid.uuid4().hex[:8]}"
        spec = {"service": body.targetService, "namespace": body.namespace,
                "duration": body.duration, "level": body.captureLevel}
        created = await create_item(deps, TRACES, "GryviaTraceSession", name, spec)
        return _trace_view(created)

    @router.get("/api/network/traces/{name}")
    @deps.limiter.limit("30/minute")
    async def get_trace(request: Request, name: str, _=Depends(deps.verify_auth), __=Depends(require_admin)):
        _name_or_404(name)
        return _trace_view(await get_item(deps, TRACES, name))

    return router


def _name_or_404(name: str) -> None:
    if len(name) > 63 or not K8S_NAME.match(name):
        raise HTTPException(status_code=404, detail="not found")
