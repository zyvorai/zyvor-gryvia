# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Cross-node collective comparison: matching by (comm ordinal, seq, op), skew, partial groups."""

import routers.flight as flight_module
from routers.collectives import STRAGGLER_FLOOR_NS, compare_collectives
from routers.flight import merge


def span(node, rank, seq, dur, ordinal=1, op="allreduce", world=4, nbytes=4096, time=None, late=False):
    item = {
        "node": node,
        "operation": op,
        "comm_ordinal": ordinal,
        "comm_seq": seq,
        "comm_rank": rank,
        "comm_world": world,
        "bytes": nbytes,
        "duration_ns": dur,
    }
    if time:
        item["time"] = time
    if late:
        item["comm_late"] = True
    return item


def test_skew_slowest_rank_and_node_across_nodes():
    events = [span("a", 0, 1, 1_000), span("a", 1, 1, 1_500), span("b", 2, 1, 9_000), span("b", 3, 1, 1_200)]
    result = compare_collectives(events)
    group = result["worst"][0]
    assert result["matched"] == 1 and result["partial"] == 0
    assert group["skewNs"] == 8_000 and group["slowestRank"] == 2 and group["slowestNode"] == "b"
    assert group["fastestRank"] == 0 and group["missingRanks"] == []


def test_matching_is_by_ordinal_seq_and_op_not_size_or_arrival():
    events = [
        span("a", 0, 1, 100),
        span("b", 1, 1, 200),  # comm 1 seq 1
        span("a", 0, 2, 100),
        span("b", 1, 2, 250),  # comm 1 seq 2
        span("a", 0, 1, 7_000, ordinal=2),
        span("b", 1, 1, 7_100, ordinal=2),  # other comm, same seq
        span("a", 0, 3, 100, op="allgather"),
        span("b", 1, 3, 100, op="allreduce"),  # same seq, other op
    ]
    result = compare_collectives(events)
    keys = {(g["commOrdinal"], g["seq"], g["operation"]): g for g in result["worst"]}
    assert keys[(1, 1, "allreduce")]["skewNs"] == 100
    assert keys[(1, 2, "allreduce")]["skewNs"] == 150
    assert keys[(2, 1, "allreduce")]["skewNs"] == 100
    assert result["singleRank"] == 2  # the two different-op calls do not pair up
    assert result["matched"] == 3


def test_partial_groups_report_missing_ranks_and_never_invent():
    events = [span("a", 0, 1, 100, world=4), span("b", 2, 1, 900, world=4)]
    group = compare_collectives(events)["worst"][0]
    assert group["missingRanks"] == [1, 3] and group["partial"] is True
    assert group["skewNs"] == 800 and len(group["ranks"]) == 2  # only observed ranks


def test_single_rank_group_has_no_skew_and_unknown_world_has_no_missing_list():
    result = compare_collectives([span("a", 0, 1, 500, world=0)])
    assert result["matched"] == 0 and result["singleRank"] == 1 and result["worst"] == []
    result = compare_collectives([span("a", 0, 1, 500, world=0), span("b", 3, 1, 900, world=0)])
    group = result["worst"][0]
    assert group["missingRanks"] == [] and group["partial"] is True and group["world"] == 0


def test_inconsistent_groups_are_not_compared():
    for events in (
        [span("a", 0, 1, 100, nbytes=1024), span("b", 1, 1, 9_000, nbytes=2048)],  # different payload
        [span("a", 0, 1, 100, world=4), span("b", 1, 1, 9_000, world=8)],  # different world
        [span("a", 0, 1, 100), span("b", 0, 1, 9_000)],  # same rank twice
    ):
        result = compare_collectives(events)
        assert result["inconsistent"] == 1 and result["matched"] == 0 and result["worst"] == []


def test_malformed_and_unranked_spans_are_dropped():
    bad = [
        {"node": "a", "operation": "allreduce", "comm_ordinal": 1, "comm_seq": 1},  # no rank
        span("a", True, 1, 100),  # bool rank
        span("a", 0, 0, 100),  # seq 0
        span("a", 0, 1, -5),  # negative duration
        "junk",
        None,
        span("", 0, 1, 100),
    ]
    result = compare_collectives(bad + [span("a", 0, 9, 100)])
    assert result["droppedSpans"] == len(bad) and result["groups"] == 1


def test_arrival_skew_uses_event_time_minus_duration():
    events = [
        span("a", 0, 1, 1_000_000_000, time="2026-01-01T00:00:10Z"),  # started 00:00:09
        span("b", 1, 1, 100_000_000, time="2026-01-01T00:00:10Z"),
    ]  # started 00:00:09.9
    group = compare_collectives(events)["worst"][0]
    assert group["arrivalSkewNs"] == 900_000_000 and group["lastArrivingRank"] == 1
    assert "arrivalSkewNs" not in compare_collectives([span("a", 0, 1, 1), span("b", 1, 1, 2)])["worst"][0]


def test_late_flag_is_surfaced():
    result = compare_collectives([span("a", 0, 1, 1, late=True), span("b", 1, 1, 2)])
    assert result["late"] == 1 and result["worst"][0]["late"] is True


def test_slow_rank_finding_needs_repeated_evidence():
    slow = 3 * STRAGGLER_FLOOR_NS
    events = []
    for seq in range(1, 7):
        events += [span("a", 0, seq, 1_000_000), span("b", 1, seq, slow)]
    result = compare_collectives(events)
    assert result["slowestRanks"][0] == {"rank": 1, "node": "b", "slowestInGroups": 6, "stragglingGroups": 6}
    assert result["findings"][0]["code"] == "nccl_collective_slow_rank"
    assert "not GPU or network time" in result["findings"][0]["evidence"]
    assert compare_collectives(events[:4])["findings"] == []  # 2 groups: not enough


def body(node, spans, namespace="ml", job="train"):
    return {
        "node": node,
        "namespace": namespace,
        "job": job,
        "counts": {},
        "events": [],
        "findings": [],
        "collectives": [{"identity": {"namespace": namespace, "job": job, "node": node}, **s} for s in spans],
    }


def test_merge_exposes_comparison_and_filters_foreign_identities():
    bodies = [
        body("a", [span("a", 0, 1, 100)]),
        body("b", [span("b", 1, 1, 400)]),
        body("c", [span("c", 2, 1, 999)], namespace="other"),
    ]
    result = merge("ml", "train", bodies, {"total": 3, "reachable": 3, "reporting": 3})
    cmp_ = result["collectiveComparison"]
    assert cmp_["matched"] == 1 and cmp_["worst"][0]["skewNs"] == 300
    assert [r["rank"] for r in cmp_["worst"][0]["ranks"]] == [0, 1]  # the foreign job's span is excluded
    assert "findings" not in cmp_ and cmp_["limits"]


def test_merge_without_collectives_is_unchanged_shape():
    result = merge("ml", "train", [], {"total": 1, "reachable": 1, "reporting": 0})
    assert result["collectiveComparison"]["groups"] == 0 and result["events"] == []


def test_merge_bounds_collectives_per_node():
    many = [span("a", 0, seq, 100) for seq in range(1, flight_module.NODE_COLLECTIVE_LIMIT + 50)]
    result = merge("ml", "train", [body("a", many)], {"total": 1, "reachable": 1, "reporting": 1})
    assert result["collectiveComparison"]["groups"] == flight_module.NODE_COLLECTIVE_LIMIT
