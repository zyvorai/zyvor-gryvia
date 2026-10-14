# Security

Report vulnerabilities privately to **security@zyvor.dev**, or open a private advisory:
https://github.com/zyvorai/zyvor-gryvia-apple/security/advisories/new. Please do not open a public issue for a vulnerability.
Include the version (`gryvia.py --version`), macOS version, and steps to reproduce. Never include a real token.

## What this project protects

Gryvia Apple is a control plane and native agents that run **jobs on your Macs**. Treat it like any scheduler:
whoever holds an admin token can run code on every enrolled Mac that has an enabled runtime.

| Surface | Boundary |
|---|---|
| Tokens | Three roles (`admin`, `tenant`, `agent`), compared in constant time, minimum 24 characters, generated with `secrets` into `credentials.env` (0600). The dashboard keeps a token in tab memory (optionally `sessionStorage` for the tab) and the native app in the login Keychain. Tokens are never written to `localStorage`, logs or job environments. |
| Transport | `http://` is accepted only for loopback; any other origin must be HTTPS (CLI, app and agent enforce this). The server refuses a non-loopback bind without `--cert/--key`. |
| Job execution | The agent runs only runtimes listed in its own `agent.json` (absolute executables). Job arguments are bounded (count, length), never shell-interpolated, and the agent's own tokens are removed from the child environment. |
| Files | Model, dataset and adapter paths are confined to the managed folders (`~/gryvia/state/models`, `…/runs`); downloads accept only https on huggingface.co / hf.co, enforce a size cap and never forward credentials across hosts. |
| Replicas | Deployed models bind to `127.0.0.1`; the gateway allowlists origins, refuses redirects and never forwards the caller's platform token. |
| Tenants | Tenant tokens see and act only on their own jobs; quotas and budgets are enforced at admission. |
| Provenance | Model artifacts carry SHA-256 hashes and (when enabled) node-signed attestations; this is fleet-internal provenance, not a global supply-chain guarantee. |

## Not in scope / known limits

- A Mac with a runtime enabled will execute what an authorised admin or tenant submits; there is **no sandbox** around runtimes. Do not enrol machines you would not let an admin run code on.
- GPU utilisation and memory reservations are measurements and bookkeeping, not isolation.
- Release artifacts are **ad hoc signed** unless a release states otherwise (see `docs/RELEASE.md`); verify checksums.
