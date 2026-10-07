"""Opt-in two-person operations. ConfigMap resourceVersion is the distributed state lock.

No arbitrary patches, commands or external URLs are accepted. A target's UID/resourceVersion
and the old field are captured at proposal time. Executions and rollbacks fail on intervening
changes. A crash after claiming an operation leaves Executing/RollingBack for manual review;
there is no automatic replay of an uncertain write.
"""

from datetime import datetime, timedelta, timezone
import json
import uuid

from fastapi import APIRouter, Depends, HTTPException, Path, Request
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


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter(route_class=BoundedRoute, dependencies=[Depends(deps.verify_auth), Depends(require_admin)])
    OperationID = Path(pattern=ID_PATTERN, min_length=32, max_length=32)

    @router.post("/api/intelligence/actions", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create(request: Request, body: Proposal):
        return await propose(request, deps, body)

    @router.get("/api/intelligence/actions")
    @deps.limiter.limit("30/minute")
    async def actions(request: Request):
        enabled(deps)
        actor(request)
        result = await run(
            deps.k8s_core.list_namespaced_config_map, deps.job_namespace, label_selector=f"{LABEL}=true", limit=100
        )
        return {
            "items": [serialize(cm) for cm in result.items],
            "truncated": bool(getattr(result.metadata, "_continue", None)),
        }

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
