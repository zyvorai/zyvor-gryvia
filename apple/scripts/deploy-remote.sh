#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
# Deploy Gryvia Apple to a remote Mac (default host: mini) over ssh.
#
# Usage: scripts/deploy-remote.sh [HOST] [--dir DIR] [--quick] [--verify-only] [--dry-run]
#   HOST           ssh host or alias (default: mini)
#   --dir DIR      checkout dir on the remote, relative to $HOME (default: gryvia)
#   --quick        skip the remote test suite and smoke run
#   --verify-only  only check the existing deployment (launchd + health)
#   --dry-run      print the plan, change nothing
#
# Idempotent. Secrets live in DIR/state/credentials.env (0600) and are never
# regenerated once present. Server binds to 127.0.0.1:8443 on the remote; reach it with
#   ssh -L 8443:127.0.0.1:8443 HOST
set -euo pipefail

HOST=mini; DIR=gryvia; QUICK=0; VERIFY=0; DRY=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir) DIR="$2"; shift 2 ;;
    --quick) QUICK=1; shift ;;
    --verify-only) VERIFY=1; shift ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *) HOST="$1"; shift ;;
  esac
done

cd "$(dirname "$0")/.."
G=$'\033[32m'; Y=$'\033[33m'; R=$'\033[31m'; B=$'\033[1m'; N=$'\033[0m'
step() { printf '%s==>%s %s\n' "$B" "$N" "$*"; }
ok()   { printf '  %s✔%s %s\n' "$G" "$N" "$*"; }
die()  { printf '  %s✘%s %s\n' "$R" "$N" "$*" >&2; exit 1; }

SOCK="${TMPDIR:-/tmp}/gryvia-ssh-$$"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new
     -o ControlMaster=auto -o ControlPersist=60 -o ControlPath="$SOCK")
# Run a remote command with Homebrew on PATH and pipefail, so `| tail` cannot hide failures.
rsh() { ssh "${SSH_OPTS[@]}" "$HOST" "export PATH=/opt/homebrew/bin:/usr/local/bin:\$PATH; set -o pipefail; $*"; }
trap 'ssh -O exit -o ControlPath="$SOCK" "$HOST" >/dev/null 2>&1 || true' EXIT

if [[ $DRY == 1 ]]; then
  step "Dry run: would deploy to $HOST:~/$DIR"
  cat <<PLAN
  1. ssh preflight (macOS, Swift, Python >= 3.12)
  2. rsync apple/ -> ~/$DIR/app  (excluding .build, node_modules, local, dist)
  3. swift build -c release --product gryvia-agent$([[ $QUICK == 1 ]] || echo ' + verify.sh + mac-smoke.sh')
  4. create ~/$DIR/state/{config.json,agent.json,credentials.env} if missing (0600)
  5. install launchd: dev.zyvor.gryvia.server, dev.zyvor.gryvia.agent
  6. health check http://127.0.0.1:8443/healthz and node registration
PLAN
  exit 0
fi

step "Preflight on $HOST"
rsh true || die "cannot ssh to '$HOST' (is it awake? try: ssh $HOST)"
REMOTE_PY=$(rsh 'for p in /opt/homebrew/bin/python3.14 /opt/homebrew/bin/python3.13 /opt/homebrew/bin/python3.12 /usr/local/bin/python3 python3; do
  c=$(command -v "$p" 2>/dev/null) || continue
  "$c" -c "import sys;sys.exit(0 if sys.version_info>=(3,12) else 1)" 2>/dev/null && { echo "$c"; exit 0; }
done; exit 1') || die "no Python >= 3.12 on $HOST (install with: brew install python@3.14)"
rsh 'command -v swift >/dev/null' || die "swift not found on $HOST (xcode-select --install)"
ok "macOS $(rsh sw_vers -productVersion), $(rsh "$REMOTE_PY --version"), $(rsh 'swift --version 2>&1 | head -1' | cut -c1-60)"

# Full Xcode (SwiftUI macros + XCTest) vs Command Line Tools only.
if rsh 'test -d /Applications/Xcode.app && DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer xcodebuild -version >/dev/null 2>&1'; then
  XCODE=1; DEVDIR=/Applications/Xcode.app/Contents/Developer
  ok "Xcode found: full build + XCTest"
else
  XCODE=0; DEVDIR=
  printf '  %s!%s Xcode missing or license not accepted: agent-only build, XCTest skipped\n' "$Y" "$N"
fi
if [[ $XCODE == 1 ]]; then BUILD_ARGS=''; SKIP_TEST=0; else BUILD_ARGS='--product gryvia-agent'; SKIP_TEST=1; fi
rsh() { ssh "${SSH_OPTS[@]}" "$HOST" "export PATH=/opt/homebrew/bin:/usr/local/bin:\$PATH; ${DEVDIR:+export DEVELOPER_DIR=$DEVDIR;} set -o pipefail; $*"; }

REMOTE_HOME=$(rsh 'printf %s "$HOME"')
APP="$REMOTE_HOME/$DIR/app"; STATE="$REMOTE_HOME/$DIR/state"

health() {
  rsh 'for i in 1 2 3 4 5 6 7 8 9 10; do curl -fs http://127.0.0.1:8443/healthz && exit 0; sleep 1; done; exit 1' >/dev/null \
    || die "server did not become healthy (see: make mini-logs)"
  ok "server healthy on $HOST:8443"
  rsh "launchctl list | grep -E 'dev.zyvor.gryvia' || true" | sed 's/^/    /'
  if rsh "test -f '$STATE/credentials.env'"; then
    rsh "set -a; . '$STATE/credentials.env'; '$REMOTE_PY' '$APP/scripts/gryvia.py' --json status" >/dev/null \
      && ok "admin token accepted" || die "admin token rejected"
  fi
}

if [[ $VERIFY == 1 ]]; then step "Verify only"; health; exit 0; fi

step "Sync to $HOST:~/$DIR/app"
rsh "mkdir -p '$APP' '$STATE' && chmod 700 '$STATE'"
rsync -az --delete --exclude .build --exclude node_modules --exclude local --exclude dist \
  --exclude __pycache__ --exclude '*.db' -e "ssh ${SSH_OPTS[*]}" ./ "$HOST:$APP/"
ok "synced"

step "Build"
rsh "cd '$APP/native' && swift build -c release $BUILD_ARGS 2>&1 | tail -3"
ok "swift build -c release"

if [[ $QUICK == 0 ]]; then
  step "Test"
  rsh "cd '$APP' && REQUIRE_SWIFT=1 SKIP_SWIFT_TEST=$SKIP_TEST SWIFT_BUILD_ARGS='$BUILD_ARGS' PYTHON='$REMOTE_PY' bash scripts/verify.sh 2>&1 | tail -6"
  ok "verify.sh"
  # smoke binds :8443, so stop the service first and restart it afterwards
  rsh "launchctl bootout gui/\$(id -u)/dev.zyvor.gryvia.server 2>/dev/null; cd '$APP' && PYTHON='$REMOTE_PY' bash scripts/mac-smoke.sh 2>&1 | tail -2"
  ok "mac-smoke.sh"
fi

step "Configure"
if rsh "test -f '$STATE/credentials.env'"; then
  ok "existing credentials kept ($STATE/credentials.env)"
else
  rsh "cd '$APP' && '$REMOTE_PY' scripts/configure.py --output '$STATE' --node \"\$(scutil --get LocalHostName 2>/dev/null || hostname -s)\" --python '$REMOTE_PY'" | sed 's/^/    /'
  ok "generated tokens"
fi

step "Runtimes"
rsh "cd '$APP' && '$REMOTE_PY' scripts/update-runtimes.py '$STATE/agent.json' --python '$REMOTE_PY'" | sed 's/^/    /'

step "Install launchd services"
rsh "cd '$APP' && '$REMOTE_PY' scripts/install-server.py --python '$REMOTE_PY' --root '$APP' --config '$STATE/config.json' --db '$STATE/gryvia.db'"
rsh "set -a; . '$STATE/credentials.env'; cd '$APP' && '$REMOTE_PY' scripts/install-agent.py --binary '$APP/native/.build/release/gryvia-agent' --config '$STATE/agent.json' --replace"
ok "services installed"

step "Health"
health
for i in 1 2 3 4 5 6 7 8 9 10; do
  rsh "set -a; . '$STATE/credentials.env'; '$REMOTE_PY' '$APP/scripts/gryvia.py' --json status | '$REMOTE_PY' -c 'import json,sys;sys.exit(0 if any(n[\"online\"] for n in json.load(sys.stdin)[\"nodes\"]) else 1)'" && { ok "agent registered and online"; break; }
  [[ $i == 10 ]] && die "agent did not register (see: make mini-logs)"; sleep 2
done

cat <<DONE

${B}Deployed.${N}
  dashboard   ssh -L 8443:127.0.0.1:8443 $HOST   then open http://127.0.0.1:8443
  token       $HOST:$STATE/credentials.env
  CLI         ssh $HOST 'set -a; . $STATE/credentials.env; $REMOTE_PY $APP/scripts/gryvia.py status'
  logs        make mini-logs
DONE
