# Network intelligence: where each controller gets its data

The `network-intelligence` operator (`operators/network-intelligence`, chart `helm/network-intelligence`) reconciles ten
kinds. Each one now reads a real source, and reports in `status.conditions` when it cannot. This page says which source,
what the figures mean, the limits, and what has not been verified.

**Verification status.** Unit tests cover the collector client (`httptest` servers, request signing against the
collector's own cross-language vector, merging, partial node failure, TLS) and every controller (fake Kubernetes client,
fake sources). `.github/workflows/e2e-netintel.yml` installs the chart on kind with a **fake collector** (canned JSON with
the collector's real field names, HMAC-verified): it proves the operator side only. **Nothing here has been run against
the real eBPF collector fleet at scale, real Netra, Cilium or Hubble**, and the e2e workflow had not been run by its
author when this was written.

## Sources at a glance

| Kind | Source | What it fills | Not filled (no source) |
|------|--------|---------------|------------------------|
| `GryviaServiceGraph` | collector `GET /api/v1/graph`, merged over all pods; Services of the target namespaces | `status.nodes`, `status.edges` (source, destination, protocol, port, `bytesTotal`, `flowCount`, `latencyP50`, `throughput` = cumulative bytes as text) | verdict, `latencyP99`, node health (stays `unknown`), `spec.depth` |
| `GryviaTrafficInsight` | same graph + `GET /api/v1/anomalies` | `topTalkers` (inbound edges by cumulative bytes), `p50Latency` (byte-flow-weighted moving average of the inbound edges, not a percentile), `throughputBps` (growth of the inbound byte counters between two reconciles; unset on the first), `anomalies` of the last hour | `p99Latency`, `dropCount` |
| `GryviaNetworkAnomaly` | collector `GET /api/v1/anomalies`, merged, deduplicated | `status.anomalies` for `spec.targetService` (severity from value/threshold: >5 critical, >3 high, >1.5 medium, else low) | rule thresholds are not evaluated: `detectionRules` only select types (`latency`->`latency_spike`, `throughput`->`traffic_burst`, `connections`->`new_connection`); `errors`/`drops` have no collector anomaly |
| `GryviaSecurityPolicy` | collector `GET /api/v1/security/alerts`, merged | `alertsTriggered`, `detectionCounts` (by rule type), `lastAlert`, `phase` (`Active` / `Degraded`), the alert webhook (payload contract unchanged) | per-namespace filtering (alerts carry no namespace), blocking |
| `GryviaNetworkCost` | Kubernetes: `GryviaNetworkUsageRecord` (tenant namespaces) + `GryviaNetworkRate` (cluster), **no HTTP** | `status.reports`: one per namespace and UTC day | same-node and unknown-zone egress (unpriced, not reported) |
| `GryviaTrainingInsight` | `GryviaFabricSignal.status` (`spec.jobRef == targetJob`, same namespace) | `ncclP99ms`, `collectiveMaxSkewMs`, `overlapIdleRatio`, `rdmaRetryRate`, `scoreDelta`, a straggler when the collector flagged one, `bottleneck` (`communication` only with evidence, else `unknown`) | `rankStats`, `commPattern`, `commComputeRatio` (the signal has per-job aggregates only) |
| `GryviaInferenceInsight` | `GryviaFabricSignal.status` (`spec.jobRef == targetService`) | `engine`, `ttftP99ms`, `itlP99ms`, `queueTimeP99ms`, `e2eP99ms`, `inferWaitP99ms`, `requestsWaiting`, `kvCacheUsage`, `latencyBreakdown.gpuQueueNs` (engine queue p99), `totalNs`/`p99TotalNs` (engine end-to-end p99), `bottleneck` (`queue`/`network_wait`/`engine`/`unknown`, a heuristic) | dns/tcp/tls/gpuExec/postprocess, p50/p95 |
| `GryviaTraceSession` | Netra `GET /api/v1/flows/history` over the session window | result ConfigMap `trace-<name>` (`flows`, at most 2000, replaced each poll) and `flowsCaptured` | l3/l4/l7 level, `captureHeaders`, `filters.srcIP` |
| `GryviaFlowPolicy` | Kubernetes (Services, pods) -> `CiliumNetworkPolicy`; Netra for `matchedFlows` | the Cilium policy, `phase`, `matchedFlows` (Netra records of the source pods in the last 15 min, optionally to the port) | `matchedFlows` without Netra (left unset) |
| `GryviaAutoPolicy` | the same collector graph + Services; learned edges in ConfigMap `autopolicy-<name>-learned` | suggested `GryviaFlowPolicy` objects, `learnedPolicies`, `suggestedPolicies`, `appliedPolicies` (approved ones) | see below |

## The collector client (`pkg/sources`)

- **Discovery.** Running pods with an IP labelled `app.kubernetes.io/component=collector` in `--collector-namespace`
  (default: `$POD_NAMESPACE`, else `gryvia-network`). The pod list bypasses the informer cache.
- **Fan-out.** One `GET` per pod (bounded concurrency `--collector-concurrency`, per-request timeout
  `--collector-timeout`, response size limit 8 MiB, at most 500 pods), results merged: graph edges with the same
  (source, target, protocol, port) add bytes and flow counts and average latency weighted by flow counts; anomalies and
  alerts are concatenated, deduplicated and sorted.
- **Signing.** With `--collector-token-file`, every request carries `X-Gryvia-Time` and `X-Gryvia-Signature =
  hex(HMAC-SHA256(token, "GRYVIA-API-V1\nGET\n<request-uri>\n<unix>"))` (the collector's scheme, 30 s skew). The file is
  read on every request, so a rotated Secret takes effect without a restart; a missing or short (<32 characters) file
  fails closed (`Misconfigured`), it never falls back to unsigned.
- **TLS.** `--collector-tls` (system roots) or `--collector-ca-file` (pinned CA; implies https), `--collector-server-name`
  (collectors are reached by pod IP, so the certificate must carry this name), `--collector-client-cert-file` /
  `--collector-client-key-file` for mTLS. TLS >= 1.2, redirects are never followed with a signed request.
- **Netra** (optional): environment `GRYVIA_NETRA_URL`, `GRYVIA_NETRA_TOKEN` (bearer), `GRYVIA_NETRA_CA_FILE`,
  `GRYVIA_NETRA_INSECURE=1` (same switch as the gateway). Read: `GET /api/v1/flows/history?since=..&limit=..` (unverified
  against a live netrad; the shape follows the gateway's client).

## Conditions

Every status has `SourceAvailable`:

| Status / reason | Meaning |
|-----------------|---------|
| `True` / `Available` | Read. The message lists limits that apply (for example "no verdicts"). |
| `True` / `PartialData` | Some collector pods answered ("2 of 3"): figures cover those nodes only. |
| `False` / `NoCollectors` | No running collector pod in the namespace (collector not deployed, or another namespace). |
| `False` / `Unreachable` | Pods exist but none answered (message has the HTTP status, for example 401 for a wrong token). |
| `False` / `Misconfigured` | Token file, CA or client certificate unusable. |
| `False` / `NotConfigured` | Netra is not configured (`GRYVIA_NETRA_URL`). |
| `False` / `NoSignal`, `NoData`, `Stale`, `CRDNotInstalled` | Fabric-signal or usage-record sources (missing object, no status yet, older than 5 min, CRD absent). |

While a source is unavailable a controller keeps the last known data (it never overwrites it with zeros) and does not
advance its timestamp. Other conditions: `RatesConfigured` (NetworkCost), `Enforceable` (FlowPolicy),
`EnforceNotAutomatic` (AutoPolicy `mode: enforce`), `AutoBlockIgnored` (SecurityPolicy).

## Behaviour changes to know about

- **`GryviaSecurityPolicy.spec.autoBlock` is deprecated and ignored.** The operator never blocked anything on an alert
  (its code decoded the collector's answer with the wrong shape and JSON names, so it could never fire); the field stays in the schema so existing objects validate, and a condition
  says so when it is `true`. The `/api/v1/health` call is gone.
- **AutoPolicy never applies anything.** It writes `GryviaFlowPolicy` suggestions (label `gryvia.io/suggested=true`,
  `gryvia.io/auto-policy=<name>`, owner reference, deterministic name `auto-<src>-to-<dst>-<port>-<hash>`); the
  FlowPolicy controller reports them `Suggested` and creates no `CiliumNetworkPolicy` until the label is removed
  (`gryvia network policy apply NAME`). Mode `enforce` is treated as `suggest`; its old direct creation of a
  selector-less CiliumNetworkPolicy was removed. Suggestions are only made for sources in the AutoPolicy's own namespace
  (a CiliumNetworkPolicy only selects pods of its namespace), after the learning window has elapsed since creation. The
  confidence is a heuristic from the flow count (>=100: 0.9, >=10: 0.75, else 0.5). A deleted suggestion is recreated on
  the next cycle while the edge is still learned; exclude the service with `excludeServices` to stop it. Approving an
  allow policy limits the source pods' egress to what it lists (Cilium default-deny on selected endpoints): review the
  set before approving.
- **FlowPolicy resolves Service endpoints.** An endpoint with a `service` and no labels now takes the Service's pod selector;
  an unresolvable Service makes the policy `Failed` (`UnresolvedEndpoint`) instead of producing a namespace-wide policy.
  The Cilium policy is owned by the FlowPolicy (garbage-collected with it).
- **`GryviaNetworkAnomaly.autoMitigate` now acts on real data.** Before, the metrics were always empty so it never fired.
  It creates a temporary ingress-deny CiliumNetworkPolicy (`app=<targetService>`, 15 minutes) for new critical/high
  collector anomalies. Enable it only after reading what the collector reports.
- **NetworkCost** uses 1 GB = 10^9 bytes (was 2^30) and only egress with a price, like the gateway; reports are recomputed per
  namespace/day instead of appended on every reconcile. `totalCostUSD` holds the rate's currency (a condition says when it
  is not USD).
- New status fields (all optional): `conditions` everywhere; `latencyP50`, `bytesTotal`, `flowCount` on graph edges;
  signal figures on Training/InferenceInsight.

## Enabling it

```bash
helm install network-intelligence ./helm/network-intelligence --namespace gryvia-network --create-namespace \
  --set ebpf.enabled=true --set ebpf.security.apiTokenSecret=collector-api-token \
  --set operator.sources.enabled=true
```

`operator.sources.enabled` adds the operator flags, the Secret mounts and the RBAC (the operator's own ten kinds, read of
`GryviaFabricSignal`/`GryviaNetworkUsageRecord`/`GryviaNetworkRate`, management of `CiliumNetworkPolicy`; pods, services and
ConfigMaps were already granted). The token Secret defaults to `ebpf.security.apiTokenSecret`. For TLS set
`operator.sources.collector.tls`, `.caSecret`, `.serverName` (and `.clientCertSecret` for mTLS). With the default chart
values nothing changes in the rendered manifests. Not yet run on a cluster.

For the network-cost and fabric sources the collector needs `ebpf.attributeNetwork` + `ebpf.publishNetworkUsage` and
`ebpf.publishFabricStatus` (see `docs/network-cost-attribution.md`, `docs/fabric-scheduling.md`).

## Limits and unverified

- Collector graph edges are cumulative counters of live edges (an edge disappears ten minutes after its last flow), not a
  windowed rate; the node namespace in the collector graph is the flow event's namespace for both ends, so namespace scoping
  matches Services by name. Two Services with the same name in different target namespaces are ambiguous and skipped by
  AutoPolicy.
- Alert de-duplication uses `status.lastAlert`: an alert older than the newest one already counted (from a slow node) is not
  counted. A restarted collector loses its alert ring.
- The operator lists Pods and ConfigMaps through the manager's cache (cluster-wide informers); large clusters may prefer to
  restrict the cache (not done).
- Unverified: the real collector's `/api/v1/*` responses at scale, request signing against a deployed collector with
  `-api-token-file`, TLS/mTLS against real certificates, Netra's history API, CiliumNetworkPolicy behaviour of the generated
  policies, RDMA/NCCL fabric signals on hardware, and the anomaly/severity heuristics on real traffic.
- Nothing here uses Hubble or Prometheus.
