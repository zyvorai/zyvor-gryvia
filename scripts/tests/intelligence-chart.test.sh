#!/usr/bin/env bash
# Render both operation modes and assert least-scope persistence/opt-in mutation permissions.
set -euo pipefail
cd "$(dirname "$0")/../.."
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
helm template gryvia helm/gryvia -n gryvia-system --kube-version 1.33.0 > "$tmpdir/off.yaml"
helm template gryvia helm/gryvia -n gryvia-system --kube-version 1.33.0 \
  --set apiGateway.intelligenceActions=true > "$tmpdir/on.yaml"
python3 - "$tmpdir" <<'PY'
import pathlib
import sys
import yaml

root = pathlib.Path(sys.argv[1])
for enabled, file in [(False, 'off.yaml'), (True, 'on.yaml')]:
    objects = list(yaml.safe_load_all((root / file).read_text()))
    gateway = next(o for o in objects if o and o['kind'] == 'Deployment' and o['metadata']['name'] == 'gryvia-api-gateway')
    env = gateway['spec']['template']['spec']['containers'][0]['env']
    assert any(e['name'] == 'GRYVIA_INTELLIGENCE_ACTIONS' for e in env) == enabled
    assert any(e['name'] == 'GRYVIA_INTELLIGENCE_RETENTION_DAYS' and e['value'] == '30' for e in env) == enabled
    roles = [o for o in objects if o and o['kind'] == 'Role' and o['metadata']['name'].endswith('-intelligence-actions')]
    assert bool(roles) == enabled
    if enabled:
        assert roles[0]['metadata']['namespace'] == 'gryvia-system'
        assert roles[0]['rules'] == [{'apiGroups': [''], 'resources': ['configmaps'], 'verbs': ['create', 'get', 'list', 'update', 'delete']}]
    cluster = next(o for o in objects if o and o['kind'] == 'ClusterRole' and o['metadata']['name'].endswith('-api-gateway'))
    node_patch = any('nodes' in r.get('resources', []) and 'patch' in r.get('verbs', []) for r in cluster['rules'])
    quota_patch = any('gryviaquotas' in r.get('resources', []) and 'patch' in r.get('verbs', []) for r in cluster['rules'])
    assert node_patch == enabled and quota_patch == enabled
    assert not any('configmaps' in r.get('resources', []) for r in cluster['rules'])
print('Approved-operation Helm modes passed')
PY
