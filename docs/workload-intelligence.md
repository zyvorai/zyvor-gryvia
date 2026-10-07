# Workload intelligence

Gryvia's Intelligence page (`/intelligence`, under Work) combines explicit workload analysis,
live job evidence and opt-in approved operations. It uses the existing gateway and dashboard;
no new CRDs or scheduling controller are installed. The Python SDK exposes `client.intelligence`.

## Delivered behavior and limits

| Roadmap area | Implementation in this change | Limit |
|---|---|---|
| Workload preflight | Weight-memory estimate, explicit extra memory, node shape, reservations, quota, storage, RDMA/interconnect and budget checks | Supplied snapshots; not a compatibility certification. Submit-time: the job webhook warns and opt-in `aiOperator.preflightEnforce` rejects (see below) |
| Scheduling explanations | Reads namespace/marking-filtered job conditions and pod scheduling failures | Recorded selection is not pod placement; queue wait-time predictions are not fabricated |
| Training bottlenecks | Framework timing adapter, per-rank data/collective ratios, straggler comparisons and missing observations | Heuristic analysis; GPU utilization must come from a real measurement. GPU/NCCL qualification still required |
| Useful-work cost | Per-run/checkpoint/evaluation/token unit costs; live job cost joined by job UID to usage records | Estimates, not payments. Actual job endpoint does not invent checkpoint or delivered-token counts |
| Inference SLOs | TTFT/inter-token/error policy with queue/KV pressure, fresh-data gate and bounded scale-up recommendation | Advisory, not a continuous SLO controller. No single-sample scale-down. HPA and scale-to-zero retain their ownership |
| Model laboratory | Executable streaming benchmark harness, quality/latency/error gates, comparable workload digests, Pareto frontier and cost ranking | Quality is normalized exact-match on supplied expected answers. Not an arbitrary quality evaluation framework |
| Checkpoint recovery | Model/dataset binding, stream-verification of every rank's committed payload, recovery compatibility and lost-step policy | Optimizer resharding only for DCP checkpoints (`dcp_checkpoint.py`, CPU-tested); no live training resize. Storage durability must be qualified separately |
| Dataset locality/cache | Digest-aware replica planning, transfer estimates and atomic SHA256 cache for mounted files | No cross-cluster replication, directory cache or automatic dataset-placement controller |
| Fabric qualification | Fresh link observations, throughput-ratio and error evaluation; complements `scripts/qualify-platform.py` | Only supplied links are qualified. No automatic NCCL tuning or fabric repair |
| Capacity simulator | Deterministic GPU-lane reservations, distributed node shape, rates, queue delay and utilization | Fixed-duration heuristic, not a replay of Kueue fairness/preemption or a procurement guarantee |
| Sovereign fleet placement | Fresh capacity, allowed regions, verified dataset identity, credential readiness and currency filtering | Planner only. Existing MultiKueue remains the dispatch integration. No new replication or failover/fencing controller |
| Approved operations/copilot | Durable typed proposals, separate OIDC reviewer, target preconditions, execution and explicit rollback | Three supported operations only; uncertain writes require inspection, never replay |

All ten `POST /api/intelligence/{area}` analysis endpoints accept **caller-supplied** data.
They do not fetch arbitrary URLs or apply changes. Reports identify estimates, supplied observations,
simulations and cluster observations. Missing or stale telemetry is never converted to synthetic success.
Schemas are available from `GET /api/intelligence/schemas`; the dashboard renders bounded forms
from those schemas. Optional telemetry remains absent until supplied. UI arrays are limited to 100
entries; the API permits the documented larger limits. Request bodies are capped at 512 KiB.

## API

All routes require the gateway's existing authentication. Analysis inputs are private to each request
and are not stored. Live job reads use the same namespace and data markings as the existing job UI.

| Route | Access / behavior |
|---|---|
| `GET /api/intelligence/capabilities` | Authenticated; modes, enabled operations and limits |
| `GET /api/intelligence/schemas` | Authenticated; JSON schemas for form/SDK clients |
| `POST /api/intelligence/preflight` | Weight-memory and supplied-pool feasibility |
| `POST /api/intelligence/training` | Supplied framework timing evidence |
| `POST /api/intelligence/economics` | Explicit cost population and result counts |
| `POST /api/intelligence/serving` | Inference policy recommendation |
| `POST /api/intelligence/laboratory` | Comparable benchmark ranking |
| `POST /api/intelligence/recovery` | Checkpoint compatibility and loss budget |
| `POST /api/intelligence/locality` | Dataset replica/transfer planning |
| `POST /api/intelligence/fabric` | Supplied link qualification |
| `POST /api/intelligence/capacity` | Demand simulation (8192 nodes / 65536 lanes / 2000 jobs maximum) |
| `POST /api/intelligence/federation` | Residency-aware cluster planning |
| `GET /api/intelligence/jobs/{name}/explain` | Scoped job conditions and real pod scheduling failures |
| `GET /api/intelligence/jobs/{name}/economics` | Scoped UID-attributed usage estimates; rejects mixed currencies and invalid records |
| `GET /api/intelligence/inventory` | Provider admin; registered GPU capacity, explicitly unknown free capacity |
| `GET/POST /api/intelligence/actions` | Opt-in named provider admin; list (paged) / create durable proposals |
| `GET /api/intelligence/actions/export`, `POST /api/intelligence/actions/sweep` | Opt-in named provider admin; export all records, apply retention now |
| `GET /api/intelligence/actions/{id}` | Opt-in named provider admin; inspect operation |
| `POST /api/intelligence/actions/{id}/{approve,reject,execute,rollback}` | Opt-in named provider admin; state transition |

### Submit-time preflight

A `GryviaAIJob` opts in with annotations; the ai-operator evaluates them against pools built from
the cluster's GPU nodes (grouped by `gryvia.io/gpu`, GPUs per node, `gryvia.io/gpu-memory` in GiB
or `nvidia.com/gpu.memory` in MiB, `gryvia.io/rdma=true` and `gryvia.io/interconnect`). Cordoned
nodes are not counted.

```yaml
metadata:
  annotations:
    gryvia.io/model-params-billions: "70"
    gryvia.io/weight-bits: "16"              # 4, 8, 16 (default) or 32
    gryvia.io/tensor-parallel: "8"           # default 1, at most GPUs per node
    gryvia.io/extra-memory-gib: "12"         # activations, optimizer, KV cache, runtime
    gryvia.io/require-fast-interconnect: "true"
```

`spec.network: rdma` requires RDMA. Malformed annotations are denied by the webhook. Otherwise the
webhook only adds a warning when no pool fits (`Blocked`) or memory is unknown (`Incomplete`).
With `aiOperator.preflightEnforce=true` (operator flag `--preflight-enforce`, independent of the
admission gate) a Pending job that is `Blocked` is rejected with reason `PreflightBlocked`.
An empty cluster, unknown GPU memory and lookup errors allow the job. Quota and budget are the
admission gate's checks, not preflight's. The Go port (`operators/ai-operator/pkg/preflight`) and
the gateway engine are kept equal by shared golden cases (`pkg/preflight/testdata/golden.json`).

### Framework timings

Install the SDK from this checkout: `pip install ./sdk/python`. In the trainer:

```python
from gryvia.training_telemetry import TrainingTelemetry

timings = TrainingTelemetry(rank=rank)
with timings.measure("step"):
    with timings.measure("data"):
        batch = next(loader)
    # Forward, loss and backward...
    with timings.measure("collective"):
        # Use a real measured synchronous collective here.
        synchronize_gradients()
sample = timings.snapshot()  # GPU utilization stays unknown unless supplied explicitly
```

CUDA/NCCL calls may return before work finishes. Use framework events or synchronization;
otherwise this measures launch overhead, not device duration. Report samples from every rank
with `expectedRanks`, `ranks` and `maxAgeSeconds`. Reset the window after publishing.
The collector's existing `/api/flight/jobs/{job}/diagnosis` supplies complementary network/storage
evidence; framework and collector observations retain their separate source and coverage semantics.

### Model benchmark

Supply a JSON corpus with `messages` and `expected` for each prompt. No benchmark is run automatically:

```json
[{"messages":[{"role":"user","content":"Answer with the word ready"}],"expected":"ready"}]
```

Use an actual model endpoint and rates from your deployment. The token is read from
`GRYVIA_BENCHMARK_TOKEN` and is never written to the report.

```bash
python3 scripts/benchmark-model.py \
  --url https://your-model.example/v1/chat/completions \
  --corpus prompts.json --model your-model --name a100-fp16 \
  --gpu-type A100 --engine vllm --precision fp16 \
  --hourly-cost 2 --currency USD --concurrency 4 --requests 100 \
  --output benchmark.json
```

The example rate is a scenario input, not a Gryvia price. The script measures first visible
content latency and total token throughput, using server-reported `completion_tokens` only.
Missing usage blocks qualification; it never counts SSE chunks as tokens. Failed requests
remain in the report and laboratory error policy. Quality is an explicitly named exact-match
suite, not a general LLM quality score. Compare the same workload digest and quality suite.

### Verified checkpoints

Before the trainer creates any checkpoint in a new root:

```python
from checkpoint_contract import bind_contract, verify_committed
bind_contract(checkpoint_root, model_sha256, dataset_sha256, framework="pytorch")
# All ranks continue to use coordinated_checkpoint.save_global.
metadata = verify_committed(checkpoint_root, model_sha256, dataset_sha256)
```

Package `examples/training/checkpoint_contract.py` and `coordinated_checkpoint.py` with the trainer.
Contracts cannot be rebound or retroactively assigned to an existing committed checkpoint.
Verification streams every rank's blob and refuses incomplete manifests, mismatched hashes,
symlinks and oversized payloads. It does not deserialize pickle/torch payloads. The returned
`durableStorage` remains **false**: operators must qualify durability. `optimizerReshardable` is
**true** only for a DCP checkpoint (`dcp_checkpoint.save`, `format: dcp` in COMMIT), whose files are
all verified the same way; opaque per-rank blobs stay false. Recovery plans require that evidence separately.

### Dataset/model file cache

```python
from gryvia.artifact_cache import materialize
artifact = materialize("/mounted-source/dataset.jsonl", "/tenant-cache", expected_sha256)
dataset_path = artifact["path"]
```

The cache rehashes hits, copies through a bounded temporary file, checks the digest, fsyncs and
atomically publishes a read-only content-addressed file. Filesystem locks serialize writers.
Use a tenant-private cache on storage with qualified POSIX semantics; authorizing readers,
storage quotas and retention remain deployment responsibilities. This is a single-file cache,
not a distributed parallel filesystem. No cache permission implies cross-tenant access.

## Approved operations

Off by default. Enable `apiGateway.intelligenceActions=true` in the existing Helm install and
configure real OIDC administrators. The option adds node/quota patch permissions and a Role
for operation ConfigMaps **only in the gateway namespace**. Keep that namespace out of tenant
RBAC. Shared API keys and dashboard key sessions cannot propose or review operations.

Supported changes: inference replicas (1–100), `GryviaQuota.spec.gpuQuota.maxGPUs`, and node
cordon (`spec.unschedulable=true`). Cordon prevents new placements; it does **not** drain,
reset, reboot or kill current workloads. Inference services managed by HPA or scale-to-zero
cannot be changed through this path. Cluster objects still require a namespace field for a
uniform proposal schema; that field is not a namespace boundary for a cluster-scoped object.

1. Propose: capture target UID, resourceVersion, previous value, reason and evidence.
2. Another named OIDC administrator approves or rejects within 15 minutes.
3. Execute: claim the ConfigMap with its resourceVersion, recheck target identity/version,
   then apply the typed desired-state patch with a target resourceVersion precondition.
4. Rollback: requires the applied target resourceVersion still to match. It restores the captured
   value; concurrent changes refuse rollback. Workloads may already have reacted to desired state.

Issuer and subject together identify a person. A proposer cannot review, execute or roll back
their own operation. Approval alone never applies a change. `Applied` means the Kubernetes
API accepted the desired state, not that replicas became healthy or recovery succeeded.
The gateway's existing audit middleware records HTTP transitions; operation records also retain
actor and timestamps. ConfigMaps survive gateway restarts and coordinate multiple replicas.

If a process crashes after claiming execution, a transport fails during the write, or final
record persistence fails, the operation remains `Executing` / `RollingBack`. Inspect the target's
`gryvia.io/intelligence-action` annotation and Kubernetes audit history; do not replay it.
Reconcile manually and create a fresh proposal if needed.

Listing is paged (`?limit=` up to 500, then `?continue=<token>` from the previous page).
`GET /api/intelligence/actions/export` returns up to 10,000 records for archiving.
Finished records (`Rejected`, `Applied`, `RolledBack`, and expired `Proposed`/`Approved`) older than
`apiGateway.intelligenceRetentionDays` (default 30, `0` keeps all) are deleted at most once an hour
by each gateway replica, on a list request, or immediately with `POST /api/intelligence/actions/sweep`.
Deletes carry a resourceVersion precondition. `Executing`/`RollingBack` records are never deleted.

The copilot receives `propose_operation` only for an OIDC provider administrator when this
option is enabled. It can create a typed proposal, never approve, execute or roll back one.
The prompt requires an explicit user change request; the approval boundary is enforced by code.

## Validation and release gates

Unit/API tests cover unknown and future/stale observations, failed benchmarks, incompatible
checkpoint identities/world sizes, reservation/node shapes, currency separation, tenant scoping,
payload bounds, two-person review, CAS ownership, rollback and uncertain writes. SDK tests
exercise timing/cache adapters; checkpoint tests verify every rank; benchmark tests use a stand-in
stream. Dashboard tests cover form defaults and actual analysis submission/error behavior.

Required hardware qualification remains: actual model engines and token timing, distributed GPU
checkpoint recovery, NCCL/RDMA measurements and storage durability. Required integration work for
the broader roadmap remains continuous SLO control, automatic distributed cache placement and
multi-cluster failover/fencing (optimizer resharding is available through DCP checkpoints, CPU-tested only).
These are not represented as complete by this bundle.
