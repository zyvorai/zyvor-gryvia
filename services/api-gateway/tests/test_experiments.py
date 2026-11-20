# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

from routers.experiments import leaderboard

NS = "default"


def seed(fake_k8s, name="lr-sweep", direction="minimize", board=None, ns=NS):
    fake_k8s.add("gryvialiveexperiments", {
        "metadata": {"name": name},
        "spec": {"description": "lr sweep", "comparison": {"primaryMetric": "loss", "direction": direction},
                 "jobs": [{"name": "a", "jobRef": "job-a"}, {"name": "b", "jobRef": "job-b"}]},
        "status": {"phase": "Running", "startTime": "2026-01-01T00:00:00Z", "gpuHoursSaved": 3.5, "costSaved": 12,
                   "leaderboard": board if board is not None else [
                       {"rank": 1, "job": "a", "primaryMetricValue": 0.5, "status": "Running"},
                       {"rank": 2, "job": "b", "primaryMetricValue": 0.4, "status": "Running"}]},
    }, namespace=ns)


def test_leaderboard_is_reranked_with_deltas_minimize():
    rows = leaderboard([{"rank": 1, "job": "a", "primaryMetricValue": 0.5}, {"rank": 2, "job": "b", "primaryMetricValue": 0.4}], "minimize")
    assert [r["job"] for r in rows] == ["b", "a"] and [r["rank"] for r in rows] == [1, 2]
    assert rows[0]["deltaToBest"] == 0 and abs(rows[1]["deltaToBest"] - 0.1) < 1e-9 and abs(rows[1]["deltaPct"] - 25) < 1e-6


def test_leaderboard_maximize_and_zero_best_and_non_numbers():
    rows = leaderboard([{"job": "a", "primaryMetricValue": 0.9}, {"job": "b", "primaryMetricValue": 0.7}, {"job": "c", "primaryMetricValue": "nan?"}], "maximize")
    assert [r["job"] for r in rows] == ["a", "b", "c"] and rows[1]["deltaToBest"] < 0 and "deltaToBest" not in rows[2]
    z = leaderboard([{"job": "a", "primaryMetricValue": 0}, {"job": "b", "primaryMetricValue": 2}], "minimize")
    assert z[1]["deltaPct"] is None


def test_unknown_direction_keeps_operator_ranks():
    rows = leaderboard([{"rank": 2, "job": "b", "primaryMetricValue": 1}, {"rank": 1, "job": "a", "primaryMetricValue": 9}], "sideways")
    assert [r["job"] for r in rows] == ["a", "b"] and "deltaToBest" not in rows[0]


def test_list_and_get_project_spec_and_status(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("experiments")
    item = c.get("/api/experiments/lr-sweep").json()
    assert item["spec"]["primaryMetric"] == "loss" and item["spec"]["direction"] == "minimize"
    assert item["spec"]["jobs"] == [{"name": "a", "jobRef": "job-a"}, {"name": "b", "jobRef": "job-b"}]
    st = item["status"]
    assert st["phase"] == "Running" and st["gpuHoursSaved"] == 3.5 and st["costSaved"] == 12
    assert st["leaderboard"][0]["job"] == "b" and st["leaderboard"][0]["rank"] == 1
    assert c.get("/api/experiments").json()["items"][0]["metadata"]["name"] == "lr-sweep"


def test_empty_and_404(make_client):
    c = make_client("experiments")
    assert c.get("/api/experiments").json() == {"items": []}
    assert c.get("/api/experiments/nope").status_code == 404


def test_experiment_without_status_is_projected(make_client, fake_k8s):
    fake_k8s.add("gryvialiveexperiments", {"metadata": {"name": "new"}, "spec": {"comparison": {"primaryMetric": "acc", "direction": "maximize"}, "jobs": []}}, namespace=NS)
    item = make_client("experiments").get("/api/experiments/new").json()
    assert item["status"] == {} and item["spec"]["primaryMetric"] == "acc"


from tests.test_oidc import claims, env, keys  # noqa: E402,F401  (fixtures)
from tests.test_roles import gw  # noqa: E402,F401  (fixture)


def test_tenant_sees_only_its_own_namespace(gw):
    seed(gw.k8s, "alpha-exp", ns="tenant-alpha")
    seed(gw.k8s, "beta-exp", ns="tenant-beta")
    tok = gw.tenant_token(org="alpha")
    names = [i["metadata"]["name"] for i in gw.get("/api/experiments", tok).json()["items"]]
    assert names == ["alpha-exp"]
    assert gw.get("/api/experiments/beta-exp", tok).status_code == 404
    assert gw.get("/api/experiments/alpha-exp", tok).status_code == 200
