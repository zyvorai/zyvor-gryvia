# Time-window fairshare

Lookback of tenant GPU-hours versus `GryviaTenant.spec.quotas.fairShareWeight`, with a recommended
weight adjustment. Inspired by Run:ai time-based fairshare; Gryvia stays recommend-only unless apply
is enabled.

**Status:** unit-tested helpers for fractional billing; fairshare report via API/CLI/UI. Weight apply
is opt-in. Not verified as changing live Kueue scheduling fairness on GPU hardware.

## Report

```bash
gryvia tenant fairshare --days 7
# Admin gateway:
curl -H "Authorization: Bearer $KEY" "https://gryvia.example/api/fairshare?days=7"
```

Each row: current weight, GPU-hours in the window, target vs actual share, recommended weight and delta.

## Apply (opt-in)

Helm:

```yaml
apiGateway:
  fairshareApply: true
platformCompletion:
  kueueFairSharing: true   # so ClusterQueues honor fairShareWeight
```

Then `POST /api/fairshare/apply` with `{"dryRun": false}` (admin). Default body is dry-run.

## Fractional / MIG billing

Usage records bill `effectiveGpus` (from `gryvia.io/gpu-share` or MIG profile memory) into `gpuHours`.
`spec.gpus` remains the whole-unit count for compatibility. See the quota-operator
`EffectiveGPUs` helper.
