# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Quota use per tenant: GPUs held by running jobs against ``GryviaTenant.spec.quotas.concurrentGPUs``.

Run:ai shows allocated against deserved quota per project, and AMD's Resource Manager a quota-utilization panel; this
is the same view from GryviaAIJob and GryviaTenant objects. ``blocked`` marks a tenant that is at its quota
with jobs still waiting (raise the quota or free GPUs); ``borrowing`` one holding more than its quota (cohort
borrowing under Kueue). A tenant without ``concurrentGPUs`` has no quota to measure against and reports no percentage.
"""
from typing import Any, Dict, List

from fastapi import APIRouter, Depends, Request

from . import tenancy
from .common import Deps
from .jobview import WAITING_PHASES, load_jobs, ns_of, num, status_of, tenant_of_ns, total_gpus
from .tenancy import is_admin

RUNNING_PHASES = ("Running", "Scheduling")


def quota_usage(tenants: List[Dict[str, Any]], jobs: List[Dict[str, Any]]) -> Dict[str, Any]:
    held: Dict[str, int] = {}
    waiting: Dict[str, int] = {}
    running_jobs: Dict[str, int] = {}
    waiting_jobs: Dict[str, int] = {}
    for j in jobs:
        t, phase = tenant_of_ns(ns_of(j)), status_of(j).get("phase", "")
        g = total_gpus(j)
        if phase in RUNNING_PHASES:
            held[t] = held.get(t, 0) + g
            running_jobs[t] = running_jobs.get(t, 0) + 1
        elif phase in WAITING_PHASES and not status_of(j).get("startTime"):
            waiting[t] = waiting.get(t, 0) + g
            waiting_jobs[t] = waiting_jobs.get(t, 0) + 1
    names = {tenancy.tenant_name(t): t for t in tenants}
    rows: List[Dict[str, Any]] = []
    for name in sorted(set(names) | set(held) | set(waiting)):
        quotas = ((names.get(name) or {}).get("spec") or {}).get("quotas") or {}
        quota = int(num(quotas.get("concurrentGPUs")))
        h, w = held.get(name, 0), waiting.get(name, 0)
        rows.append({
            "tenant": name, "quotaGpus": quota or None, "gpusInUse": h, "gpusWaiting": w,
            "runningJobs": running_jobs.get(name, 0), "waitingJobs": waiting_jobs.get(name, 0),
            "utilizationPct": round(100.0 * h / quota, 2) if quota else None,
            "headroomGpus": max(0, quota - h) if quota else None,
            "borrowing": bool(quota) and h > quota,
            "blocked": bool(quota) and h >= quota and w > 0,
            "registered": name in names,
        })
    rows.sort(key=lambda r: (-(r["utilizationPct"] if r["utilizationPct"] is not None else -1), r["tenant"]))
    quota_total = sum(r["quotaGpus"] or 0 for r in rows)
    return {"items": rows, "totals": {
        "quotaGpus": quota_total, "gpusInUse": sum(r["gpusInUse"] for r in rows),
        "gpusWaiting": sum(r["gpusWaiting"] for r in rows),
        "blockedTenants": sum(1 for r in rows if r["blocked"])}}


def build_router(deps: Deps) -> APIRouter:
    router = APIRouter()

    @router.get("/api/quota-usage")
    @deps.limiter.limit("30/minute")
    async def get_quota_usage(request: Request, _=Depends(deps.verify_auth)):
        jobs = await load_jobs(request, deps)
        tenants = await tenancy.list_tenants(deps.k8s_custom)
        if not is_admin(request):
            mine = set(tenancy.caller_tenant_names(request))
            tenants = [t for t in tenants if tenancy.tenant_name(t) in mine]
            jobs = [j for j in jobs if tenant_of_ns(ns_of(j)) in mine]
        return quota_usage(tenants, jobs)

    return router
