# NetPredator - Network Intelligence Layer

NetPredator is the network intelligence layer for the Gryvia GPU platform. It provides Cilium/eBPF-based network visibility, security, and traffic control through Kubernetes custom resources. The operator registers ten controllers; this page details the first six (`GryviaSecurityPolicy`, `GryviaNetworkCost`, `GryviaTrainingInsight` and `GryviaInferenceInsight` are also registered but not described here).

> **Status.** The ten controllers read real sources: the per-node collector pods (`/api/v1/graph`, `/anomalies`, `/security/alerts`; discovered by label, HMAC-signed, merged), Netra (optional, `GRYVIA_NETRA_URL`), and Kubernetes objects (`GryviaNetworkUsageRecord`, `GryviaNetworkRate`, `GryviaFabricSignal`). Every status carries a `SourceAvailable` condition that says when a source is missing. Hubble and Prometheus are not used. See [`docs/network-intelligence-sources.md`](../../docs/network-intelligence-sources.md) for what each controller reads, its limits and what is unverified. Verified with unit tests (fake client, fake sources, `httptest`) and a kind e2e workflow that uses a FAKE collector; **not** verified against the real collector at scale, Cilium or Hubble. `GryviaFlowPolicy` creates a CiliumNetworkPolicy and needs Cilium.

## Architecture

```
  GryviaFlowPolicy      -> CiliumNetworkPolicy (Cilium CNI)
  GryviaAutoPolicy      -> learned edges -> suggested GryviaFlowPolicy (never applied automatically)
  GryviaServiceGraph  \
  GryviaTrafficInsight  }-- collector pods: /api/v1/graph, /api/v1/anomalies  (label app.kubernetes.io/component=collector,
  GryviaNetworkAnomaly  |                                                       HMAC-signed with the mounted token, merged)
  GryviaSecurityPolicy /   /api/v1/security/alerts
  GryviaTraceSession    -- Netra flow history (optional)
  GryviaNetworkCost     -- GryviaNetworkUsageRecord + GryviaNetworkRate (Kubernetes objects)
  GryviaTrainingInsight \__ GryviaFabricSignal.status (published by the collector)
  GryviaInferenceInsight/
```

Operator flags for the collector (all optional): `--collector-namespace`, `--collector-port`, `--collector-token-file`,
`--collector-tls`, `--collector-ca-file`, `--collector-client-cert-file`, `--collector-client-key-file`,
`--collector-server-name`, `--collector-timeout`, `--collector-concurrency`; Netra: environment `GRYVIA_NETRA_URL`,
`GRYVIA_NETRA_TOKEN`, `GRYVIA_NETRA_CA_FILE`, `GRYVIA_NETRA_INSECURE`.

## Custom Resources

### GryviaFlowPolicy

Intent-based network policy that translates high-level traffic intents into CiliumNetworkPolicies.

**Intents:**
- `low-latency` - Sets DSCP EF marking and high priority
- `high-throughput` - Optimizes for bulk data transfer
- `secure` - Enables WireGuard encryption via Cilium
- `default` - Standard traffic handling

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaFlowPolicy
metadata:
  name: gpu-training-data
  namespace: ml-training
spec:
  source:
    service: data-loader
    namespace: ml-training
    labels:
      app: data-loader
  destination:
    service: model-server
    namespace: ml-training
    port: 8080
    labels:
      app: model-server
  protocol: tcp
  action: allow
  intent: high-throughput
  priority: 100
```

### GryviaTrafficInsight

Traffic analysis from the collector graph: top talkers, a smoothed p50 latency, throughput (growth of the byte counters between reconciles) and the collector's anomalies for the service. p99 latency and drop counts have no source and stay unset.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaTrafficInsight
metadata:
  name: model-server-insight
  namespace: ml-training
spec:
  service: model-server
  namespace: ml-training
  window: "5m"
  metrics:
    - latency
    - throughput
    - drops
    - retransmits
```

### GryviaAutoPolicy

Self-healing firewall that learns traffic patterns and automatically generates network policies.

**Modes:**
- `learn` - Record service-to-service edges learned from the collector graph (ConfigMap `autopolicy-<name>-learned`)
- `suggest` - After the learning window, write `GryviaFlowPolicy` suggestions (label `gryvia.io/suggested=true`, never applied automatically; approve with `gryvia network policy apply`)
- `enforce` - Behaves like `suggest` (condition `EnforceNotAutomatic`): nothing is applied without a human

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAutoPolicy
metadata:
  name: ml-namespace-autopolicy
  namespace: ml-training
spec:
  mode: learn
  learningWindow: "24h"
  targetNamespaces:
    - ml-training
    - ml-inference
  excludeServices:
    - kube-dns
    - metrics-server
  approvalRequired: true
```

### GryviaTraceSession

Time-limited network trace/debug sessions. Flows come from Netra's history for the window (needs `GRYVIA_NETRA_URL` on the operator; otherwise the condition says nothing is captured) and are written to the results ConfigMap. `level`, `captureHeaders` and `filters.srcIP` are not applied.

**Trace levels:**
- `l3` - IP-level flow capture
- `l4` - TCP/UDP connection tracking
- `l7` - Application-layer protocol inspection (HTTP headers with `captureHeaders: true`)

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaTraceSession
metadata:
  name: debug-model-server
  namespace: ml-training
spec:
  service: model-server
  namespace: ml-training
  duration: "10m"
  level: l4
  filters:
    port: 8080
    protocol: tcp
  captureHeaders: false
```

### GryviaServiceGraph

Service dependency graph: nodes are the namespaces' Services, edges the merged collector graph (bytes, flow count, smoothed p50 latency; no verdicts or health).

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaServiceGraph
metadata:
  name: ml-platform-graph
  namespace: ml-training
spec:
  namespaces:
    - ml-training
    - ml-inference
    - data-pipeline
  refreshInterval: "30s"
  includeExternal: true
  depth: 3
```

### GryviaNetworkAnomaly

Network anomaly detection with threshold-based rules, webhook alerting, and automatic mitigation.

**Detections** are the collector's own (`/api/v1/anomalies`): latency spikes, traffic bursts, new connections, DNS failures. `detectionRules` only select which types are kept (`latency`, `throughput`, `connections`); thresholds are not evaluated here.

**Auto-mitigation:** For critical/high severity anomalies, creates temporary CiliumNetworkPolicy deny rules, with automatic expiration after 15 minutes.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaNetworkAnomaly
metadata:
  name: model-server-anomaly-detector
  namespace: ml-training
spec:
  targetService: model-server
  detectionRules:
    - metric: latency
      operator: gt
      threshold: 500
      window: "5m"
    - metric: drops
      operator: gt
      threshold: 100
      window: "1m"
    - metric: connections
      operator: gt
      threshold: 10000
      window: "5m"
  alertWebhook: "https://alerts.example.com/network"
  autoMitigate: true
```

## Prerequisites

- Kubernetes cluster with Cilium CNI (needed for CiliumNetworkPolicy resources)
- The eBPF collector (helm `ebpf.enabled`) for graph, anomalies and security alerts; Netra for trace flows and matched flows (both optional; objects report `SourceAvailable=False` without them)

## Building

```bash
make build       # Build the operator binary
make test        # Run tests
make docker-build # Build Docker image
make docker-push  # Push Docker image
```

## Deploying

```bash
make deploy      # Apply CRDs and deploy the operator
make undeploy    # Remove the operator deployment
```

Note: `make deploy` applies `config/`, which does not exist in this directory, so it will fail as is. Use the Helm chart in `helm/network-intelligence` (or the main `helm/gryvia` chart) instead.
