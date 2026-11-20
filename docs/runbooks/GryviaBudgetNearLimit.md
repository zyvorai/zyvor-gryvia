# GryviaBudgetNearLimit

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`gryvia_quota_budget_percent_used > 90` for 10 minutes.

## What it means

A team's monthly budget (GryviaQuota `spec.budget`) is more than 90% used. With `hardLimit` set, new jobs are refused at 100%.

## How to check

```bash
kubectl get gryviaquota <name> -o jsonpath='{.status.budgetStatus}'
kubectl get gryviausagerecords -A | head   # the underlying records
gryvia_usage_cost_total by tenant on the 'Usage and cost' dashboard
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Real spend growth.
- Wrong or default rates (`sku="default"` in usage metrics): the price table may not match reality.
- Records for jobs that ran far longer than expected.

## Mitigation

- Raise the budget or stop expensive jobs.
- Verify the rate table (GryviaGpuSku) matches what you actually pay.

## Limits (honest)

Costs are estimates from job wall-clock time and configured rates; they are not invoices and do not include storage or network.
