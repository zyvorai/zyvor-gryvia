# GryviaGatewayAuthFailureSpike

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

More than 1 failed authentication per second and more than half of all attempts failing, for 10 minutes.

## What it means

Many requests fail authentication (login, API key, session, OIDC). The metric never records who, only method and result.

## How to check

```bash
sum by (method, result) (rate(gryvia_gateway_auth_attempts_total[5m]))   # which method
kubectl -n gryvia-system logs deploy/gryvia-api-gateway | grep -i -E 'auth|401|403'
sum(rate(gryvia_gateway_rate_limit_hits_total[5m]))   # is the rate limiter already engaging
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- A client with a stale or rotated API key or an expired session (most common).
- OIDC misconfiguration after an IdP change.
- Credential guessing against /api/auth/login (rate limited to 10/minute per client IP).

## Mitigation

- For a misconfigured client, fix its credentials. For an attack, restrict the gateway with a NetworkPolicy/ingress allow-list and rotate the API key (`auth.apiKey` / existing secret).
- Do not leave the default lab key in use.

## Limits (honest)

The client IP is not a metric label (cardinality and privacy); use the gateway log or ingress logs to find the source.
