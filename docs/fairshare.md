# Time-window fairshare

Lookback of tenant GPU-hours versus `GryviaTenant.spec.quotas.fairShareWeight`, with recommended
weight adjustments.

**Status:** report + apply via CLI and gateway. Weights feed Kueue `fairShareWeight` (when
`platformCompletion.kueueFairSharing`) and KAI `overQuotaWeight`. Not verified as changing live
scheduling on GPU hardware.

## Report

```bash
gryvia tenant fairshare --days 7
curl -H "Authorization: Bearer $KEY" "https://gryvia.example/api/fairshare?days=7"
```

## Time-decayed usage

By default every GPU-hour in the window counts the same. Add a half-life to weigh recent use more, as KAI Scheduler
and Run:ai time-based fairshare do: a GPU-hour used one half-life ago counts half, two half-lives ago a quarter.

```bash
gryvia tenant fairshare --days 30 --half-life-days 7
curl -H "Authorization: Bearer $KEY" "https://gryvia.example/api/fairshare?days=30&halfLifeDays=7"
```

With a half-life, `gpuHours` in the report is the decayed figure, so a tenant that burst a month ago and went quiet
no longer holds its weight down. `halfLifeDays` (above 0, at most 365) is also accepted by `POST /api/fairshare/apply`.
It only changes the recommendation; applying still writes `fairShareWeight`.

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
