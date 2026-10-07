#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
dir="$(mktemp -d)"
trap 'rm -r "$dir"' EXIT
on="--set gpuOperator.resetAgent.enabled=true"
exe="$on --set gpuOperator.resetAgent.execute=true --set gpuOperator.resetAgent.nvidiaSmi=/run/nvidia/driver/usr/bin/nvidia-smi --set gpuOperator.resetAgent.driverRoot=/run/nvidia/driver"
helm template gryvia helm/gryvia $on > "$dir/dry"
helm template gryvia helm/gryvia $exe --set gpuOperator.resetAgent.allowDriverReload=true --set gpuOperator.resetAgent.allowReboot=true > "$dir/kured"
helm template gryvia helm/gryvia $exe --set gpuOperator.resetAgent.allowReboot=true --set gpuOperator.resetAgent.rebootMethod=nsenter > "$dir/nsenter"
if helm template gryvia helm/gryvia $on --set gpuOperator.resetAgent.rebootMethod=magic >/dev/null 2>&1; then
  echo "unknown rebootMethod rendered" >&2; exit 1
fi
python3 - "$dir" <<'PY'
import sys, yaml
def load(name):
    docs = [d for d in yaml.safe_load_all(open(f"{sys.argv[1]}/{name}")) if d and d['metadata'].get('labels', {}).get('app.kubernetes.io/component') == 'gpu-reset-agent']
    by = {d['kind']: d for d in docs}
    pod = by['DaemonSet']['spec']['template']['spec']
    c = pod['containers'][0]
    verbs = [r['verbs'] for r in by['ClusterRole']['rules'] if 'pods' in r['resources']][0]
    return by, pod, c, verbs

by, pod, c, verbs = load('dry')
assert 'Role' not in by and verbs == ['list'], 'default must not get lease or pod-delete rights'
assert not any(a.startswith('--allow') for a in c['args']) and not pod.get('hostPID')
assert c['securityContext'].get('runAsNonRoot') and 'volumes' not in pod

by, pod, c, verbs = load('kured')
assert by['Role']['rules'][0]['resources'] == ['leases'] and by['RoleBinding']['roleRef']['name'] == by['Role']['metadata']['name']
assert 'delete' in verbs
for a in ['--allow-driver-reload=true', '--allow-reboot=true', '--reboot-method=kured', '--reboot-sentinel=/var/run/gryvia/reboot-required']:
    assert a in c['args'], a
assert any(e['name'] == 'POD_NAMESPACE' for e in c['env'])
vols = {v['name']: v for v in pod['volumes']}
assert vols['reboot-sentinel']['hostPath'] == {'path': '/var/run/gryvia', 'type': 'DirectoryOrCreate'} and 'driver-root' in vols
assert not pod.get('hostPID') and c['securityContext']['privileged']

by, pod, c, verbs = load('nsenter')
assert pod['hostPID'] is True and '--reboot-method=nsenter' in c['args'] and 'delete' not in verbs
assert 'reboot-sentinel' not in {v['name'] for v in pod['volumes']}
print('GPU recovery chart: default dry-run, kured and nsenter renders passed')
PY
