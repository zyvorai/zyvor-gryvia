# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Helpers shared by the job-analytics routes (queue, wait statistics, failures, right-sizing).

All of them read GryviaAIJob objects only; nothing here touches the cluster except through ``list_items``.
"""
import math
from datetime import datetime
from typing import Any, Dict, List, Optional

from fastapi import Request

from .common import Deps, list_items
from .markings import can_see
from .tenancy import is_admin
from .uiutil import namespaces, parse_ts

PLURAL = "gryviaaijobs"
QUEUE_ANNOTATION = "gryvia.io/queue-name"
WAITING_PHASES = ("Pending", "Queued", "Scheduling", "")


def tenant_of_ns(ns: str) -> str:
    return ns[len("tenant-"):] if ns.startswith("tenant-") else ns


def spec_of(job: Dict[str, Any]) -> Dict[str, Any]:
    return job.get("spec") or {}


def status_of(job: Dict[str, Any]) -> Dict[str, Any]:
    return job.get("status") or {}


def name_of(job: Dict[str, Any]) -> str:
    return (job.get("metadata") or {}).get("name", "")


def ns_of(job: Dict[str, Any]) -> str:
    return (job.get("metadata") or {}).get("namespace", "")


def queue_of(job: Dict[str, Any]) -> str:
    ann = (job.get("metadata") or {}).get("annotations") or {}
    return spec_of(job).get("queueName") or ann.get(QUEUE_ANNOTATION) or "default"


def created_of(job: Dict[str, Any]) -> Optional[datetime]:
    return parse_ts((job.get("metadata") or {}).get("creationTimestamp"))


def num(v: Any) -> float:
    return float(v) if isinstance(v, (int, float)) and not isinstance(v, bool) and v == v else 0.0


def total_gpus(job: Dict[str, Any]) -> int:
    """GPUs the job asks for in total: per-pod GPUs times the node count of a distributed job."""
    spec = spec_of(job)
    nodes = num((spec.get("distributed") or {}).get("nodes")) or 1
    return int(num(spec.get("gpus")) * max(1, nodes))


def percentile(sorted_values: List[float], p: float) -> Optional[float]:
    """Nearest-rank percentile of an already sorted list (None when empty)."""
    if not sorted_values:
        return None
    rank = max(1, math.ceil(p / 100.0 * len(sorted_values)))
    return sorted_values[min(rank, len(sorted_values)) - 1]


async def load_jobs(request: Request, deps: Deps) -> List[Dict[str, Any]]:
    """The caller's jobs (marking-aware, tenant-scoped); the provider admin sees every namespace."""
    if is_admin(request):
        items = await list_items(deps, PLURAL)
    else:
        items = []
        for ns in namespaces(request, deps):
            items.extend(await list_items(deps, PLURAL, namespace=ns))
    return [j for j in items if can_see(request, j)]
