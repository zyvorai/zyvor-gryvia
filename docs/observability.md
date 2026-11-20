# Observability

What Gryvia exports, which dashboards and alerts use it, how to switch it on, and what is not verified.

**Verification status.** The dashboards and rules were validated by metric name (every referenced metric is defined by an exporter in
this repository or allow-listed) and by structure. They were never rendered against a live Prometheus or Grafana, `promtool` was not
available on the development machine (CI runs it), and no GPU, RDMA NIC or Kubernetes cluster was involved. Alert thresholds are
starting points, not tuned on real fleets. Treat the first week in your environment as calibration.

## Metric catalog

### Collector (`collector/`, port 9090 `/metrics`, chart `helm/network-intelligence`)

The collector exports Prometheus metrics next to its JSON API. Protect `/metrics` with `ebpf.security` (bearer token, TLS); see
[collector-security.md](collector-security.md).

| Family | Metrics |
|---|---|
| Network flows | `gryvia_network_flow_bytes_total`, `_latency_seconds`, `_connections_active`, `_drops_total` (labels `src_service, dst_service, protocol, namespace`), `gryvia_network_dns_latency_seconds{namespace}`, `gryvia_tcp_rtt_seconds`, `gryvia_tcp_cwnd_histogram` |
| Network cost | `gryvia_network_cost_bytes_total{zone_type,namespace}` (zone_type: same-zone, cross-zone, same-node, unknown-zone, internet), `gryvia_netcost_unattributed_bytes_total` |
| NCCL / GPU | `gryvia_nccl_operation_duration_seconds{op_type,rank}`, `gryvia_nccl_bytes_total{op_type}`, `gryvia_nccl_stragglers_detected_total`, `gryvia_gpu_memcpy_bytes_total{direction}`, `gryvia_gpu_memcpy_duration_seconds`, `gryvia_training_comm_compute_ratio{job}`, `gryvia_pipeline_bottleneck{phase}` |
| RDMA / NIC | `gryvia_rdma_send_bytes_total`, `_recv_bytes_total`, `gryvia_rdma_completion_latency_seconds`, `gryvia_nic_counter_rate{device,port,counter}`, `gryvia_ibverbs_*`, `gryvia_pfc_*`, `gryvia_roce_*` |
| Fabric status | `gryvia_fabric_*{namespace,job}`: `score_delta`, `straggler_rank`, `nccl_p99_seconds`, `rdma_retry_rate`, `cnp_rate`, `pfc_rate`, `nic_retry_rate`, `nic_error_rate`, `gds_hit_ratio`, `overlap_idle_ratio`, `infer_wait_p99_seconds`, `exfil_events`, `ucx_slow_p99_seconds`, `collectives_compared`, `collective_max_skew_seconds`, `gpu_idle_during_comm_ratio`, `sm_active_during_compute`, `gpu_correlation_coverage` (see [fabric-status.md](fabric-status.md)) |
| Inference | `gryvia_inference_latency_seconds{namespace,job,engine,metric}`, `gryvia_inference_requests{...,state}`, `gryvia_inference_kv_cache_usage_ratio` (opt-in scraping of vLLM/Triton/TGI, see [inference-latency.md](inference-latency.md)) |
| Health | `gryvia_ebpf_program_attached{object,program,kind}`, `gryvia_measurement_dropped_events{source}` |
| Security | `gryvia_security_alerts_total{event_type,severity}`, `gryvia_security_escape_attempts_total`, `gryvia_security_mining_detections_total` |

Some series exist only when a feature flag is on (NIC counters, ibverbs, inference scraping, DCGM correlation); an absent series means
"not measured", not "healthy".

### Quota operator (`operators/quota-operator/pkg/metrics`, port 8080 `/metrics`)

Computed at scrape time by a custom collector that lists `GryviaQuota`, `GryviaUsageRecord` and `GryviaTenant` from the manager cache, so
there are no reconciler hooks and no stale series after an object is deleted. Registered on controller-runtime's registry, so they sit next to
`controller_runtime_*`.

| Metric | Labels | Source |
|---|---|---|
| `gryvia_quota_gpus_allocated`, `gryvia_quota_gpus_max`, `gryvia_quota_running_jobs`, `gryvia_quota_queued_jobs` | `team` | `GryviaQuota` status and spec |
| `gryvia_quota_budget_percent_used`, `gryvia_quota_budget_spent` | `team` | `GryviaQuota.status.budgetStatus` (only quotas with a budget) |
| `gryvia_usage_gpu_hours_total`, `gryvia_usage_cost_total` | `tenant, sku, currency` | sum over existing `GryviaUsageRecord` objects |
| `gryvia_usage_records_open` | none | records with `final: false` |
| `gryvia_tenants` | none | `GryviaTenant` count |
| `gryvia_quota_metrics_list_errors` | none | kinds that could not be listed during the scrape |

Limits: the `*_total` usage series are sums over records that currently exist, so deleting old records lowers them and `rate()` will
see a reset; they are estimates from job wall-clock time, not invoices. Cardinality is bounded: quotas by team, usage by (tenant, sku,
currency) with never a job or namespace label; beyond 1000 combinations the surplus is folded into `tenant="_other"`. `GryviaBudget`
objects are not exported here; their spend and state are in each object's status (`gryvia budget`).

### API gateway (`services/api-gateway/routers/observability.py`)

Disabled by default. `/metrics` exists only when the gateway is started with `GRYVIA_METRICS_TOKEN` (at least 32 characters) and then
answers only `Authorization: Bearer <token>` (constant-time compare, same model as the collector); with no token the route is not
registered (404). The API key is deliberately not reused. Helm: `apiGateway.metrics.enabled=true` plus `apiGateway.metrics.tokenSecret`
(an existing Secret you create; the chart refuses to render without it).

| Metric | Labels |
|---|---|
| `gryvia_gateway_http_requests_total`, `gryvia_gateway_http_request_duration_seconds` | `method, route (template such as /api/jobs/{name}), status` |
| `gryvia_gateway_audit_events_total` | `method (POST, PUT, PATCH, DELETE), outcome (success, denied, error)`; never identity |
| `gryvia_gateway_auth_attempts_total` | `method (session, oidc, api_key, login), result (success, failure)`; never user identity |
| `gryvia_gateway_rate_limit_hits_total` | `route` (429 responses) |
| `gryvia_gateway_collector_targets`, `gryvia_gateway_collector_reachable` (gauges, last fan-out), `gryvia_gateway_collector_fanout_total{outcome}` | |

Limits: metrics are per gateway replica and in memory (reset on restart); unmatched paths share `route="unmatched"`; client IPs are not labels.
The fan-out gauges update only when a page that queries collectors is requested.

### Operators

All operators serve the standard controller-runtime metrics on `:8080` (`controller_runtime_reconcile_*`, `workqueue_*`, Go and process
metrics) over plain HTTP. Only the quota operator adds the custom series above; the other operators export nothing Gryvia-specific.

### External metrics (not exported by Gryvia)

Listed in `monitoring/external-metrics.txt`. Panels and alerts that need them say so in their title or description.

| Needs | Used for |
|---|---|
| **dcgm-exporter** (`dcgmExporter.enabled` in the gryvia chart, or your own) | the whole "GPUs (DCGM)" dashboard: `DCGM_FI_DEV_GPU_UTIL`, `DCGM_FI_PROF_SM_ACTIVE`, `DCGM_FI_DEV_FB_USED`, `DCGM_FI_DEV_POWER_USAGE`, `DCGM_FI_DEV_GPU_TEMP`, ... (`DCGM_FI_PROF_*` needs profiling support on the GPU) |
| **kube-state-metrics** | `GryviaPodCrashLooping`, "Pod restarts" panel |
| controller-runtime (already served by the operators) | `GryviaOperatorReconcileErrors`, the Operators dashboard |

## Dashboards (`monitoring/grafana-dashboards/`)

Raw dashboard JSON (what Grafana file provisioning and the sidecar expect), generated by `scripts/gen-dashboards.py`.

| Dashboard | Shows | Needs |
|---|---|---|
| Overview | fleet headline numbers, reconcile errors, gateway rate, drops | mixed (each panel says) |
| Fabric status | per-job fabric signals, NCCL, GDS, GPU correlation | collector |
| NIC, RDMA, PFC and CNP | NIC hardware counters, PFC/CNP, verbs, completions | collector with RDMA/XDP features |
| Inference serving | TTFT/ITL/queue p99, running/waiting, KV cache | collector with `-infer-metrics` |
| Collector health | attach state, drops by source, unattributed bytes | collector |
| Network cost | bytes by zone class and namespace | collector with cost attribution |
| eBPF network flows | flows, drops, DNS, TCP RTT | collector |
| Security events | detections, exfil signals | collector |
| Quotas and budgets | GPU quota use, queue, budget | quota operator |
| Usage and cost | GPU-hours and cost by tenant/SKU | quota operator |
| GPUs (DCGM) | utilization, SM, memory, power, temperature, XID | dcgm-exporter |
| API gateway | requests, latency, auth, fan-out | `apiGateway.metrics.enabled` |
| Operators | reconcile rate/errors/duration, queue depth | operators |

Removed because no exporter can feed them: the old per-GPU `gryvia_gpu_*` health panels (GPU health comes from dcgm-exporter, now the
DCGM dashboard), job counters and duration histograms (`gryvia_job_*`; the operators export none), storage backend and SR-IOV panels
(no storage or SR-IOV metrics exist), budget projections and "savings vs cloud" (`gryvia_budget_*`; no producer, and the savings figure had
no data source at all), and `gryvia-gpu-training.json` (replaced by Fabric status, which uses the real training and NCCL series).
The previous `monitoring/servicemonitor.yaml` was removed too; the charts create the monitors.

## Alerts (`monitoring/prometheus-rules.yaml`, runbooks in [runbooks/](runbooks/README.md))

Fabric score high, straggler persists, RDMA retries, CNP rate, PFC rate, NIC errors, inference queue time, TTFT, KV cache saturation,
eBPF program not attached, measurement drops, unattributed network bytes, quota near limit, budget near limit, gateway 5xx, gateway
auth failure spike, operator reconcile errors, pod crash looping. Each has severity, `for`, summary, description and a `runbook_url`
(GitHub blob URL of its runbook). Thresholds marked "placeholder" in the runbooks are guesses.

Gateway alerts are silent when the gateway metrics are off; a missing scrape target is not itself alerted here (use `up`).

## Enabling it

Helm (gryvia chart), for kube-prometheus-stack in the `monitoring` namespace:

```bash
kubectl -n gryvia-system create secret generic gryvia-gateway-metrics --from-literal=token="$(openssl rand -hex 24)"
helm upgrade --install gryvia helm/gryvia -n gryvia-system \
  --set monitoring.enabled=true \
  --set monitoring.prometheus.serviceMonitor.additionalLabels.release=kube-prometheus-stack \
  --set apiGateway.metrics.enabled=true --set apiGateway.metrics.tokenSecret=gryvia-gateway-metrics
```

`monitoring.enabled=true` installs (defaults are off, and the default render is unchanged):

- a `PodMonitor` for the enabled operators' `:8080/metrics` and, when `apiGateway.metrics.enabled`, a `ServiceMonitor` for the gateway that sends
  the bearer token (the token Secret must be in the monitor's namespace, `monitoring.prometheus.serviceMonitor.namespace`, default the release namespace);
- a `PrometheusRule` with the alerts; both are skipped when the `monitoring.coreos.com/v1` CRDs are absent (with `helm template` pass
  `--api-versions monitoring.coreos.com/v1` to see them);
- a `ConfigMap` with the dashboards labelled `grafana_dashboard: "1"`. The Grafana sidecar must watch that namespace (kube-prometheus-stack
  defaults to the release namespace of Grafana; set `grafana.sidecar.dashboards.searchNamespace=ALL`).

The collector is scraped by the network-intelligence chart's own ServiceMonitor (`prometheus.serviceMonitor.enabled`). Its
`prometheus.rules.enabled` and `prometheus.dashboards.enabled` (off) can install the same rules and dashboards from that chart; enable them in
one chart only. Without Helm: add `monitoring/prometheus-rules.yaml` to Prometheus `rule_files` and import the JSON files.

Chart copies of the assets live in `helm/*/monitoring-assets/` (Helm reads files only inside the chart). `scripts/sync-monitoring-assets.sh`
refreshes them and CI fails on drift.

## Guards in CI (`.github/workflows/repo-checks.yml`)

- `scripts/check-metrics-refs.py`: every metric name in dashboards and rules must be defined by a `gryvia_*` string literal in non-test code
  of `collector/`, `operators/`, `services/api-gateway/` (or a `record:` rule) or match `monitoring/external-metrics.txt`. It reads names only:
  it does not check labels or that a series has data. Tested against a deliberately bad fixture in `scripts/tests/test_monitoring_checks.py`.
- `scripts/check-monitoring-assets.py`: rule and dashboard structure (severity, `for`, annotations, runbook file exists, unique uids/ids).
- `promtool check rules monitoring/prometheus-rules.yaml` with the official release binary.
- dashboards regenerate identically from `scripts/gen-dashboards.py`; chart asset copies are current.

## Limits

- Cardinality: flow metrics carry `src_service, dst_service`; on large clusters drop them with relabelling or keep the collector's flow metrics off.
  Usage metrics are bounded as above; `gryvia_inference_*` and `gryvia_fabric_*` grow with the number of jobs.
- Names were validated, not semantics: a query can still be wrong (label mismatch, wrong unit, rate over a gauge). Expect to fix panels on first use.
- Collector series such as `gryvia_fabric_*` carry their own `job` label (the Gryvia job). The network-intelligence chart's collector ServiceMonitor
  sets `honorLabels: true` so Prometheus keeps it; a hand-written scrape config must do the same or the label becomes `exported_job` and the
  dashboards' `$job` variable and the fabric alerts stop matching.
- Operators serve metrics over plain HTTP on `:8080`, reachable by anything that can reach the pod; use a NetworkPolicy if that matters.
