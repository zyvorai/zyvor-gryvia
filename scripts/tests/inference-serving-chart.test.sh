#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

set -euo pipefail
cd "$(dirname "$0")/../.."
base="$(mktemp)"; enabled="$(mktemp)"
trap 'rm -f "$base" "$enabled"' EXIT
helm template gryvia helm/gryvia --kube-version 1.34.0 > "$base"
helm template gryvia helm/gryvia --kube-version 1.34.0 --set aiOperator.inferenceGatewayRouting=true --set-string aiOperator.inferencePrometheusURL=http://prometheus.monitoring:9090 > "$enabled"
python3 - "$base" "$enabled" <<'PY'
import sys, yaml

def check(path, enabled):
    docs = list(yaml.safe_load_all(open(path)))
    ai = next(d for d in docs if d and d.get('kind') == 'Deployment' and d['metadata']['labels'].get('app.kubernetes.io/component') == 'ai-operator')
    args = ai['spec']['template']['spec']['containers'][0]['args']
    assert ('--inference-gateway-routing=true' in args) == enabled
    assert ('--inference-prometheus-url=http://prometheus.monitoring:9090' in args) == enabled
    if not enabled:
        assert not any(a.startswith('--inference-prometheus-url=') for a in args)
    roles = [d for d in docs if d and d.get('kind') == 'ClusterRole']
    route_rules = [r for d in roles for r in d.get('rules', []) if 'gateway.networking.k8s.io' in r.get('apiGroups', [])]
    assert bool(route_rules) == enabled
    if enabled:
        assert all(r['resources'] == ['httproutes'] for r in route_rules)
        assert set(route_rules[0]['verbs']) == {'get', 'list', 'watch', 'create', 'patch', 'delete'}

check(sys.argv[1], False)
check(sys.argv[2], True)
print('Default/off and enabled routing flags/RBAC passed')
PY
helm lint helm/gryvia --set aiOperator.inferenceGatewayRouting=true
