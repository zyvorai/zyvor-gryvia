#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
base="$(mktemp)"; enabled="$(mktemp)"
trap 'rm "$base" "$enabled"' EXIT
helm template gryvia helm/gryvia > "$base"
helm template gryvia helm/gryvia --set platformCompletion.reportUnsupportedAPIs=true --set platformCompletion.inferenceTelemetry=true --set platformCompletion.gpuHealth=true --set platformCompletion.gpuRemediation=true --set platformCompletion.kueueFairSharing=true --set platformCompletion.kueueTopologyName=rack --set platformCompletion.kueueAdmissionCheck=multikueue --set platformCompletion.kueueGenerateTopology=true --set quotaOperator.kueueIntegration=true --set aiOperator.topologyPlacement=true > "$enabled"
python3 - "$base" "$enabled" <<'PY'
import sys,yaml
for path,on in zip(sys.argv[1:],[False,True]):
 docs=list(yaml.safe_load_all(open(path)))
 args={d['metadata']['labels']['app.kubernetes.io/component']:d['spec']['template']['spec']['containers'][0]['args'] for d in docs if d and d.get('kind')=='Deployment' and d['metadata']['labels'].get('app.kubernetes.io/component') in ['ai-operator','quota-operator','gpu-operator','storage-operator','network-operator']}
 assert ('--enable-gpu-remediation=true' in args['gpu-operator'])==on
 assert any(v.startswith('--inference-telemetry-image=') for v in args['ai-operator'])==on
 assert ('--kueue-fair-sharing=true' in args['quota-operator'])==on
 assert ('--kueue-topology-name=rack' in args['quota-operator'])==on
 assert ('--kueue-admission-check=multikueue' in args['quota-operator'])==on
 assert ('--kueue-generate-topology=true' in args['quota-operator'])==on
 assert ('--topology-placement=true' in args['ai-operator'])==on
 for name,flags in args.items():
  assert ('--report-unsupported-apis=true' in flags)==(on and name in ('ai-operator','storage-operator'))
 roles=[d for d in docs if d and d.get('kind')=='ClusterRole']
 assert any('pods/eviction' in r.get('resources',[]) for d in roles for r in d.get('rules',[]))==on
 assert any('topologies' in r.get('resources',[]) for d in roles for r in d.get('rules',[]))==on
print('Completion flags and conditional eviction RBAC passed')
PY
helm lint helm/gryvia
