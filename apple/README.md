<div align="center">

<img src="brand/gryvia-icon.svg" width="96" alt="Gryvia">

# Zyvor Gryvia Apple

**Put every Mac to work.** Memory-aware scheduling for native AI workloads on Apple Silicon.

[![License](https://img.shields.io/badge/license-BUSL--1.1-0088ff)](LICENSE)
[![Swift](https://img.shields.io/badge/swift-6-0088ff)](native/Package.swift)
[![Python](https://img.shields.io/badge/server-python%203.14%20%C2%B7%200%20deps-30d158)](server/app.py)

</div>

## Why Gryvia

| When this happens | Gryvia gives you |
| --- | --- |
| Three Macs, five jobs, and nobody knows which fits where | **Memory-reservation admission** with priorities, tenant quotas and fairness weights |
| "Is the mini still alive and not thermally throttled?" | Live fleet with thermal state, drain/resume, and alerts |
| A nightly eval failed at 3am | Durable leases, retries, searchable logs, webhook-free `top`/`jobs` CLI and Prometheus `/metrics` |
| You want to try it without hardware | **Demo mode**: a clearly labelled simulated fleet in the dashboard |

**Surfaces:** web dashboard (⌘K palette, timeline, alerts, templates, bulk actions, export), SwiftUI app (menu bar, Dock badge, Keychain token, Settings), CLI (`status`, `top`, `nodes`, `jobs`, `submit --wait`, `logs -f`, `metrics`, `doctor`).

> Copyright © 2026 Zyvor AI Labs Private Limited. Business Source License 1.1; see [LICENSE](LICENSE).

---

## Get started in 3 minutes

New here? The idea: you have Macs, Gryvia queues AI jobs and decides which Mac runs each one without overloading memory.

```bash
make run-server          # 1. start the control plane (creates ./local with tokens)
make run-agent           # 2. in another terminal: turn this Mac into a worker
open http://127.0.0.1:8443   # 3. connect with GRYVIA_ADMIN_TOKEN from local/credentials.env
```

Then press **Run demo job** (dashboard or app). `gryvia.py quickstart` tells you what to do next, and the dashboard's **Get started** page explains every term. Try it with no hardware at all: **Try demo** on the connect screen.

<div align="center">

# Gryvia Apple
### Apple Silicon. Orchestrated.

Native Swift Mac workers • SwiftUI desktop app • Fleet web console • Durable admission

**Zyvor AI Labs · 0.2.0 engineering beta · BUSL-1.1**

</div>

Gryvia Apple schedules native AI processes across Mac minis and Mac Studios. It adds a control plane above Metal: priority queues, per-team memory quotas, atomic reservations, fleet health, cancellation, bounded inference routing and allocation metering. The control plane runs on macOS or Linux; GPU workers run natively on macOS.

This is a standalone companion project inspired by Zyvor Gryvia's queues and fleet UX. It is included under `apple/` and does not create Kubernetes Pods for Mac jobs. The Swift worker and desktop app use a dependency-free Swift package. The control plane uses Python 3.12's standard library and SQLite; the web console has no build step or CDN dependencies.

**Validation:** 54 Python scheduler/API/inference/web-contract tests pass in the delivery environment, plus Python compilation and JavaScript syntax checks. Swift compilation, native subprocess smoke, real Metal/runtime tests and browser visual checks were not run here. GitHub Actions includes macOS build/test/smoke and Playwright browser jobs. See [TEST_RESULTS.md](TEST_RESULTS.md) and [docs/FEATURE_MATRIX.md](docs/FEATURE_MATRIX.md).

## Start locally

From the `apple/` directory on a Mac with Python 3.12+, Node for checks and Xcode Command Line Tools:

```bash
python3 scripts/configure.py --output local --budget 8
source local/credentials.env
python3 -m server.app --config local/config.json --db local/gryvia.db
```

Open **http://127.0.0.1:8443**. In a second terminal:

```bash
source local/credentials.env
cd native
swift test
swift build -c release
cd ..
native/.build/release/gryvia-agent "$PWD/local/agent.json"
```

Connect the dashboard using the administrator token from `local/credentials.env`. Do not commit that file. The generated configuration uses unique random tokens, one agent identity, one tenant and administrator roles. The agent's identity is bound to its token at the server; a worker cannot choose a different node ID.

In a third terminal:

```bash
source local/credentials.env
python3 scripts/gryvia.py submit examples/demo-job.json
python3 scripts/gryvia.py status
```

The demo is a real subprocess lifecycle smoke workload, **not GPU inference or a benchmark**.

## Native desktop app

```bash
cd native
swift run GryviaDesktop
```

The SwiftUI app supports server connection, fleet status, node drain/resume, workload submission/cancellation, tenant quota inspection and live polling. Tokens remain in memory. Build a development `.app` bundle using `bash scripts/package-macos.sh`. Developer ID signing and notarization require your Apple account; the default package is ad hoc signed.

## Run AI workloads

Install your runtime dependencies in a dedicated environment on each Mac. Configure only executable paths you trust. `scripts/configure.py --mlx-python /absolute/path/to/venv/bin/python3` enables the included MLX generation wrapper. Pin and install MLX/MLX-LM versions that you validate on your Macs; the delivered environment did not resolve or validate GPU dependencies.

Edit the local `agent.json` runtime map to enable additional adapters:

```json
{
  "server": "http://127.0.0.1:8443",
  "budgetGiB": 8,
  "runtimes": {
    "mlx": {"executable":"/absolute/venv/bin/python3", "prefix":["/absolute/repo/runtimes/mlx_generate.py"]},
    "mps": {"executable":"/absolute/venv/bin/python3", "prefix":["/absolute/repo/runtimes/mps_evaluate.py"]},
    "coreml": {"executable":"/absolute/venv/bin/python3", "prefix":["/absolute/repo/runtimes/coreml_predict.py"]},
    "llama": {"executable":"/absolute/llama-cli", "prefix":[]}
  }
}
```

Update the absolute model path in `examples/mlx-job.json` or `examples/llama-job.json`, then submit it with the CLI or dashboard. The model field is a human-readable label; **the wrapper's `--model` argument selects the actual file/model**. Core ML chooses its accelerators; this project does not expose Neural Engine partitions.

## Native inference services

To reserve a Mac for a persistent llama.cpp service, configure the `llama` executable as your `llama-server` binary, then submit `examples/llama-server-job.json` after editing its model path. Cancelling that workload stops the managed server process; drain prevents new claims while an existing service continues.

Register its model with the admin API (see `examples/inference-model.json`), and add the replica origin to `inferenceOrigins` in the server configuration:

```json
"inferenceOrigins": ["http://127.0.0.1:8080"],
"inferenceConcurrencyPerTenant": 2
```

The platform exposes `POST /v1/chat/completions` with administrator or tenant bearer authentication. It validates request bounds, enforces per-tenant in-flight limits, chooses the least-busy configured replica and refuses redirects. Platform tokens are never forwarded to workers. Only non-streaming text chat is supported. Replica URLs must be administrator-allowlisted. Use loopback or an authenticated private network/tunnel for inference workers; the proxy itself has no upstream credential management. A missing/unhealthy worker returns 502; there is no automatic retry of partially executed inference.

## Fleet deployment

The control plane defaults to loopback. Remote binding requires TLS:

```bash
python3 -m server.app --config /secure/config.json --db /data/gryvia.db \
  --host 0.0.0.0 --cert /secure/tls.crt --key /secure/tls.key
```

Issue each Mac a distinct agent token in the control-plane configuration. Configure its server HTTPS origin and local memory budget. Trust your certificate through macOS's normal trust store; there is no insecure certificate bypass.

Install the worker for the current macOS user:

```bash
source local/credentials.env
python3 scripts/install-agent.py --binary "$PWD/native/.build/release/gryvia-agent" \
  --config "$PWD/local/agent.json"
```

Read [docs/OPERATIONS.md](docs/OPERATIONS.md) before sharing a fleet. Workers execute trusted native processes as the agent's user. This is **not an isolation boundary for hostile tenant code**.

## Verification

```bash
bash scripts/verify.sh
bash scripts/mac-smoke.sh         # macOS: API + real Swift agent + demo process
npm install
npx playwright install chromium
npm run test:web                  # real web actions, fixtures, desktop/mobile screenshots in /tmp
```

CI runs these checks after you upload the project to GitHub. It has not run merely because this ZIP includes the workflow.

## Scheduling contract

- Priority first, weighted normalized memory usage second, creation time third.
- Atomic claim and memory accounting inside SQLite `BEGIN IMMEDIATE` transactions.
- Reservations include running and cancelling jobs, preventing capacity release before acknowledgement.
- Server-enforced per-attempt deadlines request worker shutdown, bound renewal to a 20-second timeout grace, and respect retry limits.
- Batched log upload preserves pending output and reports buffer overflow.
- 20-second renewable job leases; stale worker results/logs are rejected. Offline nodes and thermal/pressure gates stop new admission.
- The native agent advertises **one execution slot** and runs one process at a time. The server supports shared slots for compatible future workers, but this Swift agent does not concurrently dispatch multiple GPU processes.
- GPU shares are policy, not hardware partitions. Reservations and allocator hints are not system-wide memory enforcement.
- Allocation metering records worker seconds and reserved GiB-seconds, not measured GPU time or invoices.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for constraints, integration with existing Gryvia and the next hardware validation steps.

## Make targets

`make help` lists everything. Common: `make verify`, `make run-server`, `make run-agent`, `make doctor`,
`make deploy` (Mac mini over ssh; `MINI=host` to change), `make deploy-dry-run`, `make deploy-verify`.
The CLI prints tables on a terminal and JSON when piped: `gryvia.py status --watch`, `submit FILE --wait`, `logs ID -f`, `doctor`.
