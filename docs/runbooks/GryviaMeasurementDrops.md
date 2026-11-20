# GryviaMeasurementDrops

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`increase(gryvia_measurement_dropped_events[10m]) > 0` for 5 minutes.

## What it means

The collector lost events before analysis (ring buffer/perf overflow or a consumer backlog). Every number derived from that source under-counts while drops continue.

## How to check

```bash
gryvia_measurement_dropped_events   # the source label says where: <object>/drops for kernel producers, userspace/... for decoders
kubectl -n gryvia-network top pod -l app.kubernetes.io/component=collector   # CPU pressure
kubectl -n gryvia-network logs <collector-pod> | grep -i drop
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Event rate above what the collector can consume (busy node, CPU limits too low).
- Ring buffers too small for burst rates.

## Mitigation

- Raise the collector CPU limit; reduce the probe set on busy nodes; increase ring buffer sizes where the chart exposes them.
- Treat metrics from the affected source as lower bounds until the drops stop.

## Limits (honest)

The gauge is monotonic since collector start, so the alert looks at the increase. A restart resets it.
