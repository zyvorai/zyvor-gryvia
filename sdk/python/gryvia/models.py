# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Pydantic v2 models for Gryvia API resources."""

from __future__ import annotations

from datetime import datetime
from typing import Any

from pydantic import BaseModel, Field


# ---------------------------------------------------------------------------
# Cluster stats
# ---------------------------------------------------------------------------

class ClusterStats(BaseModel):
    """Overall cluster statistics."""

    total_gpus: int = Field(alias="totalGPUs")
    available_gpus: int = Field(alias="availableGPUs")
    allocated_gpus: int = Field(alias="allocatedGPUs")
    utilization_percent: float = Field(alias="utilizationPercent")
    total_jobs: int = Field(alias="totalJobs")
    running_jobs: int = Field(alias="runningJobs")
    pending_jobs: int = Field(alias="pendingJobs")
    completed_jobs: int = Field(alias="completedJobs")
    failed_jobs: int = Field(alias="failedJobs")
    total_nodes: int = Field(alias="totalNodes")
    timestamp: str

    model_config = {"populate_by_name": True}


# ---------------------------------------------------------------------------
# GPU metrics
# ---------------------------------------------------------------------------

class GPUMetric(BaseModel):
    """A single GPU metric reading."""

    node: str
    gpu_index: int = Field(alias="gpuIndex")
    utilization: float
    temperature: float
    memory_used: float = Field(alias="memoryUsed")
    memory_total: float = Field(alias="memoryTotal")
    timestamp: str

    model_config = {"populate_by_name": True}


class GPUMetricsResponse(BaseModel):
    """Response from the GPU metrics endpoint."""

    time_range: str = Field(alias="timeRange")
    metrics: list[GPUMetric]

    model_config = {"populate_by_name": True}


# ---------------------------------------------------------------------------
# Jobs
# ---------------------------------------------------------------------------

class JobMetadata(BaseModel):
    """Kubernetes-style metadata for a job."""

    name: str = ""
    namespace: str = ""
    uid: str = ""
    labels: dict[str, str] = Field(default_factory=dict)
    annotations: dict[str, str] = Field(default_factory=dict)
    creation_timestamp: str | None = Field(None, alias="creationTimestamp")

    model_config = {"populate_by_name": True, "extra": "allow"}


class JobSpec(BaseModel):
    """Spec for a GryviaAIJob."""

    image: str = ""
    gpus: int = 0
    gpu_type: str = Field("", alias="gpuType")
    framework: str = ""
    command: list[str] = Field(default_factory=list)
    args: list[str] = Field(default_factory=list)
    env: list[dict[str, str]] = Field(default_factory=list)

    model_config = {"populate_by_name": True, "extra": "allow"}


class JobStatus(BaseModel):
    """Status of a GryviaAIJob."""

    phase: str = "Unknown"
    start_time: str | None = Field(None, alias="startTime")
    completion_time: str | None = Field(None, alias="completionTime")
    message: str = ""

    model_config = {"populate_by_name": True, "extra": "allow"}


class Job(BaseModel):
    """A GryviaAIJob resource."""

    api_version: str = Field("gryvia.io/v1alpha1", alias="apiVersion")
    kind: str = "GryviaAIJob"
    metadata: JobMetadata = Field(default_factory=JobMetadata)
    spec: JobSpec = Field(default_factory=JobSpec)
    status: JobStatus = Field(default_factory=JobStatus)

    model_config = {"populate_by_name": True, "extra": "allow"}


class JobListResponse(BaseModel):
    """Paginated list of jobs."""

    items: list[Job]
    total: int
    limit: int
    offset: int

    model_config = {"populate_by_name": True}


# ---------------------------------------------------------------------------
# Job metrics
# ---------------------------------------------------------------------------

class JobMetrics(BaseModel):
    """Aggregated job metrics."""

    total_jobs: int = Field(alias="totalJobs")
    by_status: dict[str, int] = Field(default_factory=dict, alias="byStatus")
    by_framework: dict[str, int] = Field(default_factory=dict, alias="byFramework")
    average_duration_hours: float = Field(alias="averageDurationHours")
    timestamp: str

    model_config = {"populate_by_name": True}


# ---------------------------------------------------------------------------
# Quotas
# ---------------------------------------------------------------------------

class Quota(BaseModel):
    """A GryviaQuota resource (raw Kubernetes object)."""

    api_version: str = Field("gryvia.io/v1alpha1", alias="apiVersion")
    kind: str = "GryviaQuota"
    metadata: dict[str, Any] = Field(default_factory=dict)
    spec: dict[str, Any] = Field(default_factory=dict)
    status: dict[str, Any] = Field(default_factory=dict)

    model_config = {"populate_by_name": True, "extra": "allow"}


class QuotaListResponse(BaseModel):
    """Paginated list of quotas."""

    items: list[Quota]
    total: int
    limit: int
    offset: int

    model_config = {"populate_by_name": True}


class QuotaUsageEntry(BaseModel):
    """Quota usage for a single team."""

    team: str
    max_gpus: int = Field(alias="maxGPUs")
    allocated_gpus: int = Field(alias="allocatedGPUs")
    utilization_percent: float = Field(alias="utilizationPercent")
    running_jobs: int = Field(alias="runningJobs")
    queued_jobs: int = Field(alias="queuedJobs")
    monthly_budget: float = Field(alias="monthlyBudget")
    spent_this_month: float = Field(alias="spentThisMonth")
    remaining_budget: float = Field(alias="remainingBudget")

    model_config = {"populate_by_name": True}


class QuotaUsageResponse(BaseModel):
    """Quota usage across all teams."""

    quotas: list[QuotaUsageEntry]
    total: int
    limit: int
    offset: int
    timestamp: str

    model_config = {"populate_by_name": True}


# ---------------------------------------------------------------------------
# Nodes
# ---------------------------------------------------------------------------

class Node(BaseModel):
    """A GryviaGPUNode resource (raw Kubernetes object)."""

    api_version: str = Field("gryvia.io/v1alpha1", alias="apiVersion")
    kind: str = "GryviaGPUNode"
    metadata: dict[str, Any] = Field(default_factory=dict)
    spec: dict[str, Any] = Field(default_factory=dict)
    status: dict[str, Any] = Field(default_factory=dict)

    model_config = {"populate_by_name": True, "extra": "allow"}


class NodeListResponse(BaseModel):
    """Paginated list of nodes."""

    items: list[Node]
    total: int
    limit: int
    offset: int

    model_config = {"populate_by_name": True}


class NodeHealthEntry(BaseModel):
    """Health info for a single GPU node."""

    node_name: str = Field(alias="nodeName")
    gpu_type: str = Field(alias="gpuType")
    gpu_count: int = Field(alias="gpuCount")
    phase: str
    health: str
    issues: list[str] = Field(default_factory=list)
    rdma_enabled: bool = Field(alias="rdmaEnabled")

    model_config = {"populate_by_name": True}


class NodeHealthResponse(BaseModel):
    """Node health status response."""

    nodes: list[NodeHealthEntry]
    total: int
    limit: int
    offset: int
    timestamp: str

    model_config = {"populate_by_name": True}


# ---------------------------------------------------------------------------
# Costs
# ---------------------------------------------------------------------------

class TeamCost(BaseModel):
    """Cost breakdown for a team."""

    team: str
    cost: float


class GPUTypeCost(BaseModel):
    """Cost breakdown for a GPU type."""

    type: str
    cost: float
    hours: float


class CostMetrics(BaseModel):
    """Cost metrics and analysis."""

    monthly: list[dict[str, Any]] = Field(default_factory=list)
    has_historical_data: bool = Field(alias="hasHistoricalData")
    by_team: list[TeamCost] = Field(default_factory=list, alias="byTeam")
    by_gpu_type: list[GPUTypeCost] = Field(default_factory=list, alias="byGPUType")
    total_cost: float = Field(alias="totalCost")
    timestamp: str

    model_config = {"populate_by_name": True}
