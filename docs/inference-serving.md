# Inference scaling and weighted canaries

## Scaling on GPU utilization and request rate

The existing `spec.autoscaling` targets now create `autoscaling/v2` HPA Pods metrics:

| Field | Custom metric | Units/target |
|---|---|---|
| `targetGPUUtilization` | `gryvia_gpu_utilization` | Average percentage per stable serving pod, 1-100 |
| `targetRequestsPerSecond` | `gryvia_inference_requests_per_second` | Average requests/second per stable serving pod, positive integer |

Both can be set. Kubernetes chooses the largest replica recommendation, subject to the configured min/max.
When neither is set, CPU utilization at 80% remains the default. Scale-down has a 300-second stabilization
window. Negative targets and GPU percentages above 100 produce `AutoscalingValid=False`, with no HPA.

**A custom metrics adapter is required.** Gryvia configures the HPA, not the metrics API server.
`examples/inference/prometheus-adapter-values.yaml` contains example adapter rules; check your actual
DCGM label names and scrape configuration before installing them. Every series must be attributable to
one namespace and pod. Use per-pod DCGM values; node-wide GPU utilization is unsuitable for tenant scaling.
The RPS rule assumes the serving application or proxy exports the illustrated request counter. These
rules do not add instrumentation to vLLM/Triton automatically. Missing series must stay missing, not
be rewritten to zero. The adapter and metrics pipeline need your usual access controls.

`AutoscalingValid=True` means configuration is valid. `AutoscalingReady` mirrors the HPA's current
`ScalingActive` condition, including failures to fetch custom metrics; stale/unobserved HPA status is
Unknown. It is not proof that every GPU metric is accurate or every desired replica is ready.
The HPA controls the stable Deployment only; the existing canary replica calculation is retained.

## Scale to zero

`spec.scaleToZero` scales an idle service down to zero replicas and back up when a request arrives through the
[LLM gateway](llm-gateway.md):

```yaml
spec:
  scaleToZero:
    enabled: true
    idleSeconds: 900               # default 900, at least 60
    coldStartTimeoutSeconds: 300   # default 300, 10 to 3600
```

- **Idle.** The idle period counts from the later of the last request the gateway proxied (annotation
  `gryvia.io/last-request`, written at most once a minute, or every `idleSeconds / 3` when that is shorter) and the last wake-up (`status.lastWakeAt`, first set when
  scale-to-zero is turned on, so enabling it on a long-idle service does not scale it down at once). A service with
  no ready replica gets `coldStartTimeoutSeconds` on top, so a slow model load is not cut short. Past the deadline
  the stable Deployment goes to 0 replicas, `status.phase` becomes `ScaledToZero`, `status.scaledToZeroAt` is set
  and the condition `ScaleToZero` is True (`Idle`). The Service and `status.endpoint` stay.
- **Wake.** The annotation `gryvia.io/wake-requested` (any value) scales it back to `max(1, autoscaling.minReplicas)`
  (or `spec.replicas` without autoscaling); the controller removes the annotation and sets `status.lastWakeAt`.
  The phase is `Deploying` until a replica is ready, then `Ready`. The gateway sets this annotation when a request
  for a scaled-to-zero model arrives and holds the request until the service is ready (see
  [LLM gateway: scale to zero](llm-gateway.md#scale-to-zero)); you can also wake a service by hand:
  `kubectl annotate gryviainferenceservice chat gryvia.io/wake-requested=now`.
- **Autoscaling.** At zero the HPA is left untouched (Kubernetes does not scale a Deployment with 0 replicas, and
  `minReplicas` stays at least 1). After a wake-up the HPA owns the replica count again.
- **Not with a canary.** While `spec.canary.enabled` is true, scale-to-zero is not applied and `ScaleToZero` is
  False (`Unsupported`).
- Turning scale-to-zero off while at zero scales the service back up.

Only traffic through the LLM gateway counts as activity and wakes the service. A caller of `status.endpoint` (or
the Service) gets no activator: its connections fail while the service is at zero, and its requests do not keep it
up.

## Continuous SLO control

`spec.slo` keeps latency and error objectives on the stable track by raising the HPA's `minReplicas` while an
objective is breached **and** the engine is overloaded, and lowering it again after a run of healthy windows. It never
writes the Deployment's replica count; the HPA stays the only writer.

```yaml
spec:
  autoscaling: {enabled: true, minReplicas: 2, maxReplicas: 8}
  slo:
    enabled: true
    maxTTFTMilliseconds: 800        # worst replica's TTFT p99 (engine metrics)
    maxInterTokenMilliseconds: 60   # worst replica's inter-token latency p99
    maxErrorRate: 0.01              # stable-track errors / requests (serving sidecar)
    minRequests: 100                # default; smaller windows are not judged
    windowSeconds: 60               # default 60, 30..600
    scaleUpStep: 1                  # default 1, at most 10
    scaleDownAfterWindows: 10       # default 10, at least 3
    metricsJob: chat                # collector job label; default the service name
```

At least one objective is required. Each window the controller queries the operator Prometheus
(`aiOperator.inferencePrometheusURL`):

| Signal | Query (labels `namespace`, and `inference` + `track="stable"` or `job`) |
|--------|----------------------------------------------------------------------------|
| Requests, error rate | `gryvia_inference_requests_total`, `gryvia_inference_errors_total` over the window |
| TTFT, ITL | `max(gryvia_inference_latency_seconds{metric="ttft_p99"})`, `{metric="itl_p99"}` |
| Overload | `sum(gryvia_inference_requests{state="waiting"}) > 0` or `max(gryvia_inference_kv_cache_usage_ratio) >= 0.9` |

TTFT, ITL and the overload signals come from the collector's engine scraper ([inference latency](inference-latency.md));
pods of an SLO-controlled service carry the label `gryvia.io/job=<metricsJob>`, which the scraper's discovery
(`-infer-metrics-discover`) uses to attribute them. Adding `spec.slo` therefore rolls the pods once.

Decisions, reported in `status.slo` (`state`, `floorReplicas`, `healthyWindows`, `breaches`, `lastEvaluated`,
`message`) and the condition `SLOControl`:

- **Breach and overload** (`ScaledUp`): the floor becomes `max(floor, ready replicas) + scaleUpStep`, capped at
  `maxReplicas`.
- **Breach without overload** (`Breached`): the floor is held. Slow tokens with an empty queue are a model or hardware
  problem that more replicas would not fix. Without any queue or KV-cache metric the floor is also held.
- **Healthy**: the run of healthy windows grows; after `scaleDownAfterWindows` the floor drops by one (`ScaledDown`),
  never below `autoscaling.minReplicas`. The HPA's own 300 s scale-down stabilization still applies.
- **Too little traffic** (`InsufficientTraffic`) or **missing / stale metrics** (`Unknown`, any configured objective
  older than 90 s or absent): everything is held, nothing is guessed.

SLO control needs `spec.autoscaling.enabled` (condition False, `RequiresAutoscaling`), is not applied together with
`spec.scaleToZero` (`ScaleToZero`), and without the operator Prometheus URL reports `NotConfigured` and leaves the HPA
at the spec minimum. Disabling it removes the floor on the next reconcile. Unit tests with a fake Prometheus cover the
decision table, stale and missing samples, the floor rising and falling through the HPA, and the gates
(`operators/ai-operator/controllers/inference_slo_test.go`). It has not been run against a live vLLM under load.

## Weighted HTTP routing

Enable the operator's optional capability:

```bash
helm upgrade gryvia ./helm/gryvia -n gryvia-system --reuse-values \
  --set aiOperator.inferenceGatewayRouting=true
```

This adds `--inference-gateway-routing=true` and HTTPRoute RBAC. Install the Gateway API v1 standard
CRDs and a compatible Gateway controller separately. Gryvia does not install either or create a Gateway.
No HTTPRoute API calls occur for default services that have never requested routing.

Opt a service in with annotations (full example in `examples/inference/weighted-canary.yaml`):

```yaml
metadata:
  annotations:
    gryvia.io/gateway-parent: public
    gryvia.io/gateway-section: https
    gryvia.io/gateway-hostname: chat.example.com
spec:
  canary:
    enabled: true
    weight: 10
    modelVersion: llama-v2
    autoPromote: true
    promoteAfterSeconds: 300
```

The Gateway must be in the service's namespace. Its listener must allow this HTTPRoute and, for
TLS, provide the appropriate certificate. The hostname and listener annotations are optional.
Gryvia creates `<name>-route`, `<name>-route-stable` and `<name>-route-canary` (long names are hashed).
The two track Services select exclusively stable or canary pods. Gateway backend weights total 100;
weight 10 produces 90/10 independently of replica counts. Gateway implementations distribute requests
statistically; a small request sample need not be exactly 90/10, and existing streaming requests are
not moved between backends when weights change.

An unready canary gets zero Gateway traffic. An old or incomplete Deployment rollout does not authorize traffic
to an updated version. `GatewayRouting=True` requires current-generation `Accepted=True` and
`ResolvedRefs=True` on the requested parent. Missing APIs, disabled routing and pending/rejected routes
are exposed through this condition. Accepted configuration is not a data-plane traffic measurement.

Use the **Gateway address and configured hostname** to get weighted traffic. The existing
`status.endpoint` remains the internal aggregate ClusterIP Service for compatibility; direct calls to
that Service keep the older pod-count traffic split. Gryvia does not infer the public Gateway address.

## Promotion and rollback

With routing requested, automatic promotion is blocked while the route is disabled, unaccepted, stale,
owned by another object, using different weights, or sending zero canary traffic. Once readiness and
current routing are observed together, a hold period of `promoteAfterSeconds` begins. Readiness loss,
route rejection, weight/parent/version or pod-template changes reset the hold period. It is sampled
at reconcile intervals, not continuously monitored. Deployment creation time alone is insufficient.

Promotion and the existing readiness-based rollback return the generated route to stable-only.
Opt-in error-rate and latency evaluation is described below. A healthy readiness probe does not prove
model quality. Promotion still uses the existing Deployment rollout; there may be a period when
the stable endpoint serves the previous model during rollout. Client streaming/draining behavior depends
on the Gateway, server and pod termination settings.

To remove routing, clear all Gateway annotations while the capability is enabled. Gryvia deletes its
owned HTTPRoute first, then the track Services once the route is gone. Name collisions are left untouched.
To disable the operator capability globally, first remove per-service routes; switching the flag off stops
management and does not remove already-created routes. Deleting the inference resource garbage-collects
its owned route and Services.

## Measured canary SLO evaluation

Configure the operator's Prometheus base URL with `aiOperator.inferencePrometheusURL` in Helm or
`--inference-prometheus-url=http://prometheus.monitoring:9090`. The default is empty. The administrator
chooses this endpoint; services cannot supply endpoints or arbitrary PromQL. Use a trusted Prometheus
or query proxy reachable by the operator. URL credentials, query parameters and redirects are rejected;
authentication headers and custom CA configuration are not provided by this feature.

Add both `gryvia.io/canary-max-error-rate` (a fraction from 0 to 1) and
`gryvia.io/canary-max-p95-seconds` (positive seconds) to the inference service. Optional
`gryvia.io/canary-min-requests` defaults to 100 requests. Invalid or partial policies fail configuration
validation. `examples/inference/slo-canary.yaml` includes a complete service example.

Instrumentation must provide these normalized metrics, with **all** of the following labels:
`namespace`, `inference` (service name), `track="canary"`, `model_version`, `deployment_uid` (canary
Deployment UID), and `revision` (the Deployment's `gryvia.io/spec-hash` annotation). The histogram also
needs `le`. A serving proxy/exporter can obtain these values from Kubernetes metadata. No backend
instrumentation or automatic label enrichment is installed by Gryvia.

| Metric | Required meaning |
|---|---|
| `gryvia_inference_requests_total` | Counter of completed requests, including failed requests |
| `gryvia_inference_errors_total` | Counter of failed requests; export zero before the first failure |
| `gryvia_inference_request_duration_seconds_bucket` | Cumulative classic histogram of completed request durations in seconds |

Every serving replica must be represented; duplicate scrapes or incomplete series distort analysis.
Use the same request population for all three metrics and define which responses count as errors in
your instrumentation. The operator sums `increase` of request/error counters over 60 seconds and
computes p95 from summed histogram bucket rates. Counter resets are handled by Prometheus. Counts
are extrapolated estimates and can be fractional. A zero-traffic window has no usable error ratio;
missing errors or histogram data must stay missing, rather than being filled with synthetic success.
This is completed-request latency, not token latency or time to first token.

`CanaryAnalysisReady` reports `SLOPassed`, `SLOBreached`, `WarmingUp`, `InsufficientTraffic`,
`NotConfigured` or `TelemetryUnavailable`. After a ready canary is observed, Gryvia waits a full
60-second window for its current policy and template revision. Samples and underlying raw metric
timestamps must be no more than 30 seconds old. Each evaluation has a four-second total timeout;
responses are limited to 64 KiB. Missing, malformed, partial, stale or non-finite results block promotion
and clear the consecutive SLO breach count. They do not trigger an SLO rollback. Readiness failures
continue to follow the existing startup grace and rollback policy.

A measured threshold breach counts once per `healthCheck.intervalSeconds` (default 30). With
`healthCheck.autoRollback` enabled, `failureThreshold` consecutive breaches reject the version and
remove the canary, returning its Gateway route to stable-only. A ready canary continues receiving its
configured traffic share while awaiting analysis or a rollback threshold, allowing telemetry to accrue.
If automatic rollback is disabled, failures block promotion and require operator intervention.

Promotion requires sampled passing evaluations for `promoteAfterSeconds`; the persisted success
hold resets on missing data, breaches, readiness loss, policy/version/template changes or a changed
Prometheus endpoint. A requested Gateway must independently pass its existing routing hold.
The hold survives short operator restarts. Gaps between successful evaluations longer than twice the
health interval (at least 60 seconds) reset the hold. These are checks at reconcile intervals, not proof of continuous
health between samples. The feature evaluates serving reliability; it does not assess output quality.

## Validation and limits

Scale-to-zero: fake-client tests of the controller cover idle scale-down at the deadline (and the requeue for it),
a request annotation postponing it, staying at zero without a wake request, the wake annotation consumed and the
replicas restored, the cold-start allowance for a service that never became ready, the HPA left alone at zero and
woken into its range, turning the feature off at zero, the canary exclusion, and a malformed annotation
(`operators/ai-operator/controllers/inference_scaletozero_test.go`). On kind, the step "LLM gateway - scale to zero
and a cold start through the gateway" of `.github/workflows/e2e-ml.yml` publishes a stand-in model with
`idleSeconds: 60` and checks that a gateway request writes `gryvia.io/last-request`, that after a minute idle the
service is `ScaledToZero` with its Deployment at 0 and no pods, and that the next gateway request wakes it, waits
and is answered (200) and metered. Not covered end to end: a real engine's model load time (the stand-in starts in
seconds) and the 503 after the cold-start timeout (unit-tested in the gateway).

Unit tests cover HPA targets/errors, status freshness, route weighting, cold/unready canaries, defaults,
cleanup, ownership and promotion gating. An `integration` Go test runs against envtest with Kubernetes
API defaults/validation and the official Gateway API HTTPRoute CRD. Its Gateway and HPA conditions are
explicit fixtures: no Gateway data plane, HPA controller, GPU, metrics adapter or model server is running.
A CI workflow installs the pinned test dependencies and runs the same checks.

The kind e2e (`e2e-ml.yml`) installs metrics-server and watches the default CPU HPA scale a CPU-bound stand-in
to `maxReplicas`, with `AutoscalingReady` mirroring `ScalingActive`. Still to be observed on a real cluster:
HPA changes on the GPU and requests-per-second metrics through an adapter, request distributions through your
chosen Gateway, canary rollback and streaming/drain behavior with actual serving images.

Prometheus query semantics and response formats: [functions](https://prometheus.io/docs/prometheus/latest/querying/functions/) and [HTTP API](https://prometheus.io/docs/prometheus/latest/querying/api/).

Opt-in proxy telemetry and live Prometheus validation are described in [platform completion](platform-completion.md). Real model-server and GPU Gateway qualification remains required.
