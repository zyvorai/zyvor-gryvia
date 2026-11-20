# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Lineage graph (dataset -> job -> model -> inference service) for the dashboard.

Read-only and derived from existing resources, nothing is stored:
  * dataset -> job: the job mounts the PVC the dataset was materialized into (same namespace);
  * job -> model: GryviaModelRegistry.spec.source.jobRef;
  * model -> service: GryviaInferenceService.spec.modelRef.
Tenants see only nodes in their own namespaces; edges to hidden nodes are dropped.
"""
from typing import Any, Dict, List, Set

from fastapi import APIRouter, Depends, Request

from .common import Deps, list_items
from .datasets import dataset_namespace
from .markings import can_see
from .uiutil import namespaces

JOBS, MODELS, SERVICES, DATASETS = ("gryviaaijobs", "gryviamodelregistries", "gryviainferenceservices",
                                    "gryviadatasets")


def _ns(obj: Dict[str, Any], default: str) -> str:
    return (obj.get("metadata") or {}).get("namespace") or default


def _name(obj: Dict[str, Any]) -> str:
    return (obj.get("metadata") or {}).get("name", "")


def _claims(job: Dict[str, Any]) -> Set[str]:
    vols = (job.get("spec") or {}).get("volumes") or []
    names = {(v.get("persistentVolumeClaim") or {}).get("claimName") for v in vols}
    return {n for n in names if n}


def build_graph(datasets: List[Dict[str, Any]], jobs: List[Dict[str, Any]], models: List[Dict[str, Any]],
                services: List[Dict[str, Any]], default_ns: str) -> Dict[str, Any]:
    nodes: Dict[str, Dict[str, Any]] = {}
    edges: List[Dict[str, str]] = []

    def node(kind: str, name: str, ns: str, **extra: Any) -> str:
        nid = f"{kind}/{ns}/{name}"
        nodes[nid] = {"id": nid, "kind": kind, "name": name, "namespace": ns, **extra}
        return nid

    def edge(src: str, dst: str) -> None:
        if src in nodes and dst in nodes:
            edges.append({"from": src, "to": dst})

    pvc_to_dataset: Dict[tuple, str] = {}
    for d in datasets:
        ns = dataset_namespace(d, default_ns)
        nid = node("dataset", _name(d), ns, state=(d.get("status") or {}).get("state"))
        pvc = (d.get("status") or {}).get("pvcName")
        if pvc:
            pvc_to_dataset[(ns, pvc)] = nid
    for j in jobs:
        ns = _ns(j, default_ns)
        node("job", _name(j), ns, phase=(j.get("status") or {}).get("phase"))
    for m in models:
        ns = _ns(m, default_ns)
        spec = m.get("spec") or {}
        node("model", _name(m), ns, version=spec.get("version"), stage=spec.get("stage"))
    for s in services:
        node("service", _name(s), _ns(s, default_ns))

    for j in jobs:
        ns = _ns(j, default_ns)
        jid = f"job/{ns}/{_name(j)}"
        for claim in _claims(j):
            if (ns, claim) in pvc_to_dataset:
                edge(pvc_to_dataset[(ns, claim)], jid)
    for m in models:
        ns = _ns(m, default_ns)
        ref = ((m.get("spec") or {}).get("source") or {}).get("jobRef")
        if ref:
            edge(f"job/{ns}/{ref}", f"model/{ns}/{_name(m)}")
    for s in services:
        ns = _ns(s, default_ns)
        ref = (s.get("spec") or {}).get("modelRef")
        if ref:
            edge(f"model/{ns}/{ref}", f"service/{ns}/{_name(s)}")
    return {"nodes": list(nodes.values()), "edges": edges}


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/lineage")
    @deps.limiter.limit("30/minute")
    async def lineage(request: Request, _=Depends(deps.verify_auth)):
        allowed = None if getattr(request.state, "role", None) == "admin" else set(namespaces(request, deps))

        async def fetch(plural: str) -> List[Dict[str, Any]]:
            items = [o for o in await list_items(deps, plural) if can_see(request, o)]
            if allowed is None:
                return items
            return [o for o in items
                    if (dataset_namespace(o, deps.job_namespace) if plural == DATASETS
                        else _ns(o, deps.job_namespace)) in allowed]

        graph = build_graph(await fetch(DATASETS), await fetch(JOBS), await fetch(MODELS), await fetch(SERVICES),
                            deps.job_namespace)
        return graph

    return router
