# Architecture and integration

## Components

The Python HTTPS control plane persists state in SQLite WAL. API requests authenticate as administrator, tenant or an agent bound to a node. The local web console calls that same API, and the SwiftUI desktop app uses the shared GryviaCore client. Mac workers initiate outbound polling, so the platform does not need to open a command-execution port on every Mac.

The native agent reports Metal device identity, unified-memory capability, installed RAM, recommended Metal working set and thermal state. Its Metal allocated-byte counter measures the **agent's allocations**, not GPU allocations made by all other processes. The scheduling budget is explicitly configured by the operator and capped at 80% of installed RAM. That percentage is a conservative default, not a measured universal recommendation.

Jobs transition queued → running → succeeded/failed. Cancellation transitions queued → cancelled or running → cancelling → cancelled. A cancelled running job continues to reserve capacity until its agent acknowledges completion or its lease expires. Lease expiry retries only within the job's configured limit. Server-side deadlines transition running jobs to cancelling with a timeout reason. Successful exit cannot override a timeout; timeout failures may retry within the configured limit. Renewal is capped at the attempt deadline plus 20 seconds. Each retry gets a new opaque lease; stale attempts cannot complete or append logs.

## Resource accounting

MemoryGiB is a reservation for expected job demand. The controller atomically checks the node's remaining budget and the tenant's concurrent quota. Its fairness score is normalized memory use divided by queue weight; this is **not multi-resource DRF**. Runtime compatibility, node selection, health, drain and available execution slots are admission gates.

The worker has one slot. A persistent inference process can multiplex requests within that slot using its own engine. The gateway bounds platform request concurrency and output size; it does not create tensor-parallel groups, manage KV cache or implement continuous batching itself.

Metering records each attempt's time from claim to completion/lease expiry, multiplied by its memory reservation. Queued time is excluded. These figures are allocation estimates. SQLite contains allocation history and audits; it is not a billing/payment ledger.

## Native execution and fencing limits

Executables are configured locally and launched with `Process` and an argument vector, without a shell. Runtime tokens are removed from the child environment. The agent tries to stop its child immediately on failed renewal, cancels with SIGTERM and uses SIGKILL after one second if necessary.

This release manages the direct child only. Runtime launchers that fork detached descendants are unsupported. Do not configure backgrounding wrappers or shell commands. If an agent is suspended by macOS or killed while its child survives, a lease is a database fence, not proof that GPU work has stopped. For that reason retries default to zero; choose retries only for idempotent jobs. Stronger process-tree supervision, host reconciliation and epoch fencing are necessary before unattended hostile or partition-tolerant production operation.

GPU preemption requires runtime cooperation. This release supports cancellation and priority ordering, not checkpoint preemption/resumption. MLX generation checks active allocator demand against its reservation and sets memory/cache hints; total process RSS and unrelated applications remain outside that allocator. MPS uses an allocator fraction when the agent reports Metal's recommended working-set size. Core ML and llama.cpp are governed by admission reservations and their own runtime settings.

## Existing Gryvia integration

Upstream inspected for design: zyvorai/zyvor-gryvia at commit 38486f9f662a0f61f6beb83b253b7b45017f469d.

A future Gryvia adapter should translate the CRD's native-runtime job spec into this API, and map the external job status back into the CRD status. It must not send a native Mac job to the current NVIDIA Pod builder. Refactor backend dispatch into Kubernetes and native-Mac implementations, and keep tenancy and placement policy shared. The companion lives under `apple/` in the Gryvia repository. Its Mac job API remains separate from the Kubernetes CRD execution path.

## Multi-Mac roadmap

This release distributes independent jobs and routes inference to configured replicas. A model is not automatically sharded across all RAM in the fleet. Multi-node MLX requires gang leases, exact rank placement, runtime-version compatibility, topology qualification and coordinated rollback on partial launch. TCP ring is the portable starting point. Thunderbolt JACCL/RDMA must be feature-detected and qualified on the actual machines. Neither distributed training nor automatic topology mutation is included in this version.

Primary references used during design:

- https://developer.apple.com/documentation/metal/mtldevice/recommendedmaxworkingsetsize
- https://developer.apple.com/documentation/metal/mtldevice/currentallocatedsize
- https://ml-explore.github.io/mlx/build/html/usage/distributed.html
- https://github.com/ml-explore/mlx-lm/blob/main/mlx_lm/generate.py
- https://docs.pytorch.org/docs/stable/generated/torch.mps.set_per_process_memory_fraction.html


## Fleet policy, provenance and energy (0.3.0)

- **Owner policy.** Each node has a policy (`always`, `idle`, `paused`, plus battery, Low Power Mode and call guards). The agent reports *presence* (seconds since the last input event, screen lock, display sleep, power source, whether an input device is running; nothing else). `Store.gate` combines policy and presence; `claim` returns nothing while the gate is closed; `register` asks running jobs to yield (`owner returned`) and they are requeued without consuming an attempt. LoRA runs resume from the last saved adapter weights because the job keeps its run folder.
- **Provenance.** Agents hold an Ed25519 identity key (CryptoKit). A completed job carries a signed statement over the job id, spec hash (computed by the server at claim), result-line hash and exit status. The server pins each node's key on first use (trust on first use), verifies with `server/ed25519.py` (RFC 8032, verify only) and records a chain on every model produced by download, fine-tune, fuse or quantise. `requireVerified` makes `serve`, `benchmark`, `evaluate`, `quantize` and `fuse` refuse unverified models.
- **Energy.** An optional root helper publishes package power (`powermetrics`); the agent reports it; the server integrates it over a job's lease window and subtracts the node's idle median. Always an estimate.
- **MCP.** `mcp/gryvia_mcp.py` exposes the read API (and, with `--allow-write`, submit/cancel/policy) over stdio; roles are enforced by the server.
