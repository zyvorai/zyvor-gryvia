#!/usr/bin/env bash
# Federation is opt-in: the default render passes no federation flags and grants no Workload patch; allowedServers
# adds the probe flags only; failover adds its flag and the Workload patch rule, and refuses to render without
# allowedServers and aiOperator.kueueIntegration.
set -euo pipefail
cd "$(dirname "$0")/../.."
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
render() { helm template gryvia helm/gryvia -n gryvia-system --kube-version 1.33.0 "$@"; }
render > "$tmpdir/off.yaml"
render --set 'aiOperator.federation.allowedServers={https://w1:6443,https://w2:6443}' > "$tmpdir/probe.yaml"
render --set aiOperator.kueueIntegration=true --set 'aiOperator.federation.allowedServers={https://w1:6443}' \
  --set aiOperator.federation.failover=true --set aiOperator.federation.credentialsNamespace=fed-creds > "$tmpdir/failover.yaml"
for bad in "--set aiOperator.federation.failover=true" \
           "--set aiOperator.federation.failover=true --set aiOperator.kueueIntegration=true"; do
  # shellcheck disable=SC2086
  if render $bad > /dev/null 2>"$tmpdir/err"; then
    echo "rendered with: $bad" >&2
    exit 1
  fi
  grep -q "aiOperator.federation.failover requires" "$tmpdir/err"
done
python3 - "$tmpdir" <<'PY'
import pathlib
import sys
import yaml

root = pathlib.Path(sys.argv[1])
for mode in ('off', 'probe', 'failover'):
    objects = [o for o in yaml.safe_load_all((root / f'{mode}.yaml').read_text()) if o]
    dep = next(o for o in objects if o['kind'] == 'Deployment' and o['metadata']['name'] == 'gryvia-ai-operator')
    args = dep['spec']['template']['spec']['containers'][0]['args']
    fed = [a for a in args if a.startswith('--federation')]
    want = {
        'off': [],
        'probe': ['--federation-allowed-servers=https://w1:6443,https://w2:6443',
                  '--federation-credentials-namespace=gryvia-system'],
        'failover': ['--federation-allowed-servers=https://w1:6443', '--federation-credentials-namespace=fed-creds',
                     '--federation-failover=true'],
    }[mode]
    assert fed == want, (mode, fed)
    role = next(o for o in objects if o['kind'] == 'ClusterRole' and o['metadata']['name'].endswith('-manager-role'))
    patch = any('workloads' in r.get('resources', []) and 'patch' in r['verbs'] and 'kueue.x-k8s.io' in r['apiGroups']
                for r in role['rules'])
    assert patch == (mode == 'failover'), (mode, patch)
print('Federation Helm modes passed')
PY
