"""Opt-in two-person operations. ConfigMap resourceVersion is the distributed state lock.

No arbitrary patches, commands or external URLs are accepted. A target's UID/resourceVersion
and the old field are captured at proposal time. Executions and rollbacks fail on intervening
changes. A crash after claiming an operation leaves Executing/RollingBack for manual review;
there is no automatic replay of an uncertain write.
"""

from datetime import datetime, timedelta, timezone
import json
import uuid
from typing import Optional

from fastapi import APIRouter, Depends, HTTPException, Path, Query, Request
from kubernetes.client.exceptions import ApiException
from pydantic import Field, model_validator

from intelligence.models import Input, Name
from .common import Deps, GROUP, VERSION, get_item, require_admin, run
from .tenancy import is_admin
from .intelligence_http import BoundedRoute

LABEL = "gryvia.io/intelligence-action"
ANNOTATION = "gryvia.io/intelligence-action"
PREFIX = "gryvia-action-"
ID_PATTERN = r"^[a-f0-9]{32}$"
TARGETS = {
    "inference-replicas": ("gryviainferenceservices", "replicas"),
    "quota-max-gpus": ("gryviaquotas", "maxGPUs"),
    "node-quarantine": (None, "unschedulable"),
}


class Proposal(Input):
    kind: str = Field(pattern="^(inference-replicas|quota-max-gpus|node-quarantine)$")
    name: Name
    namespace: Name
    value: int = Field(ge=0, le=1048576, strict=True)
    reason: str = Field(min_length=10, max_length=2000)
    evidence: str = Field(min_length=10, max_length=6000)

    @model_validator(mode="after")
    def range(self):
        if self.kind == "inference-replicas" and not 1 <= self.value <= 100:
            raise ValueError("replicas must be 1-100")
        if self.kind == "node-quarantine" and self.value != 1:
            raise ValueError("quarantine only allows cordon=true; uncordon is deliberately not supported")
        return self


def actor(request):
    claims = getattr(request.state, "user_claims", None) or {}
    if getattr(request.state, "auth_method", None) != "oidc" or not claims.get("sub") or not claims.get("iss"):
        raise HTTPException(status_code=403, detail="Operations require a named OIDC identity (issuer and subject)")
    return {"issuer": claims["iss"], "subject": claims["sub"]}


def enabled(deps):
    if not deps.intelligence_actions:
        raise HTTPException(
            status_code=503, detail="Approved operations are disabled; enable apiGateway.intelligenceActions"
        )


def serialize(cm):
    if (cm.metadata.labels or {}).get(LABEL) != "true":
        raise HTTPException(status_code=409, detail="operation record has unexpected ownership")
    try:
        data = json.loads(cm.data["operation.json"])
    except (KeyError, TypeError, ValueError) as exc:
        raise HTTPException(status_code=409, detail="operation record is unreadable") from exc
    return data


async def load(deps, ident):
    try:
        cm = await run(deps.k8s_core.read_namespaced_config_map, PREFIX + ident, deps.job_namespace)
        return cm, serialize(cm)
    except ApiException as exc:
        raise HTTPException(
            status_code=404 if exc.status == 404 else 503, detail="operation record unavailable"
        ) from exc


async def save(deps, cm, data):
    body = {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {
            "name": cm.metadata.name,
            "namespace": deps.job_namespace,
            "resourceVersion": cm.metadata.resource_version,
            "labels": {LABEL: "true"},
        },
        "data": {"operation.json": json.dumps(data)},
    }
    try:
        return await run(deps.k8s_core.replace_namespaced_config_map, cm.metadata.name, deps.job_namespace, body)
    except ApiException as exc:
        raise HTTPException(
            status_code=409 if exc.status == 409 else 503,
            detail="operation record changed or could not be persisted; reread before retrying",
        ) from exc


def node_dict(node):
    return {
        "metadata": {
            "name": node.metadata.name,
            "uid": node.metadata.uid,
            "resourceVersion": node.metadata.resource_version,
            "annotations": node.metadata.annotations or {},
        },
        "spec": {"unschedulable": bool(node.spec.unschedulable)},
    }


async def target(deps, proposal):
    if proposal["kind"] == "node-quarantine":
        try:
            return node_dict(await run(deps.k8s_core.read_node, proposal["name"]))
        except ApiException as exc:
            raise HTTPException(status_code=404 if exc.status == 404 else 503, detail="node unavailable") from exc
    plural = TARGETS[proposal["kind"]][0]
    return await get_item(deps, plural, proposal["name"], namespace=proposal["namespace"])


def value_of(obj, kind):
    spec = obj.get("spec") or {}
    if kind == "quota-max-gpus":
        return (spec.get("gpuQuota") or {}).get("maxGPUs")
    if kind == "node-quarantine":
        return bool(spec.get("unschedulable", False))
    return spec.get("replicas")


def check_identity(obj):
    md = obj.get("metadata") or {}
    if not md.get("uid") or not md.get("resourceVersion"):
        raise HTTPException(status_code=409, detail="target UID or resourceVersion unavailable")
    return md


def managed_elsewhere(obj, kind):
    if kind != "inference-replicas":
        return
    spec = obj.get("spec") or {}
    if (spec.get("autoscaling") or {}).get("enabled") or (spec.get("scaleToZero") or {}).get("enabled"):
        raise HTTPException(
            status_code=409, detail="replicas are managed by HPA or scale-to-zero; disable it before proposing"
        )


async def propose(request, deps, body):
    enabled(deps)
    if not is_admin(request):
        raise HTTPException(status_code=403, detail="operation proposals require a provider administrator")
    who = actor(request)
    obj = await target(deps, body.model_dump())
    md = check_identity(obj)
    managed_elsewhere(obj, body.kind)
    old = value_of(obj, body.kind)
    desired = True if body.kind == "node-quarantine" else body.value
    if old == desired:
        raise HTTPException(status_code=409, detail="target already has the requested value")
    ident = uuid.uuid4().hex
    now = datetime.now(timezone.utc)
    data = {
        "id": ident,
        "state": "Proposed",
        "proposal": body.model_dump(),
        "proposedBy": who,
        "createdAt": now.isoformat(),
        "expiresAt": (now + timedelta(minutes=15)).isoformat(),
        "targetUID": md["uid"],
        "targetVersion": md["resourceVersion"],
        "previousValue": old,
    }
    cm = {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": PREFIX + ident, "namespace": deps.job_namespace, "labels": {LABEL: "true"}},
        "data": {"operation.json": json.dumps(data)},
    }
    try:
        await run(deps.k8s_core.create_namespaced_config_map, deps.job_namespace, cm)
    except ApiException as exc:
        raise HTTPException(status_code=503, detail="could not persist operation proposal") from exc
    return data


async def apply(deps, data, obj, rollback=False):
    proposal = data["proposal"]
    md = check_identity(obj)
    if md["uid"] != data["targetUID"]:
        raise HTTPException(status_code=409, detail="target was replaced")
    expected = data.get("appliedVersion") if rollback else data["targetVersion"]
    if md["resourceVersion"] != expected:
        raise HTTPException(status_code=409, detail="target changed since proposal or execution")
    managed_elsewhere(obj, proposal["kind"])
    desired = (
        data["previousValue"] if rollback else (True if proposal["kind"] == "node-quarantine" else proposal["value"])
    )
    key = TARGETS[proposal["kind"]][1]
    patch = {
        "metadata": {
            "resourceVersion": expected,
            "annotations": {ANNOTATION: data["id"] + ("-rollback" if rollback else "")},
        },
        "spec": {"gpuQuota": {key: desired}} if proposal["kind"] == "quota-max-gpus" else {key: desired},
    }
    try:
        if proposal["kind"] == "node-quarantine":
            result = node_dict(await run(deps.k8s_core.patch_node, proposal["name"], patch))
        else:
            result = await run(
                (
                    deps.k8s_custom.patch_namespaced_custom_object
                    if proposal["kind"] == "inference-replicas"
                    else deps.k8s_custom.patch_cluster_custom_object
                ),
                **({"namespace": proposal["namespace"]} if proposal["kind"] == "inference-replicas" else {}),
                group=GROUP,
                version=VERSION,
                plural=TARGETS[proposal["kind"]][0],
                name=proposal["name"],
                body=patch,
            )
    except ApiException as exc:
        if exc.status == 409:
            raise HTTPException(
                status_code=409, detail="target write rejected by resourceVersion precondition"
            ) from exc
        # A transport error or 5xx can happen after the API accepted the patch. Never mark it retryable.
        raise HTTPException(
            status_code=503, detail="target write outcome uncertain; inspect target and operation"
        ) from exc
    return result


def unexpired(data):
    if datetime.fromisoformat(data["expiresAt"]) <= datetime.now(timezone.utc):
        raise HTTPException(status_code=409, detail="operation expired; create a fresh proposal")


TERMINAL = {"Rejected", "Applied", "RolledBack"}
EXPORT_LIMIT = 10000
SWEEP_INTERVAL = timedelta(hours=1)


def finished_at(data):
    """When a record stopped changing, or None while it can still change (never for uncertain writes)."""
    state = data.get("state")
    if state in TERMINAL:
        stamp = data.get("completedAt") or data.get("reviewedAt") or data.get("createdAt")
    elif state == "Proposed" or state == "Approved":
        stamp = data.get("expiresAt")
    else:
        return None
    try:
        return datetime.fromisoformat(stamp)
    except (TypeError, ValueError):
        return None


def sweepable(data, now, retention_days):
    if retention_days <= 0:
        return False
    done = finished_at(data)
    if done is None or (data.get("state") in ("Proposed", "Approved") and done > now):
        return False
    return now - done >= timedelta(days=retention_days)


async def list_page(deps, limit, token=None):
    kwargs = {"label_selector": f"{LABEL}=true", "limit": limit}
    if token:
        kwargs["_continue"] = token
    try:
        result = await run(deps.k8s_core.list_namespaced_config_map, deps.job_namespace, **kwargs)
    except ApiException as exc:
        raise HTTPException(
            status_code=410 if exc.status == 410 else 503, detail="operation records unavailable"
        ) from exc
    return result.items, getattr(result.metadata, "_continue", None) or None


async def all_records(deps, cap=EXPORT_LIMIT):
    out, token = [], None
    while True:
        items, token = await list_page(deps, min(500, cap), token)
        out.extend(items)
        if not token or len(out) >= cap:
            return out[:cap], bool(token)


async def sweep(deps, now=None):
    """Delete terminal or expired records older than the retention window. Uncertain writes are kept."""
    now = now or datetime.now(timezone.utc)
    deleted = 0
    records, _ = await all_records(deps)
    for cm in records:
        try:
            data = serialize(cm)
        except HTTPException:
            continue
        if not sweepable(data, now, deps.intelligence_retention_days):
            continue
        try:
            await run(
                deps.k8s_core.delete_namespaced_config_map,
                cm.metadata.name,
                deps.job_namespace,
                body={"preconditions": {"resourceVersion": cm.metadata.resource_version}},
            )
            deleted += 1
        except ApiException as exc:
            if exc.status not in (404, 409):
                raise HTTPException(status_code=503, detail="could not delete an operation record") from exc
    return deleted


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter(route_class=BoundedRoute, dependencies=[Depends(deps.verify_auth), Depends(require_admin)])
    OperationID = Path(pattern=ID_PATTERN, min_length=32, max_length=32)
    last_sweep = {"at": None}

    @router.post("/api/intelligence/actions", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create(request: Request, body: Proposal):
        return await propose(request, deps, body)

    @router.get("/api/intelligence/actions")
    @deps.limiter.limit("30/minute")
    async def actions(
        request: Request,
        limit: int = Query(100, ge=1, le=500),
        cont: Optional[str] = Query(None, alias="continue", max_length=4096),
    ):
        enabled(deps)
        actor(request)
        now = datetime.now(timezone.utc)
        if deps.intelligence_retention_days > 0 and (
            last_sweep["at"] is None or now - last_sweep["at"] >= SWEEP_INTERVAL
        ):
            last_sweep["at"] = now
            try:
                await sweep(deps, now)
            except HTTPException:
                pass  # retention is best effort; listing must not fail because of it
        items, token = await list_page(deps, limit, cont)
        return {"items": [serialize(cm) for cm in items], "continue": token, "truncated": bool(token)}

    @router.get("/api/intelligence/actions/export")
    @deps.limiter.limit("5/minute")
    async def export(request: Request):
        enabled(deps)
        actor(request)
        records, truncated = await all_records(deps)
        items = []
        for cm in records:
            try:
                items.append(serialize(cm))
            except HTTPException:
                continue
        return {"items": items, "truncated": truncated, "exportedAt": datetime.now(timezone.utc).isoformat()}

    @router.post("/api/intelligence/actions/sweep")
    @deps.limiter.limit("5/minute")
    async def sweep_now(request: Request):
        enabled(deps)
        actor(request)
        if deps.intelligence_retention_days <= 0:
            raise HTTPException(
                status_code=409, detail="retention is disabled (apiGateway.intelligenceRetentionDays=0)"
            )
        deleted = await sweep(deps)
        last_sweep["at"] = datetime.now(timezone.utc)
        return {"deleted": deleted, "retentionDays": deps.intelligence_retention_days}

    @router.get("/api/intelligence/actions/{ident}")
    @deps.limiter.limit("30/minute")
    async def get(request: Request, ident: str = OperationID):
        enabled(deps)
        actor(request)
        _, data = await load(deps, ident)
        return data

    @router.post("/api/intelligence/actions/{ident}/{transition}")
    @deps.limiter.limit("10/minute")
    async def transition(
        request: Request,
        ident: str = OperationID,
        transition: str = Path(pattern="^(approve|reject|execute|rollback)$"),
    ):
        enabled(deps)
        who = actor(request)
        cm, data = await load(deps, ident)
        if who == data["proposedBy"]:
            raise HTTPException(
                status_code=403, detail="proposer cannot approve, execute or roll back their own operation"
            )
        now = datetime.now(timezone.utc).isoformat()
        if transition in ("approve", "reject"):
            if data["state"] != "Proposed":
                raise HTTPException(status_code=409, detail="operation is no longer proposed")
            unexpired(data)
            data.update(state="Approved" if transition == "approve" else "Rejected", reviewedBy=who, reviewedAt=now)
            await save(deps, cm, data)
            return data
        rollback = transition == "rollback"
        if data["state"] != ("Applied" if rollback else "Approved"):
            raise HTTPException(status_code=409, detail="invalid operation state; uncertain writes are never replayed")
        if not rollback:
            unexpired(data)
        obj = await target(deps, data["proposal"])
        # Check everything before acquiring execution ownership.
        md = check_identity(obj)
        if md["uid"] != data["targetUID"] or md["resourceVersion"] != (
            data.get("appliedVersion") if rollback else data["targetVersion"]
        ):
            raise HTTPException(status_code=409, detail="target changed; create a fresh proposal")
        managed_elsewhere(obj, data["proposal"]["kind"])
        data.update(state="RollingBack" if rollback else "Executing", executedBy=who, executionStartedAt=now)
        claimed = await save(deps, cm, data)
        result = await apply(deps, data, obj, rollback)
        data.update(
            state="RolledBack" if rollback else "Applied",
            completedAt=datetime.now(timezone.utc).isoformat(),
            appliedVersion=check_identity(result)["resourceVersion"],
            desiredValueObserved=value_of(result, data["proposal"]["kind"]),
        )
        # If this persistence fails, record remains uncertain and cannot be executed again.
        await save(deps, claimed, data)
        return data

    return router
