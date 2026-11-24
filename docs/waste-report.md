# GPU waste report

Metered GPU-hours against measured GPU utilization, per tenant. Metering bills reserved GPU time (a GPU held for
ten hours is ten GPU-hours however busy it was). The waste report puts the measured utilization next to it so a
team sees how much of what it holds it uses. This is the allocated / effective / waste split that cost tools such as
Kubecost and Cast AI show per workload, done per tenant from Gryvia's own usage records.

**Status:** report only (no controller acts on it). Verified with unit and endpoint tests against fakes; not run
against a real Prometheus with DCGM data.

## How it is computed

- Allocated: `gpuHours` and `cost` from the tenant's `GryviaUsageRecord`s in the window.
- Utilization: the gateway asks Prometheus for
  `avg by (namespace) (avg_over_time(DCGM_FI_DEV_GPU_UTIL{namespace!=""}[<days>d]))` (the DCGM exporter must label
  series with the pod namespace) and gives each usage record its namespace's value. A tenant's utilization is the
  GPU-hour weighted mean over its records that have telemetry.
- Effective hours: `gpuHours * utilization`. Wasted hours: the rest. Wasted cost: `cost * (1 - utilization)`.
- A tenant below `lowUtilPct` (default 30) is `underused`; one with no matching series is `noTelemetry`. Its hours
  and cost still count as allocated, but never as waste.
- Values outside 0..100 are clamped; NaN and non-numbers are ignored.

The namespace average covers every GPU in the namespace, not only the GPUs of the metered jobs, so a namespace that
runs an idle notebook next to a busy job reads as partly used. Treat the number as a prompt to look, not as an
invoice line.

## Use

```bash
gryvia waste                              # last 7 days, table
gryvia waste --days 30 --low-util-pct 40
gryvia waste -o json
curl -H "Authorization: Bearer $KEY" "https://gryvia.example/api/waste?days=7&format=csv"
```

`GET /api/waste` takes `days` (1 to 90), `lowUtilPct` (0 to 100), `tenant` (admins only; a tenant user always gets its
own tenant) and `format=json|csv`. If Prometheus is not configured or unreachable the report still returns, with
`telemetry: false` and every tenant `noTelemetry`.
