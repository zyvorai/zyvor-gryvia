# Feature and validation matrix

| Feature | Delivered behavior | Validation |
|---|---|---|
| Swift native agent | Metal inventory, outbound polling, subprocess launch, lease renewal, cancellation, timeout | Source included; macOS CI configured; not compiled here |
| SwiftUI desktop | Connect, fleet, drain, workloads, submission, cancellation, tenant inspection | Source included; macOS build pending |
| Web UX | Overview, fleet, workloads, model catalog, tenant quotas, allocation metering, audit, logs, filtering | HTML contract and JS syntax pass; browser suite included, unrun here |
| Durable scheduler | Priority, weighted memory fairness, quota, exclusive/shared admission, node selectors | Unit/integration tests pass |
| Reservations | Atomic claims, capacity held through cancellation, expiry and retry fencing | Concurrency and stale-attempt tests pass |
| Fleet health | Node heartbeat freshness, drain, thermal gate | Tests pass; actual Metal reports unverified |
| Memory pressure | Server gate accepts reported warning/critical pressure | Tested as controller input; native OS pressure collection not implemented |
| MLX generation | Native wrapper, token logs, memory hints and bounded output | Python compile passes; dependency/hardware run pending |
| llama.cpp | Local allowlisted CLI/server execution | Example job supplied; hardware run pending |
| PyTorch MPS | Synchronized GPU benchmark wrapper, allocator hint | Python compile passes; MPS run pending |
| Core ML | Numeric JSON prediction wrapper, framework chooses accelerators | Python compile passes; Core ML run pending |
| Inference gateway | Non-streaming OpenAI-style chat, bounds, concurrency limits, least-busy replica | Real HTTP integration test against a mock worker passes |
| Replica lifecycle | Run native model servers as managed jobs; administrator registers endpoint origins | No automatic readiness registration, autoscale or model eviction |
| Tenancy | Server-side ownership, bound agent identities, model tenant markings | Role and tenant isolation tests pass |
| TLS | Mandatory for external bind; Swift rejects remote plaintext origins | Client test source and server configuration supplied |
| Allocation metering | Per-attempt worker seconds and reserved GiB-seconds | Unit test passes; not GPU-utilization billing |
| Audit/logs | Durable events, bounded per-job logs, stale lease rejection | API tests pass; not a cryptographic audit ledger |
| Deploy | launchd install/uninstall, Docker control plane, .app packaging, GitHub CI | Script syntax checks; actual macOS signing/install pending |
| Independent multi-Mac jobs | Each registered Mac claims compatible jobs | Controller behavior tested; physical fleet run pending |
| Shared hardware partitioning | No hard GPU percentages or MIG-style partitions | Not available in this project |
| Distributed MLX gang jobs | Not implemented | Requires hardware qualification and coordinated launch |
| Checkpoint preemption | Not implemented | Cancellation is not checkpoint recovery |
| Streaming/continuous batching | Not implemented by the gateway | Runtime may provide batching internally |
| OIDC, payment billing, hostile-code isolation | Not implemented | Static roles and trusted-worker beta only |

“All features” is not a production certification. This matrix defines this release's concrete implemented scope and identifies the substantial capabilities that still require engineering or hardware validation.
| Idle-time harvesting | Per-Mac owner policy, presence sensing, yield and requeue without consuming a retry | Server and CLI tests; real run on a Mac mini (job yielded, requeued at attempt 0, resumed and finished) |
| Power / thermal policy | Battery %, Low Power Mode and call guards in the gate | Unit tests; battery rules not exercised on a laptop |
| Energy estimate | Optional root `powermetrics` helper, per-job joules above idle, tenant kWh | Math and file parsing tested; helper not run on hardware (needs sudo) |
| Signed provenance | Ed25519 attestations, pinned keys, verified chains, `--require-verified` | RFC 8032 vectors and a CryptoKit signature verified by the server; real signed download on a Mac mini |
| MCP server | Read-only by default; `--allow-write` adds submit, cancel, policy | 8 tests; not exercised against a real MCP client |
| App Intents | Fleet status, demo job, pause this Mac | Compiles; registration with Shortcuts needs an Xcode-built app (not verified) |
