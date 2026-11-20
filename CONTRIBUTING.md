# Contributing to Gryvia

Thank you for your interest in contributing to Gryvia!

## Code of Conduct

We pledge to make participation in our project a harassment-free experience for everyone. See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## How to Contribute

### Reporting Bugs

Create detailed bug reports with:
- Clear title and description
- Steps to reproduce
- Expected vs actual behavior
- Version information
- Relevant logs

### Pull Requests

1. Fork the repository (source-available under the [Zyvor Production License v1.0](LICENSE); contributions are licensed under it)
2. Create feature branch: `git checkout -b feature/my-feature`
3. Make changes and add tests
4. Commit: `git commit -m "feat: Add feature"`
5. Push: `git push origin feature/my-feature`
6. Create Pull Request

### Commit Message Format

```
<type>: <description>

[optional body]
```

Types: feat, fix, docs, style, refactor, test, chore

## Development Setup

```bash
git clone https://github.com/zyvorai/gryvia.git
cd gryvia
make build          # Build operators, CLI, Web UI and API gateway
make test           # Operator (Go), CLI (Rust) and API gateway (pytest) tests; SDK and web-ui tests are run separately
make lint           # golangci-lint on operators, clippy, web-ui lint, flake8 on the gateway
```

### Prerequisites

- Go 1.27 (see the `go` line in each `go.mod`)
- Rust (stable)
- Node.js 22 (what CI uses)
- Docker / Podman
- kubectl + helm

## Code Style

- **Go**: `gofmt`, `golangci-lint` (v1.57+), production logging (`Development: false`)
- **Rust**: `rustfmt`, `clippy` with `-D warnings`
- **Python**: `black`, `flake8` (the gateway CI job checks `main.py`); avoid bare `Exception`
- **TypeScript**: `prettier`, `eslint`, proper types (no `any`)
- **CSS**: plain global CSS with design tokens, no Tailwind; follow `docs/design/APPLE-UX-CONTRACT.md` (light and dark themes, no hard-coded colors)
- **Dockerfiles**: Multi-arch support (`TARGETARCH` build arg), non-root user, distroless base
- **Helm**: Resource limits required, security contexts, no `privileged: true`
- **CI**: Pin actions to SHA, add `permissions` block, add `concurrency` group

## Security Guidelines

- Never commit credentials or secrets (use `data:` with base64 placeholders)
- Pin container images to specific versions, never use `:latest`
- Use `hmac.compare_digest()` for secret comparison in Python
- Validate all user input at system boundaries
- Set `allowPrivilegeEscalation: false` and drop ALL capabilities

## Testing

- Write unit tests for new code
- Run `make test` before submitting PR
- Add integration tests for features (`tests/e2e/` is manual and not run in CI)
- CI (`repo-checks.yml`) also enforces: no legacy `Fabric[A-Z]` identifiers, generated CRD docs are current, every documented `gryvia ...` command is accepted (`python3 scripts/check-cli-docs.py`), example manifests match the CRD schemas (`python3 scripts/check-examples.py`), and shellcheck on scripts
- Ensure Makefile targets use subshells: `(cd dir && cmd)` not `cd dir && cmd && cd ..`

## Documentation

- Update docs for new features
- Keep README.md architecture section current, and say plainly what is implemented versus planned or unverified (needs GPU or RDMA hardware, a real IdP, payments). Use API version `gryvia.io/v1alpha1`
- Include examples in `examples/` directory

## Community

- GitHub Issues: Bug reports
- GitHub Discussions: Questions
- Documentation: the `docs/` directory and the `website/` docs site

Thank you for contributing!
