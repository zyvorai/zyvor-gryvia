# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

NS = "default"


def seed(fake_k8s, ns=NS):
    fake_k8s.add("gryviadatasets", {"metadata": {"name": "ds"}, "spec": {"namespace": ns},
                                    "status": {"state": "Ready", "pvcName": "ds-pvc"}})
    fake_k8s.add("gryviaaijobs", {"metadata": {"name": "train"},
                                  "spec": {"volumes": [{"name": "d", "persistentVolumeClaim": {"claimName": "ds-pvc"}}]},
                                  "status": {"phase": "Succeeded"}}, namespace=ns)
    fake_k8s.add("gryviamodelregistries", {"metadata": {"name": "bert"},
                                           "spec": {"version": "v1", "stage": "dev", "source": {"jobRef": "train"}}},
                 namespace=ns)
    fake_k8s.add("gryviainferenceservices", {"metadata": {"name": "svc"}, "spec": {"modelRef": "bert"}}, namespace=ns)


def test_empty(make_client):
    assert make_client("lineage").get("/api/lineage").json() == {"nodes": [], "edges": []}


def test_chain(make_client, fake_k8s):
    seed(fake_k8s)
    g = make_client("lineage").get("/api/lineage").json()
    assert {n["id"] for n in g["nodes"]} == {"dataset/default/ds", "job/default/train",
                                              "model/default/bert", "service/default/svc"}
    assert g["edges"] == [{"from": "dataset/default/ds", "to": "job/default/train"},
                          {"from": "job/default/train", "to": "model/default/bert"},
                          {"from": "model/default/bert", "to": "service/default/svc"}]


def test_tenant_sees_only_own_namespace(make_client, fake_k8s):
    seed(fake_k8s, ns="other")
    g = make_client("lineage", role="tenant", tenants=["alpha"]).get("/api/lineage").json()
    assert g == {"nodes": [], "edges": []}
