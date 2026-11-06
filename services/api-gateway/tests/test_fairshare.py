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
