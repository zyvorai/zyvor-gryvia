# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Cross-node comparison of identified NCCL collectives (pure functions, no I/O).

Input is the per-node Flight Recorder ``collectives`` lists merged by
``routers.flight.merge``: one entry per (rank, collective call) with the
communicator ORDINAL, the per-communicator SEQUENCE, the operation, bytes, the
probe-observed rank and world size, and the host-side call duration.

Matching key: (comm_ordinal, comm_seq, operation) inside one job. Limits, all
deliberate and repeated in the result's ``limits``:

* The communicator ordinal is job-scoped (order in which a process created or
  first used communicators). The ncclComm_t pointer is per process and is never
  compared. If ranks create communicators in a different order, groups do not
  line up; the function then sees differing bytes or world sizes and marks the
  group ``inconsistent`` instead of naming a slow rank.
* Sequence numbers count from the moment the probes attached to the process;
  a group containing a ``comm_late`` span may be misaligned.
* Durations are host-side NCCL API call durations. For asynchronous launches
  that is enqueue time, not GPU or network time: a large skew says one rank's
  call was slow (or blocked), it does not identify a GPU or network cause.
* Arrival skew uses each node's wall clock (event time minus duration) and is
  only as good as the nodes' clock synchronisation.
* Ranks that did not report are listed (``missingRanks``) but never invented.
"""

from datetime import datetime
from typing import Any, Dict, List, Optional, Tuple

MAX_EVENTS = 20000  # spans considered per call (bounded work)
MAX_GROUPS_RETURNED = 20  # worst groups returned
MAX_MISSING_RANKS = 64
MAX_RANK = 1 << 20
MAX_DURATION_NS = 10**12
STRAGGLER_RATIO = 2.0  # slowest > 2x fastest ...
STRAGGLER_FLOOR_NS = 5_000_000  # ... and at least 5 ms slower: same thresholds as the collector
MIN_GROUPS_FOR_FINDING = 5

LIMITS = [
    "Durations are host-side NCCL API call durations (enqueue time for asynchronous launches), "
    "not GPU or network time.",
    "The communicator ordinal is job-scoped, not the ncclComm_t pointer; it assumes every process creates "
    "communicators in the same order.",
    "Sequence numbers start when the probes attached; groups with late-attached communicators may be misaligned.",
    "Arrival skew depends on node clock synchronisation.",
    "Missing ranks are reported, never estimated.",
]


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _parse_time(value: Any) -> Optional[float]:
    if not isinstance(value, str) or not value:
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    except ValueError:
        return None


def _span(item: Any) -> Optional[Dict[str, Any]]:
    """Validated span or None. Unknown ranks and malformed entries are dropped, never repaired."""
    if not isinstance(item, dict):
        return None
    ordinal, seq, rank = item.get("comm_ordinal"), item.get("comm_seq"), item.get("comm_rank")
    op, duration = item.get("operation"), item.get("duration_ns")
    if not (_is_int(ordinal) and ordinal > 0 and _is_int(seq) and seq > 0 and _is_int(rank) and 0 <= rank < MAX_RANK):
        return None
    if not (isinstance(op, str) and op and len(op) <= 32 and _is_int(duration) and 0 <= duration < MAX_DURATION_NS):
        return None
    world, size = item.get("comm_world", 0), item.get("bytes", 0)
    if not (_is_int(world) and 0 <= world < MAX_RANK and _is_int(size) and size >= 0):
        return None
    node = item.get("node")
    if not isinstance(node, str) or not node or len(node) > 253:
        return None
    end = _parse_time(item.get("time"))
    return {
        "ordinal": ordinal,
        "seq": seq,
        "rank": rank,
        "op": op,
        "duration": duration,
        "world": world,
        "bytes": size,
        "node": node,
        "late": item.get("comm_late") is True,
        "start": None if end is None else end - duration / 1e9,
    }


def _group(key: Tuple[int, int, str], spans: List[Dict[str, Any]]) -> Dict[str, Any]:
    ordinal, seq, op = key
    sizes = {s["bytes"] for s in spans}
    worlds = {s["world"] for s in spans if s["world"] > 0}
    ranks = [s["rank"] for s in spans]
    duplicate = len(set(ranks)) != len(ranks)
    inconsistent = duplicate or len(sizes) > 1 or len(worlds) > 1
    world = max(worlds) if len(worlds) == 1 else 0
    by_rank = sorted(spans, key=lambda s: (s["rank"], s["node"]))
    present = set(ranks)
    missing = [] if not world else [r for r in range(min(world, MAX_RANK)) if r not in present][:MAX_MISSING_RANKS]
    group: Dict[str, Any] = {
        "commOrdinal": ordinal,
        "seq": seq,
        "operation": op,
        "bytes": max(sizes),
        "world": world,
        "ranks": [{"rank": s["rank"], "node": s["node"], "durationNs": s["duration"]} for s in by_rank],
        "missingRanks": missing,
        "partial": bool(missing) or not world,
        "late": any(s["late"] for s in spans),
        "inconsistent": inconsistent,
        "skewNs": 0,
        "slowestRank": None,
        "slowestNode": None,
        "fastestRank": None,
    }
    if inconsistent or len(by_rank) < 2:
        return group
    slow = max(by_rank, key=lambda s: (s["duration"], -s["rank"]))
    fast = min(by_rank, key=lambda s: (s["duration"], s["rank"]))
    group.update(
        skewNs=slow["duration"] - fast["duration"],
        slowestRank=slow["rank"],
        slowestNode=slow["node"],
        fastestRank=fast["rank"],
    )
    starts = [s["start"] for s in by_rank]
    if all(t is not None for t in starts):
        last = max(by_rank, key=lambda s: (s["start"], s["rank"]))
        group["arrivalSkewNs"] = round((max(starts) - min(starts)) * 1e6) * 1000  # microsecond resolution
        group["lastArrivingRank"] = last["rank"]
        group["lastArrivingNode"] = last["node"]
    return group


def _straggling(group: Dict[str, Any]) -> bool:
    if group["slowestRank"] is None:
        return False
    durations = [r["durationNs"] for r in group["ranks"]]
    fast, slow = min(durations), max(durations)
    return slow > STRAGGLER_FLOOR_NS and fast > 0 and slow > STRAGGLER_RATIO * fast


def compare_collectives(events: List[Any]) -> Dict[str, Any]:
    """Compare the same collective across ranks and nodes; see the module docstring for the limits."""
    buckets: Dict[Tuple[int, int, str], List[Dict[str, Any]]] = {}
    dropped = 0
    for item in events[:MAX_EVENTS]:
        span = _span(item)
        if span is None:
            dropped += 1
            continue
        buckets.setdefault((span["ordinal"], span["seq"], span["op"]), []).append(span)
    groups = [_group(key, spans) for key, spans in sorted(buckets.items())]
    compared = [g for g in groups if g["slowestRank"] is not None]
    slow_counts: Dict[Tuple[int, str], Dict[str, int]] = {}
    for g in compared:
        entry = slow_counts.setdefault((g["slowestRank"], g["slowestNode"]), {"slowest": 0, "straggling": 0})
        entry["slowest"] += 1
        entry["straggling"] += 1 if _straggling(g) else 0
    slowest_ranks = [
        {"rank": rank, "node": node, "slowestInGroups": c["slowest"], "stragglingGroups": c["straggling"]}
        for (rank, node), c in slow_counts.items()
    ]
    slowest_ranks.sort(key=lambda r: (-r["stragglingGroups"], -r["slowestInGroups"], r["rank"]))
    worst = sorted(compared, key=lambda g: (-g["skewNs"], g["commOrdinal"], g["seq"]))[:MAX_GROUPS_RETURNED]
    findings = []
    straggle_total = sum(r["stragglingGroups"] for r in slowest_ranks)
    if slowest_ranks and straggle_total >= MIN_GROUPS_FOR_FINDING:
        top = slowest_ranks[0]
        if top["stragglingGroups"] * 2 > straggle_total:
            findings.append(
                {
                    "node": top["node"],
                    "code": "nccl_collective_slow_rank",
                    "evidence": (
                        f"Rank {top['rank']} (node {top['node']}) had the longest host-side NCCL call in "
                        f"{top['stragglingGroups']} of {straggle_total} matched collectives whose slowest call "
                        f"was over {STRAGGLER_RATIO:g}x and {STRAGGLER_FLOOR_NS // 10**6} ms above the fastest. "
                        "Durations are API call durations, not GPU or network time."
                    ),
                }
            )
    return {
        "groups": len(groups),
        "matched": len(compared),
        "singleRank": sum(1 for g in groups if len(g["ranks"]) < 2),
        "inconsistent": sum(1 for g in groups if g["inconsistent"]),
        "partial": sum(1 for g in groups if g["partial"]),
        "late": sum(1 for g in groups if g["late"]),
        "droppedSpans": dropped,
        "slowestRanks": slowest_ranks[:20],
        "worst": worst,
        "findings": findings,
        "limits": LIMITS,
    }
