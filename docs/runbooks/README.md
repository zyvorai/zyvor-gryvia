# Gryvia alert runbooks

One page per alert in [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml); each alert's `runbook_url` points here.
Thresholds in the rules are starting points that were not tuned on real fleets. Read [observability.md](../observability.md) for the metric catalog and its limits.

| Alert | Severity | Component |
|---|---|---|
| [GryviaFabricScoreHigh](GryviaFabricScoreHigh.md) | warning | fabric |
| [GryviaStragglerPersists](GryviaStragglerPersists.md) | warning | fabric |
| [GryviaRdmaRetryHigh](GryviaRdmaRetryHigh.md) | warning | fabric |
| [GryviaCnpRateHigh](GryviaCnpRateHigh.md) | warning | fabric |
| [GryviaPfcRateHigh](GryviaPfcRateHigh.md) | warning | fabric |
| [GryviaNicErrors](GryviaNicErrors.md) | warning | fabric |
| [GryviaInferenceQueueHigh](GryviaInferenceQueueHigh.md) | warning | inference |
| [GryviaInferenceTTFTHigh](GryviaInferenceTTFTHigh.md) | warning | inference |
| [GryviaKVCacheSaturated](GryviaKVCacheSaturated.md) | warning | inference |
| [GryviaEbpfProgramNotAttached](GryviaEbpfProgramNotAttached.md) | warning | collector |
| [GryviaMeasurementDrops](GryviaMeasurementDrops.md) | warning | collector |
| [GryviaUnattributedNetworkBytesHigh](GryviaUnattributedNetworkBytesHigh.md) | warning | collector |
| [GryviaQuotaNearLimit](GryviaQuotaNearLimit.md) | warning | quota |
| [GryviaBudgetNearLimit](GryviaBudgetNearLimit.md) | warning | quota |
| [GryviaGatewayHigh5xx](GryviaGatewayHigh5xx.md) | warning | gateway |
| [GryviaGatewayAuthFailureSpike](GryviaGatewayAuthFailureSpike.md) | warning | gateway |
| [GryviaOperatorReconcileErrors](GryviaOperatorReconcileErrors.md) | warning | platform |
| [GryviaPodCrashLooping](GryviaPodCrashLooping.md) | critical | platform |

## Calling a collector

Collectors listen on port 9090 of each pod (label `app.kubernetes.io/component=collector`, namespace `gryvia-network` by default).
If the chart's `ebpf.security.apiTokenSecret` is set, every `/api/v1/*` request must be signed with that token (see
[collector-security.md](../collector-security.md)); without it, plain `curl` works. Example (adjust the secret name and pod IP; add `--cacert` when TLS is on):

```bash
TOKEN=$(kubectl -n gryvia-network get secret <apiTokenSecret> -o jsonpath='{.data.token}' | base64 -d)
URI=/api/v1/fabric
TS=$(date +%s)
SIG=$(printf 'GRYVIA-API-V1\nGET\n%s\n%s' "$URI" "$TS" | openssl dgst -sha256 -hmac "$TOKEN" -hex | sed 's/^.* //')
curl -s -H "X-Gryvia-Time: $TS" -H "X-Gryvia-Signature: $SIG" "http://<collector-pod-ip>:9090$URI"
```

Useful URIs: `/api/v1/fabric`, `/api/v1/gpu/nccl`, `/api/v1/ebpf/status`, `/api/v1/inference`, `/api/v1/flight/diagnosis`, `/metrics`
(`/metrics` takes the separate `-metrics-token-file` bearer token when configured). The signing recipe was written from the
documented scheme and the test vectors, and was not run against a live collector from these runbooks.
