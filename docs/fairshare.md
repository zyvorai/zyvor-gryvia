# Time-window fairshare

Lookback of tenant GPU-hours versus `GryviaTenant.spec.quotas.fairShareWeight`, with recommended
weight adjustments — the open alternative to Run:ai’s closed time-based fairshare product.

**Status:** report + apply via CLI and gateway. Weights feed Kueue `fairShareWeight` (when
`platformCompletion.kueueFairSharing`) and KAI `overQuotaWeight`. Not verified as changing live
scheduling on GPU hardware.

## Report

```bash
gryvia tenant fairshare --days 7
curl -H "Authorization: Bearer $KEY" "https://gryvia.example/api/fairshare?days=7"
```

## Apply

```bash
# Preview
gryvia tenant fairshare --days 7 --apply --dry-run
# Patch tenants
gryvia tenant fairshare --days 7 --apply
# One tenant
gryvia tenant fairshare --apply --tenant research
```

Helm (gateway apply + Kueue honor weights):

```yaml
apiGateway:
  fairshareApply: true
platformCompletion:
  kueueFairSharing: true
```

Then `POST /api/fairshare/apply` with `{"dryRun": false}` (admin).

## Fractional / MIG billing

Usage records bill `effectiveGpus` (from `gryvia.io/gpu-share` or MIG profile memory) into `gpuHours`.
`spec.gpus` remains the whole-unit count for compatibility.
