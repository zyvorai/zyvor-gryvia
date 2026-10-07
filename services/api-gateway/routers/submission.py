"""Shared, fail-closed job submission admission checks."""

import json
import re
from copy import deepcopy

from fastapi import HTTPException
from kubernetes.client.exceptions import ApiException

from .common import run

MAX_BODY = 512 * 1024
NAME = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
UNKNOWN_FIELD = re.compile(r'unknown field "([^"]{1,200})"')
WORD = re.compile(r"^[A-Za-z]{1,60}$")
WARNING = re.compile(r'\d{3} \S+ "((?:[^"\\]|\\.){1,1000})"')


def admission_warnings(headers):
    """Warning headers from the dry run, e.g. the job webhook's preflight findings."""
    if not headers:
        return []
    values = headers.getlist("Warning") if hasattr(headers, "getlist") else [headers.get("Warning") or ""]
    found = [m.replace('\\"', '"')[:300] for v in values for m in WARNING.findall(v or "")]
    return list(dict.fromkeys(found))[:10]


def rejection_reasons(exc):
    """Field paths and reason codes from a Kubernetes Status, never its messages or values."""
    try:
        status = json.loads(exc.body or "{}")
    except (TypeError, ValueError, RecursionError):
        return ""
    if not isinstance(status, dict):
        return ""
    parts = []
    details = status.get("details")
    causes = details.get("causes") if isinstance(details, dict) else None
    for cause in causes if isinstance(causes, list) else []:
        if isinstance(cause, dict) and isinstance(cause.get("field"), str):
            reason = cause.get("reason")
            parts.append(
                f"{cause['field'][:200]}: {reason if isinstance(reason, str) and WORD.match(reason) else 'Invalid'}"
            )
    message = status.get("message")
    if isinstance(message, str):
        parts += [f"{field}: unknown field" for field in UNKNOWN_FIELD.findall(message)]
    parts = list(dict.fromkeys(parts))[:10]
    reason = status.get("reason")
    head = reason if isinstance(reason, str) and WORD.match(reason) else ""
    return "; ".join(([head] if head else []) + parts)


async def read_job(request, namespace):
    raw = bytearray()
    async for chunk in request.stream():
        raw.extend(chunk)
        if len(raw) > MAX_BODY:
            raise HTTPException(413, "Job request exceeds 512 KiB")
    try:
        body = json.loads(raw)
    except (ValueError, RecursionError):
        raise HTTPException(400, "Request body must be valid JSON") from None
    return normalize_job(body, namespace)


def normalize_job(body, namespace):
    if not isinstance(body, dict):
        raise HTTPException(400, "Request body must be a JSON object")
    if set(body) - {"apiVersion", "kind", "metadata", "spec"}:
        raise HTTPException(
            400, "Only apiVersion, kind, metadata and spec may be submitted"
        )
    if (
        body.get("apiVersion") != "gryvia.io/v1alpha1"
        or body.get("kind") != "GryviaAIJob"
    ):
        raise HTTPException(400, "Expected gryvia.io/v1alpha1 GryviaAIJob")
    md, spec = body.get("metadata"), body.get("spec")
    if not isinstance(md, dict) or not isinstance(spec, dict):
        raise HTTPException(400, "metadata and spec must be JSON objects")
    name = md.get("name")
    if not isinstance(name, str) or len(name) > 63 or not NAME.fullmatch(name):
        raise HTTPException(400, "metadata.name must be a DNS label of 1–63 characters")
    if set(md) - {"name", "namespace", "labels", "annotations"}:
        raise HTTPException(
            400, "Server metadata, owner references and finalizers cannot be submitted"
        )
    for field in ("labels", "annotations"):
        value = md.get(field, {})
        if not isinstance(value, dict) or any(
            not isinstance(k, str) or not isinstance(v, str) for k, v in value.items()
        ):
            raise HTTPException(400, f"metadata.{field} must map strings to strings")
    if not isinstance(spec.get("image"), str) or not spec["image"].strip():
        raise HTTPException(400, "spec.image is required")
    count = spec.get("gpus")
    if (
        isinstance(count, bool)
        or not isinstance(count, int)
        or not 1 <= count <= 1048576
    ):
        raise HTTPException(400, "spec.gpus must be an integer from 1 to 1048576")
    if "gpuCount" in spec:
        raise HTTPException(400, "Use spec.gpus; gpuCount is not a CRD field")
    body = deepcopy(body)
    body["metadata"]["namespace"] = namespace
    return body


async def admit_job(k8s, body, namespace, enforced=True):
    """Use the real CRD schema/admission chain; never persist a preview."""
    create = getattr(k8s, "create_namespaced_custom_object_with_http_info", None)
    try:
        result = await run(
            create or k8s.create_namespaced_custom_object,
            group="gryvia.io",
            version="v1alpha1",
            namespace=namespace,
            plural="gryviaaijobs",
            body=deepcopy(body),
            dry_run="All",
            field_validation="Strict",
        )
    except ApiException as exc:
        # Kubernetes error bodies may echo sensitive env values. Never return them.
        status = exc.status if exc.status in (400, 403, 404, 409, 422, 429) else 503
        reasons = rejection_reasons(exc)
        raise HTTPException(
            status,
            f"Job admission preflight failed ({reasons}); no job was created"
            if reasons
            else "Job admission preflight failed; no job was created",
        ) from None
    except Exception:
        raise HTTPException(
            503, "Job admission preflight unavailable; no job was created"
        ) from None
    warnings = admission_warnings(result[2]) if create and isinstance(result, tuple) and len(result) > 2 else []
    return {
        "admitted": True,
        "persisted": False,
        "namespace": namespace,
        "name": body["metadata"]["name"],
        "source": "kubernetes-dry-run",
        "checks": ["gateway-input", "crd-schema", "kubernetes-admission"],
        "enforcedOnCreate": enforced,
        "warnings": warnings,
        "limitations": [
            "No GPU capacity is reserved",
            "Scheduling and controller quota checks remain pending",
            "Admission can change before submission; every submission checks again"
            if enforced
            else "Submission does not repeat this check (apiGateway.submissionAdmission.enforce is off)",
        ],
    }
