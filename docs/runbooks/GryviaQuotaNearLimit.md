# GryviaQuotaNearLimit

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`gryvia_quota_gpus_allocated / gryvia_quota_gpus_max > 0.9` for 15 minutes.

## What it means

A team has allocated more than 90% of its GryviaQuota GPUs. New jobs beyond the limit queue or are rejected by the quota controller.

## How to check

```bash
kubectl get gryviaquotas
kubectl get gryviaquota <name> -o yaml   # status.currentUsage, status.phase
kubectl get gryviaaijobs -A   # who holds the GPUs
Dashboard 'Gryvia / Quotas and budgets'
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Legitimate high utilisation (the quota is doing its job).
- Stuck or finished jobs still counted as allocated (check job phases).

## Mitigation

- Raise `spec.gpuQuota.maxGPUs` if the team should have more.
- Finish or delete jobs that no longer need GPUs.
- Check that the quota controller is reconciling (GryviaOperatorReconcileErrors).

## Limits (honest)

Values come from GryviaQuota status as last written by the quota controller; a stalled controller shows stale numbers.
