#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Tests for scripts/release-check.sh: tag parsing, --dry-run, and the pass/fail exit paths against fake
# curl, cosign, helm and kubectl binaries. No network.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
SCRIPT="scripts/release-check.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILED=0
PASSED=0
CODE=0
OUT=""
ok()   { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
fail() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; printf '%s\n' "$OUT" | sed 's/^/       | /' | tail -30; }
has()  { if grep -qF -- "$2" <<<"$OUT"; then ok "$1"; else fail "$1 (expected: $2)"; fi; }
lacks(){ if grep -qF -- "$2" <<<"$OUT"; then fail "$1 (unexpected: $2)"; else ok "$1"; fi; }
code() { if [[ "$CODE" == "$2" ]]; then ok "$1"; else OUT="exit=$CODE"$'\n'"$OUT"; fail "$1 (expected exit $2, got $CODE)"; fi; }

mkdir -p "$TMP/bin"
# curl: token, manifest (index with two platforms), release assets. FAKE_* variables inject failures.
cat >"$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
out="" hdr="" url="" wcode=0
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out="$2"; shift 2 ;;
	-D) hdr="$2"; shift 2 ;;
	-w) wcode=1; shift 2 ;;
	http*) url="$1"; shift ;;
	*) shift ;;
	esac
done
echo "curl $url" >>"$FAKE_LOG"
emit() { if [ -n "$out" ]; then printf '%s' "$1" >"$out"; else printf '%s' "$1"; fi; }
case "$url" in
*/token?*) emit '{"token":"anon"}' ;;
*/manifests/*)
	img="${url##*/v2/}"; img="${img%%/manifests*}"
	if [ -n "${FAKE_MISSING:-}" ] && [[ "$img" == *"$FAKE_MISSING"* ]]; then
		[ -n "$out" ] && echo '{"errors":[{"code":"MANIFEST_UNKNOWN"}]}' >"$out"
		[ "$wcode" = 1 ] && printf '404'
		exit 0
	fi
	if [ "${FAKE_SINGLE_ARCH:-0}" = 1 ]; then body='{"schemaVersion":2,"config":{}}'
	else body='{"manifests":[{"platform":{"os":"linux","architecture":"amd64"}},{"platform":{"os":"linux","architecture":"arm64"}},{"platform":{"os":"unknown","architecture":"unknown"}}]}'; fi
	printf '%s' "$body" >"$out"
	[ -n "$hdr" ] && printf 'HTTP/2 200\r\nDocker-Content-Digest: sha256:%064d\r\n' 1 >"$hdr"
	[ "$wcode" = 1 ] && printf '200'
	;;
*.sha256)
	f="${url%.sha256}"
	if [[ "$f" == *gryvia-crds.yaml ]]; then body=$'---\nkind: CustomResourceDefinition\n---\nkind: CustomResourceDefinition\n'; else body="content of ${f##*/}"; fi
	if [ "${FAKE_BAD_SHA:-0}" = 1 ] && [[ "$f" == *linux-arm64 ]]; then h=deadbeef; else h="$(printf '%s' "$body" | shasum -a 256 | cut -d' ' -f1)"; fi
	emit "$h  ${f##*/}"
	;;
*/releases/download/*)
	[ "${FAKE_NO_RELEASE:-0}" = 1 ] && exit 22
	if [[ "$url" == *gryvia-crds.yaml ]]; then emit $'---\nkind: CustomResourceDefinition\n---\nkind: CustomResourceDefinition\n'
	else emit "content of ${url##*/}"; fi
	;;
*) exit 6 ;;
esac
exit 0
FAKE
cat >"$TMP/bin/cosign" <<'FAKE'
#!/usr/bin/env bash
echo "cosign $*" >>"$FAKE_LOG"
if [ -n "${FAKE_COSIGN_FAIL:-}" ] && [[ "$*" == *"$FAKE_COSIGN_FAIL"* ]]; then echo "Error: no matching signatures" >&2; exit 1; fi
exit 0
FAKE
cat >"$TMP/bin/helm" <<'FAKE'
#!/usr/bin/env bash
echo "helm $*" >>"$FAKE_LOG"
case "$1" in
pull)
	[ "${FAKE_NO_CHART:-0}" = 1 ] && { echo "Error: not found" >&2; exit 1; }
	dest=""; name="${2##*/}"
	while [ $# -gt 0 ]; do [ "$1" = --untardir ] && dest="$2"; shift; done
	mkdir -p "$dest/$name"; printf 'name: %s\n' "$name" >"$dest/$name/Chart.yaml" ;;
template) [ "${FAKE_BAD_TEMPLATE:-0}" = 1 ] && { echo "Error: template broke" >&2; exit 1; } ;;
esac
exit 0
FAKE
cat >"$TMP/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
echo "kubectl $*" >>"$FAKE_LOG"
case "$*" in *"config current-context"*) echo fake-ctx ;; *"get deploy -o name"*) echo deployment.apps/gryvia-ui ;; esac
[ "${FAKE_ROLLOUT_FAIL:-0}" = 1 ] && [[ "$*" == *rollout* ]] && exit 1
exit 0
FAKE
chmod +x "$TMP"/bin/*

run() { # run [ENV=VAL ...] -- args
	local envs=()
	while [[ $# -gt 0 && "$1" != "--" ]]; do envs+=("$1"); shift; done
	shift
	: >"$TMP/calls.log"
	OUT="$(env "${envs[@]}" PATH="$TMP/bin:$PATH" FAKE_LOG="$TMP/calls.log" bash "$SCRIPT" "$@" 2>&1)"
	CODE=$?
}

echo "arguments"
run -- 1.0.0
code "a tag without v exits 64" 64
run --
code "no tag exits 64" 64
run -- v1.0
code "an incomplete version exits 64" 64
run -- v1.0.0 --bogus
code "unknown option exits 64" 64
run -- v1.0.0-rc1 --dry-run
code "a release candidate tag is accepted" 0
has "images use the version without v" "gryvia-ebpf-collector:1.0.0-rc1"
has "cosign identity names release.yml" "release\\.yml@refs/.+"
has "CRD bundle is listed" "releases/download/v1.0.0-rc1/gryvia-crds.yaml"
run -- v1.0.0 --dry-run --install --registry example.io/acme --repo acme/g
has "custom registry" "example.io/acme/gryvia-ui:1.0.0"
has "custom repo identity" "https://github.com/acme/g/"
has "install is planned" "helm upgrade --install gryvia oci://example.io/acme/charts/gryvia"

echo "everything is fine"
run -- v1.0.0
code "passes" 0
has "result" "RESULT: PASS"
has "checks the collector image" "gryvia-ebpf-collector:1.0.0 pullable anonymously"
has "reports both platforms" "linux/amd64 linux/arm64"
has "verifies signatures" "PASS cosign gryvia-ui"
has "renders the charts" "PASS helm template gryvia"
has "verifies checksums" "PASS asset gryvia-v1.0.0-linux-arm64 checksum"
has "counts the CRDs" "2 CRDs"
has "install is skipped by default" "SKIP install on a cluster"
if grep -q "^curl https://ghcr.io/token" "$TMP/calls.log"; then ok "gets an anonymous token"; else fail "no token request"; fi
if grep -q "certificate-identity-regexp" "$TMP/calls.log" && grep -q "certificate-oidc-issuer https://token.actions.githubusercontent.com" "$TMP/calls.log"; then ok "cosign gets identity and issuer"; else OUT="$(cat "$TMP/calls.log")"; fail "cosign flags"; fi

echo "failures"
run FAKE_MISSING=ebpf-collector -- v1.0.0
code "a missing image exits 1" 1
has "names it" "FAIL image gryvia-ebpf-collector:1.0.0 pullable anonymously"
has "explains" "HTTP 404"
run FAKE_SINGLE_ARCH=1 -- v1.0.0
code "a single-arch image exits 1" 1
has "explains the platforms" "want linux/amd64 linux/arm64"
run FAKE_COSIGN_FAIL=gryvia-ui@ -- v1.0.0
code "a bad signature exits 1" 1
has "names the image" "FAIL cosign gryvia-ui"
run FAKE_NO_CHART=1 -- v1.0.0
code "a missing chart exits 1" 1
has "skips template" "SKIP helm template gryvia"
run FAKE_BAD_TEMPLATE=1 -- v1.0.0
code "a chart that does not render exits 1" 1
run FAKE_BAD_SHA=1 -- v1.0.0
code "a wrong checksum exits 1" 1
has "shows both hashes" "expected deadbeef"
run FAKE_NO_RELEASE=1 -- v1.0.0
code "missing release assets exit 1" 1
run FAKE_NO_RELEASE=1 -- v1.0.0 --skip-download
code "--skip-download skips them" 0
run FAKE_COSIGN_FAIL=gryvia -- v1.0.0 --skip-cosign
code "--skip-cosign skips signatures" 0
lacks "does not call cosign" "PASS cosign"

echo "install"
run -- v1.0.0 --install
code "install passes" 0
has "installs" "PASS helm install gryvia 1.0.0"
has "waits for the rollout" "PASS deployments roll out"
if grep -q "helm uninstall" "$TMP/calls.log"; then ok "cleans up"; else fail "did not uninstall"; fi
run -- v1.0.0 --install --keep-install
if grep -q "helm uninstall" "$TMP/calls.log"; then fail "--keep-install uninstalled"; else ok "--keep-install keeps it"; fi
run FAKE_ROLLOUT_FAIL=1 -- v1.0.0 --install
code "a failed rollout exits 1" 1

echo
echo "$PASSED passed, $FAILED failed"
[[ "$FAILED" == 0 ]]
