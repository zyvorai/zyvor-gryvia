# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""LLM gateway routes: API keys, published models and token usage.

Keys are Secrets in the gateway's key namespace (chart value llmGateway.keyNamespace), labelled
gryvia.io/llm-key=true, holding only the sha256 of the key; the key itself is returned once, on create. A key
belongs to one namespace: a tenant creates, sees and revokes keys of its own namespaces only, and a key reaches the
models of its namespace plus the shared ones (annotation gryvia.io/llm-shared=true).

POST /api/llm/chat (the dashboard's playground) forwards a chat completion to the LLM gateway with the caller's LLM
key from the X-LLM-Key header, so the gateway's key scoping, token quotas and metering apply. The key is neither
stored nor logged. A rejected key answers 400, not 401, so the dashboard session is not ended.
"""
import hashlib
import re
import secrets
from collections import OrderedDict
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Literal, Optional

import httpx
from fastapi import APIRouter, Depends, Header, HTTPException, Query, Request
from kubernetes.client.exceptions import ApiException
from pydantic import BaseModel, ConfigDict, Field

from . import sse
from .common import Deps, http_error, list_items, run
from .uiutil import NAME_PATTERN
from .usage import fetch_records, parse_bound

KEY_LABEL = "gryvia.io/llm-key"
TENANT_LABEL = "gryvia.io/tenant"
NAME_LABEL = "gryvia.io/llm-key-name"
NAMESPACE_LABEL = "gryvia.io/llm-key-namespace"
PREFIX_ANNOTATION = "gryvia.io/llm-key-prefix"
DESCRIPTION_ANNOTATION = "gryvia.io/description"
MODEL_ANNOTATION = "gryvia.io/llm-model"
SERVED_ANNOTATION = "gryvia.io/llm-served-model"
SHARED_ANNOTATION = "gryvia.io/llm-shared"
PRICE_IN_ANNOTATION = "gryvia.io/llm-price-input-per-1m"
PRICE_OUT_ANNOTATION = "gryvia.io/llm-price-output-per-1m"
_MODEL_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$")
_DNS = r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"
_KEY_RE = re.compile(r"^gk-[A-Za-z0-9_-]{20,128}$")
CHAT_TIMEOUT_SECONDS = 300.0


class CreateKey(BaseModel):
    model_config = ConfigDict(extra="forbid")
    name: str = Field(min_length=1, max_length=63, pattern=NAME_PATTERN)
    namespace: str = Field(default="", max_length=63, pattern=r"^(" + _DNS[1:-1] + r")?$")
    description: str = Field(default="", max_length=256)


class PlaygroundMessage(BaseModel):
    model_config = ConfigDict(extra="forbid")
    role: Literal["system", "user", "assistant"]
    content: str = Field(max_length=32000)


class PlaygroundChat(BaseModel):
    model_config = ConfigDict(extra="forbid")
    model: str = Field(min_length=1, max_length=63, pattern=_MODEL_RE.pattern)
    messages: List[PlaygroundMessage] = Field(min_length=1, max_length=100)
    temperature: Optional[float] = Field(default=None, ge=0, le=2)
    max_tokens: Optional[int] = Field(default=None, ge=1, le=32768)
    stream: bool = False


def gateway_error(status: int, data: Any) -> HTTPException:
    err = (data or {}).get("error") if isinstance(data, dict) else None
    msg = err.get("message") if isinstance(err, dict) else str(err or "")
    if status == 401:
        return HTTPException(status_code=400, detail=f"the LLM gateway rejected the key: {msg}"[:500])
    code = status if status in (400, 404, 413, 429, 503) else 502
    return HTTPException(status_code=code, detail=f"LLM gateway answered {status}: {msg}"[:500])


def hash_key(raw: str) -> str:
    return hashlib.sha256(raw.encode()).hexdigest()


def secret_name(namespace: str, name: str) -> str:
    return f"{namespace}.{name}"


def key_to_ui(secret: Any) -> Dict[str, Any]:
    md = secret.metadata
    labels, ann = md.labels or {}, md.annotations or {}
    created = md.creation_timestamp
    return {
        "id": md.name,
        "name": labels.get(NAME_LABEL, md.name),
        "namespace": labels.get(NAMESPACE_LABEL, ""),
        "tenant": labels.get(TENANT_LABEL, ""),
        "prefix": ann.get(PREFIX_ANNOTATION, ""),
        "description": ann.get(DESCRIPTION_ANNOTATION, ""),
        "created": created.isoformat() if hasattr(created, "isoformat") else created,
    }


def model_to_ui(obj: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    md, st = obj.get("metadata") or {}, obj.get("status") or {}
    ann = md.get("annotations") or {}
    model = (ann.get(MODEL_ANNOTATION) or "").strip()
    if not _MODEL_RE.match(model):
        return None

    def price(key: str) -> Optional[float]:
        try:
            v = float(ann.get(key, ""))
        except ValueError:
            return None
        return v if v >= 0 else None

    return {
        "model": model,
        "namespace": md.get("namespace", ""),
        "service": md.get("name", ""),
        "servedModel": ann.get(SERVED_ANNOTATION) or model,
        "shared": ann.get(SHARED_ANNOTATION) == "true",
        "ready": bool(st.get("endpoint")),
        "phase": st.get("phase") or "",
        "priceInputPer1M": price(PRICE_IN_ANNOTATION),
        "priceOutputPer1M": price(PRICE_OUT_ANNOTATION),
    }


def _int(v: Any) -> int:
    return int(v) if isinstance(v, (int, float)) and not isinstance(v, bool) else 0


def summarize(records: List[Dict[str, Any]], group_by: str) -> Dict[str, Any]:
    """Token records grouped by model, tenant, namespace or day (UTC), plus totals."""
    groups: "OrderedDict[str, Dict[str, Any]]" = OrderedDict()
    total: Dict[str, Any] = {"inputTokens": 0, "outputTokens": 0, "cost": 0.0}
    currency = ""
    for r in sorted(records, key=lambda r: ((r.get("spec") or {}).get("start") or "")):
        spec, md = r.get("spec") or {}, r.get("metadata") or {}
        key = {
            "model": spec.get("model") or "",
            "tenant": spec.get("tenant") or "",
            "namespace": md.get("namespace") or "",
            "day": (spec.get("start") or "")[:10],
        }[group_by]
        g = groups.setdefault(key, {group_by: key, "inputTokens": 0, "outputTokens": 0, "cost": 0.0})
        tin, tout = _int(spec.get("inputTokens")), _int(spec.get("outputTokens"))
        cost = float(spec.get("cost") or 0)
        g["inputTokens"] += tin
        g["outputTokens"] += tout
        g["cost"] = round(g["cost"] + cost, 6)
        total["inputTokens"] += tin
        total["outputTokens"] += tout
        total["cost"] = round(total["cost"] + cost, 6)
        currency = currency or spec.get("currency") or ""
    return {"groupBy": group_by, "items": list(groups.values()), "total": total, "currency": currency or "USD"}


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    def key_namespace() -> str:
        if not deps.llm_key_namespace:
            raise HTTPException(status_code=503, detail="The LLM gateway is not enabled (chart value llmGateway.enabled)")
        return deps.llm_key_namespace

    def allowed_namespaces(request: Request) -> Optional[List[str]]:
        if getattr(request.state, "role", None) == "admin":
            return None
        return list(getattr(request.state, "tenant_namespaces", None) or [deps.job_namespace])

    def tenant_for(request: Request, namespace: str) -> str:
        tenant = getattr(request.state, "tenant", None)
        if tenant and getattr(request.state, "role", None) != "admin":
            return tenant
        return namespace[len("tenant-"):] if namespace.startswith("tenant-") else namespace

    async def list_keys(request: Request) -> List[Any]:
        ns = key_namespace()
        try:
            res = await run(deps.k8s_core.list_namespaced_secret, namespace=ns, label_selector=f"{KEY_LABEL}=true")
        except Exception as exc:  # noqa: BLE001
            raise http_error(exc, "list LLM keys") from exc
        allowed = allowed_namespaces(request)
        return [s for s in res.items
                if allowed is None or (s.metadata.labels or {}).get(NAMESPACE_LABEL) in allowed]

    @router.get("/api/llm-keys")
    @deps.limiter.limit("30/minute")
    async def get_keys(request: Request, _=Depends(deps.verify_auth)):
        return {"items": sorted((key_to_ui(s) for s in await list_keys(request)), key=lambda k: (k["namespace"], k["name"]))}

    @router.post("/api/llm-keys", status_code=201)
    @deps.limiter.limit("10/minute")
    async def create_key(request: Request, body: CreateKey, _=Depends(deps.verify_auth)):
        kns = key_namespace()
        allowed = allowed_namespaces(request)
        ns = body.namespace
        if allowed is not None:
            if ns and ns not in allowed:
                raise HTTPException(status_code=403, detail=f"namespace {ns} is not one of yours")
            ns = ns or allowed[0]
        ns = ns or deps.job_namespace
        raw = "gk-" + secrets.token_urlsafe(32)
        tenant = tenant_for(request, ns)
        name = secret_name(ns, body.name)
        secret = {
            "apiVersion": "v1",
            "kind": "Secret",
            "type": "Opaque",
            "metadata": {
                "name": name,
                "namespace": kns,
                "labels": {KEY_LABEL: "true", TENANT_LABEL: tenant[:63], NAME_LABEL: body.name, NAMESPACE_LABEL: ns},
                "annotations": {PREFIX_ANNOTATION: raw[:10], **({DESCRIPTION_ANNOTATION: body.description} if body.description else {})},
            },
            "stringData": {"hash": hash_key(raw), "namespace": ns},
        }
        try:
            await run(deps.k8s_core.create_namespaced_secret, namespace=kns, body=secret)
        except ApiException as exc:
            if exc.status == 409:
                raise HTTPException(status_code=409, detail=f"key {body.name} already exists in {ns}") from exc
            raise http_error(exc, "create LLM key") from exc
        return {"id": name, "name": body.name, "namespace": ns, "tenant": tenant, "prefix": raw[:10], "key": raw,
                "gatewayURL": deps.llm_gateway_url or "",
                "note": "Store the key now: it is not shown again."}

    @router.delete("/api/llm-keys/{name}")
    @deps.limiter.limit("10/minute")
    async def delete_key(request: Request, name: str, namespace: str = Query("", max_length=63),
                         _=Depends(deps.verify_auth)):
        kns = key_namespace()
        keys = await list_keys(request)
        match = [s for s in keys if s.metadata.name == name or (
            (s.metadata.labels or {}).get(NAME_LABEL) == name
            and (not namespace or (s.metadata.labels or {}).get(NAMESPACE_LABEL) == namespace))]
        if not match:
            raise HTTPException(status_code=404, detail=f"LLM key {name} not found")
        if len(match) > 1:
            raise HTTPException(status_code=409, detail=f"key {name} exists in several namespaces; pass ?namespace=")
        sid = match[0].metadata.name
        try:
            await run(deps.k8s_core.delete_namespaced_secret, name=sid, namespace=kns)
        except Exception as exc:  # noqa: BLE001
            raise http_error(exc, f"delete LLM key {name}") from exc
        return {"status": "deleted", "id": sid}

    @router.get("/api/llm/models")
    @deps.limiter.limit("30/minute")
    async def get_models(request: Request, _=Depends(deps.verify_auth)):
        allowed = allowed_namespaces(request)
        items = []
        for obj in await list_items(deps, "gryviainferenceservices"):
            m = model_to_ui(obj)
            if m and (allowed is None or m["namespace"] in allowed or m["shared"]):
                items.append(m)
        items.sort(key=lambda m: (m["model"], m["namespace"]))
        return {"items": items, "gatewayURL": deps.llm_gateway_url or "", "enabled": bool(deps.llm_key_namespace)}

    @router.get("/api/llm/usage")
    @deps.limiter.limit("30/minute")
    async def get_usage(request: Request, _=Depends(deps.verify_auth),
                        group_by: Literal["model", "tenant", "namespace", "day"] = Query("model", alias="groupBy"),
                        start: Optional[str] = Query(None, alias="from"), end: Optional[str] = Query(None, alias="to")):
        lo, hi = parse_bound(start, "from", False), parse_bound(end, "to", True)
        if lo is None and hi is None:
            lo = datetime.now(timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0) - timedelta(days=29)
        allowed = allowed_namespaces(request)
        try:
            records = await fetch_records(deps.k8s_custom, allowed)
        except Exception as exc:  # noqa: BLE001
            raise http_error(exc, "list token usage") from exc
        tokens = []
        for r in records:
            spec = r.get("spec") or {}
            if spec.get("kind") != "tokens":
                continue
            try:
                s = datetime.fromisoformat((spec.get("start") or "").replace("Z", "+00:00"))
            except ValueError:
                continue
            if (lo and s < lo) or (hi and s >= hi):
                continue
            tokens.append(r)
        out = summarize(tokens, group_by)
        out["quotas"] = await quotas_today(allowed, records)
        return out

    async def quotas_today(allowed: Optional[List[str]], records: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
        try:
            quotas = await list_items(deps, "gryviaquotas")
        except HTTPException:
            return []
        today = datetime.now(timezone.utc).strftime("%Y-%m-%d")
        used_by_ns: Dict[str, int] = {}
        for r in records:
            spec = r.get("spec") or {}
            if spec.get("kind") == "tokens" and (spec.get("start") or "").startswith(today):
                ns = (r.get("metadata") or {}).get("namespace", "")
                used_by_ns[ns] = used_by_ns.get(ns, 0) + _int(spec.get("inputTokens")) + _int(spec.get("outputTokens"))
        out = []
        for q in quotas:
            spec = q.get("spec") or {}
            per = spec.get("tokensPerDay")
            nss = spec.get("namespaces") or []
            if not per or (allowed is not None and not set(nss) & set(allowed)):
                continue
            out.append({"name": (q.get("metadata") or {}).get("name", ""), "namespaces": nss, "tokensPerDay": per,
                        "usedToday": sum(used_by_ns.get(n, 0) for n in nss)})
        return out

    @router.post("/api/llm/chat")
    @deps.limiter.limit("30/minute")
    async def chat(request: Request, body: PlaygroundChat, _=Depends(deps.verify_auth),
                   key: str = Header("", alias="X-LLM-Key", max_length=200)):
        if not deps.llm_key_namespace or not deps.llm_gateway_url:
            raise HTTPException(status_code=503,
                                detail="The LLM gateway is not enabled (chart value llmGateway.enabled)")
        if not _KEY_RE.match(key):
            raise HTTPException(status_code=400, detail="send an LLM key (gk-...) in the X-LLM-Key header")
        url = f"{deps.llm_gateway_url}/v1/chat/completions"
        payload = body.model_dump(exclude_none=True)
        if not body.stream:
            payload.pop("stream")
        headers = {"Authorization": f"Bearer {key}"}
        if body.stream:
            return await sse.relay(url, payload, timeout=CHAT_TIMEOUT_SECONDS, label="the LLM gateway",
                                   error=gateway_error, headers=headers, transport=deps.llm_transport)
        try:
            async with httpx.AsyncClient(timeout=CHAT_TIMEOUT_SECONDS, follow_redirects=False, trust_env=False,
                                         transport=deps.llm_transport) as client:
                resp = await client.post(url, json=payload, headers=headers)
        except httpx.HTTPError as exc:
            raise HTTPException(status_code=502, detail=f"the LLM gateway is unreachable: {type(exc).__name__}")
        try:
            data = resp.json()
        except ValueError:
            data = {"error": {"message": resp.text[:300]}}
        if resp.status_code != 200:
            raise gateway_error(resp.status_code, data)
        return data

    return router
