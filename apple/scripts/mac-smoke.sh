#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
set -euo pipefail
cd "$(dirname "$0")/.."
PY="${PYTHON:-python3}"
workdir=$(mktemp -d)
server_pid=''
agent_pid=''
cleanup() {
  [[ -z "$agent_pid" ]] || kill "$agent_pid" 2>/dev/null || true
  [[ -z "$server_pid" ]] || kill "$server_pid" 2>/dev/null || true
  rm -rf "$workdir"
}
trap cleanup EXIT
(cd native && swift build --product gryvia-agent)
"$PY" scripts/configure.py --output "$workdir/config"
source "$workdir/config/credentials.env"
"$PY" -m server.app --config "$workdir/config/config.json" --db "$workdir/db" > "$workdir/server.log" 2>&1 &
server_pid=$!
for _ in {1..30}; do
  if curl -fsS http://127.0.0.1:8443/healthz >/dev/null; then break; fi
  sleep 0.2
done
native/.build/debug/gryvia-agent "$workdir/config/agent.json" > "$workdir/agent.log" 2>&1 &
agent_pid=$!
"$PY" scripts/gryvia.py submit examples/demo-job.json > "$workdir/job.json"
"$PY" - "$workdir/job.json" <<'PY'
import json,os,sys,time,urllib.request
ident=json.load(open(sys.argv[1]))['id']
for _ in range(30):
    req=urllib.request.Request('http://127.0.0.1:8443/api/state',headers={'Authorization':'Bearer '+os.environ['GRYVIA_ADMIN_TOKEN']})
    with urllib.request.urlopen(req) as r:state=json.load(r)
    job=next(j for j in state['jobs'] if j['id']==ident)
    if job['state']=='succeeded':print('Swift agent → API → subprocess → logs → completion: PASS');break
    if job['state'] in ['failed','cancelled']:raise SystemExit('Smoke failed: '+str(job))
    time.sleep(1)
else:raise SystemExit('Smoke timeout')
PY
