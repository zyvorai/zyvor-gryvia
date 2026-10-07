"""Bounded, finite inputs shared by the workbench, SDK and telemetry adapters."""

from datetime import datetime
from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

Name = Annotated[str, Field(min_length=1, max_length=63, pattern=r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")]
Label = Annotated[str, Field(min_length=1, max_length=128)]
Positive = Annotated[float, Field(gt=0, le=1e15)]
Nonnegative = Annotated[float, Field(ge=0, le=1e15)]
Digest = Annotated[str, Field(pattern=r"^[a-f0-9]{64}$")]


class Input(BaseModel):
    model_config = ConfigDict(extra="forbid", allow_inf_nan=False)


class Observation(Input):
    observedAt: datetime
    windowSeconds: Positive
    source: Label

    @model_validator(mode="after")
    def timezone_required(self):
        if self.observedAt.tzinfo is None:
            raise ValueError("observedAt requires a timezone")
        return self


class Pool(Input):
    name: Name
    gpuType: Label
    nodes: int = Field(ge=1, le=4096)
    gpusPerNode: int = Field(ge=1, le=1024)
    memoryGiB: Positive
    hourlyRate: Nonnegative
    currency: str = Field(default="USD", pattern=r"^[A-Z]{3}$")
    rdma: bool = False
    interconnect: Literal["none", "nvlink", "nvswitch"] = "none"
    reservedGPUs: int = Field(default=0, ge=0, le=1048576)

    @model_validator(mode="after")
    def capacity(self):
        if self.reservedGPUs > self.nodes * self.gpusPerNode:
            raise ValueError("reservedGPUs exceeds pool capacity")
        return self


class Preflight(Input):
    gpuType: Label
    nodes: int = Field(default=1, ge=1, le=4096)
    gpusPerNode: int = Field(default=1, ge=1, le=1024)
    parametersBillions: Positive
    weightBits: Literal[4, 8, 16, 32] = 16
    training: bool = False
    # Caller supplies memory not explained by weights: activations, KV cache, optimizer, runtime.
    extraMemoryGiBPerGPU: Nonnegative
    tensorParallel: int = Field(default=1, ge=1, le=1024)
    durationHours: Positive
    budget: Nonnegative | None = None
    quotaGPUs: int | None = Field(default=None, ge=0, le=1048576)
    requireRDMA: bool = False
    requireFastInterconnect: bool = False
    storageReady: bool | None = None
    checkpointReady: bool | None = None
    pools: list[Pool] = Field(max_length=128)

    @model_validator(mode="after")
    def parallel(self):
        if self.tensorParallel > self.gpusPerNode:
            raise ValueError("tensorParallel exceeds GPUs per node")
        if len({p.name for p in self.pools}) != len(self.pools):
            raise ValueError("pool names must be unique")
        return self


class RankSample(Observation):
    rank: int = Field(ge=0, le=65535)
    stepSeconds: Positive
    dataWaitSeconds: Nonnegative | None = None
    collectiveSeconds: Nonnegative | None = None
    gpuUtilization: float | None = Field(default=None, ge=0, le=100)

    @model_validator(mode="after")
    def timings(self):
        for v in (self.dataWaitSeconds, self.collectiveSeconds):
            if v is not None and v > self.stepSeconds:
                raise ValueError("component time cannot exceed step time")
        return self


class Training(Input):
    expectedRanks: int = Field(ge=1, le=65536)
    maxAgeSeconds: int = Field(default=120, ge=1, le=3600)
    ranks: list[RankSample] = Field(max_length=1024)

    @model_validator(mode="after")
    def ranks_unique(self):
        if len({r.rank for r in self.ranks}) != len(self.ranks):
            raise ValueError("rank observations must be unique")
        if any(r.rank >= self.expectedRanks for r in self.ranks):
            raise ValueError("rank outside expectedRanks")
        return self


class Economics(Input):
    currency: str = Field(pattern=r"^[A-Z]{3}$")
    cost: Nonnegative
    failedRunCost: Nonnegative = 0
    completedRuns: int = Field(default=0, ge=0, le=1000000000)
    committedCheckpoints: int = Field(default=0, ge=0, le=1000000000)
    successfulEvaluations: int = Field(default=0, ge=0, le=1000000000)
    deliveredTokens: int = Field(default=0, ge=0, le=1000000000000000)
    costBasis: Literal["estimate", "metered-estimate"] = "estimate"

    @model_validator(mode="after")
    def wasted(self):
        if self.failedRunCost > self.cost:
            raise ValueError("failedRunCost exceeds total cost")
        return self


class ServingSample(Observation):
    requests: int = Field(ge=0, le=1000000000)
    errors: int = Field(ge=0, le=1000000000)
    ttftP95Ms: Nonnegative | None = None
    interTokenP95Ms: Nonnegative | None = None
    queueDepth: int | None = Field(default=None, ge=0, le=1000000000)
    kvCachePercent: float | None = Field(default=None, ge=0, le=100)

    @model_validator(mode="after")
    def counts(self):
        if self.errors > self.requests:
            raise ValueError("errors exceeds requests")
        return self


class Serving(Input):
    sample: ServingSample
    replicas: int = Field(ge=1, le=100)
    minReplicas: int = Field(ge=1, le=100)
    maxReplicas: int = Field(ge=1, le=100)
    maxTTFTMs: Positive
    maxInterTokenMs: Positive
    maxErrorRate: float = Field(default=0.01, ge=0, le=1)
    minRequests: int = Field(default=100, ge=1, le=1000000000)
    maxAgeSeconds: int = Field(default=120, ge=1, le=3600)

    @model_validator(mode="after")
    def bounds(self):
        if not self.minReplicas <= self.replicas <= self.maxReplicas:
            raise ValueError("replicas must be inside min/max bounds")
        return self


class Benchmark(Observation):
    name: Name
    model: Label
    gpuType: Label
    engine: Literal["vllm", "triton", "tensorrt-llm", "llama.cpp"]
    precision: Label
    concurrency: int = Field(ge=1, le=1000000)
    workloadDigest: Digest
    qualitySuite: Label
    qualityScore: float = Field(ge=0, le=1)
    tokensPerSecond: Positive
    ttftP95Ms: Nonnegative
    hourlyCost: Nonnegative
    currency: str = Field(default="USD", pattern=r"^[A-Z]{3}$")
    successfulRequests: int = Field(ge=1, le=1000000000)
    failedRequests: int = Field(default=0, ge=0, le=1000000000)


class Laboratory(Input):
    benchmarks: list[Benchmark] = Field(max_length=256)
    model: Label
    workloadDigest: Digest
    qualitySuite: Label
    currency: str = Field(default="USD", pattern=r"^[A-Z]{3}$")
    minQuality: float = Field(ge=0, le=1)
    maxTTFTMs: Positive
    minSuccessfulRequests: int = Field(default=100, ge=1, le=1000000000)
    maxErrorRate: float = Field(default=0.01, ge=0, le=1)
    maxAgeSeconds: int = Field(default=86400, ge=1, le=2592000)

    @model_validator(mode="after")
    def unique(self):
        if len({b.name for b in self.benchmarks}) != len(self.benchmarks):
            raise ValueError("benchmark names must be unique")
        return self


class Checkpoint(Input):
    step: int = Field(ge=0, le=1000000000000)
    modelDigest: Digest
    datasetDigest: Digest
    framework: Literal["pytorch", "generic"]
    worldSize: int = Field(ge=1, le=65536)
    ranks: list[int] = Field(max_length=65536)
    integrityVerified: bool
    globallyCommitted: bool
    durableStorage: bool
    optimizerReshardable: bool = False
    committedAt: datetime

    @model_validator(mode="after")
    def time_and_ranks(self):
        if self.committedAt.tzinfo is None:
            raise ValueError("committedAt requires a timezone")
        if len(set(self.ranks)) != len(self.ranks) or any(r < 0 or r >= self.worldSize for r in self.ranks):
            raise ValueError("invalid or duplicate checkpoint ranks")
        return self


class Recovery(Input):
    checkpoint: Checkpoint | None
    modelDigest: Digest
    datasetDigest: Digest
    framework: Literal["pytorch", "generic"]
    targetWorldSize: int = Field(ge=1, le=65536)
    currentStep: int = Field(ge=0, le=1000000000000)
    maxLostSteps: int = Field(ge=0, le=1000000000000)
    rendezvousReady: bool


class Replica(Input):
    cluster: Name
    pool: Name
    digest: Digest
    verified: bool
    ready: bool


class LocalityCandidate(Input):
    cluster: Name
    pool: Name
    freeGPUs: int = Field(ge=0, le=1048576)
    transferBytesPerSecond: Positive | None = None
    egressCostPerGiB: Nonnegative = 0


class Locality(Input):
    datasetDigest: Digest
    datasetGiB: Nonnegative
    requestedGPUs: int = Field(ge=1, le=1048576)
    replicas: list[Replica] = Field(max_length=512)
    candidates: list[LocalityCandidate] = Field(max_length=128)


class FabricLink(Observation):
    sourceNode: Name
    destinationNode: Name
    measuredGbps: Nonnegative
    expectedGbps: Positive
    errors: int = Field(ge=0, le=1000000000)
    transport: Literal["rdma", "tcp", "nvlink"]


class Fabric(Input):
    links: list[FabricLink] = Field(max_length=1024)
    maxAgeSeconds: int = Field(default=120, ge=1, le=86400)
    minimumBandwidthRatio: float = Field(default=0.7, gt=0, le=1)


class Demand(Input):
    name: Name
    gpuType: Label
    arrivalSeconds: Nonnegative
    durationSeconds: Positive
    nodes: int = Field(default=1, ge=1, le=4096)
    gpusPerNode: int = Field(ge=1, le=1024)


class Capacity(Input):
    pools: list[Pool] = Field(max_length=128)
    demand: list[Demand] = Field(max_length=2000)
    currency: str = Field(default="USD", pattern=r"^[A-Z]{3}$")

    @model_validator(mode="after")
    def unique(self):
        if len({p.name for p in self.pools}) != len(self.pools):
            raise ValueError("pool names must be unique")
        if len({d.name for d in self.demand}) != len(self.demand):
            raise ValueError("demand names must be unique")
        if any(p.currency != self.currency for p in self.pools):
            raise ValueError("pool currencies must match scenario currency")
        if sum(p.nodes for p in self.pools) > 8192 or sum(p.nodes * p.gpusPerNode for p in self.pools) > 65536:
            raise ValueError("scenario exceeds 8192 nodes or 65536 GPU lanes")
        return self


class Cluster(Observation):
    name: Name
    region: Label
    ready: bool
    gpuType: Label
    freeGPUs: int = Field(ge=0, le=1048576)
    hourlyRate: Nonnegative
    currency: str = Field(default="USD", pattern=r"^[A-Z]{3}$")
    datasetDigests: list[Digest] = Field(max_length=256)
    credentialConfigured: bool


class Federation(Input):
    allowedRegions: list[Label] = Field(min_length=1, max_length=128)
    gpuType: Label
    gpus: int = Field(ge=1, le=1048576)
    datasetDigest: Digest
    currency: str = Field(default="USD", pattern=r"^[A-Z]{3}$")
    clusters: list[Cluster] = Field(max_length=128)
    maxAgeSeconds: int = Field(default=120, ge=1, le=3600)

    @model_validator(mode="after")
    def unique(self):
        if len({c.name for c in self.clusters}) != len(self.clusters):
            raise ValueError("cluster names must be unique")
        return self
