# Release status and how to cut a release

## v0.3.0 (in preparation)

Packaging is implemented and exercised locally; nothing has been published yet.

- **Verified locally (macOS 27.2, Apple silicon, Swift 6.4, Python 3.14):** `scripts/package-mac.sh 0.3.0` produced the DMG, pkg and zip; both `.sha256` files verify; the pkg payload contains the app, the agent helper and the icon; `scripts/package-release.sh` produced the server and SDK tarballs, the SBOM and `SHA256SUMS`; the archives contain no `credentials.env`, `config.json`, databases or caches (asserted by a test).
- **Not verified:** the pkg installed on a second Mac; Developer ID signing and notarization (no credentials available); the Homebrew cask (tap not created); the release workflow on GitHub (not yet run).
- **Signing status of artifacts:** ad hoc signed app, unsigned DMG and pkg, not notarized. Gatekeeper blocks the first launch on other Macs: Control-click → Open, or System Settings → Privacy & Security → Open Anyway. Verify with `shasum -a 256 -c Gryvia-0.3.0.dmg.sha256`.

## Cutting a release

1. `python3 scripts/set-version.py X.Y.Z` (updates `VERSION`, `Info.plist`, `pyproject.toml`, `package.json`, README, About panels) and add `## vX.Y.Z` to `CHANGELOG.md`.
2. `make verify` and `bash scripts/mac-smoke.sh` on a Mac; run `scripts/package-mac.sh X.Y.Z` and `scripts/package-release.sh`.
3. Push the tag `vX.Y.Z`. `.github/workflows/release.yml` builds the archives on Ubuntu, the DMG/pkg/zip on macOS, attests them and creates the GitHub release with notes from the changelog.
4. Download the **published** files, check `shasum -c`, install the pkg on a clean Mac, run `gryvia.py doctor`, and record "Verified / Not verified" here.
5. Update `packaging/homebrew/gryvia.rb` (version, sha256) in the tap.

### Signing and notarization (optional, needs your Apple credentials)

Set repository secrets and the workflow signs automatically; locally export the same variables:

| Variable / secret | Meaning |
|---|---|
| `DEVELOPER_ID_APP` | `Developer ID Application: NAME (TEAMID)`: signs the app, agent and DMG |
| `DEVELOPER_ID_INSTALLER` | `Developer ID Installer: NAME (TEAMID)`: signs the pkg |
| `NOTARY_PROFILE` | profile created with `xcrun notarytool store-credentials` |
| `MACOS_CERTS_P12`, `MACOS_CERTS_PASSWORD`, `NOTARY_KEY_P8`, `NOTARY_KEY_ID`, `NOTARY_ISSUER` | CI-only: certificate bundle and App Store Connect API key |

## Artifacts

| File | What |
|---|---|
| `Gryvia-X.Y.Z.dmg` / `.pkg` / `.zip` | Gryvia.app (SwiftUI app + `gryvia-agent` + control plane under `Contents/Resources/gryvia`) |
| `gryvia-server-X.Y.Z.tar.gz` | control plane only (server, runtimes, web, scripts, docs) |
| `gryvia-sdk-X.Y.Z.tar.gz` | Swift sources to build the agent and app |
| `gryvia-X.Y.Z.cdx.json` | CycloneDX SBOM |
| `SHA256SUMS`, `*.sha256` | checksums |
