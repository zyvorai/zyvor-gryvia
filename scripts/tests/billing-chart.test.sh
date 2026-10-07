#!/usr/bin/env bash
# The billing ledger is opt-in: the default render has no ledger flag, webhooks, RBAC or Stripe env; turning it on
# adds exactly those, Stripe env only appears with a secretName, and the ledger refuses to render without webhooks.
set -euo pipefail
cd "$(dirname "$0")/../.."
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
render() { helm template gryvia helm/gryvia -n gryvia-system --kube-version 1.33.0 "$@"; }
render > "$tmpdir/off.yaml"
render --set billing.ledger.enabled=true > "$tmpdir/ledger.yaml"
render --set billing.ledger.enabled=true --set billing.stripe.secretName=gryvia-stripe > "$tmpdir/stripe.yaml"
if render --set billing.ledger.enabled=true --set webhook.enabled=false > /dev/null 2>"$tmpdir/err"; then
  echo "billing.ledger.enabled rendered without webhook.enabled" >&2
  exit 1
fi
grep -q "billing.ledger.enabled requires webhook.enabled" "$tmpdir/err"
python3 - "$tmpdir" <<'PY'
import pathlib
import sys
import yaml

root = pathlib.Path(sys.argv[1])
for mode in ('off', 'ledger', 'stripe'):
    on = mode != 'off'
    objects = [o for o in yaml.safe_load_all((root / f'{mode}.yaml').read_text()) if o]

    def deployment(name):
        return next(o for o in objects if o['kind'] == 'Deployment' and o['metadata']['name'] == name)

    args = deployment('gryvia-quota-operator')['spec']['template']['spec']['containers'][0]['args']
    assert ('--billing-ledger=true' in args) == on, (mode, args)

    env = {e['name']: e for e in deployment('gryvia-api-gateway')['spec']['template']['spec']['containers'][0]['env']}
    assert ('GRYVIA_BILLING_LEDGER' in env) == on, mode
    stripe = mode == 'stripe'
    assert ('GRYVIA_STRIPE_SECRET_KEY' in env) == stripe and ('GRYVIA_STRIPE_WEBHOOK_SECRET' in env) == stripe, mode
    if stripe:
        ref = env['GRYVIA_STRIPE_SECRET_KEY']['valueFrom']['secretKeyRef']
        assert ref == {'name': 'gryvia-stripe', 'key': 'secret-key'}, ref
        ref = env['GRYVIA_STRIPE_WEBHOOK_SECRET']['valueFrom']['secretKeyRef']
        assert ref == {'name': 'gryvia-stripe', 'key': 'webhook-secret'}, ref

    vwc = next(o for o in objects if o['kind'] == 'ValidatingWebhookConfiguration' and o['metadata']['name'].endswith('-usagerecord'))
    hooks = {w['name']: w for w in vwc['webhooks']}
    usage_ops = hooks['vgryviausagerecord.gryvia.io']['rules'][0]['operations']
    assert usage_ops == (['UPDATE', 'DELETE'] if on else ['UPDATE']), (mode, usage_ops)
    for kind, plural in (('gryvialedgerentry', 'gryvialedgerentries'), ('gryviainvoice', 'gryviainvoices')):
        hook = hooks.get(f'v{kind}.gryvia.io')
        assert bool(hook) == on, (mode, kind)
        if hook:
            assert hook['failurePolicy'] == 'Fail'
            assert hook['rules'][0]['resources'] == [plural]
            assert hook['rules'][0]['operations'] == ['UPDATE', 'DELETE']
            assert hook['rules'][0]['scope'] == 'Cluster'
            assert hook['clientConfig']['service']['path'] == f'/validate-gryvia-io-v1alpha1-{kind}'

    def rules(suffix):
        role = next(o for o in objects if o['kind'] == 'ClusterRole' and o['metadata']['name'].endswith(suffix))
        return role['rules']

    def verbs(rs, resource):
        return sorted({v for r in rs if resource in r.get('resources', []) for v in r['verbs']})

    assert verbs(rules('-manager-role'), 'gryvialedgerentries') == (['create', 'get', 'list', 'watch'] if on else []), mode
    gateway = rules('-api-gateway')
    assert verbs(gateway, 'gryvialedgerentries') == (['get', 'list'] if on else []), mode
    assert verbs(gateway, 'gryviainvoices') == (['create', 'delete', 'get', 'list', 'patch'] if on else []), mode
print('Billing ledger Helm modes passed')
PY
