# Gryvia Python SDK

Python SDK for [Gryvia](https://github.com/zyvorai/gryvia) -- a Kubernetes-native GPU compute platform for AI/ML workloads.

> **Status.** This is an async REST client for the Gryvia API gateway (jobs, nodes, quotas, costs, metrics). It has unit tests against mocked HTTP but has not been validated against a production gateway. It does **not** cover SKUs/catalog, tenants, usage, invoices or flight-recorder endpoints, and it is not a Kubernetes client (see `sdk/go` for a controller-runtime client of the CRDs). Publication of a `gryvia` package on PyPI is not verified, so install from source. Gateway auth: an API key or session token grants admin, an OIDC token is tenant-scoped.

**Known mismatch: `Jobs.stream_logs`.** It assumes server-sent events at `/api/jobs/{name}/logs?follow=...&tailLines=...`. The gateway's log endpoint returns a single JSON document (`{pod, container, lines, truncated}`) selected with `?pod=&tail=` (1 to 2000 lines) and does not stream, so `stream_logs` does not work against the current gateway. Use the CLI (`gryvia logs`) or call the endpoint directly until the SDK is updated.

## Installation

Install from source (recommended):

```bash
cd sdk/python
pip install -e .
```

`pip install gryvia` only works if the package has been published to an index you use; that is not verified.

## Quick Start

```python
import asyncio
from gryvia import Gryvia

async def main():
    async with Gryvia(
        api_url="https://gryvia.example.com",
        token="your-api-token",
    ) as tr:
        # Cluster overview
        stats = await tr.metrics.cluster_stats()
        print(f"GPUs: {stats.available_gpus}/{stats.total_gpus} available")
        print(f"Running jobs: {stats.running_jobs}")

        # List jobs
        jobs = await tr.jobs.list(limit=10)
        for job in jobs.items:
            print(f"  {job.metadata.name}: {job.status.phase}")

asyncio.run(main())
```

## Configuration

The client accepts explicit parameters or reads from environment variables:

| Parameter | Env Var | Description |
|-----------|---------|-------------|
| `api_url` | `GRYVIA_API_URL` | Base URL of the API gateway |
| `token` | `GRYVIA_TOKEN` | Bearer token for authentication |

```python
# Explicit
tr = Gryvia(api_url="https://...", token="...")

# From environment
# export GRYVIA_API_URL=https://...
# export GRYVIA_TOKEN=...
tr = Gryvia()
```

## Usage

### Job Management

```python
async with Gryvia(...) as tr:
    # Submit a job from a dict
    job = await tr.jobs.create({
        "apiVersion": "gryvia.io/v1alpha1",
        "kind": "GryviaAIJob",
        "metadata": {"name": "my-training"},
        "spec": {
            "image": "nvcr.io/nvidia/pytorch:24.01-py3",
            "gpus": 4,
            "gpuType": "A100-80G",
            "type": "training",
            "command": ["torchrun", "--nproc_per_node=4", "train.py"],
        },
    })

    # Submit from a YAML file
    job = await tr.jobs.create("jobs/my-training.yaml")

    # Wait for completion (polls every 5s, times out after 1h)
    final = await tr.jobs.wait_for_completion("my-training", timeout=7200)
    print(f"Job finished: {final.status.phase}")

    # Get job details
    job = await tr.jobs.get("my-training")

    # Delete a job
    await tr.jobs.delete("my-training")

    # Job metrics
    metrics = await tr.jobs.metrics(time_range="24h")
    print(f"Average duration: {metrics.average_duration_hours}h")
```

### GPU Metrics

```python
async with Gryvia(...) as tr:
    # Cluster stats
    stats = await tr.metrics.cluster_stats()

    # Per-GPU metrics
    gpu = await tr.metrics.gpu(time_range="1h")
    for m in gpu.metrics:
        print(f"  {m.node} GPU{m.gpu_index}: {m.utilization}% util, {m.temperature}C")
```

### Nodes

```python
async with Gryvia(...) as tr:
    # List nodes
    nodes = await tr.nodes.list()

    # Get a specific node
    node = await tr.nodes.get("gpu-node-01")

    # Node health checks
    health = await tr.nodes.health()
    for n in health.nodes:
        print(f"  {n.node_name}: {n.health} ({n.gpu_type} x{n.gpu_count})")
        for issue in n.issues:
            print(f"    WARNING: {issue}")
```

### Quotas

```python
async with Gryvia(...) as tr:
    # List quotas
    quotas = await tr.quotas.list()

    # Get a specific quota
    quota = await tr.quotas.get("team-alpha")

    # Quota usage summary
    usage = await tr.quotas.usage()
    for q in usage.quotas:
        print(f"  {q.team}: {q.allocated_gpus}/{q.max_gpus} GPUs, "
              f"${q.spent_this_month:.2f}/${q.monthly_budget:.2f} budget")
```

### Cost Analysis

```python
async with Gryvia(...) as tr:
    # Full cost metrics
    costs = await tr.costs.get()
    print(f"Total cost: ${costs.total_cost:.2f}")

    # Convenience methods
    total = await tr.costs.total()
    by_team = await tr.costs.by_team()
    by_gpu = await tr.costs.by_gpu_type()
```

### Workload intelligence

```python
async with Gryvia(...) as tr:
    caps = await tr.intelligence.capabilities()
    why = await tr.intelligence.explain("my-training")
    report = await tr.intelligence.analyze("preflight", inputs)  # inputs follow GET /api/intelligence/schemas
```

`propose`, `actions` and `transition` drive the opt-in approved operations and need an OIDC admin token. The trainer-side
helpers `gryvia.training_telemetry.TrainingTelemetry` and `gryvia.artifact_cache.materialize` are described in
[workload intelligence](../../docs/workload-intelligence.md).

## Error Handling

```python
from gryvia import Gryvia, NotFoundError, AuthenticationError

async with Gryvia(...) as tr:
    try:
        job = await tr.jobs.get("nonexistent")
    except NotFoundError as e:
        print(f"Not found: {e}")
    except AuthenticationError:
        print("Bad token")
```

All exceptions inherit from `GryviaError`:

- `AuthenticationError` (401)
- `AuthorizationError` (403)
- `NotFoundError` (404)
- `ValidationError` (400)
- `RateLimitError` (429)
- `ServerError` (5xx)
- `TimeoutError` (polling timeout)
- `ConfigurationError` (missing api_url/token)

## Development

```bash
pip install -e ".[dev]"
pytest
```
