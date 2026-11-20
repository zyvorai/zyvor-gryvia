#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Verify a published Gryvia release from the outside, the way a user sees it:
#   1. every image exists for the tag and can be pulled anonymously (registry API, no credentials), for amd64 and arm64
#   2. cosign verifies the images and charts against release.yml's keyless identity
#   3. both Helm charts pull from OCI and `helm template` renders them
#   4. the CLI binaries, gryvia-crds.yaml and their checksums are on the GitHub release and match
#   5. optionally (--install) the chart installs on the current kube context and its deployments roll out
# It prints a checklist and exits non-zero if anything failed. It never pushes tags or triggers workflows.
#
# usage: scripts/release-check.sh vX.Y.Z[-rcN] [--install] [--skip-cosign] [--skip-download]
#            [--registry ghcr.io/zyvorai] [--repo zyvorai/gryvia] [--namespace NS] [--keep-install] [--dry-run]
# Needs curl, python3, helm; cosign unless --skip-cosign; kubectl with --install.
# Not run against a real release by the author (none is published yet): tests use fakes
# (scripts/tests/release-check.test.sh).
set -uo pipefail

usage() { sed -n '2,/^set -uo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }

TAG="" INSTALL=0 SKIP_COSIGN=0 SKIP_DL=0 REGISTRY=ghcr.io/zyvorai REPO=zyvorai/gryvia NS=gryvia-release-check KEEP=0 DRY=0
while [ $# -gt 0 ]; do
	case "$1" in
	--install) INSTALL=1; shift ;;
	--skip-cosign) SKIP_COSIGN=1; shift ;;
	--skip-download) SKIP_DL=1; shift ;;
	--registry) REGISTRY="${2:?}"; shift 2 ;;
	--repo) REPO="${2:?}"; shift 2 ;;
	--namespace) NS="${2:?}"; shift 2 ;;
	--keep-install) KEEP=1; shift ;;
	--dry-run) DRY=1; shift ;;
	-h | --help) usage; exit 0 ;;
	-*) echo "unknown option: $1" >&2; exit 64 ;;
	*) [ -z "$TAG" ] || { echo "only one tag" >&2; exit 64; }; TAG="$1"; shift ;;
	esac
done
if ! [[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
	echo "usage: release-check.sh vX.Y.Z[-rcN] [options]  (got '${TAG:-nothing}')" >&2
	exit 64
fi
VER="${TAG#v}"
HOST="${REGISTRY%%/*}"
ORG="${REGISTRY#*/}"
IMAGES=(gpu-operator ai-operator quota-operator storage-operator network-operator network-intelligence-operator ebpf-collector api-gateway ui)
CHARTS=(gryvia network-intelligence)
CLI_TARGETS=(linux-amd64 linux-arm64 darwin-arm64)
IDENTITY_RE="^https://github.com/${REPO}/\\.github/workflows/release\\.yml@refs/.+\$"
ISSUER=https://token.actions.githubusercontent.com

if [ "$DRY" = 1 ]; then
	echo "release-check $TAG (version $VER)"
	for i in "${IMAGES[@]}"; do echo "  image  $REGISTRY/gryvia-$i:$VER  (anonymous manifest GET on $HOST, amd64+arm64)"; done
	for i in "${IMAGES[@]}"; do echo "  cosign verify --certificate-identity-regexp '$IDENTITY_RE' --certificate-oidc-issuer $ISSUER $REGISTRY/gryvia-$i@<digest>"; done
	for c in "${CHARTS[@]}"; do echo "  chart  helm pull oci://$REGISTRY/charts/$c --version $VER; helm template; cosign verify"; done
	for t in "${CLI_TARGETS[@]}"; do echo "  cli    https://github.com/$REPO/releases/download/$TAG/gryvia-$TAG-$t (+ .sha256)"; done
	echo "  crds   https://github.com/$REPO/releases/download/$TAG/gryvia-crds.yaml (+ .sha256)"
	[ "$INSTALL" = 1 ] && echo "  install helm upgrade --install gryvia oci://$REGISTRY/charts/gryvia --version $VER -n $NS on the current kube context, wait, uninstall"
	exit 0
fi

command -v curl >/dev/null || { echo "curl is required" >&2; exit 69; }
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 69; }
command -v helm >/dev/null || { echo "helm is required" >&2; exit 69; }
[ "$SKIP_COSIGN" = 1 ] || command -v cosign >/dev/null || { echo "cosign is required (or pass --skip-cosign)" >&2; exit 69; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
NAMES=() RES=() NOTE=()
FAILS=0
rec() { NAMES+=("$1") RES+=("$2") NOTE+=("${3:-}"); [ "$2" = FAIL ] && FAILS=$((FAILS + 1)); printf '  %-4s %-52s %s\n' "$2" "$1" "${3:-}"; }

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# registry_manifest REPOSITORY REF: anonymous token + manifest GET. Sets MANIFEST_FILE, DIGEST, HTTP; returns 0 on 200.
ACCEPT='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
registry_manifest() {
	local repo="$1" ref="$2" tok
	tok="$(curl -sS --max-time 30 "https://$HOST/token?service=$HOST&scope=repository:$repo:pull" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token",""))' 2>/dev/null)"
	MANIFEST_FILE="$WORK/manifest.json"
	HTTP="$(curl -sS --max-time 30 -o "$MANIFEST_FILE" -D "$WORK/headers.txt" -w '%{http_code}' \
		-H "Accept: $ACCEPT" ${tok:+-H "Authorization: Bearer $tok"} "https://$HOST/v2/$repo/manifests/$ref" 2>/dev/null || echo 000)"
	DIGEST="$(tr -d '\r' <"$WORK/headers.txt" 2>/dev/null | awk 'tolower($1)=="docker-content-digest:"{print $2}' | tail -n1)"
	[ "$HTTP" = 200 ]
}

platforms() { python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
print(" ".join(sorted({m["platform"]["os"] + "/" + m["platform"]["architecture"] for m in d.get("manifests", []) if "platform" in m and m["platform"]["architecture"] != "unknown"})))
' "$1" 2>/dev/null; }

cosign_verify() { # cosign_verify LABEL REF@DIGEST
	if [ "$SKIP_COSIGN" = 1 ]; then rec "$1" SKIP "--skip-cosign"; return; fi
	if [ -z "$DIGEST" ]; then rec "$1" FAIL "no digest to verify"; return; fi
	if out="$(cosign verify --certificate-identity-regexp "$IDENTITY_RE" --certificate-oidc-issuer "$ISSUER" "$2" 2>&1 >/dev/null)"; then rec "$1" PASS; else rec "$1" FAIL "$(printf '%s' "$out" | tail -n1 | cut -c1-100)"; fi
}

echo "== images ($REGISTRY, tag $VER)"
DIGESTS=()
for i in "${IMAGES[@]}"; do
	if registry_manifest "$ORG/gryvia-$i" "$VER"; then
		p="$(platforms "$MANIFEST_FILE")"
		case " $p " in
		*" linux/amd64 "*)
			case " $p " in *" linux/arm64 "*) has_both=1 ;; *) has_both=0 ;; esac ;;
		*) has_both=0 ;;
		esac
		case "$has_both" in
		1) rec "image gryvia-$i:$VER pullable anonymously" PASS "$p" ;;
		*) rec "image gryvia-$i:$VER pullable anonymously" FAIL "platforms: ${p:-none (single-arch manifest?)}; want linux/amd64 linux/arm64" ;;
		esac
		DIGESTS+=("$DIGEST")
	else
		rec "image gryvia-$i:$VER pullable anonymously" FAIL "HTTP $HTTP (missing, or the package is private)"
		DIGESTS+=("")
	fi
done

echo "== signatures (identity $IDENTITY_RE)"
for idx in "${!IMAGES[@]}"; do
	DIGEST="${DIGESTS[$idx]}"
	cosign_verify "cosign gryvia-${IMAGES[$idx]}" "$REGISTRY/gryvia-${IMAGES[$idx]}@$DIGEST"
done

echo "== charts"
for c in "${CHARTS[@]}"; do
	rm -rf "${WORK:?}/chart-$c"
	mkdir -p "$WORK/chart-$c"
	if helm pull "oci://$REGISTRY/charts/$c" --version "$VER" --untar --untardir "$WORK/chart-$c" >"$WORK/helm.log" 2>&1; then
		rec "chart $c $VER pulls from OCI" PASS
		if out="$(helm template rc "$WORK/chart-$c/$c" -n gryvia-system 2>&1 >/dev/null)"; then rec "helm template $c" PASS; else rec "helm template $c" FAIL "$(printf '%s' "$out" | tail -n1 | cut -c1-100)"; fi
	else
		rec "chart $c $VER pulls from OCI" FAIL "$(tail -n1 "$WORK/helm.log" | cut -c1-100)"
		rec "helm template $c" SKIP "chart not pulled"
	fi
	if registry_manifest "$ORG/charts/$c" "$VER"; then cosign_verify "cosign chart $c" "$REGISTRY/charts/$c@$DIGEST"; else DIGEST=""; cosign_verify "cosign chart $c" ""; fi
done

echo "== GitHub release assets"
if [ "$SKIP_DL" = 1 ]; then
	rec "CLI binaries and CRD bundle" SKIP "--skip-download"
else
	base="https://github.com/$REPO/releases/download/$TAG"
	assets=()
	for t in "${CLI_TARGETS[@]}"; do assets+=("gryvia-$TAG-$t"); done
	assets+=(gryvia-crds.yaml)
	for a in "${assets[@]}"; do
		f="$WORK/$a"
		if ! curl -fsSL --max-time 120 -o "$f" "$base/$a" 2>/dev/null || ! curl -fsSL --max-time 30 -o "$f.sha256" "$base/$a.sha256" 2>/dev/null; then
			rec "asset $a and its .sha256" FAIL "download failed"
			continue
		fi
		want="$(awk '{print $1; exit}' "$f.sha256")"
		got="$(sha "$f")"
		if [ "$want" = "$got" ]; then rec "asset $a checksum" PASS; else rec "asset $a checksum" FAIL "expected $want, got $got"; fi
	done
	if [ -s "$WORK/gryvia-crds.yaml" ]; then
		n="$(grep -c '^kind: CustomResourceDefinition' "$WORK/gryvia-crds.yaml" || true)"
		if [ "${n:-0}" -ge 1 ]; then rec "gryvia-crds.yaml holds CRDs" PASS "$n CRDs"; else rec "gryvia-crds.yaml holds CRDs" FAIL "no CustomResourceDefinition in it"; fi
	fi
fi

if [ "$INSTALL" = 1 ]; then
	echo "== install on kube context '$(kubectl config current-context 2>/dev/null || echo none)' (namespace $NS)"
	if ! command -v kubectl >/dev/null; then
		rec "install chart" FAIL "kubectl not found"
	elif helm upgrade --install gryvia "oci://$REGISTRY/charts/gryvia" --version "$VER" -n "$NS" --create-namespace \
		--set namespace.create=false --set nvidiaDevicePlugin.enabled=false --set dcgmExporter.enabled=false --set monitoring.enabled=false \
		--wait --timeout 300s >"$WORK/install.log" 2>&1; then
		rec "helm install gryvia $VER" PASS
		bad=0
		for d in $(kubectl -n "$NS" get deploy -o name 2>/dev/null); do kubectl -n "$NS" rollout status "$d" --timeout=300s >/dev/null 2>&1 || bad=1; done
		if [ "$bad" = 0 ]; then rec "deployments roll out" PASS; else rec "deployments roll out" FAIL "see: kubectl -n $NS get pods"; fi
		if [ "$KEEP" = 0 ]; then helm uninstall gryvia -n "$NS" >/dev/null 2>&1; kubectl delete namespace "$NS" --wait=false >/dev/null 2>&1; fi
	else
		rec "helm install gryvia $VER" FAIL "$(tail -n1 "$WORK/install.log" | cut -c1-100)"
	fi
else
	rec "install on a cluster" SKIP "pass --install to try it on the current kube context"
fi

echo
echo "release-check $TAG"
printf '| %-4s | %-52s | %s |\n' Result Check Detail
printf '|------|%s|--------|\n' "$(printf -- '-%.0s' $(seq 1 54))"
for i in "${!NAMES[@]}"; do printf '| %-4s | %-52s | %s |\n' "${RES[$i]}" "${NAMES[$i]}" "${NOTE[$i]}"; done
echo
if [ "$FAILS" -gt 0 ]; then echo "RESULT: FAIL ($FAILS failed)"; exit 1; fi
echo "RESULT: PASS"
