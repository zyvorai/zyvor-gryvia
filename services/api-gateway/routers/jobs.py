"""Job runtime detail routes: pods, logs and events of a GryviaAIJob for the dashboard."""
import re
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional, Tuple

from fastapi import APIRouter, Depends, HTTPException, Path, Query, Request
from kubernetes.client.exceptions import ApiException

from .common import Deps, http_error, patch_item, run
from .uiutil import NAME_MAX, NAME_PATTERN, find_one, prune

PLURAL = "gryviaaijobs"
KIND = "GryviaAIJob"
JOB_LABEL = "gryvia.io/job"
MAX_EVENTS = 100
MAX_EVENT_PODS = 20
MAX_LINE = 4096
_CTRL_RE = re.compile(r"[\x00-\x08\x0a-\x1f\x7f]")  # all control chars except \t
TERMINAL_PHASES = {"Succeeded", "Failed", "Cancelled", "Rejected"}
KUEUE_QUEUE_LABEL = "kueue.x-k8s.io/queue-name"


def resize_bounds(job: Dict[str, Any]) -> Tuple[int, int]:
    """(minNodes, nodes) of an elastic job that can be resized now; HTTPException 409 otherwise."""
    dist = (job.get("spec") or {}).get("distributed") or {}
    elastic = dist.get("elastic")
    if not dist.get("enabled") or not isinstance(elastic, dict):
        raise HTTPException(status_code=409, detail="only elastic jobs (spec.distributed.elastic) can be resized")
    phase = (job.get("status") or {}).get("phase")
    if phase in TERMINAL_PHASES:
        raise HTTPException(status_code=409, detail=f"job is {phase}")
    if ((job.get("metadata") or {}).get("labels") or {}).get(KUEUE_QUEUE_LABEL):
        raise HTTPException(status_code=409, detail="Kueue picks the size of a Kueue-managed job")
    return int(elastic.get("minNodes") or 1), int(dist.get("nodes") or 0)


def _ts(value: Any) -> Optional[str]:
    """RFC3339 string for a datetime or string timestamp; None if unknown."""
    if value is None or value == "":
        return None
    if isinstance(value, datetime):
        if value.tzinfo is None:
            value = value.replace(tzinfo=timezone.utc)
        return value.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    return str(value)


def _sort_key(value: Any) -> str:
    return _ts(value) or ""


def _pod_meta(pod: Any) -> Tuple[str, Any]:
    md = getattr(pod, "metadata", None)
    return getattr(md, "name", None) or "", getattr(md, "creation_timestamp", None)


def _container_state(cs: Any) -> Tuple[Optional[str], Optional[str]]:
    state = getattr(cs, "state", None)
    for key in ("running", "waiting", "terminated"):
        st = getattr(state, key, None) if state is not None else None
        if st is not None:
            return key, getattr(st, "reason", None) or None
    return None, None


def _pod_ui(pod: Any) -> Dict[str, Any]:
    name, _created = _pod_meta(pod)
    spec, st = getattr(pod, "spec", None), getattr(pod, "status", None)
    containers: List[Dict[str, Any]] = []
    restarts = 0
    for cs in (getattr(st, "container_statuses", None) or []):
        state, reason = _container_state(cs)
        count = getattr(cs, "restart_count", None) or 0
        restarts += count
        containers.append(prune({
            "name": getattr(cs, "name", None),
            "ready": getattr(cs, "ready", None),
            "state": state,
            "restartCount": count,
            "reason": reason,
        }))
    msg = getattr(st, "message", None)
    return prune({
        "name": name,
        "phase": getattr(st, "phase", None),
        "node": getattr(spec, "node_name", None) or None,
        "podIP": getattr(st, "pod_ip", None) or None,
        "startTime": _ts(getattr(st, "start_time", None)),
        "restarts": restarts,
        "message": msg if isinstance(msg, str) and msg else None,
        "containers": containers,
    })


def _clean_line(line: str) -> str:
    return _CTRL_RE.sub("", line)[:MAX_LINE]


def _event_ui(ev: Any) -> Dict[str, Any]:
    obj = getattr(ev, "involved_object", None)
    first = getattr(ev, "first_timestamp", None) or getattr(ev, "event_time", None)
    last = (getattr(ev, "last_timestamp", None) or getattr(ev, "event_time", None)
            or getattr(ev, "first_timestamp", None))
    kind, oname = getattr(obj, "kind", None), getattr(obj, "name", None)
    return prune({
        "type": getattr(ev, "type", None),
        "reason": getattr(ev, "reason", None),
        "message": getattr(ev, "message", None),
        "count": getattr(ev, "count", None) or 1,
        "firstSeen": _ts(first),
        "lastSeen": _ts(last),
        "object": f"{kind}/{oname}" if kind and oname else None,
    })


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()
    JobName = Path(min_length=1, max_length=NAME_MAX, pattern=NAME_PATTERN)

    async def _job_pods(request: Request, name: str) -> Tuple[str, List[Any]]:
        _job, ns = await find_one(request, deps, PLURAL, name)
        try:
            res = await run(deps.k8s_core.list_namespaced_pod, ns, label_selector=f"{JOB_LABEL}={name}")
        except Exception as exc:  # noqa: BLE001
            raise http_error(exc, f"list pods of job {name}") from exc
        return ns, list(getattr(res, "items", None) or [])

    @router.get("/api/jobs/{name}/pods")
    @deps.limiter.limit("60/minute")
    async def job_pods(request: Request, name: str = JobName, _=Depends(deps.verify_auth)):
        _ns, pods = await _job_pods(request, name)
        return {"items": [_pod_ui(p) for p in pods]}

    @router.post("/api/jobs/{name}/resize")
    @deps.limiter.limit("10/minute")
    async def resize_job(request: Request, name: str = JobName, _=Depends(deps.verify_auth)):
        """Set spec.distributed.elastic.desiredNodes; the ai-operator resizes the job's Indexed Job."""
        try:
            body = await request.json()
        except ValueError:
            raise HTTPException(status_code=400, detail="request body must be JSON")
        nodes = body.get("nodes") if isinstance(body, dict) else None
        if not isinstance(nodes, int) or isinstance(nodes, bool):
            raise HTTPException(status_code=400, detail="nodes must be an integer")
        job, ns = await find_one(request, deps, PLURAL, name)
        low, high = resize_bounds(job)
        if not low <= nodes <= high:
            raise HTTPException(status_code=400,
                                detail=f"nodes must be between minNodes ({low}) and distributed.nodes ({high})")
        patch = {"spec": {"distributed": {"elastic": {"desiredNodes": nodes}}}}
        updated = await patch_item(deps, PLURAL, name, patch, ns)
        current = ((updated.get("status") or {}).get("elastic") or {}).get("currentNodes")
        return {"name": name, "namespace": ns, "desiredNodes": nodes, "minNodes": low, "maxNodes": high,
                "currentNodes": current}

    @router.get("/api/jobs/{name}/logs")
    @deps.limiter.limit("60/minute")
    async def job_logs(request: Request, name: str = JobName,
                       pod: Optional[str] = Query(default=None, max_length=253),
                       tail: int = Query(default=200, ge=1, le=2000),
                       _=Depends(deps.verify_auth)):
        ns, pods = await _job_pods(request, name)
        chosen = None
        if pod is not None:
            chosen = next((p for p in pods if _pod_meta(p)[0] == pod), None)
            if chosen is None:
                raise HTTPException(status_code=404, detail=f"pod {pod}: Not Found")
        elif pods:
            def rank(p: Any) -> Tuple[int, str]:
                running = getattr(getattr(p, "status", None), "phase", None) == "Running"
                st = getattr(getattr(p, "status", None), "start_time", None) or _pod_meta(p)[1]
                return (1 if running else 0, _sort_key(st))
            chosen = max(pods, key=rank)
        if chosen is None:
            return {"pod": None, "container": None, "lines": [], "truncated": False,
                    "message": "no pods yet"}

        pod_name = _pod_meta(chosen)[0]
        cstatuses = getattr(getattr(chosen, "status", None), "container_statuses", None) or []
        cspecs = getattr(getattr(chosen, "spec", None), "containers", None) or []
        container = (getattr(cstatuses[0], "name", None) if cstatuses
                     else getattr(cspecs[0], "name", None) if cspecs else None)
        kwargs: Dict[str, Any] = {"tail_lines": tail + 1, "timestamps": True}
        if container:
            kwargs["container"] = container
        try:
            raw = await run(deps.k8s_core.read_namespaced_pod_log, pod_name, ns, **kwargs)
        except ApiException as exc:
            if exc.status == 400:
                return {"pod": pod_name, "container": container, "lines": [], "truncated": False,
                        "message": "container is not running yet"}
            raise http_error(exc, f"read logs of pod {pod_name}") from exc
        except Exception as exc:  # noqa: BLE001
            raise http_error(exc, f"read logs of pod {pod_name}") from exc

        # Split on \n only (str.splitlines also splits on \r, \x0b, \x1c... which would gryviaate lines).
        lines = raw.split("\n") if isinstance(raw, str) else []
        if lines and lines[-1] == "":
            lines.pop()
        truncated = len(lines) > tail
        if truncated:
            lines = lines[-tail:]
        return {"pod": pod_name, "container": container,
                "lines": [_clean_line(ln) for ln in lines], "truncated": truncated}

    @router.get("/api/jobs/{name}/events")
    @deps.limiter.limit("60/minute")
    async def job_events(request: Request, name: str = JobName, _=Depends(deps.verify_auth)):
        ns, pods = await _job_pods(request, name)
        targets = [(KIND, name)] + [("Pod", _pod_meta(p)[0]) for p in pods[:MAX_EVENT_PODS]]
        events: List[Dict[str, Any]] = []
        for kind, oname in targets:
            selector = f"involvedObject.name={oname},involvedObject.kind={kind}"
            try:
                res = await run(deps.k8s_core.list_namespaced_event, ns, field_selector=selector)
            except Exception as exc:  # noqa: BLE001
                raise http_error(exc, f"list events of {kind}/{oname}") from exc
            for ev in (getattr(res, "items", None) or []):
                obj = getattr(ev, "involved_object", None)
                # Defend against a server that ignores the selector.
                if getattr(obj, "name", oname) == oname and getattr(obj, "kind", kind) == kind:
                    events.append(_event_ui(ev))
        events.sort(key=lambda e: e.get("lastSeen") or "", reverse=True)
        return {"items": events[:MAX_EVENTS]}

    return router
