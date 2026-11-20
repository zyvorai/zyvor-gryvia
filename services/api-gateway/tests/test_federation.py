# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

from tests.test_oidc import claims, env, keys  # noqa: F401  (fixtures)
from tests.test_roles import gw  # noqa: F401  (fixture)


def seed(fake_k8s, name="eu-us"):
    fake_k8s.add("gryviafederations", {
        "metadata": {"name": name},
        "spec": {"clusters": [
            {"name": "us-west", "region": "us-west-2", "apiServer": "https://10.0.0.1:6443", "credentials": {"secretRef": "us-west-kubeconfig"}},
            {"name": "eu", "region": "eu-central-1", "apiServer": "https://eu.example:6443"}]},
        "status": {"state": "Degraded",
                   "clusterStatus": [{"name": "us-west", "state": "Healthy", "lastHealthCheck": "2026-10-01T00:00:00Z",
                                      "utilization": {"gpus": 24, "percentage": 75.0}},
                                     {"name": "eu", "state": "Unreachable"}],
                   "aggregateStats": {"totalGPUs": 32, "availableGPUs": 8, "jobsRunning": 3, "totalCostPerHour": 41.5}},
    })


def test_projection_and_no_credentials_leak(make_client, fake_k8s):
    seed(fake_k8s)
    c = make_client("federation")
    item = c.get("/api/federations/eu-us").json()
    assert item["status"] == {"state": "Degraded", "totalGPUs": 32, "availableGPUs": 8, "jobsRunning": 3, "totalCostPerHour": 41.5}
    us, eu = item["spec"]["clusters"]
    assert us == {"name": "us-west", "region": "us-west-2", "hasCredentials": True, "state": "Healthy",
                  "lastHealthCheck": "2026-10-01T00:00:00Z", "gpus": 24, "utilizationPercent": 75.0}
    assert eu["hasCredentials"] is False and eu["state"] == "Unreachable" and "gpus" not in eu
    blob = str(c.get("/api/federations").json())
    assert "kubeconfig" not in blob and "10.0.0.1" not in blob and "eu.example" not in blob and "apiServer" not in blob


def test_empty_and_404(make_client):
    c = make_client("federation")
    assert c.get("/api/federations").json() == {"items": []}
    assert c.get("/api/federations/nope").status_code == 404


def test_admin_only(gw):
    seed(gw.k8s)
    assert gw.get("/api/federations").status_code == 200
    tok = gw.tenant_token(org="alpha")
    assert gw.get("/api/federations", tok).status_code == 403
    assert gw.get("/api/federations/eu-us", tok).status_code == 403
