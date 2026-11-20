# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""Role model and tenant isolation through the real gateway (main.py) with OIDC tokens from a fake IdP."""
import pytest

from tests.test_oidc import API_KEY, claims, env, keys  # noqa: F401  (fixtures)


def job(name, gpus=1, gpu_type="A100-80G"):
    return {"metadata": {"name": name},
            "spec": {"gpus": gpus, "gpuType": gpu_type, "image": "x"},
            "status": {"phase": "Succeeded", "startTime": "2026-03-01T00:00:00Z",
                       "completionTime": "2026-03-01T02:00:00Z"}}


def urec(name, tenant, cost):
    return {"metadata": {"name": name}, "spec": {"tenant": tenant, "job": name, "sku": "a100", "gpuType": "A100-80G",
                                                 "gpus": 1, "start": "2026-03-01T00:00:00Z", "gpuHours": 2,
                                                 "cost": cost, "currency": "USD", "final": True}}


@pytest.fixture
def gw(env, fake_k8s, monkeypatch, keys):  # noqa: F811
    c, main, idp = env
    monkeypatch.setattr(main.deps, "k8s_custom", fake_k8s)
    main.limiter.enabled = False
    for t in ("alpha", "beta"):
        fake_k8s.add("gryviatenants", {"metadata": {"name": t}, "spec": {}})
    fake_k8s.add("gryviaaijobs", job("a-job"), "tenant-alpha")
    fake_k8s.add("gryviaaijobs", job("b-job", gpus=4), "tenant-beta")
    fake_k8s.add("gryviaaijobs", job("default-job"), "default")
    fake_k8s.add("gryviaquotas", {"metadata": {"name": "q-alpha"}, "spec": {"team": "alpha", "namespaces": ["tenant-alpha"]}})
    fake_k8s.add("gryviaquotas", {"metadata": {"name": "q-beta"}, "spec": {"team": "beta", "namespaces": ["tenant-beta", "x"]}})
    fake_k8s.add("gryviagpunodes", {"metadata": {"name": "n1"}, "spec": {"nodeName": "n1"}})
    fake_k8s.add("gryviagpuskus", {"metadata": {"name": "a100"}, "spec": {"gpuType": "A100-80G", "hourlyRate": 10}})

    class G:
        client, main_, k8s = c, main, fake_k8s

        def tenant_token(self, **over):
            return keys[0].sign(claims(**over))

        def get(self, path, token=None, **kw):
            return c.get(path, headers={"Authorization": f"Bearer {token or API_KEY}"}, **kw)

        def call(self, method, path, token=None, **kw):
            return c.request(method, path, headers={"Authorization": f"Bearer {token or API_KEY}"}, **kw)

    return G()


def alpha(g):
    return g.tenant_token(org="alpha")


def beta(g):
    return g.tenant_token(org="beta")


# -- identity ----------------------------------------------------------------


def test_api_key_and_session_are_admin(gw):
    body = gw.get("/api/auth/me").json()
    assert body["role"] == "admin" and body["tenant"] is None and body["tenantNamespaces"] is None
    session = gw.main_.issue_session_token("admin")["token"]
    assert gw.get("/api/auth/me", session).json()["role"] == "admin"


def test_oidc_user_is_tenant_with_its_namespace(gw):
    body = gw.get("/api/auth/me", alpha(gw)).json()
    assert body["role"] == "tenant" and body["tenant"] == "alpha"
    assert body["tenantNamespaces"] == ["tenant-alpha"]


def test_claim_may_name_the_namespace_or_be_a_group(gw):
    assert gw.get("/api/auth/me", gw.tenant_token(org="tenant-beta")).json()["tenantNamespaces"] == ["tenant-beta"]
    body = gw.get("/api/auth/me", gw.tenant_token(org=None, groups=["nope", "beta", "alpha"])).json()
    assert body["tenants"] == ["alpha", "beta"] and body["tenantNamespaces"] == ["tenant-alpha", "tenant-beta"]


def test_unknown_tenant_claim_is_403(gw):
    for tok in (gw.tenant_token(org="ghost"), gw.tenant_token(org="default"), gw.tenant_token(org=None),
                gw.tenant_token(org=None, groups=["kube-system"])):
        r = gw.get("/api/jobs", tok)
        assert r.status_code == 403 and "tenant" in r.json()["detail"]


def test_tenant_cannot_name_an_arbitrary_namespace(gw):
    # A token claiming another namespace (not a tenant) never reaches that namespace's jobs.
    assert gw.get("/api/jobs", gw.tenant_token(org="default")).status_code == 403


def test_oidc_admin_group_is_admin(gw, monkeypatch):
    monkeypatch.setattr(gw.main_, "OIDC_ADMIN_GROUPS", {"gryvia-admins"})
    tok = gw.tenant_token(org=None, groups=["gryvia-admins"])
    assert gw.get("/api/auth/me", tok).json()["role"] == "admin"
    assert gw.get("/api/nodes", tok).status_code == 200
    # ... and a non-admin group is not
    assert gw.get("/api/nodes", gw.tenant_token(org="alpha", groups=["other"])).status_code == 403


def test_admin_groups_default_to_nobody(gw):
    assert gw.get("/api/nodes", gw.tenant_token(org="alpha", groups=["gryvia-admins", "admin"])).status_code == 403


def test_legacy_namespaces_only_without_tenant_objects(gw, monkeypatch):
    monkeypatch.setattr(gw.main_, "OIDC_LEGACY_NAMESPACES", True)
    assert gw.get("/api/jobs", gw.tenant_token(org="ghost")).status_code == 403  # tenants exist -> strict
    for key in [k for k in gw.k8s.store if k[0] == "gryviatenants"]:
        del gw.k8s.store[key]
    gw.main_.tenancy.clear_cache()
    body = gw.get("/api/auth/me", gw.tenant_token(org="tenant-alpha")).json()
    assert body["role"] == "tenant" and body["tenantNamespaces"] == ["tenant-alpha"]
    monkeypatch.setattr(gw.main_, "OIDC_LEGACY_NAMESPACES", False)
    assert gw.get("/api/jobs", gw.tenant_token(org="tenant-alpha")).status_code == 403


def test_tenant_directory_is_cached_briefly(gw):
    calls = []
    real = gw.k8s.list_cluster_custom_object

    def counting(group, version, plural, **kw):
        calls.append(plural)
        return real(group, version, plural, **kw)

    gw.k8s.list_cluster_custom_object = counting
    tok = alpha(gw)
    gw.get("/api/auth/me", tok)
    gw.get("/api/auth/me", tok)
    assert calls.count("gryviatenants") == 1


def test_tenant_directory_failure_is_503_not_open(gw):
    def boom(*a, **kw):
        raise RuntimeError("apiserver down")

    gw.k8s.list_cluster_custom_object = boom
    assert gw.get("/api/jobs", alpha(gw)).status_code == 503


# -- admin-only routes -------------------------------------------------------

ADMIN_GET = [
    "/api/nodes", "/api/nodes/n1", "/api/nodes/health", "/api/metrics/gpu",
    "/api/network/flows", "/api/network/policies", "/api/network/insights", "/api/network/graph",
    "/api/network/anomalies", "/api/network/costs", "/api/network/traces", "/api/network/traces/t1",
    "/api/security/alerts", "/api/security/policies",
    "/api/ai/training/insight", "/api/ai/training/nccl", "/api/gpu/memory",
]


@pytest.mark.parametrize("path", ADMIN_GET)
def test_tenant_is_blocked_on_admin_get_routes(gw, path):
    assert gw.get(path, alpha(gw)).status_code == 403
    assert gw.get(path).status_code != 403  # admin gets past the guard


def test_tenant_is_blocked_on_admin_writes(gw):
    t = alpha(gw)
    posts = [
        ("/api/skus", {"name": "x", "gpuType": "L40", "hourlyRate": 1}),
        ("/api/tenants", {"name": "x"}),
        ("/api/network/policies", {"sourceService": "a", "destinationService": "b", "port": 80, "protocol": "TCP",
                                   "action": "allow", "intent": "x"}),
        ("/api/network/traces", {"targetService": "a", "namespace": "n", "duration": "5m", "captureLevel": "L4"}),
        ("/api/network/policies/p/apply", None),
        ("/api/security/policies", {"name": "x", "targetNamespaces": [], "detectionRules": []}),
    ]
    for path, body in posts:
        assert gw.call("POST", path, t, json=body).status_code == 403, path
    assert gw.call("PUT", "/api/skus/a100", t, json={"gpuType": "A", "hourlyRate": 0}).status_code == 403
    assert gw.call("DELETE", "/api/skus/a100", t).status_code == 403
    assert gw.call("DELETE", "/api/tenants/alpha", t).status_code == 403
    assert ("gryviagpuskus", None, "a100") in gw.k8s.store


def test_unauthenticated_is_401_on_admin_routes(gw):
    assert gw.client.get("/api/nodes").status_code == 401
    assert gw.client.get("/api/network/flows").status_code == 401


# -- tenant filtering --------------------------------------------------------


def test_jobs_are_isolated(gw):
    names = lambda r: sorted(j["metadata"]["name"] for j in r.json()["items"])  # noqa: E731
    assert names(gw.get("/api/jobs", alpha(gw))) == ["a-job"]
    assert names(gw.get("/api/jobs", beta(gw))) == ["b-job"]
    assert names(gw.get("/api/jobs")) == ["default-job"]  # admin: the gateway's job namespace as before
    assert gw.get("/api/jobs/a-job", alpha(gw)).status_code == 200
    assert gw.get("/api/jobs/b-job", alpha(gw)).status_code == 404
    assert gw.get("/api/jobs/default-job", alpha(gw)).status_code == 404
    for sub in ("pods", "logs", "events"):
        assert gw.get(f"/api/jobs/b-job/{sub}", alpha(gw)).status_code == 404


def test_tenant_cannot_delete_another_tenants_job(gw):
    assert gw.call("DELETE", "/api/jobs/b-job", alpha(gw)).status_code == 404
    assert ("gryviaaijobs", "tenant-beta", "b-job") in gw.k8s.store
    assert gw.call("DELETE", "/api/jobs/a-job", alpha(gw)).status_code == 200
    assert ("gryviaaijobs", "tenant-alpha", "a-job") not in gw.k8s.store


def test_tenant_job_is_created_in_its_own_namespace(gw):
    body = {"apiVersion": "gryvia.io/v1alpha1", "kind": "GryviaAIJob",
            "metadata": {"name": "new", "namespace": "tenant-beta"}, "spec": {"image": "i", "gpus": 1}}
    r = gw.call("POST", "/api/jobs", alpha(gw), json=body)
    assert r.status_code == 200
    assert ("gryviaaijobs", "tenant-alpha", "new") in gw.k8s.store
    assert ("gryviaaijobs", "tenant-beta", "new") not in gw.k8s.store


def test_quotas_are_isolated(gw):
    teams = lambda r: [q["metadata"]["name"] for q in r.json()["items"]]  # noqa: E731
    assert teams(gw.get("/api/quotas", alpha(gw))) == ["q-alpha"]
    assert teams(gw.get("/api/quotas", beta(gw))) == ["q-beta"]
    assert sorted(teams(gw.get("/api/quotas"))) == ["q-alpha", "q-beta"]
    assert gw.get("/api/quotas/q-beta", alpha(gw)).status_code == 404
    assert gw.get("/api/quotas/q-alpha", alpha(gw)).status_code == 200
    assert gw.get("/api/quotas/q-beta").status_code == 200
    assert gw.get("/api/quotas/nope", alpha(gw)).status_code == 404


def test_quota_usage_is_isolated(gw):
    assert [q["team"] for q in gw.get("/api/quota/usage", alpha(gw)).json()["quotas"]] == ["alpha"]
    r = gw.get("/api/quota/usage")
    assert sorted(q["team"] for q in r.json()["quotas"]) == ["alpha", "beta"]


def test_costs_are_isolated_and_priced_from_the_catalog(gw):
    # a-job: 2 GPU-hours on 1 GPU at the SKU rate 10 = 20; b-job: 4 GPUs * 2h * 10 = 80
    ra, rb = gw.get("/api/metrics/costs", alpha(gw)).json(), gw.get("/api/metrics/costs", beta(gw)).json()
    assert [t["team"] for t in ra["byTeam"]] == ["alpha"] and ra["totalCost"] == 20.0
    assert [t["team"] for t in rb["byTeam"]] == ["beta"] and rb["totalCost"] == 80.0
    assert ra["source"] == "jobs"


def test_costs_fall_back_to_builtin_prices_without_skus(gw):
    del gw.k8s.store[("gryviagpuskus", None, "a100")]
    assert gw.get("/api/metrics/costs", alpha(gw)).json()["totalCost"] == 8.0  # 2h * 4.00


def test_costs_prefer_usage_records(gw):
    gw.k8s.add("gryviausagerecords", urec("r-a", "alpha", 5.5), "tenant-alpha")
    gw.k8s.add("gryviausagerecords", urec("r-b", "beta", 99), "tenant-beta")
    ra = gw.get("/api/metrics/costs", alpha(gw)).json()
    assert ra["source"] == "usage-records" and ra["totalCost"] == 5.5 and [t["team"] for t in ra["byTeam"]] == ["alpha"]
    assert ra["byGPUType"] == [{"type": "A100-80G", "cost": 5.5, "hours": 2.0}]
    assert gw.get("/api/metrics/costs").json()["totalCost"] == 104.5


def test_job_metrics_and_cluster_stats_are_scoped(gw):
    assert gw.get("/api/metrics/jobs", alpha(gw)).json()["totalJobs"] == 1
    assert gw.get("/api/metrics/jobs", beta(gw)).json()["totalJobs"] == 1
    s = gw.get("/api/cluster/stats", alpha(gw)).json()
    assert s["totalJobs"] == 1 and s["totalNodes"] == 0 and s["totalGPUs"] == 0
    assert gw.get("/api/cluster/stats").json()["totalNodes"] == 1


def test_usage_is_isolated_end_to_end(gw):
    gw.k8s.add("gryviausagerecords", urec("r-a", "alpha", 5), "tenant-alpha")
    gw.k8s.add("gryviausagerecords", urec("r-b", "beta", 7), "tenant-beta")
    ra = gw.get("/api/usage?tenant=beta", alpha(gw)).json()
    assert [i["key"] for i in ra["items"]] == ["alpha"] and ra["totals"]["cost"] == 5
    assert gw.get("/api/usage").json()["totals"]["cost"] == 12
    assert "r-b" not in gw.get("/api/usage/export?tenant=beta", alpha(gw)).text


def test_workspaces_and_catalog_via_gateway(gw):
    gw.k8s.add("gryviaworkspaces", {"metadata": {"name": "ws-b"}, "spec": {"type": "jupyter"}}, "tenant-beta")
    assert gw.get("/api/workspaces", alpha(gw)).json()["items"] == []
    assert gw.get("/api/workspaces/ws-b", alpha(gw)).status_code == 404
    assert [i["metadata"]["name"] for i in gw.get("/api/skus", alpha(gw)).json()["items"]] == ["a100"]
    me = gw.get("/api/tenants", alpha(gw)).json()["items"]
    assert [t["metadata"]["name"] for t in me] == ["alpha"]
