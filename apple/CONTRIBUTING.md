# Contributing

1. **Open an issue first** for anything beyond a typo, so we agree on the shape.
2. **Respect the boundaries** in [AGENTS.md](AGENTS.md): the control plane and CLI use only the Python standard library; the agent never shell-interpolates; tokens never touch `localStorage` or logs.
3. **Add tests.** Server behaviour in `tests/test_*.py`, runtime wrappers against fake tools in `tests/test_runtimes.py`, UI flows in `tests/browser.cjs`, native logic in `native/Tests/`.
4. **Run what CI runs:**
   ```bash
   make verify          # Python tests, header check, JS syntax, swift test + release build
   make web-test        # Playwright dashboard test (npm install first)
   bash scripts/mac-smoke.sh   # native agent -> API -> subprocess -> logs -> completion
   ```
   Python 3.14, Swift 6 / Xcode on macOS 15+, Node 22.
5. **UI changes** follow [docs/UX-CONTRACT.md](docs/UX-CONTRACT.md): tokens only (`brand/tokens.json`), light and dark, both window styles, 390 px with no horizontal scroll.
6. **Source headers**: every `.py`, `.sh`, `.swift`, `.js`, `.css` file carries the SPDX line (`python3 scripts/check-headers.py --fix`).
7. Add a line under `## Unreleased` in [CHANGELOG.md](CHANGELOG.md). Version bumps go through `scripts/set-version.py`.

Be honest in docs and results: say what was measured, on what hardware, and what was not verified.

By contributing you agree that your contribution may be incorporated into and distributed as part of Gryvia Apple under the project's current license, including the Business Source License 1.1 and its Change License, and you grant Zyvor AI Labs Private Limited the rights necessary to distribute, sublicense, and commercially license that contribution. You represent that you have the legal right to do so.
