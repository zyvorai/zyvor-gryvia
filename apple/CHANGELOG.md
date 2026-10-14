# Changelog

## Unreleased

- Idle-time harvesting, power/thermal policy and energy estimates, MCP server and App Intents, signed model provenance (in progress for 0.3.0).

## v0.3.0 (unreleased)

### Added
- **Model workbench**: Hugging Face onboarding with SHA-256 and size cap, llama.cpp benchmarks, prompt-suite evaluation (GGUF and MLX/adapters), quantise, compare, streaming Playground.
- **Training and run**: datasets, MLX LoRA fine-tuning with live loss, adapter fusing, workflows with dependencies, llama-server / mlx_lm.server deployments behind the gateway, tenant GiB-hour budgets, Prometheus `/metrics`.
- **Live view**: GPU detection (one GPU per Mac; core count and measured utilisation from the IORegistry), real-time queue → admission → Mac flow, scheduler decision feed, SSE stream.
- **Release engineering**: DMG, pkg and zip (`scripts/package-mac.sh`), server and SDK tarballs, `SHA256SUMS`, CycloneDX SBOM, Homebrew cask template, single-source `VERSION`.
- **Brand and docs**: Zyvor blue tokens, G icon, macOS 27 system fonts, BUSL-1.1 headers, UX contract, security policy.

### Fixed
- Agent no longer deadlocks in `Process.waitUntilExit()` after a child exits.
- Eval results larger than the 2,000-character log cap are assembled from per-case lines.
- Cancelled or expired deployments clear their endpoint; MLX replicas route with `default_model`.
- Dock tile shows the Gryvia icon independent of the LaunchServices cache.

## v0.2.0

Web dashboard v2 (command palette, demo mode, timeline, alerts, templates, export), native Live/Model hub, CLI tables, `make deploy` to a remote Mac, Python 3.14 / Swift 6.

## v0.1.0 — 2026-10-14

Initial engineering beta: durable scheduling/control-plane API, native Swift worker and SwiftUI app, fleet web console, runtime wrappers, tenant policy, inference routing, allocation metering, launchd deployment and CI.
