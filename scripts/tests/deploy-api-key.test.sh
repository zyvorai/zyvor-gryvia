#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# deploy_resolve_api_key (scripts/lib/deploy-guards.sh): which key a deploy uses, tested against
# a fake kubectl and a temporary HOME. No cluster, no network.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

# Fake kubectl: prints $FAKE_SECRET_B64 for `get secret ... jsonpath`, or fails when FAKE_KUBECTL_FAIL=1.
cat > "$TMP/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
[[ "${FAKE_KUBECTL_FAIL:-}" == 1 ]] && exit 1
printf '%s' "${FAKE_SECRET_B64:-}"
FAKE
chmod +x "$TMP/bin/kubectl"
export PATH="$TMP/bin:$PATH"

pass=0; fail=0
check() { # check <name> <got> <want>
  if [[ "$2" == "$3" ]]; then pass=$((pass + 1)); echo "  ok   $1"
  else fail=$((fail + 1)); echo "  FAIL $1: got '$2', want '$3'"; fi
}

# run <explicit> : resolve in a fresh HOME; prints "KEY|SOURCE|FILE-CONTENT|MODE"
run() {
  local explicit="$1" home; home="$(mktemp -d -p "$TMP")"
  # shellcheck disable=SC1091
  ( export HOME="$home"; [[ -n "${SAVED:-}" ]] && { mkdir -p "$home/.gryvia"; printf '%s\n' "$SAVED" > "$home/.gryvia/api-key"; }
    source "$ROOT/scripts/lib/deploy-guards.sh"
    deploy_resolve_api_key "$explicit"
    printf '%s|%s|%s|%s' "$API_KEY" "$API_KEY_SOURCE" "$(cat "$home/.gryvia/api-key")" "$(stat -c %a "$home/.gryvia/api-key" 2>/dev/null || stat -f %Lp "$home/.gryvia/api-key")" )
}
field() { cut -d'|' -f"$2" <<<"$1"; }

b64() { printf '%s' "$1" | base64 | tr -d '\n'; }

echo "== first install: nothing saved, no secret"
unset SAVED FAKE_SECRET_B64 FAKE_KUBECTL_FAIL
out="$(run "")"
check "uses the lab default" "$(field "$out" 1)" "Admin@321"
check "says it is the default" "$(field "$out" 2)" "the lab default (first install)"
check "saves it to the file" "$(field "$out" 3)" "Admin@321"
check "file mode 600" "$(field "$out" 4)" "600"

echo "== a rotated key in the cluster Secret is kept on a plain redeploy"
export FAKE_SECRET_B64; FAKE_SECRET_B64="$(b64 'rotated-secret-key')"
SAVED="Admin@321"; out="$(run "")"
check "uses the installed key, not the stale file" "$(field "$out" 1)" "rotated-secret-key"
check "source is the Secret" "$(field "$out" 2)" "the installed gryvia-api-key Secret"
check "the stale file is corrected" "$(field "$out" 3)" "rotated-secret-key"

echo "== an explicit GRYVIA_API_KEY wins over the Secret and the file"
SAVED="old"; out="$(run "from-env")"
check "uses the explicit key" "$(field "$out" 1)" "from-env"
check "source is the env var" "$(field "$out" 2)" "GRYVIA_API_KEY"
check "saves it" "$(field "$out" 3)" "from-env"

echo "== no Secret: the saved file is kept"
unset FAKE_SECRET_B64; SAVED="saved-on-host"; out="$(run "")"
check "uses the saved key" "$(field "$out" 1)" "saved-on-host"
src="$(field "$out" 2)"
check "source is the saved file" "$([[ "$src" == */.gryvia/api-key ]] && echo yes || echo "$src")" yes

echo "== kubectl failing (no cluster access) falls back to the file"
export FAKE_KUBECTL_FAIL=1; SAVED="saved-on-host"; out="$(run "")"
check "uses the saved key" "$(field "$out" 1)" "saved-on-host"
unset FAKE_KUBECTL_FAIL

echo "== keys with shell metacharacters survive"
unset SAVED; out="$(run 'p$a"s s\x'"'"'q')"
check "explicit key is stored verbatim" "$(field "$out" 3)" 'p$a"s s\x'"'"'q'

echo
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
