# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import json

import pytest

NS = "default"
PSPACE = {"lr": {"type": "float", "min": 1e-5, "max": 1e-2},
          "bs": {"type": "choice", "values": [16, 32]}}
BODY = {"name": "t1", "algorithm": "Bayesian", "objectiveMetric": "accuracy", "maxTrials": 20,
        "parameterSpace": json.dumps(PSPACE), "jobTemplate": {"image": "img:1", "gpus": 1}}


def seed(fake_k8s, name="t1", status=None):
    obj = {"metadata": {"name": name},
           "spec": {"searchAlgorithm": "random", "maxTrials": 10, "objective": {"metricName": "acc", "direction": "maximize"},
                    "parameterSpace": [{"name": "lr", "type": "float", "min": 0.1, "max": 1.0},
                                       {"name": "bs", "type": "categorical", "values": ["16", "32"]}],
                    "jobTemplate": {"type": "training", "image": "i", "gpus": 1}}}
    if status is not None:
        obj["status"] = status
    fake_k8s.add("gryviaautotuners", obj, namespace=NS)


STATUS = {"phase": "Running", "trialsCompleted": 2, "trialsRunning": 1,
          "bestTrial": {"name": "t1-2", "phase": "Succeeded", "metricValue": 0.9, "parameters": {"lr": "0.5"}},
          "trials": [
              {"name": "t1-1", "phase": "Succeeded", "metricValue": 0.8, "parameters": {"lr": "0.2"},
               "startTime": "2026-01-01T00:00:00Z", "completionTime": "2026-01-01T00:01:05Z"},
              {"name": "t1-3", "phase": "Running", "parameters": {"lr": "0.7"}}]}


def test_list_empty(make_client):
    r = make_client("tuners").get("/api/tuners")
    assert r.status_code == 200 and r.json() == {"items": []}


def test_list_get_shape(make_client, fake_k8s):
    seed(fake_k8s, status=STATUS)
    c = make_client("tuners")
    item = c.get("/api/tuners/t1").json()
    assert c.get("/api/tuners").json()["items"] == [item]
    assert item["spec"] == {"algorithm": "random", "objectiveMetric": "acc", "metricName": "acc",
                                "direction": "maximize", "maxTrials": 10,
                            "parameterSpace": {"lr": {"type": "float", "min": 0.1, "max": 1.0},
                                               "bs": {"type": "choice", "values": ["16", "32"]}}}
    assert item["status"] == {"phase": "Running", "trialsCompleted": 2, "trialsRunning": 1,
                              "bestMetricValue": 0.9, "bestTrialId": "t1-2"}


def test_no_status_no_gryviaated_values(make_client, fake_k8s):
    seed(fake_k8s)
    assert make_client("tuners").get("/api/tuners/t1").json()["status"] == {}


def test_get_404(make_client):
    c = make_client("tuners")
    assert c.get("/api/tuners/nope").status_code == 404
    assert c.get("/api/tuners/nope/trials").status_code == 404


def test_trials(make_client, fake_k8s):
    seed(fake_k8s, status=STATUS)
    items = make_client("tuners").get("/api/tuners/t1/trials").json()["items"]
    assert items[0] == {"trialId": "t1-1", "parameters": {"lr": "0.2"}, "metricValue": 0.8,
                        "status": "Succeeded", "duration": "1m 5s"}
    assert items[1] == {"trialId": "t1-3", "parameters": {"lr": "0.7"}, "status": "Running"}


def test_trials_empty(make_client, fake_k8s):
    seed(fake_k8s)
    assert make_client("tuners").get("/api/tuners/t1/trials").json() == {"items": []}


def test_create(make_client, fake_k8s):
    r = make_client("tuners").post("/api/tuners", json=BODY)
    assert r.status_code == 201
    stored = fake_k8s.store[("gryviaautotuners", NS, "t1")]
    assert stored["kind"] == "GryviaAutoTuner"
    s = stored["spec"]
    assert s["searchAlgorithm"] == "bayesian" and s["maxTrials"] == 20
    assert s["objective"] == {"metricName": "accuracy", "direction": "maximize"}
    assert s["parameterSpace"] == [{"name": "lr", "type": "float", "min": 1e-5, "max": 1e-2},
                                   {"name": "bs", "type": "categorical", "values": ["16", "32"]}]
    assert s["jobTemplate"] == {"type": "training", "image": "img:1", "gpus": 1}
    assert r.json()["spec"]["parameterSpace"]["bs"]["type"] == "choice"


def test_create_asha_and_dict_space(make_client, fake_k8s):
    b = {**BODY, "algorithm": "ASHA", "parameterSpace": PSPACE, "direction": "minimize",
         "ashaConfig": {"maxEpochs": 9, "reductionFactor": 3}}
    assert make_client("tuners").post("/api/tuners", json=b).status_code == 201
    s = fake_k8s.store[("gryviaautotuners", NS, "t1")]["spec"]
    assert s["ashaConfig"] == {"maxEpochs": 9, "reductionFactor": 3}
    assert s["objective"]["direction"] == "minimize"


def test_create_duplicate(make_client, fake_k8s):
    seed(fake_k8s)
    assert make_client("tuners").post("/api/tuners", json=BODY).status_code == 409


@pytest.mark.parametrize("patch", [
    {"name": "Bad_Name"}, {"algorithm": "genetic"}, {"objectiveMetric": ""}, {"objectiveMetric": "a b"},
    {"maxTrials": 0}, {"maxTrials": 10001}, {"parameterSpace": "{not json"}, {"parameterSpace": "[]"},
    {"parameterSpace": "{}"}, {"parameterSpace": json.dumps({"x": {"type": "float", "min": 1, "max": 1}})},
    {"parameterSpace": json.dumps({"x": {"type": "float", "min": "a", "max": 2}})},
    {"parameterSpace": json.dumps({"x": {"type": "choice", "values": []}})},
    {"parameterSpace": json.dumps({"x": {"type": "weird"}})},
    {"parameterSpace": json.dumps({"bad name": {"type": "int", "min": 1, "max": 2}})},
    {"parameterSpace": json.dumps({"x": {"type": "int", "min": 1, "max": 2, "evil": 1}})},
    {"jobTemplate": None}, {"jobTemplate": {"image": "i"}}, {"algorithm": "ASHA"},
    {"direction": "sideways"},
])
def test_create_validation(make_client, fake_k8s, patch):
    r = make_client("tuners").post("/api/tuners", json={**BODY, **patch})
    assert r.status_code == 422
    assert not fake_k8s.store


def test_create_missing_jobtemplate_key(make_client):
    b = {k: v for k, v in BODY.items() if k != "jobTemplate"}
    assert make_client("tuners").post("/api/tuners", json=b).status_code == 422


def test_direction_and_metric_name(make_client, fake_k8s):
    seed(fake_k8s)
    fake_k8s.store[("gryviaautotuners", NS, "t1")]["spec"]["objective"]["direction"] = "minimize"
    spec = make_client("tuners").get("/api/tuners/t1").json()["spec"]
    assert spec["direction"] == "minimize" and spec["metricName"] == "acc"


def test_direction_defaults_to_maximize(make_client, fake_k8s):
    seed(fake_k8s)
    del fake_k8s.store[("gryviaautotuners", NS, "t1")]["spec"]["objective"]["direction"]
    assert make_client("tuners").get("/api/tuners/t1").json()["spec"]["direction"] == "maximize"


def test_delete(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("tuners")
    assert c.delete("/api/tuners/t1").json() == {"status": "deleted", "name": "t1"}
    assert not fake_k8s.store
    assert c.delete("/api/tuners/t1").status_code == 404
