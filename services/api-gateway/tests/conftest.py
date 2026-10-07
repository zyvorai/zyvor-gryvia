"""Test helpers: an in-memory fake of the Kubernetes CustomObjectsApi and a way to
mount one router module with fake dependencies (no cluster, no main.py import)."""
import copy
import os
import sys
import types
from typing import Any, Dict, Optional

import pytest
from fastapi import FastAPI, Request
from fastapi.testclient import TestClient
from kubernetes.client.exceptions import ApiException

sys.path.insert(0, os.path.dirname(os.path.dirname(__file__)))

from routers.common import Deps  # noqa: E402


class FakeCustomObjects:
    """Stores objects keyed by (plural, namespace-or-None, name)."""

    def __init__(self) -> None:
        self.store: Dict[tuple, Dict[str, Any]] = {}

    # -- seeding helper -------------------------------------------------
    def add(self, plural: str, obj: Dict[str, Any], namespace: Optional[str] = None) -> None:
        obj = copy.deepcopy(obj)
        obj.setdefault("metadata", {}).setdefault("creationTimestamp", "2026-01-01T00:00:00Z")
        if namespace:
            obj["metadata"]["namespace"] = namespace
        self.store[(plural, namespace, obj["metadata"]["name"])] = obj

    # -- CustomObjectsApi surface used by routers.common ----------------
    def list_cluster_custom_object(self, group, version, plural, **kw):
        return {"items": [copy.deepcopy(o) for (p, _, _), o in self.store.items() if p == plural]}

    def list_namespaced_custom_object(self, group, version, namespace, plural, **kw):
        return {"items": [copy.deepcopy(o) for (p, ns, _), o in self.store.items() if p == plural and ns == namespace]}

    def get_cluster_custom_object(self, group, version, plural, name, **kw):
        return self._get(plural, None, name)

    def get_namespaced_custom_object(self, group, version, namespace, plural, name, **kw):
        return self._get(plural, namespace, name)

    def create_cluster_custom_object(self, group, version, plural, body, **kw):
        return self._create(plural, None, body)

    def create_namespaced_custom_object(self, group, version, namespace, plural, body, **kw):
        if kw.get("dry_run") == "All":
            if (plural, namespace, body["metadata"]["name"]) in self.store:
                raise ApiException(status=409, reason="AlreadyExists")
            return copy.deepcopy(body)
        return self._create(plural, namespace, body)

    def delete_cluster_custom_object(self, group, version, plural, name, **kw):
        return self._delete(plural, None, name)

    def delete_namespaced_custom_object(self, group, version, namespace, plural, name, **kw):
        return self._delete(plural, namespace, name)

    def patch_cluster_custom_object(self, group, version, plural, name, body, **kw):
        return self._patch(plural, None, name, body)

    def patch_namespaced_custom_object(self, group, version, namespace, plural, name, body, **kw):
        return self._patch(plural, namespace, name, body)

    # -- internals ------------------------------------------------------
    def _get(self, plural, ns, name):
        try:
            return copy.deepcopy(self.store[(plural, ns, name)])
        except KeyError:
            raise ApiException(status=404, reason="Not Found")

    def _create(self, plural, ns, body):
        key = (plural, ns, body["metadata"]["name"])
        if key in self.store:
            raise ApiException(status=409, reason="AlreadyExists")
        obj = copy.deepcopy(body)
        obj["metadata"].setdefault("creationTimestamp", "2026-01-01T00:00:00Z")
        self.store[key] = obj
        return copy.deepcopy(obj)

    def _delete(self, plural, ns, name):
        if (plural, ns, name) not in self.store:
            raise ApiException(status=404, reason="Not Found")
        del self.store[(plural, ns, name)]
        return {"status": "Success"}

    def _patch(self, plural, ns, name, body):
        obj = self.store.get((plural, ns, name))
        if obj is None:
            raise ApiException(status=404, reason="Not Found")

        def merge(dst, src):
            for k, v in src.items():
                if isinstance(v, dict) and isinstance(dst.get(k), dict):
                    merge(dst[k], v)
                else:
                    dst[k] = copy.deepcopy(v)

        merge(obj, body)
        return copy.deepcopy(obj)


class NoopLimiter:
    def limit(self, *_a, **_kw):
        return lambda fn: fn


class FakeCore:
    """Minimal CoreV1Api stand-in; tests can set attributes as needed."""

    def list_node(self, *a, **kw):
        class R:
            items = []
        return R()

    # -- pods / logs / events (in-memory; seed via add_pod / add_event / set_log) --
    def _ensure(self):
        if not hasattr(self, "pods"):
            self.pods, self.logs, self.events = [], {}, []
            self.log_calls = []

    def add_pod(self, name, namespace="default", job=None, phase="Running", start_time="2026-01-01T00:00:00Z",
                containers=None, node="node-1", pod_ip="10.0.0.1", message=None):
        """containers: list of (name, state, restarts, reason, ready)."""
        self._ensure()
        ns = types.SimpleNamespace
        statuses = []
        for cname, state, restarts, reason, ready in (containers or [("main", "running", 0, None, True)]):
            statuses.append(ns(name=cname, ready=ready, restart_count=restarts, state=ns(
                running=ns(reason=reason) if state == "running" else None,
                waiting=ns(reason=reason) if state == "waiting" else None,
                terminated=ns(reason=reason) if state == "terminated" else None)))
        pod = ns(metadata=ns(name=name, namespace=namespace, creation_timestamp=start_time,
                             labels={"gryvia.io/job": job} if job else {}),
                 spec=ns(node_name=node, containers=[ns(name=c.name) for c in statuses]),
                 status=ns(phase=phase, pod_ip=pod_ip, start_time=start_time, message=message,
                           container_statuses=statuses))
        self.pods.append(pod)
        return pod

    def set_log(self, pod, text_or_exc):
        self._ensure()
        self.logs[pod] = text_or_exc

    def add_event(self, kind, name, namespace="default", type="Normal", reason="Scheduled", message="m",
                  count=1, first="2026-01-01T00:00:00Z", last="2026-01-01T00:00:00Z"):
        self._ensure()
        ns = types.SimpleNamespace
        self.events.append(ns(involved_object=ns(kind=kind, name=name), metadata=ns(namespace=namespace),
                              type=type, reason=reason, message=message, count=count,
                              first_timestamp=first, last_timestamp=last))

    def list_namespaced_pod(self, namespace, label_selector=None, **kw):
        self._ensure()
        key, _, val = (label_selector or "").partition("=")
        items = [p for p in self.pods if p.metadata.namespace == namespace
                 and (not key or p.metadata.labels.get(key) == val)]
        return types.SimpleNamespace(items=items)

    def read_namespaced_pod_log(self, name, namespace, tail_lines=None, timestamps=False, **kw):
        self._ensure()
        self.log_calls.append(dict(name=name, namespace=namespace, tail_lines=tail_lines,
                                   timestamps=timestamps, **kw))
        res = self.logs.get(name, "")
        if isinstance(res, Exception):
            raise res
        lines = res.split("\n")
        if tail_lines is not None:
            lines = lines[-tail_lines:]
        return "\n".join(lines)

    # -- secrets (dict bodies in, SimpleNamespace objects out, like the real client) --
    def _secrets(self):
        if not hasattr(self, "secrets"):
            self.secrets = {}
        return self.secrets

    def create_namespaced_secret(self, namespace, body, **kw):
        store = self._secrets()
        md = body["metadata"]
        if (namespace, md["name"]) in store:
            raise ApiException(status=409, reason="AlreadyExists")
        store[(namespace, md["name"])] = copy.deepcopy(body)
        return body

    def list_namespaced_secret(self, namespace, label_selector=None, **kw):
        ns = types.SimpleNamespace
        want = dict(p.split("=", 1) for p in (label_selector or "").split(",") if "=" in p)
        items = []
        for (sns, _), body in self._secrets().items():
            md = body["metadata"]
            if sns != namespace or any((md.get("labels") or {}).get(k) != v for k, v in want.items()):
                continue
            items.append(ns(metadata=ns(name=md["name"], namespace=sns, labels=md.get("labels"),
                                        annotations=md.get("annotations"), creation_timestamp="2026-01-01T00:00:00Z"),
                            data=body.get("stringData")))
        return ns(items=items)

    def delete_namespaced_secret(self, name, namespace, **kw):
        if (namespace, name) not in self._secrets():
            raise ApiException(status=404, reason="Not Found")
        del self.secrets[(namespace, name)]

    def list_namespaced_event(self, namespace, field_selector=None, **kw):
        self._ensure()
        sel = dict(part.split("=", 1) for part in (field_selector or "").split(",") if "=" in part)
        items = [e for e in self.events if e.metadata.namespace == namespace
                 and all({"involvedObject.name": e.involved_object.name,
                          "involvedObject.kind": e.involved_object.kind}.get(k) == v for k, v in sel.items())]
        return types.SimpleNamespace(items=items)


async def _allow(request: Request):  # verify_auth stand-in: the provider admin
    request.state.role = "admin"
    request.state.tenant = None
    request.state.tenants = []
    request.state.tenant_namespaces = None


@pytest.fixture(autouse=True)
def _fresh_tenant_cache():
    from routers import tenancy

    tenancy.clear_cache()
    yield
    tenancy.clear_cache()


@pytest.fixture
def fake_k8s() -> FakeCustomObjects:
    return FakeCustomObjects()


def _stub_auth(role: str, tenants):
    """verify_auth stand-in for a given role; tenant callers get namespaces tenant-<name>."""
    async def auth(request: Request):
        request.state.role = role
        request.state.tenants = list(tenants)
        request.state.tenant = tenants[0] if tenants else None
        request.state.tenant_namespaces = [f"tenant-{t}" for t in tenants] or None
    return auth


@pytest.fixture
def make_client(fake_k8s):
    """make_client("network") -> TestClient with routers/network.py mounted (admin caller by default).
    make_client("usage", role="tenant", tenants=["alpha"]) mounts it for a tenant user."""
    import importlib

    def _make(module: str, role: str = "admin", tenants=(), **deps_kw) -> TestClient:
        deps = Deps(verify_auth=_stub_auth(role, tenants), k8s_custom=fake_k8s, k8s_core=FakeCore(),
                    limiter=NoopLimiter(), job_namespace="default", **deps_kw)
        app = FastAPI()
        app.include_router(importlib.import_module(f"routers.{module}").build_router(deps))
        return TestClient(app)

    return _make
