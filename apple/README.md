<div align="center">

<img src="brand/gryvia-icon.svg" width="96" alt="Gryvia">

# Gryvia for Apple Silicon

[![verify](https://github.com/zyvorai/zyvor-gryvia-apple/actions/workflows/ci.yml/badge.svg)](https://github.com/zyvorai/zyvor-gryvia-apple/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-BUSL--1.1-0088ff)](LICENSE)
[![Platforms](https://img.shields.io/badge/macOS%2015%2B%20%C2%B7%20Apple%20silicon-000000)](docs/TESTING.md)
[![Python](https://img.shields.io/badge/control%20plane-python%203.14%20%C2%B7%200%20deps-30d158)](server/app.py)
[![Swift](https://img.shields.io/badge/agent%20%26%20app-swift%206-0088ff)](native/Package.swift)

![Gryvia Live view](site/img/live-dark.jpg)

### Your Macs, scheduled.

**A multi-tenant control plane for Apple Silicon fleets.** It admits and places inference, fine-tuning and benchmark jobs against each Mac's unified-memory budget, with quotas, leases, provenance and a live view of every GPU.

**Memory-aware admission** · **Live GPU cores** · **Model workbench** · **Owner-aware** · **Signed provenance** · **Source-available**

🎮 **[Live demo](https://zyvorai.github.io/zyvor-gryvia-apple/demo/)** · 🌐 **[Site](https://zyvorai.github.io/zyvor-gryvia-apple/)** · 🚀 **[Quickstart](#quickstart)** · 🔒 **[Security](SECURITY.md)** · 📦 **[Releases](docs/RELEASE.md)** · 🧪 **[Results](TEST_RESULTS.md)**

</div>

> **Zyvor AI Labs · 0.3.0 · BUSL-1.1.** Source-available (not OSI open source); each version converts to Apache-2.0 four years after its first public release. Copyright © 2026 Zyvor AI Labs Private Limited.

## Why Gryvia

| When this happens | Gryvia gives you |
|---|---|
| Three Macs, five jobs, and nobody knows what fits where | **Admission control** against each Mac's unified-memory budget, with priorities, tenant quotas and fairness weights |
| "Is the mini alive, and is it throttling?" | A live fleet view: GPU cores lit by measured utilisation, thermal state, drain and resume, alerts |
| The Mac on someone's desk should not run your 3 am fine-tune while they are working | **Owner policy**: run only when idle, yield instantly when they return, respect battery, Low Power Mode and calls |
| "Which Mac produced this model, and was it tampered with?" | **Signed provenance**: node-signed attestations, pinned keys, verified chains, an optional deploy gate |
| You want an AI assistant to check or operate the fleet | **MCP server** (read-only by default) and Shortcuts actions |
| You want to try it without hardware | **Demo mode**: a clearly labelled simulated fleet in the browser |

## What is in the box

- **Control plane** (`server/`): Python standard library + SQLite. Priority, fairness, quotas, dependencies, retries, leases, yield-and-resume, model registry, results, provenance, OpenAI-style gateway with streaming, Prometheus `/metrics`.
- **Agent** (`native/Sources/GryviaAgent`): Swift. Reports GPU model, core count and measured utilisation (IORegistry), memory, thermal state, owner presence and power; runs locally configured runtimes; signs attestations.
- **Apps**: web dashboard (`web/`, also the static demo), SwiftUI Mac app with menu-bar pause and Dock badge, CLI (`scripts/gryvia.py`), MCP server (`mcp/`).
- **Runtimes** (`runtimes/`): llama.cpp (bench, evaluate, quantise, serve), MLX (LoRA, fuse, serve, generate), Core ML, Hugging Face download with size cap and SHA-256.

## Quickstart

1. **Look around, no install.** Open the [live demo](https://zyvorai.github.io/zyvor-gryvia-apple/demo/) and press **Try demo**.
2. **Run it on this Mac.**
   ```bash
   make run-server     # control plane; creates ./local with tokens (0600)
   make run-agent      # in another terminal: this Mac becomes a worker
   open http://127.0.0.1:8443   # paste GRYVIA_ADMIN_TOKEN from local/credentials.env, press "Run demo job"
   ```
3. **Enrol another Mac.** `make deploy MINI=your-mac` syncs, builds, tests and installs launchd services over ssh. `python3 scripts/gryvia.py quickstart` tells you what to do next.

Install the app instead: download the DMG or pkg from the [releases](https://github.com/zyvorai/zyvor-gryvia-apple/releases). Builds are **ad hoc signed and not notarized** unless the release notes say otherwise; verify `shasum -a 256 -c`, then Control-click → Open.

## Honest limits

One GPU per Mac; GPU utilisation is device-wide, not per job; reservations are admission bookkeeping, not isolation; energy is an estimate and needs an optional root helper; training is single-Mac LoRA; there is no sandbox around runtimes, so enrol only Macs you would let an admin run code on. See [SECURITY.md](SECURITY.md) and [TEST_RESULTS.md](TEST_RESULTS.md) for what was measured, on what, and what was not verified.

## Where it fits

Gryvia is not a way to split one model across Macs (see exo and MLX distributed for that). It answers who gets which Mac, when, and within what limits. [docs/COMPARISON.md](docs/COMPARISON.md) has the longer comparison.

## Docs

[Architecture](docs/ARCHITECTURE.md) · [Operations](docs/OPERATIONS.md) · [UX contract](docs/UX-CONTRACT.md) · [Testing](docs/TESTING.md) · [Release](docs/RELEASE.md) · [Feature matrix](docs/FEATURE_MATRIX.md) · [Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md)
