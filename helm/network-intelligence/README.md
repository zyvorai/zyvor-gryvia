# Gryvia Network Intelligence Helm Chart

Deploys the Gryvia network-intelligence operator and, optionally, the experimental eBPF collector DaemonSet. This is the
sixth Gryvia operator; the other five are installed by [`helm/gryvia`](../gryvia/README.md). The two charts are
independent: neither requires the other.

**Status: experimental.**

- The operator reconciles ten kinds (`GryviaFlowPolicy`, `GryviaTrafficInsight`, `GryviaAutoPolicy`,
  `GryviaTraceSession`, `GryviaServiceGraph`, `GryviaNetworkAnomaly`, `GryviaSecurityPolicy`, `GryviaNetworkCost`,
  `GryviaTrainingInsight`, `GryviaInferenceInsight`). Several of them read data from Hubble, Prometheus or the collector; see
  the [operator README](../../operators/network-intelligence/README.md) for which paths are real.
- The collector (`ebpf.enabled`) is **off by default**, runs privileged with `hostNetwork` and `hostPID`, and its image
  (`gryvia-ebpf-collector`) is built in CI and is in the release workflow, but **no tagged release has published it
  yet**: until one does, build it from `collector/Dockerfile` and set `collector.image.repository`/`tag`. It was verified on Linux 7.0 x86_64 only; GPU, RDMA,
  arm64 and the gated attachments are unverified on hardware. See [collector/README.md](../../collector/README.md) and
  [ebpf/README.md](../../ebpf/README.md).
- The keys `security.enabled`, `security.autoBlock`, `ha.enabled` and the top-level `labels` and `annotations` were
  removed from `values.yaml`: no template read them (the operator has no `--security-*` flags and no auto-blocking
  exists; earlier chart versions passed them as arguments Go's flag parser rejects). Setting them was already a no-op.
- The operator no longer uses a fixed collector URL: it finds the collector pods by label
  (`app.kubernetes.io/component=collector`) in its own namespace (or `operator.sources.collector.namespace`; without the chart wiring the built-in default is `gryvia-network`), signs its
  requests with the API token from a mounted Secret and merges the answers of all pods. Without that wiring
  (`operator.sources.enabled=false`, the default) it looks in `gryvia-network` and sends unsigned requests; a status then says `SourceAvailable=False` with the reason. See
  [docs/network-intelligence-sources.md](../../docs/network-intelligence-sources.md). Not yet run against the real collector
  on a cluster (the e2e workflow uses a fake collector). The default chart also lacks RBAC for the operator's own ten
  kinds; `operator.sources.enabled=true` adds it.
- Flow data can also come from [Netra](https://github.com/zyvorai/netra) through the gateway
  (`apiGateway.netra.url` in the main chart), which needs neither this collector nor its privileges.

## Prerequisites

- Kubernetes 1.30 or newer (as the main chart), Helm 3
- For the collector: a Linux kernel with BTF (`/sys/kernel/btf/vmlinux`); Linux 6.6 or newer for the TCX programs
- Prometheus Operator (optional, for the ServiceMonitors)

## Installation

```bash
# published chart
helm install network-intelligence oci://ghcr.io/zyvorai/charts/gryvia-network-intelligence \
  --namespace gryvia-network --create-namespace

# or from a checkout
helm install network-intelligence ./helm/network-intelligence \
  --namespace gryvia-network --create-namespace
```

The chart is also in the classic repository (`helm repo add gryvia https://zyvorai.github.io/gryvia/charts`), where its
name is `gryvia-network-intelligence`.

## Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `operator.image.repository` / `.tag` | Operator image | `ghcr.io/zyvorai/gryvia-network-intelligence-operator`, chart appVersion |
| `operator.replicas`, `operator.resources` | Operator size | `1`, 100m/128Mi requests, 500m/512Mi limits |
| `operator.sources.enabled` | Wire the operator to its data sources: flags, token/TLS Secret mounts and the RBAC of its ten kinds (fabric signals, usage records and rates read-only, CiliumNetworkPolicy) | `false` |
| `operator.sources.collector.namespace`, `.port`, `.timeout` | Where the collector pods are (default: the release namespace), their port and the per-request timeout | `""`, `9090`, `5s` |
| `operator.sources.collector.tokenSecret`, `.tokenKey` | Secret with the collector API token (defaults to `ebpf.security.apiTokenSecret`); read on every request, requests are HMAC-signed | `""`, `token` |
| `operator.sources.collector.tls`, `.caSecret`, `.caKey`, `.serverName`, `.clientCertSecret` | https to the collectors, CA bundle, certificate name to verify, mTLS client certificate | `false`, `""`, `ca.crt`, `gryvia-collector`, `""` |
| `operator.sources.netra.url`, `.tokenSecret`, `.tokenKey`, `.caSecret`, `.caKey` | Netra flow history for `GryviaTraceSession` and `matchedFlows` (env `GRYVIA_NETRA_URL` etc.) | `""` |
| `ebpf.enabled` | Run the eBPF collector DaemonSet | `false` |
| `collector.image.repository` / `.tag` | Collector image (in the release workflow, not yet published by a tagged release) | `ghcr.io/zyvorai/gryvia-ebpf-collector` |
| `collector.hostNetwork` | Host networking for the collector; the pod's port 9090 is then bound on the node | `true` |
| `ebpf.interface` | Interface for the XDP/TCX programs (`-iface`); empty means they are not attached | `""` |
| `ebpf.xdpMux` | Attach `xdp_mux` as the interface's only XDP program and chain `roce_cnp`, `pfc_pause`, `dns_tracker`, `packet_filter` and `roce_ecn` behind it (`-xdp-mux`); needs `ebpf.interface`. Without it only one XDP program can attach per interface. Tested in CI on the loopback interface, not on a NIC | `false` |
| `ebpf.enablePrograms` | Opt-in eBPF programs to attach (`-enable-programs`): `nccl_transport`, `p2p_fallback`, `capture_gate`, `gpu_oom`, `graph_stall`, `gdr_fail`, `infer_ttft`, `weight_mmap`, `gpu_dev`, `ucx_complete`, or `all`. They are skipped by default because nothing in the collector reads their signals yet | `[]` |
| `ebpf.cgroupPath` | cgroup v2 path for sockops/sk_msg (`-cgroup-path`); empty means not attached | `""` |
| `ebpf.ncclLib`, `ebpf.cudaLib` | Library paths for the GPU uprobes; empty means auto-discover | `""` |
| `ebpf.flightTokenSecret`, `ebpf.flightTokenKey` | Secret holding the Flight Recorder token (`-flight-token-file`); empty disables that endpoint | `""`, `token` |
| `ebpf.diagnosis.cgroupSignals` | Let the unified diagnosis read the pods' cgroup counters and pressure files from the host cgroup mount (`-flight-diagnosis-cgroup`, read-only); otherwise those signals are reported unavailable | `false` |
| `ebpf.diagnosis.thresholds` | Overrides for the diagnosis thresholds (rendered to a ConfigMap, `-flight-diagnosis-thresholds`) | `{}` |
| `ebpf.flightStore.enabled` | Persist incident history on the node (`-flight-store-dir`); `.volume` is `emptyDir` or `hostPath`, plus `.hostPath`, `.sizeLimit`, `.retention`, `.maxBytes`, `.fsync`, `.incidentMinDuration` | `false` |
| `ebpf.fabricStatusPerNode` | With `publishFabricStatus`: write this node's own `status.nodes[]` entry by server-side apply instead of merge-patching the top level (`-fabric-status-per-node`); needs the ai-operator's `aiOperator.mergeFabricSignals`; no extra RBAC | `false` |
| `ebpf.publishFabricStatus` | Patch the status of existing `GryviaFabricSignal` objects every 30 s (`-publish-fabric-status`); adds `list` on `gryviafabricsignals` and `patch` on `gryviafabricsignals/status` to the collector ClusterRole | `false` |
| `ebpf.publishNodeFabric` | Write this node's fabric health to the cluster-scoped `GryviaNodeFabric` named after the node every 30 s (`-publish-node-fabric`); adds `create`, `patch` on `gryvianodefabrics` to the collector ClusterRole | `false` |
| `ebpf.inferMetrics.targets`, `.discover`, `.ports`, `.interval` | Read serving-engine latency (TTFT, ITL, queue time) from vLLM / Triton / TGI metrics endpoints (`-infer-metrics`, `-infer-metrics-discover`); read-only GETs, no new RBAC; see [docs/inference-latency.md](../../docs/inference-latency.md) | `[]`, `false`, `8000,8002,8080`, `15s` |
| `ebpf.traceCorrelation` | Attach W3C trace ids to the Flight Recorder timeline and serve the HMAC-protected trace lookup (`-trace-correlate`); needs `ebpf.interface` and `ebpf.flightTokenSecret` | `false` |
| `ebpf.quotaPace.enabled` | Attach `quota_pace`, the one eBPF program that changes sockets (`-quota-pace`); needs `ebpf.cgroupPath`; alone it paces nothing | `false` |
| `ebpf.quotaPace.sync` | **Mutating.** Grant pace leases from `GryviaQuota` `spec.network.maxEgressMbps` (`-quota-pace-sync`); needs `ebpf.quotaPace.enabled`; adds `list` on `gryviaquotas` to the collector ClusterRole | `false` |
| `ebpf.quotaPace.dryRun` | Log what `sync` would do and write nothing (`-quota-pace-dry-run`); needs `ebpf.quotaPace.sync` | `false` |
| `prometheus.serviceMonitor.enabled` | Create ServiceMonitors (only when the Prometheus Operator CRD exists) | `true` |
| `namespace.name`, `namespace.create` | Target namespace | `gryvia-network`, `true` |
| `ha.leaderElection` | Operator leader election | `true` |

The collector runs as root and privileged (a requirement of loading eBPF programs); its port 9090 serves unauthenticated
endpoints except the Flight Recorder route. Do not expose it outside the cluster operators: restrict ingress with a
NetworkPolicy or use node firewall rules, since a pod NetworkPolicy does not apply with `hostNetwork: true`. Details
and a policy example are in [docs/flight-recorder.md](../../docs/flight-recorder.md).

The program list is not configurable per program: the collector loads every `.o` in its image and attaches what it can.
Check which ones attached on a node:

```bash
kubectl -n gryvia-network port-forward pod/<collector-pod> 9090:9090
curl -s localhost:9090/api/v1/ebpf/status
```

## Upgrading and uninstalling

```bash
helm upgrade network-intelligence ./helm/network-intelligence --namespace gryvia-network
helm uninstall network-intelligence --namespace gryvia-network
```

The CRDs come from `helm/gryvia` (or `crds/`), not from this chart, and are kept on uninstall.

## Monitoring

With `prometheus.serviceMonitor.enabled=true` and the Prometheus Operator installed, ServiceMonitors are created for the
operator (`:8080/metrics`) and the collector (`:9090/metrics`). `prometheus.rules.enabled` and `prometheus.dashboards.enabled`
(both off by default) also install the alert rules and Grafana dashboards from `monitoring/` (enable them in one chart only;
the gryvia chart's `monitoring.*` installs the same assets). See [docs/observability.md](../../docs/observability.md).
