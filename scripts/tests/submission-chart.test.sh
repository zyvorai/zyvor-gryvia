#!/usr/bin/env bash
# Default render leaves job creation unchecked; enforce adds only the gateway env var, no RBAC.
set -euo pipefail
cd "$(dirname "$0")/../.."
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
helm template gryvia helm/gryvia -n gryvia-system --kube-version 1.33.0 > "$tmpdir/off.yaml"
helm template gryvia helm/gryvia -n gryvia-system --kube-version 1.33.0 \
  --set apiGateway.submissionAdmission.enforce=true > "$tmpdir/on.yaml"
python3 - "$tmpdir" <<'PY'
import pathlib
import sys
import yaml

root = pathlib.Path(sys.argv[1])
rendered = {}
for enabled, file in [(False, 'off.yaml'), (True, 'on.yaml')]:
    objects = [o for o in yaml.safe_load_all((root / file).read_text()) if o]
    gateway = next(o for o in objects if o['kind'] == 'Deployment' and o['metadata']['name'] == 'gryvia-api-gateway')
    env = gateway['spec']['template']['spec']['containers'][0]['env']
    flags = [e for e in env if e['name'] == 'GRYVIA_SUBMISSION_ADMISSION_ENFORCE']
    assert flags == ([{'name': 'GRYVIA_SUBMISSION_ADMISSION_ENFORCE', 'value': '1'}] if enabled else []), flags
    cluster = next(o for o in objects if o['kind'] == 'ClusterRole' and o['metadata']['name'].endswith('-api-gateway'))
    assert any('gryviaaijobs' in r.get('resources', []) and 'create' in r.get('verbs', []) for r in cluster['rules'])
    rendered[enabled] = cluster['rules']
assert rendered[False] == rendered[True], 'enforce must not change gateway RBAC'
print('Submission admission Helm modes passed')
PY
