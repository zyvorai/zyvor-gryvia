# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Unit tests for lookback fairshare recommendations."""
from routers.fairshare import compute_fairshare


def test_compute_fairshare_boosts_starved_tenant():
    tenants = [
        {"metadata": {"name": "a"}, "spec": {"quotas": {"fairShareWeight": 1}}},
        {"metadata": {"name": "b"}, "spec": {"quotas": {"fairShareWeight": 1}}},
    ]
    records = [
        {"spec": {"tenant": "a", "gpuHours": 90, "start": "2026-01-01T00:00:00Z"}},
        {"spec": {"tenant": "b", "gpuHours": 10, "start": "2026-01-01T00:00:00Z"}},
    ]
    report = compute_fairshare(tenants, records, lookback_days=7)
    by = {r["tenant"]: r for r in report["items"]}
    assert by["b"]["recommendedWeight"] >= by["a"]["recommendedWeight"]
    assert by["a"]["delta"] <= 0
    assert by["b"]["delta"] >= 0


def test_compute_fairshare_empty():
    report = compute_fairshare([], [], lookback_days=7)
    assert report["items"] == []
    assert report["totalGpuHours"] == 0


def test_time_decay_favours_recent_usage():
    from datetime import datetime, timezone
    from routers.fairshare import compute_fairshare, decay_factor
    now = datetime(2026, 3, 31, tzinfo=timezone.utc)
    tenants = [{"metadata": {"name": "a"}, "spec": {"quotas": {"fairShareWeight": 1}}},
               {"metadata": {"name": "b"}, "spec": {"quotas": {"fairShareWeight": 1}}}]
    # a used 100h 14 days ago, b used 100h yesterday: equal raw usage, unequal once decayed (half-life 7d)
    records = [{"spec": {"tenant": "a", "gpuHours": 100, "start": "2026-03-17T00:00:00Z"}},
               {"spec": {"tenant": "b", "gpuHours": 100, "start": "2026-03-30T00:00:00Z"}}]
    flat = {r["tenant"]: r for r in compute_fairshare(tenants, records, lookback_days=30, now=now)["items"]}
    assert flat["a"]["shareActual"] == flat["b"]["shareActual"] == 0.5
    rep = compute_fairshare(tenants, records, lookback_days=30, half_life_days=7, now=now)
    by = {r["tenant"]: r for r in rep["items"]}
    assert rep["halfLifeDays"] == 7
    assert by["a"]["gpuHours"] == 25.0 and by["b"]["shareActual"] > by["a"]["shareActual"]
    assert by["a"]["recommendedWeight"] >= by["b"]["recommendedWeight"]
    assert decay_factor(7, 7) == 0.5 and decay_factor(0, 7) == 1.0 and decay_factor(5, None) == 1.0
    assert decay_factor(-3, 7) == 1.0  # future-dated records are not boosted


def test_halflife_query_validation(make_client):
    c = make_client("fairshare")
    assert c.get("/api/fairshare?halfLifeDays=0").status_code == 422
    assert c.get("/api/fairshare?halfLifeDays=400").status_code == 422
    assert c.get("/api/fairshare?halfLifeDays=3.5").json()["halfLifeDays"] == 3.5
