# Agent and contributor boundaries

- **Dependency-free control plane**: `server/`, `scripts/gryvia.py` and `runtimes/` import only the Python standard library (3.14). Test tooling (Playwright) is dev-only.
- **No shell interpolation**: runtimes are absolute executables from `agent.json`; arguments are passed as an argv array.
- **Confinement**: any path from a job is resolved and must live under the managed models or runs folders (`runtimes/_common.py: confine`).
- **Secrets**: never in `localStorage`, logs, job environments, screenshots or test fixtures other than the fake `browser-test-admin-token-…`.
- **Honest numbers**: GPU utilisation is device-wide; reservations are bookkeeping; energy is an estimate; say so in the UI and docs.
- **Brand**: colours come from `brand/tokens.json` (`scripts/gen-brand.py`); never hard-code the accent.
- **Verification culture**: `TEST_RESULTS.md` and `docs/RELEASE.md` list what was verified, where, and what was not.
