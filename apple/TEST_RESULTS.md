# Delivery verification — 14 October 2026

Environment: Linux container, Python 3.12.14, Node 24.19.0. No Swift compiler or Apple GPU available.

| Check | Result |
|---|---|
| Python unittest suite | 54 tests pass |
| Python compilation: server, scripts, runtime wrappers | Pass |
| `node --check web/app.js` | Pass |
| Shell script syntax | Pass |
| Swift compilation / XCTest | Not run: Swift toolchain absent |
| Native Mac agent end-to-end smoke | Not run: requires macOS; script and CI supplied |
| Metal, MLX, llama.cpp, MPS, Core ML execution | Not run: no Mac/GPU/runtime dependencies |
| Browser interactions and screenshots | Not run: Chromium unavailable; browser download returned an invalid/truncated archive |
| GitHub Actions | Dedicated workflow supplied; remote run status must be checked on the PR |

Eight new regression tests cover server deadline signalling, reservation holding, bounded timeout grace, retry deadlines, event deduplication, ordered atomic log batches, stale-attempt rejection and HTTP role enforcement. Four Swift buffer tests are supplied but unrun.

Tests include atomic concurrent claims, priority and fairness ordering, memory and tenant admission, node/runtime selectors, drain, stale-node/thermal/pressure gates, cancellation acknowledgement, retry exhaustion, stale-attempt completion/log rejection, restart persistence, allocation metering, server roles, tenant ownership and log visibility, actual HTTP inference forwarding to a mock worker, token non-forwarding, inference request/concurrency bounds, HTML IDs/assets/panels and absence of credential persistence.

A mock inference response and the CPU demo validate plumbing only. They are not measurements of GPU throughput, performance, isolation or production readiness. The included GitHub macOS workflow builds Swift and runs the CPU subprocess smoke; GPU acceptance must still run on your actual Macs.

This continuation reapplied the prior scheduler patch to main at `4f6fdd1606591f6ab1c561f143cbf02b292a787f`. The Go operator race/vet results were recorded by the previous delivery; Go is unavailable here, so those results were not independently rerun. A browser attempt failed because the Chromium executable is missing. GitHub branch creation through the Zyvor connection returned HTTP 403; no remote branch, PR or CI run was created.

## Verified 2026-10-14 (macOS 27.2 dev Mac, macOS 27.0 Mac mini, Swift 6.4, Python 3.14)

| Check | Dev Mac | Mac mini |
|---|---|---|
| Python unit tests (59) | pass | pass |
| XCTest (21) | pass | not run: needs Xcode (Command Line Tools only) |
| `swift build -c release` | pass (agent + SwiftUI app) | pass (`gryvia-agent` only; SwiftUI macros need Xcode) |
| `mac-smoke.sh` native agent → API → subprocess → logs → completion | not re-run | pass |
| Playwright dashboard test | pass | not run |
| launchd server + agent, `doctor`, `submit --wait` | n/a | pass |

Still unmeasured: Metal/MLX/llama runtimes (only the demo runtime was executed), GPU throughput and isolation.

## Branding + features round (2026-10-14)
Accent `#0088ff` (shared tokens), BUSL-1.1, new Z icon, web dashboard v2 (palette, demo mode, timeline, alerts, templates, bulk, export, settings), `/metrics`, `/api/history`, `/api/priority`, job labels, CLI `top`/`nodes`/`jobs`/`priority`/`metrics`, SwiftUI Settings/menu-bar/Dock badge. Not implemented (deliberately): webhooks and SSE streaming, which need a security review of outbound requests and long-lived connections.

## M0 + M1: GPU detection and live scheduling (2026-10-14)
- Agent reads `AGXAccelerator` from the IORegistry (no sudo): GPU model, **GPU core count**, device/renderer/tiler utilisation, GPU memory in use, CPU P/E cores. Verified against `ioreg` on both Macs: this Mac M4 = 8 GPU cores, the mini = 12 GPU cores; the mini's in-use memory (138.9 MiB) matched the registry value.
- One GPU per Apple Silicon Mac, so "GPUs" = online Macs; utilisation is device-wide (includes other apps), not per job.
- Server: `node_telemetry` ring, `decisions` ring (queue/wait/admit/finish/cancel with reasons), `finished` timestamps, `GET /api/telemetry`, SSE `GET /api/stream`.
- Web: **Live** view (GPU core grid, measured utilisation sparkline, per-job memory reservation vs measured in-use, animated queue → admission → Mac flow, decision feed), streaming with polling fallback, SF system font stack, simulated demo GPUs.
- Native: Live section with Swift Charts, animated lanes; Tenants icon, metric cards, footer fixed.
- Not yet done: M2 (model onboarding/eval/benchmark) and M3 (training, datasets, workflows, deployments).

## M2: Model hub (2026-10-14)
- Server: model registry v2 (status onboarding → ready/failed, path, sha256, size, quant, paramsB, parent), `POST /api/models/onboard` (HF repo/URL, host allowlist, traversal-safe repo/file checks), job kinds `download|benchmark|evaluate|quantize`, `results` and `suites` tables, results parsed from a structured `{"kind":"result"}` log line, quantise creates a child model.
- Runtimes: `hf_download.py` (https + huggingface.co/hf.co only, size cap, SHA-256, auth header never forwarded off-host), `llama_bench.py`, `eval_suite.py`, `quantize.py`, all confined to the managed models directory.
- On the Mac mini (real, not simulated): `onboard` downloaded `ggml-org/tiny-llamas/stories260K.gguf` (1.19 MB, SHA-256 recorded) → `bench` measured 79,929 tok/s prompt / 1,905 tok/s generation → `eval` ran 2 cases through llama.cpp, 2/2 passed. This is a toy 260K-parameter model used only to prove the pipeline; those throughput numbers say nothing about real models.
- MLX + mlx-lm installed on the mini by `make mini-setup` (default device: GPU). No MLX model was run yet.
- UI: Model hub (cards with status/tok/s/eval, onboarding wizard with memory estimate, details + history, evaluate dialog with saved suites, quantise, compare), Playground (needs a registered inference replica), CLI `models/onboard/bench/eval/results`, native Model hub section.
- Not done: streaming Playground (gateway is non-streaming), eval suites larger than the 4 KB inline limit through the UI, LoRA training (M3).

## M3: training, datasets, workflows, deployments, budgets (2026-10-14)
- Server: datasets registry (+ HF download via the `download` runtime), `run_metrics` ingested from structured log lines, job dependencies (`after`, cancelled when a dependency fails), `{{model.path}}`/`{{dataset.path}}` placeholders resolved at claim time, workflows (chain/DAG), LoRA adapter registered as a child model with lineage, serve jobs register/clear a loopback endpoint, tenant GiB-hour budgets with alerts at 80%.
- Gateway: loopback replicas allowed only with `--inference-loopback` (set by `install-server.py`), still no redirects and no forwarded platform token.
- On the Mac mini (real runs): MLX LoRA fine-tune of `mlx-community/SmolLM-135M-Instruct-4bit` on a 24-row toy FAQ dataset, 40 iterations, validation loss 5.33 → 3.84, adapter registered as a child model; `llama-server` deployment of the 1 MB test model answered a chat request through `/v1/chat/completions` (output is toy-model babble) and `undeploy` cleared the endpoint.
- Bugs caught by real runs: mlx-lm's table output was not parsed (fixed, ANSI-safe parser + test); stale endpoints survived cancellation (fixed: cancelled/expired serve jobs clear them, one replica per model).
- UI: Build (Model hub, Datasets, Training with live loss chart, Playground), Run (Workloads, Workflows, Deployments), Govern; CLI `datasets`, `dataset-add`, `train`, `metrics-run`, `deploy`, `undeploy`; native Model hub gained Deploy and a datasets list.
- Not done / limits: single-Mac, single-GPU training only; Playground is non-streaming; a loopback replica is only reachable from the Mac running the control plane; training numbers here come from a 135M toy model and a 24-row dataset and say nothing about real fine-tuning quality or speed.

## Real 1B benchmark (2026-10-14, Mac mini "Apple M6", 12 GPU cores, 16 GiB)
`bartowski/Llama-3.2-1B-Instruct-GGUF` · `Llama-3.2-1B-Instruct-Q4_K_M.gguf` · 807,694,464 bytes · SHA-256 `6f85a640…11df83`, onboarded through Gryvia (HF download, 0.8 GB).
- llama-bench (Homebrew llama.cpp, 512 prompt / 128 generated tokens, 3 repetitions): **5,999 tok/s prompt, 151.3 tok/s generation**.
- 5-case factual prompt suite (greedy decoding): **5/5 passed**, 0.7–2.0 s per case.
- Gryvia's memory estimate for the model: 1.5 GiB. Single run on an otherwise idle Mac; not a statistical benchmark.

## Real LoRA fine-tune of a 1B model (2026-10-14, Mac mini, Apple M6 / 12 GPU cores / 16 GiB)
`mlx-community/Llama-3.2-1B-Instruct-4bit` · 75-row synthetic Gryvia FAQ (15 facts × 5 phrasings, written for this test) · 150 iterations, batch 2, 8 LoRA layers, lr 5e-5, run as a Gryvia `train` job.
- Train loss 4.615 → 0.137; validation loss 5.63 → 0.132. The validation rows come from the same 15 facts, so this measures memorisation of the FAQ, **not** generalisation.
- Throughput (median over reports): ~939 tokens/s training.
- Adapter registered as `mlx-community__Llama-3.2-1B-Instruct-4bit-lora-9199db` with lineage to the base model.
- Spot check (mlx_lm generate, greedy): base model answered "What does drain do?" with plumbing; the adapter answered with the Gryvia definition. It also answered an unrelated MLX question with a Gryvia-flavoured wrong explanation, i.e. the small dataset leaks into out-of-domain answers.
- Fixed in the same session: `llama_serve.py` now exits if its parent agent goes away (an agent restart had left an orphan llama-server).

## Adapter evaluation, fuse and deploy (2026-10-14, Mac mini)
- Eval runtime gained `--engine mlx [--adapter DIR]`; evaluated inside Gryvia on a 4-case FAQ suite: **base model 1/4, LoRA adapter 4/4** (the France control case passes in both). Same caveat as before: the cases are phrased like the training facts.
- New `fuse` job (mlx_lm fuse → `models/_fused/<adapter>`) registers `<adapter>-fused` as a standalone MLX model; `serve --engine mlx` runs it with `mlx_lm.server` and the gateway routes to it (`workerModel=default_model`).
- Through the gateway the fused model answered "What is a tenant?" with the Gryvia definition and "capital of France" correctly, but answered "What does drain do?" with a generic plumbing answer: training used raw `Q:/A:` text while the chat endpoint applies the chat template. Train on chat-formatted rows if you intend to serve through chat.
- Bugs caught by these runs: adapter folder was outside the confined models directory (now `state/runs` is an allowed managed root); mlx_lm.server needed `model: default_model`.
