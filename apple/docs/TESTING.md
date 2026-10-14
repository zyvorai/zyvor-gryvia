# Testing

| Layer | Command | Needs |
|---|---|---|
| Control plane, CLI, runtimes, release scripts | `python3 -m unittest discover -s tests` | Python 3.14 |
| Source headers | `python3 scripts/check-headers.py` | Python |
| Version consistency | `python3 scripts/set-version.py --check` | Python |
| Dashboard flows (fixture server) | `make web-test` | Node 22, Playwright (`npm install`) |
| Native logic | `cd native && swift test` | Xcode / Swift 6 |
| Everything above except web | `make verify` | macOS |
| Agent → API → subprocess → logs → completion | `bash scripts/mac-smoke.sh` | macOS, port 8443 free |
| Package | `scripts/package-mac.sh $(cat VERSION)` | macOS |

## On real hardware

`make deploy MINI=host` syncs, builds, runs the suite and smoke on another Mac and installs launchd services. Real model runs (benchmark, evaluate, fine-tune, deploy) are recorded with their numbers and caveats in `TEST_RESULTS.md`. Nothing in the suite requires a network or a model download.

## What the tests deliberately do not claim

GPU throughput, isolation, energy and fine-tuning quality are measured on specific machines and reported with caveats, not asserted in CI.
