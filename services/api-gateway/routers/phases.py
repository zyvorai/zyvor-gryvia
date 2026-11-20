# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Job phase helpers shared by the gateway's aggregate endpoints.

Phases are compared case-insensitively. 'Succeeded' and 'Completed' both mean completed;
'Scheduling', 'Queued' and 'Pending' all mean pending.
"""
from typing import Any, Dict, Iterable

PENDING = frozenset({"scheduling", "queued", "pending"})
COMPLETED = frozenset({"succeeded", "completed"})
# Phases that consume (or consumed) GPU time and therefore count towards cost.
BILLABLE = frozenset({"running"}) | COMPLETED


def normalize(phase: Any) -> str:
    return phase.strip().lower() if isinstance(phase, str) else ""


def bucket(phase: Any) -> str:
    """'running' | 'pending' | 'completed' | 'failed' | 'other'."""
    p = normalize(phase)
    if p == "running":
        return "running"
    if p in PENDING:
        return "pending"
    if p in COMPLETED:
        return "completed"
    if p == "failed":
        return "failed"
    return "other"


def count_phases(phases: Iterable[Any]) -> Dict[str, int]:
    counts = {"running": 0, "pending": 0, "completed": 0, "failed": 0, "other": 0}
    for p in phases:
        counts[bucket(p)] += 1
    return counts


def is_billable(phase: Any) -> bool:
    return normalize(phase) in BILLABLE
