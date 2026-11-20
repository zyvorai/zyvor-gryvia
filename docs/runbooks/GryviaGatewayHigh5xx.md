# GryviaGatewayHigh5xx

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

More than 5% of gateway responses are 5xx (with at least 0.1 req/s) for 10 minutes.

## What it means

The API gateway is failing requests. Requires the gateway `/metrics` (apiGateway.metrics.enabled).

## How to check

```bash
sum by (route, status) (rate(gryvia_gateway_http_requests_total{status=~"5.."}[5m]))   # which route
kubectl -n gryvia-system logs deploy/gryvia-api-gateway --tail=200
kubectl -n gryvia-system get pods -l app=gryvia-api-gateway
kubectl auth can-i list gryviaaijobs --as=system:serviceaccount:gryvia-system:gryvia-api-gateway
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Kubernetes API errors or RBAC gaps for a kind the route reads.
- A CRD missing after a partial upgrade.
- Collector or Prometheus backends timing out.

## Mitigation

- Fix the cause visible in the log (RBAC, CRD, backend); roll back the last change if it started at a deploy: `helm rollback`.
- Scale the gateway if latency (p95 on the dashboard) also climbs.

## Limits (honest)

The alert is silent when metrics are disabled or the gateway is down (then 'up == 0' is the signal).
