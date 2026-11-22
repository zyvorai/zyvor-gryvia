# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Cluster view of the per-node unified diagnosis, incident history and comparison.

Same trust model as routers/flight.py: the gateway signs one request per collector, applies tenant
scoping before anything leaves the gateway, and never lets a request parameter reach a collector URL
except as a validated, urlencoded query value.

Coverage is explicit. Expected nodes come from the pods of the job (label gryvia.io/job in the job's
namespace); a node that did not report is named in ``missingNodes``. When the pods cannot be listed the
expected count is the string "unknown", and the report is never ``complete``. Unavailable telemetry is
never treated as healthy: it is carried per node.
"""
import csv
import io
import json
import logging
import re
from datetime import datetime
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import urlencode

from fastapi import APIRouter, Depends, HTTPException, Request
from fastapi.responses import Response

from .common import Deps, run
from .flight import gather, token_usable, valid_name
from .uiutil import namespaces

logger = logging.getLogger(__name__)

SEVERITY = {"info": 1, "warning": 2, "critical": 3}
CONFIDENCE = {"low": 1, "medium": 2, "high": 3}
KINDS = {"network", "storage", "cpu", "memory", "gpu-comm", "straggler", "rdma", "exfil"}
MAX_FINDINGS_PER_NODE = 20
MAX_EVIDENCE = 50
MAX_TEXT = 500
MAX_INCIDENTS = 1000
NODE_LOCAL_COVERAGE = "cluster coverage is not evaluated"   # prefix of the collector's node-local reason
INCIDENT_ID = re.compile(r"^inc-[0-9a-f]{1,32}$")
SINCE = re.compile(r"^[0-9]{1,6}(ns|us|ms|s|m|h)$")
WINDOW = re.compile(r"^[0-9]{1,4}(m|h)$")


def text(value: Any, limit: int = MAX_TEXT) -> str:
    return value[:limit] if isinstance(value, str) else ""


def number(value: Any) -> Optional[float]:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    return float(value) if value == value and abs(value) != float("inf") else None


def all_answered(coverage: Dict[str, int]) -> bool:
    return coverage.get("total", 0) > 0 and coverage.get("reachable", 0) == coverage["total"]


def rank(table: Dict[str, int], value: Any) -> int:
    return table.get(value, 0) if isinstance(value, str) else 0


def by_rank(table: Dict[str, int], level: int) -> str:
    return next((name for name, value in table.items() if value == level), "low")


async def expected_nodes(deps: Deps, namespace: str, job: str) -> Tuple[Optional[List[str]], int]:
    """(node names of the job's live pods, pods not yet on a node); (None, 0) when the pods cannot be listed."""
    try:
        pods = await run(deps.k8s_core.list_namespaced_pod, namespace=namespace,
                         label_selector=f"gryvia.io/job={job}")
    except Exception:  # noqa: BLE001
        logger.warning("could not list the pods of the job", exc_info=True)
        return None, 0
    nodes, unscheduled = set(), 0
    for pod in getattr(pods, "items", []) or []:
        phase = getattr(getattr(pod, "status", None), "phase", "")
        if phase not in ("Running", "Pending"):
            continue
        node = getattr(getattr(pod, "spec", None), "node_name", None)
        if isinstance(node, str) and node:
            nodes.add(node)
        else:
            unscheduled += 1
    return sorted(nodes), unscheduled


def clean_evidence(node: str, raw: Any) -> List[Dict[str, Any]]:
    out = []
    for item in (raw if isinstance(raw, list) else [])[:MAX_EVIDENCE]:
        if not isinstance(item, dict):
            continue
        value = number(item.get("value"))
        if value is None:
            continue
        out.append({"node": node, "source": text(item.get("source"), 32), "metric": text(item.get("metric"), 64),
                    "value": value, "window": text(item.get("window"), 32)})
    return out


def strings(raw: Any, limit: int = 30) -> List[str]:
    return [text(x) for x in (raw if isinstance(raw, list) else [])[:limit] if isinstance(x, str)]


def merge_diagnosis(namespace: str, job: str, bodies: List[Dict[str, Any]], coverage: Dict[str, int],
                    expected: Optional[List[str]], unscheduled: int = 0) -> Dict[str, Any]:
    """Merge per-node diagnoses. Findings of one kind are combined (max severity, evidence kept per node);
    a kind seen on two or more nodes raises the confidence one level, since independent nodes agree."""
    reporting: Dict[str, Dict[str, Any]] = {}
    for body in bodies:
        node = body.get("node")
        if isinstance(node, str) and node and body.get("namespace") == namespace and body.get("job") == job:
            reporting[node] = body
    kinds: Dict[str, Dict[str, Any]] = {}
    unavailable, measured, per_node = [], {}, []
    probes_skipped, probes_attached, dropped_total = [], 0, 0
    dropped_by_node: Dict[str, int] = {}
    reasons: List[str] = []
    ratio = 1.0
    sampling_note = ""
    for node in sorted(reporting):
        body = reporting[node]
        findings = [f for f in (body.get("findings") if isinstance(body.get("findings"), list) else [])
                    if isinstance(f, dict)][:MAX_FINDINGS_PER_NODE]
        for f in findings:
            kind = f.get("kind")
            if kind not in KINDS or rank(SEVERITY, f.get("severity")) == 0:
                continue
            slot = kinds.setdefault(kind, {"kind": kind, "severity": 0, "confidence": 0, "nodes": [], "evidence": [],
                                           "notMeasured": [], "perNode": []})
            sev, conf = rank(SEVERITY, f.get("severity")), rank(CONFIDENCE, f.get("confidence")) or 1
            slot["severity"] = max(slot["severity"], sev)
            slot["confidence"] = max(slot["confidence"], conf)
            slot["nodes"].append(node)
            slot["evidence"].extend(clean_evidence(node, f.get("evidence")))
            for m in strings(f.get("whatWasNotMeasured")):
                if m not in slot["notMeasured"]:
                    slot["notMeasured"].append(m)
            slot["perNode"].append({"node": node, "severity": by_rank(SEVERITY, sev),
                                    "confidence": by_rank(CONFIDENCE, conf), "summary": text(f.get("summary"))})
        for u in (body.get("unavailable") if isinstance(body.get("unavailable"), list) else [])[:60]:
            if isinstance(u, dict) and isinstance(u.get("signal"), str):
                unavailable.append({"node": node, "signal": text(u["signal"], 96), "reason": text(u.get("reason"))})
        measured[node] = strings(body.get("measured"), 40)
        node_summary = text(body.get("summary"))
        per_node.append({"node": node, "summary": node_summary})
        mc = body.get("measurementCompleteness")
        mc = mc if isinstance(mc, dict) else {}
        attached = mc.get("probesAttached")
        probes_attached += attached if isinstance(attached, int) and not isinstance(attached, bool) else 0
        for p in (mc.get("probesSkipped") if isinstance(mc.get("probesSkipped"), list) else [])[:100]:
            if isinstance(p, dict):
                probes_skipped.append({"node": node, "object": text(p.get("object"), 64),
                                       "program": text(p.get("program"), 64), "reason": text(p.get("reason"))})
        dropped = mc.get("droppedEvents") if isinstance(mc.get("droppedEvents"), dict) else {}
        total = dropped.get("total")
        total = total if isinstance(total, int) and not isinstance(total, bool) and total >= 0 else 0
        dropped_total += total
        dropped_by_node[node] = total
        sampling = mc.get("sampling") if isinstance(mc.get("sampling"), dict) else {}
        r = number(sampling.get("ratio"))
        if r is None:
            ratio = min(ratio, 0.0)   # a node that does not say is not assumed to be unsampled
            reasons.append(f"{node}: sampling ratio not reported")
        else:
            ratio = min(ratio, r)
            sampling_note = sampling_note or text(sampling.get("note"))
        for reason in strings(mc.get("reasons"), 20):
            if not reason.startswith(NODE_LOCAL_COVERAGE):
                reasons.append(f"{node}: {reason}")
        if mc.get("complete") is not True and not mc.get("reasons"):
            reasons.append(f"{node}: node did not state why its report is incomplete")
    findings_out = []
    for slot in kinds.values():
        conf = slot["confidence"]
        if len(set(slot["nodes"])) >= 2 and conf < 3:
            conf += 1
        summaries = "; ".join(f"{p['node']}: {p['summary']}" for p in slot["perNode"] if p["summary"])
        findings_out.append({"kind": slot["kind"], "severity": by_rank(SEVERITY, slot["severity"]),
                             "confidence": by_rank(CONFIDENCE, conf), "nodes": sorted(set(slot["nodes"])),
                             "evidence": slot["evidence"][:MAX_EVIDENCE], "summary": summaries[:2000],
                             "perNode": slot["perNode"], "whatWasNotMeasured": slot["notMeasured"][:30]})
    findings_out.sort(key=lambda f: (-SEVERITY[f["severity"]], -CONFIDENCE[f["confidence"]], f["kind"]))

    expected_known = expected is not None
    missing = sorted(set(expected) - set(reporting)) if expected_known else []
    if not expected_known:
        reasons.insert(0, "expected nodes unknown: the gateway could not list the pods of the job")
    if missing:
        reasons.insert(0, "no diagnosis from node(s): " + ", ".join(missing)
                       + " (collector unreachable or absent there, or the node knows nothing of the job)")
    if expected_known and not expected and not reporting:
        reasons.insert(0, "the job has no scheduled pods")
    if unscheduled:
        reasons.append(f"{unscheduled} pod(s) of the job are not scheduled to a node yet")
    if coverage.get("total", 0) == 0:
        reasons.append("no collectors discovered")
    elif coverage.get("reachable", 0) < coverage.get("total", 0):
        reasons.append(f"{coverage['total'] - coverage['reachable']} of {coverage['total']} collectors did not answer")
    if not reporting:
        reasons.append("no node reported this job")
    complete = not reasons and expected_known and bool(expected) and set(expected) == set(reporting)
    completeness = {
        "probesAttached": probes_attached, "probesSkipped": probes_skipped[:200],
        "droppedEvents": {"total": dropped_total, "byNode": dropped_by_node},
        "sampling": {"ratio": ratio, "note": sampling_note},
        "nodesExpected": len(expected) if expected_known else "unknown",
        "nodesReporting": len(reporting), "missingNodes": missing,
        "complete": complete, "reasons": reasons[:100],
    }
    partial = not complete
    if findings_out:
        top = findings_out[0]
        summary = (f"{len(findings_out)} finding(s); most significant: {top['kind']} ({top['severity']}, "
                   f"{top['confidence']} confidence)")
        if partial:
            summary += "; PARTIAL VIEW, see measurementCompleteness"
    elif not reporting:
        summary = "no node reported this job; nothing was measured and no conclusion is possible"
    elif not any(measured.values()):
        summary = "nothing could be measured on the reporting nodes; unavailable is not healthy"
    else:
        summary = "no bottleneck detected in measured signals"
        if partial:
            of = f" of {len(expected)}" if expected_known else " (expected nodes unknown)"
            summary += f" on {len(reporting)}{of} node(s); this is not a statement about the whole job"
        elif unavailable:
            summary += f" ({len(unavailable)} signal(s) unavailable, which is not evidence of health)"
    return {
        "namespace": namespace, "job": job, "scope": "cluster; measured signals only",
        "coverage": {**coverage, "complete": all_answered(coverage)},
        "partial": partial, "summary": summary, "nodes": sorted(reporting), "nodeSummaries": per_node,
        "findings": findings_out, "unavailable": unavailable[:400], "measured": measured,
        "measurementCompleteness": completeness,
    }


def safe_cell(value: Any) -> str:
    """Neutralise CSV formula injection and control characters (mirrors collector incident.SafeCell)."""
    s = "".join(" " if (ord(c) < 0x20 and c not in "\n\t") or ord(c) == 0x7F else c for c in str(value))
    return "'" + s if s and s[0] in "=+-@\t\r" else s


CSV_HEADER = ["node", "id", "namespace", "job", "kind", "severity", "confidence", "start", "end", "open",
              "close_reason", "summary", "unavailable"]


def incidents_csv(incidents: List[Dict[str, Any]]) -> str:
    out = io.StringIO()
    writer = csv.writer(out, lineterminator="\n")
    writer.writerow(CSV_HEADER)
    for i in incidents:
        columns = ("node", "id", "namespace", "job", "kind", "severity", "confidence", "start", "end")
        writer.writerow([safe_cell(i.get(k, "")) for k in columns]
                        + [str(bool(i.get("open"))).lower()]
                        + [safe_cell(i.get("closeReason", "")), safe_cell(i.get("summary", "")),
                           safe_cell(",".join(i.get("unavailable") or []))])
    return out.getvalue()


def merge_incidents(namespace: str, job: str, bodies: List[Dict[str, Any]], coverage: Dict[str, int]) -> Dict[str, Any]:
    incidents, enabled, disabled = [], [], []
    for body in bodies:
        node = body["node"]
        if body.get("enabled") is not True:
            disabled.append(node)
            continue
        enabled.append(node)
        for item in (body.get("incidents") if isinstance(body.get("incidents"), list) else [])[:MAX_INCIDENTS]:
            if not isinstance(item, dict) or item.get("namespace") != namespace:
                continue   # never trust a collector to have applied the tenant scope
            if job and item.get("job") != job:
                continue
            fields = ("id", "namespace", "job", "kind", "severity", "confidence", "start", "end", "open",
                      "closeReason", "peak", "updatedAt")
            incidents.append({**{k: item.get(k) for k in fields},
                              "summary": text(item.get("summary")),
                              "evidence": clean_evidence(node, item.get("evidence")),
                              "unavailable": strings(item.get("unavailable"), 60), "node": node})
    incidents.sort(key=lambda i: str(i.get("start") or ""), reverse=True)
    truncated = len(incidents) > MAX_INCIDENTS
    complete = coverage.get("total", 0) > 0 and coverage.get("reachable", 0) == coverage["total"] and not disabled
    return {"namespace": namespace, "job": job or None, "incidents": incidents[:MAX_INCIDENTS], "truncated": truncated,
            "history": {"enabledNodes": sorted(enabled), "disabledNodes": sorted(disabled),
                        "note": "history exists only on nodes where -flight-store-dir is set; an empty list is not "
                                "proof that nothing happened on the other nodes"},
            "coverage": {**coverage, "complete": complete}}


def merge_compare(namespace: str, job: str, bodies: List[Dict[str, Any]], coverage: Dict[str, int]) -> Dict[str, Any]:
    nodes, disabled = [], []
    for body in bodies:
        if body.get("enabled") is not True:
            disabled.append(body["node"])
            continue
        comparison = body.get("comparison")
        if isinstance(comparison, dict) and isinstance(comparison.get("metrics"), list):
            nodes.append({"node": body["node"], "comparison": {
                "a": comparison.get("a"), "b": comparison.get("b"), "metrics": comparison["metrics"][:200],
                "notes": strings(comparison.get("notes"), 10)}})
    return {"namespace": namespace, "job": job, "nodes": sorted(nodes, key=lambda n: n["node"]),
            "history": {"disabledNodes": sorted(disabled)},
            "note": "metrics are node-local and are not averaged across nodes; "
                    "an incident id resolves only on the node that recorded it",
            "coverage": {**coverage, "complete": all_answered(coverage)}}


def valid_since(value: str) -> bool:
    if not value:
        return True
    if SINCE.fullmatch(value):
        return True
    try:
        datetime.fromisoformat(value.replace("Z", "+00:00"))
        return len(value) <= 40
    except ValueError:
        return False


def valid_spec(value: str) -> bool:
    if value == "now" or INCIDENT_ID.fullmatch(value):
        return True
    try:
        datetime.fromisoformat(value.replace("Z", "+00:00"))
        return len(value) <= 40 and ("T" in value)
    except ValueError:
        return False


def add_routes(router: APIRouter, deps: Deps) -> None:
    def scope(request: Request, job: str, namespace: str) -> str:
        allowed = namespaces(request, deps)
        admin = getattr(request.state, "role", None) == "admin"
        ns = namespace or (deps.job_namespace if admin else allowed[0])
        if (job and not valid_name(job)) or not valid_name(ns):
            raise HTTPException(status_code=400, detail="Invalid job or namespace")
        if not admin and ns not in allowed:
            raise HTTPException(status_code=403, detail="Namespace is not available to this user")
        if not token_usable(deps.flight_token):
            raise HTTPException(status_code=503, detail="Flight Recorder token is not configured")
        return ns

    def need_collectors(coverage: Dict[str, int]) -> None:
        if coverage["total"] == 0 or coverage["reachable"] == 0:
            raise HTTPException(status_code=503, detail="No Flight Recorder collector is reachable")

    @router.get("/api/flight/jobs/{job}/diagnosis")
    @deps.limiter.limit("30/minute")
    async def diagnosis(request: Request, job: str, namespace: str = "", _=Depends(deps.verify_auth)):
        ns = scope(request, job, namespace)
        path = "/api/v1/flight/diagnosis?" + urlencode({"namespace": ns, "job": job})
        bodies, coverage = await gather(deps, ns, job, path=path)
        need_collectors(coverage)
        expected, unscheduled = await expected_nodes(deps, ns, job)
        return merge_diagnosis(ns, job, bodies, coverage, expected, unscheduled)

    async def incidents_query(request: Request, job: str, namespace: str, since: str, limit: int):
        if not valid_since(since):
            raise HTTPException(status_code=400, detail="since must be an RFC3339 time or a duration such as 6h")
        ns = scope(request, job, namespace)
        query = {"namespace": ns}
        if job:
            query["job"] = job
        if since:
            query["since"] = since
        query["limit"] = str(limit)
        bodies, coverage = await gather(deps, ns, job, path="/api/v1/flight/incidents?" + urlencode(query))
        need_collectors(coverage)
        return merge_incidents(ns, job, bodies, coverage)

    @router.get("/api/flight/incidents")
    @deps.limiter.limit("30/minute")
    async def incidents(request: Request, namespace: str = "", job: str = "", since: str = "", limit: int = 500,
                        _=Depends(deps.verify_auth)):
        return await incidents_query(request, job, namespace, since, max(1, min(limit, 1000)))

    @router.get("/api/flight/incidents/export")
    @deps.limiter.limit("10/minute")
    async def export(request: Request, namespace: str = "", job: str = "", since: str = "", format: str = "json",
                     _=Depends(deps.verify_auth)):
        if format not in ("json", "csv"):
            raise HTTPException(status_code=400, detail="format must be json or csv")
        merged = await incidents_query(request, job, namespace, since, 1000)
        if format == "json":
            return Response(content=json.dumps(merged), media_type="application/json",
                            headers={"Content-Disposition": 'attachment; filename="gryvia-incidents.json"'})
        return Response(content=incidents_csv(merged["incidents"]), media_type="text/csv; charset=utf-8",
                        headers={"Content-Disposition": 'attachment; filename="gryvia-incidents.csv"',
                                 "X-Content-Type-Options": "nosniff"})

    @router.get("/api/flight/compare")
    @deps.limiter.limit("30/minute")
    async def compare(request: Request, namespace: str = "", job: str = "", a: str = "", b: str = "",
                      window: str = "", _=Depends(deps.verify_auth)):
        if not job or not valid_spec(a) or not valid_spec(b) or (window and not WINDOW.fullmatch(window)):
            raise HTTPException(status_code=400, detail="job, a and b are required; a and b are an incident id, "
                                                        "an RFC3339 time or 'now'; window looks like 30m or 2h")
        ns = scope(request, job, namespace)
        query = {"namespace": ns, "job": job, "a": a, "b": b}
        if window:
            query["window"] = window
        bodies, coverage = await gather(deps, ns, job, path="/api/v1/flight/compare?" + urlencode(query),
                                        soft=(400, 404))
        need_collectors(coverage)
        return merge_compare(ns, job, bodies, coverage)
