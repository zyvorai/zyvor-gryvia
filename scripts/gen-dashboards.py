#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""Generate monitoring/grafana-dashboards/*.json (raw dashboard JSON, as Grafana file/sidecar provisioning expects).

Every query references a metric that a Gryvia component exports (collector, quota operator, api-gateway) or one of the
external metrics listed in monitoring/external-metrics.txt; scripts/check-metrics-refs.py enforces that in CI.
Dashboards were validated by metric name only and never rendered against a live Prometheus/Grafana.

Usage: python3 scripts/gen-dashboards.py   (rewrites the JSON files; commit the result)
"""
import json
import os

OUT = os.path.join(os.path.dirname(__file__), "..", "monitoring", "grafana-dashboards")
DS = {"type": "prometheus", "uid": "${datasource}"}
DCGM = " (requires dcgm-exporter)"


class Board:
    def __init__(self, uid, title, tags, description, variables=()):
        self.uid, self.title, self.tags, self.description = uid, title, tags, description
        self.panels, self.x, self.y, self.rowh, self.next_id = [], 0, 0, 0, 1
        self.variables = list(variables)

    def row(self, title):
        if self.x:
            self.y, self.x, self.rowh = self.y + self.rowh, 0, 0
        self.panels.append({"id": self._id(), "type": "row", "title": title, "collapsed": False,
                            "gridPos": {"h": 1, "w": 24, "x": 0, "y": self.y}, "panels": []})
        self.y += 1

    def _id(self):
        i, self.next_id = self.next_id, self.next_id + 1
        return i

    def _place(self, w, h):
        if self.x + w > 24:
            self.y, self.x, self.rowh = self.y + self.rowh, 0, 0
        pos = {"h": h, "w": w, "x": self.x, "y": self.y}
        self.x += w
        self.rowh = max(self.rowh, h)
        return pos

    def panel(self, kind, title, queries, unit="short", w=12, h=8, desc="", thresholds=None, extra=None):
        p = {"id": self._id(), "type": kind, "title": title, "description": desc, "datasource": DS,
             "gridPos": self._place(w, h),
             "targets": [{"refId": chr(65 + i), "datasource": DS, "expr": e, "legendFormat": lg}
                         for i, (e, lg) in enumerate(queries)],
             "fieldConfig": {"defaults": {"unit": unit}, "overrides": []}}
        if thresholds:
            p["fieldConfig"]["defaults"]["thresholds"] = {"mode": "absolute", "steps": thresholds}
        if extra:
            p.update(extra)
        self.panels.append(p)

    def ts(self, title, queries, unit="short", w=12, h=8, desc="", **kw):
        self.panel("timeseries", title, queries, unit, w, h, desc, **kw)

    def stat(self, title, expr, unit="short", w=6, h=4, desc="", thresholds=None, legend=""):
        self.panel("stat", title, [(expr, legend)], unit, w, h, desc, thresholds,
                   {"options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value"}})

    def table(self, title, expr, w=24, h=8, desc=""):
        self.panel("table", title, [(expr, "")], "short", w, h, desc,
                   extra={"targets": [{"refId": "A", "datasource": DS, "expr": expr, "instant": True, "format": "table"}]})

    def note(self, text, w=24, h=3):
        self.panels.append({"id": self._id(), "type": "text", "title": "Requirements", "gridPos": self._place(w, h),
                            "options": {"mode": "markdown", "content": text}})

    def build(self):
        tmpl = [{"name": "datasource", "type": "datasource", "query": "prometheus", "label": "Data source",
                 "current": {}, "hide": 0}]
        for name, query in self.variables:
            tmpl.append({"name": name, "type": "query", "datasource": DS, "query": {"query": query, "refId": name},
                         "definition": query, "includeAll": True, "multi": True, "allValue": ".*",
                         "current": {"text": "All", "value": "$__all"}, "refresh": 2, "sort": 1})
        return {"uid": self.uid, "title": self.title, "tags": ["gryvia"] + self.tags, "description": self.description,
                "schemaVersion": 39, "version": 1, "editable": True, "timezone": "browser", "refresh": "30s",
                "time": {"from": "now-6h", "to": "now"}, "templating": {"list": tmpl}, "annotations": {"list": []},
                "panels": self.panels}

    def write(self, fname):
        with open(os.path.join(OUT, fname), "w") as f:
            json.dump(self.build(), f, indent=2)
            f.write("\n")


def sel(var="job"):
    return '{namespace=~"$namespace", job=~"$job"}'


NS_JOB = [("namespace", "label_values(gryvia_fabric_score_delta, namespace)"),
          ("job", "label_values(gryvia_fabric_score_delta{namespace=~\"$namespace\"}, job)")]
GREEN_RED = [{"color": "green", "value": None}, {"color": "orange", "value": 0.3}, {"color": "red", "value": 0.6}]
GRR = lambda a, b: [{"color": "green", "value": None}, {"color": "orange", "value": a}, {"color": "red", "value": b}]  # noqa: E731


def overview():
    b = Board("gryvia-overview", "Gryvia / Overview", ["overview"],
              "Cluster-level health of the Gryvia components. Panels needing kube-state-metrics are labelled.")
    b.row("Fleet")
    b.stat("Tenants", "gryvia_tenants", desc="GryviaTenant objects (quota operator).")
    b.stat("GPUs allocated / limit", "sum(gryvia_quota_gpus_allocated) / sum(gryvia_quota_gpus_max)", "percentunit",
           desc="Across all GryviaQuota objects.", thresholds=GRR(0.8, 0.95))
    b.stat("Worst fabric score delta", "max(gryvia_fabric_score_delta)", "none",
           desc="0 = healthy, 1 = worst; max over every job the collectors report.", thresholds=GRR(0.3, 0.6))
    b.stat("eBPF programs not attached", "count(gryvia_ebpf_program_attached == 0) or vector(0)", "none",
           desc="Collector programs that failed to attach.", thresholds=GRR(1, 1))
    b.stat("Measurement drops", "sum(gryvia_measurement_dropped_events) or vector(0)", "none",
           desc="Events lost before analysis, monotonic since collector start.", thresholds=GRR(1, 1000))
    b.stat("Scrape targets down", "count(up{job=~\".*gryvia.*|.*collector.*\"} == 0) or vector(0)", "none",
           desc="Targets whose Prometheus job name contains gryvia or collector.", thresholds=GRR(1, 1))
    b.row("Control plane")
    b.ts("Reconcile errors per controller", [("sum by (controller) (rate(controller_runtime_reconcile_errors_total[5m]))", "{{controller}}")],
         "ops", desc="controller-runtime metric served by every operator.")
    b.ts("Gateway request rate by status class", [("sum by (status) (rate(gryvia_gateway_http_requests_total[5m]))", "{{status}}")], "reqps",
         desc="Only when the gateway /metrics is enabled (apiGateway.metrics.enabled).")
    b.ts("Pod restarts (last 1h)", [("sum by (namespace, pod) (increase(kube_pod_container_status_restarts_total{namespace=~\"gryvia.*\"}[1h]))", "{{namespace}}/{{pod}}")],
         "short", desc="Requires kube-state-metrics.")
    b.ts("Network drops", [("sum by (namespace) (rate(gryvia_network_drops_total[5m]))", "{{namespace}}")], "ops",
         desc="Collector data; needs ebpf.enabled in the network-intelligence chart.")
    b.write("gryvia-overview.json")


def fabric():
    b = Board("gryvia-fabric", "Gryvia / Fabric status", ["fabric", "training"],
              "Per-job fabric signals from the collector: NCCL, RDMA, GDS, GPU correlation. All series are node-local "
              "windows; a value can be absent when not measured.", NS_JOB)
    s = sel()
    b.row("Health")
    b.ts("Fabric score delta (0 healthy, 1 worst)", [(f"max by (namespace, job) (gryvia_fabric_score_delta{s})", "{{namespace}}/{{job}}")], "none", thresholds=GRR(0.3, 0.6))
    b.ts("Straggler rank", [(f"gryvia_fabric_straggler_rank{s}", "{{job}}")], "none", desc="Rank of the most recent NCCL straggler; 0 also means unknown.")
    b.ts("NCCL p99 of straggler-flagged collectives", [(f"gryvia_fabric_nccl_p99_seconds{s}", "{{job}}")], "s")
    b.ts("Max collective skew across local ranks", [(f"gryvia_fabric_collective_max_skew_seconds{s}", "{{job}}")], "s",
         desc="Host-side call duration slowest minus fastest, not GPU time.")
    b.ts("Collectives compared", [(f"gryvia_fabric_collectives_compared{s}", "{{job}}")], "none", desc="How many collectives the skew is computed over; low = weak evidence.")
    b.ts("UCX slow send p99", [(f"gryvia_fabric_ucx_slow_p99_seconds{s}", "{{job}}")], "s")
    b.row("Transport")
    b.ts("RDMA retry rate (per posted send)", [(f"gryvia_fabric_rdma_retry_rate{s}", "{{job}}")], "percentunit")
    b.ts("CNP per second", [(f"gryvia_fabric_cnp_rate{s}", "{{job}}")], "cps")
    b.ts("PFC pause frames per second", [(f"gryvia_fabric_pfc_rate{s}", "{{job}}")], "cps")
    b.ts("NIC retry / error rate (hardware counters)", [(f"gryvia_fabric_nic_retry_rate{s}", "retry {{job}}"), (f"gryvia_fabric_nic_error_rate{s}", "error {{job}}")], "cps")
    b.ts("GPUDirect Storage hit ratio", [(f"gryvia_fabric_gds_hit_ratio{s}", "{{job}}")], "percentunit", desc="Absent when not measurable.")
    b.ts("Inference network wait p99 (accept to first recv)", [(f"gryvia_fabric_infer_wait_p99_seconds{s}", "{{job}}")], "s",
         desc="Network wait only; for engine queue time and TTFT see the Inference dashboard.")
    b.row("GPU correlation (opt-in -dcgm-correlate)")
    b.ts("GPU idle during communication", [(f"gryvia_fabric_gpu_idle_during_comm_ratio{s}", "{{job}}"), (f"gryvia_fabric_overlap_idle_ratio{s}", "sync-in-allreduce {{job}}")], "percentunit")
    b.ts("SM active during compute", [(f"gryvia_fabric_sm_active_during_compute{s}", "{{job}}")], "percentunit")
    b.ts("DCGM sample coverage of communication time", [(f"gryvia_fabric_gpu_correlation_coverage{s}", "{{job}}")], "percentunit", desc="Low: dcgm-exporter collect interval too coarse for the correlation.")
    b.ts("Comm/compute ratio", [("gryvia_training_comm_compute_ratio", "{{job}}")], "none")
    b.ts("NCCL stragglers detected (rate)", [("rate(gryvia_nccl_stragglers_detected_total[5m])", "stragglers/s")], "ops")
    b.ts("NCCL operation p99 by type", [("histogram_quantile(0.99, sum by (le, op_type) (rate(gryvia_nccl_operation_duration_seconds_bucket[5m])))", "{{op_type}}")], "s")
    b.ts("NCCL bytes/s by type", [("sum by (op_type) (rate(gryvia_nccl_bytes_total[5m]))", "{{op_type}}")], "Bps")
    b.ts("GPU memcpy bytes/s by direction", [("sum by (direction) (rate(gryvia_gpu_memcpy_bytes_total[5m]))", "{{direction}}")], "Bps")
    b.ts("Pipeline bottleneck phase", [("gryvia_pipeline_bottleneck", "{{phase}}")], "none", desc="1 = the phase is the current bottleneck.")
    b.write("gryvia-fabric.json")


def nic():
    b = Board("gryvia-nic-rdma", "Gryvia / NIC, RDMA, PFC and CNP", ["rdma", "nic", "network"],
              "RDMA NIC hardware counters (/sys/class/infiniband), verbs activity and RoCE/PFC packet counters. "
              "Needs the collector with -nic (counters) and the XDP programs (PFC/CNP). No RDMA hardware was available "
              "when this was written: unverified on real NICs.")
    b.row("NIC hardware counters (gryvia_nic_counter_rate, per second)")
    b.ts("Port throughput", [('sum by (device, port) (gryvia_nic_counter_rate{counter=~"port_xmit_(data|bytes)"})', "tx {{device}}/{{port}}"),
                             ('sum by (device, port) (gryvia_nic_counter_rate{counter=~"port_rcv_(data|bytes)"})', "rx {{device}}/{{port}}")], "Bps",
         desc="port_*_data counters are converted from 4-byte units by the collector.")
    b.ts("Port packets", [('sum by (device, port) (gryvia_nic_counter_rate{counter="port_xmit_packets"})', "tx {{device}}/{{port}}"),
                          ('sum by (device, port) (gryvia_nic_counter_rate{counter="port_rcv_packets"})', "rx {{device}}/{{port}}")], "pps")
    b.ts("Link errors and discards", [('sum by (device, port, counter) (gryvia_nic_counter_rate{counter=~"symbol_error|link_downed|port_rcv_errors|port_xmit_discards|rx_discards_phy"})', "{{device}}/{{port}} {{counter}}")], "cps")
    b.ts("Transport retries", [('sum by (device, counter) (gryvia_nic_counter_rate{counter=~"out_of_sequence|packet_seq_err|local_ack_timeout_err|implied_nak_seq_err"})', "{{device}} {{counter}}")], "cps")
    b.ts("Congestion (ECN / CNP counters)", [('sum by (device, counter) (gryvia_nic_counter_rate{counter=~"np_cnp_sent|rp_cnp_handled|np_ecn_marked_roce_packets"})', "{{device}} {{counter}}")], "cps")
    b.row("XDP packet counters")
    b.ts("PFC pause frames per second", [("rate(gryvia_pfc_pause_frames_total[5m])", "PFC"), ("rate(gryvia_pfc_legacy_pause_frames_total[5m])", "802.3x legacy")], "cps")
    b.ts("PFC pausing a priority", [("sum by (priority) (rate(gryvia_pfc_priority_pause_frames_total[5m]))", "prio {{priority}}")], "cps")
    b.ts("CNP share of RoCEv2 packets", [("rate(gryvia_roce_cnp_packets_total[5m]) / rate(gryvia_roce_packets_total[5m])", "CNP/RoCE")], "percentunit")
    b.ts("RoCEv2 packets per second", [("rate(gryvia_roce_packets_total[5m])", "RoCEv2")], "pps")
    b.row("Verbs and completions")
    b.ts("Queue pairs created / destroyed", [("rate(gryvia_ibverbs_qp_created_total[5m])", "created"), ("rate(gryvia_ibverbs_qp_destroyed_total[5m])", "destroyed")], "ops")
    b.ts("Memory regions registered", [("rate(gryvia_ibverbs_mr_registered_total[5m])", "regions/s"), ("rate(gryvia_ibverbs_mr_registered_bytes_total[5m])", "bytes/s")], "short")
    b.ts("RDMA send/receive bytes", [("rate(gryvia_rdma_send_bytes_total[5m])", "send"), ("rate(gryvia_rdma_recv_bytes_total[5m])", "recv")], "Bps")
    b.ts("RDMA completion latency p99", [("histogram_quantile(0.99, sum by (le) (rate(gryvia_rdma_completion_latency_seconds_bucket[5m])))", "p99")], "s")
    b.write("gryvia-nic-rdma.json")


def inference():
    b = Board("gryvia-inference", "Gryvia / Inference serving", ["inference", "vllm", "triton", "tgi"],
              "Latency and queue depth read from the serving engine's own /metrics by the collector (opt-in "
              "-infer-metrics / -infer-metrics-discover). Engines: vllm, triton, tgi. Series are absent for metrics an engine does not export.",
              [("namespace", "label_values(gryvia_inference_latency_seconds, namespace)"),
               ("engine", "label_values(gryvia_inference_latency_seconds, engine)")])
    f = '{namespace=~"$namespace", engine=~"$engine"}'
    b.row("Latency (p99 over a 5 minute window)")
    for m, t in (("ttft_p99", "Time to first token p99"), ("itl_p99", "Inter-token latency p99"), ("queue_time_p99", "Queue time p99"),
                 ("e2e_p99", "End-to-end p99"), ("prefill_p99", "Prefill p99"), ("decode_p99", "Decode p99")):
        b.ts(t, [(f'gryvia_inference_latency_seconds{{namespace=~"$namespace", engine=~"$engine", metric="{m}"}}', "{{namespace}}/{{job}} ({{engine}})")], "s", w=8, h=7)
    b.ts("Mean latencies (engines exporting duration counters only)",
         [(f'gryvia_inference_latency_seconds{{namespace=~"$namespace", engine=~"$engine", metric=~"queue_time_mean|e2e_mean|compute_mean"}}', "{{job}} {{metric}}")], "s")
    b.row("Load")
    b.ts("Requests running / waiting", [(f"sum by (job, state) (gryvia_inference_requests{f})", "{{job}} {{state}}")], "none")
    b.ts("KV cache usage (worst replica)", [(f"gryvia_inference_kv_cache_usage_ratio{f}", "{{job}} ({{engine}})")], "percentunit", thresholds=GRR(0.8, 0.95))
    b.ts("Network wait before first recv p99", [('gryvia_fabric_infer_wait_p99_seconds{namespace=~"$namespace"}', "{{job}}")], "s",
         desc="Kernel-side network wait, not engine latency.")
    b.write("gryvia-inference.json")


def collector_health():
    b = Board("gryvia-collector-health", "Gryvia / Collector health", ["collector", "ebpf"],
              "Whether the eBPF programs attached and whether measurements are being lost. A drop means the "
              "affected numbers under-count.")
    b.stat("Programs attached", "sum(gryvia_ebpf_program_attached)", "none")
    b.stat("Programs NOT attached", "count(gryvia_ebpf_program_attached == 0) or vector(0)", "none", thresholds=GRR(1, 1))
    b.stat("Dropped events (total)", "sum(gryvia_measurement_dropped_events) or vector(0)", "none", thresholds=GRR(1, 1000))
    b.stat("Unattributed network bytes / s", "sum(rate(gryvia_netcost_unattributed_bytes_total[5m]))", "Bps")
    b.ts("Dropped events by source", [("sum by (source) (gryvia_measurement_dropped_events)", "{{source}}")], "none", desc="Monotonic since collector start; look at the slope.")
    b.ts("Drop rate by source", [("sum by (source) (rate(gryvia_measurement_dropped_events[5m]))", "{{source}}")], "ops")
    b.table("Programs not attached", "gryvia_ebpf_program_attached == 0", desc="One row per object/program/kind that failed to attach on some node (label instance = collector pod).")
    b.ts("Collector process memory", [('process_resident_memory_bytes{job=~".*collector.*"}', "{{instance}}")], "bytes", desc="Prometheus job label depends on your ServiceMonitor.")
    b.ts("Collector CPU", [('rate(process_cpu_seconds_total{job=~".*collector.*"}[5m])', "{{instance}}")], "none")
    b.write("gryvia-collector-health.json")


def network_cost():
    b = Board("gryvia-network-cost", "Gryvia / Network cost", ["network", "cost"],
              "Bytes by zone class per namespace (cross-zone and internet traffic is what costs money). Bytes only: "
              "prices are applied by GryviaNetworkRate in the quota operator, not by this metric. SNAT/NAT traffic is not attributed.")
    b.stat("Unattributed bytes / s", "sum(rate(gryvia_netcost_unattributed_bytes_total[5m]))", "Bps", desc="unknown, non-tenant, hostNetwork, loopback or same-node peers.")
    b.stat("Cross-zone bytes / s", 'sum(rate(gryvia_network_cost_bytes_total{zone_type="cross-zone"}[5m]))', "Bps")
    b.stat("Internet bytes / s", 'sum(rate(gryvia_network_cost_bytes_total{zone_type="internet"}[5m]))', "Bps")
    b.ts("Bytes/s by zone class", [("sum by (zone_type) (rate(gryvia_network_cost_bytes_total[5m]))", "{{zone_type}}")], "Bps")
    b.ts("Cross-zone + internet bytes/s by namespace", [('sum by (namespace) (rate(gryvia_network_cost_bytes_total{zone_type=~"cross-zone|internet"}[5m]))', "{{namespace}}")], "Bps")
    b.ts("Bytes in the last 24h by namespace and zone class", [("sum by (namespace, zone_type) (increase(gryvia_network_cost_bytes_total[24h]))", "{{namespace}} {{zone_type}}")], "bytes")
    b.ts("Unattributed bytes/s", [("sum(rate(gryvia_netcost_unattributed_bytes_total[5m]))", "unattributed")], "Bps",
         desc="A large value relative to attributed traffic means the cost numbers under-count (see docs/network-cost-attribution.md).")
    b.write("gryvia-network-cost.json")


def quotas():
    b = Board("gryvia-quotas", "Gryvia / Quotas and budgets", ["quota", "budget"],
              "From the quota operator: GryviaQuota status, refreshed each scrape. Budget series exist only for quotas with a budget.")
    b.stat("Teams with a quota", "count(gryvia_quota_gpus_max)", "none")
    b.stat("GPUs allocated", "sum(gryvia_quota_gpus_allocated)", "none")
    b.stat("GPU limit (sum)", "sum(gryvia_quota_gpus_max)", "none")
    b.stat("Queued jobs", "sum(gryvia_quota_queued_jobs)", "none")
    b.ts("GPU quota used by team", [("gryvia_quota_gpus_allocated / gryvia_quota_gpus_max", "{{team}}")], "percentunit", thresholds=GRR(0.8, 0.95))
    b.ts("GPUs allocated vs limit", [("gryvia_quota_gpus_allocated", "{{team}} allocated"), ("gryvia_quota_gpus_max", "{{team}} max")], "none")
    b.ts("Running jobs", [("gryvia_quota_running_jobs", "{{team}}")], "none")
    b.ts("Queued jobs", [("gryvia_quota_queued_jobs", "{{team}}")], "none")
    b.ts("Budget used (%)", [("gryvia_quota_budget_percent_used", "{{team}}")], "none", thresholds=GRR(80, 90))
    b.ts("Spent this month", [("gryvia_quota_budget_spent", "{{team}}")], "short")
    b.table("Quota table", "gryvia_quota_gpus_allocated", desc="allocated GPUs per team (instant).")
    b.write("gryvia-quotas.json")


def costs():
    b = Board("gryvia-costs", "Gryvia / Usage and cost", ["cost", "usage"],
              "Summed over the GryviaUsageRecords that exist (estimates from job wall-clock time, not invoices). "
              "Deleting old records lowers the totals, so use increase() only over windows shorter than record retention.")
    b.stat("GPU-hours (all records)", "sum(gryvia_usage_gpu_hours_total)", "none")
    b.stat("Cost (all records)", "sum by (currency) (gryvia_usage_cost_total)", "short", legend="{{currency}}")
    b.stat("Open usage records", "gryvia_usage_records_open", "none", desc="Jobs still running.")
    b.stat("Tenants", "gryvia_tenants", "none")
    b.ts("GPU-hours by tenant", [("sum by (tenant) (gryvia_usage_gpu_hours_total)", "{{tenant}}")], "none")
    b.ts("Cost by tenant", [("sum by (tenant, currency) (gryvia_usage_cost_total)", "{{tenant}} {{currency}}")], "short")
    b.ts("GPU-hours by SKU", [("sum by (sku) (gryvia_usage_gpu_hours_total)", "{{sku}}")], "none", desc='sku="default": the default price table was used.')
    b.ts("Cost growth per day by tenant", [("sum by (tenant, currency) (increase(gryvia_usage_cost_total[1d]))", "{{tenant}} {{currency}}")], "short")
    b.ts("Effective price per GPU-hour by SKU", [("sum by (sku, currency) (gryvia_usage_cost_total) / sum by (sku, currency) (gryvia_usage_gpu_hours_total)", "{{sku}} {{currency}}")], "short")
    b.ts("Budget used (%)", [("gryvia_quota_budget_percent_used", "{{team}}")], "none")
    b.write("gryvia-costs.json")


def gpus():
    b = Board("gryvia-gpus", "Gryvia / GPUs (DCGM)", ["gpu", "dcgm"],
              "Every panel here is fed by NVIDIA dcgm-exporter (DCGM_FI_*), not by Gryvia. Enable dcgmExporter in the chart or run your own; "
              "DCGM_FI_PROF_* fields need the profiling module on supported GPUs. Untested on real GPUs.")
    b.note("**Requires dcgm-exporter.** Nothing on this dashboard comes from a Gryvia exporter.")
    b.ts("GPU utilization" + DCGM, [("DCGM_FI_DEV_GPU_UTIL", "{{Hostname}} gpu{{gpu}}")], "percent")
    b.ts("SM active" + DCGM, [("DCGM_FI_PROF_SM_ACTIVE", "{{Hostname}} gpu{{gpu}}")], "percentunit")
    b.ts("Tensor pipe active" + DCGM, [("DCGM_FI_PROF_PIPE_TENSOR_ACTIVE", "{{Hostname}} gpu{{gpu}}")], "percentunit")
    b.ts("Framebuffer used" + DCGM, [("DCGM_FI_DEV_FB_USED * 1024 * 1024", "{{Hostname}} gpu{{gpu}}")], "bytes", desc="dcgm-exporter reports MiB.")
    b.ts("Framebuffer used ratio" + DCGM, [("DCGM_FI_DEV_FB_USED / (DCGM_FI_DEV_FB_USED + DCGM_FI_DEV_FB_FREE)", "{{Hostname}} gpu{{gpu}}")], "percentunit", thresholds=GRR(0.85, 0.95))
    b.ts("Power draw" + DCGM, [("DCGM_FI_DEV_POWER_USAGE", "{{Hostname}} gpu{{gpu}}")], "watt")
    b.ts("Temperature" + DCGM, [("DCGM_FI_DEV_GPU_TEMP", "{{Hostname}} gpu{{gpu}}")], "celsius", thresholds=GRR(80, 90))
    b.ts("XID errors" + DCGM, [("DCGM_FI_DEV_XID_ERRORS", "{{Hostname}} gpu{{gpu}}")], "none")
    b.ts("Memory copy utilization" + DCGM, [("DCGM_FI_DEV_MEM_COPY_UTIL", "{{Hostname}} gpu{{gpu}}")], "percent")
    b.write("gryvia-gpus.json")


def ebpf_network():
    b = Board("gryvia-ebpf-network", "Gryvia / eBPF network flows", ["network", "ebpf"],
              "Service-to-service flows from the collector's kernel probes (needs ebpf.enabled). Labels are src_service, "
              "dst_service, protocol, namespace: cardinality grows with the number of services.")
    b.ts("Flow bytes/s by namespace", [("sum by (namespace) (rate(gryvia_network_flow_bytes_total[5m]))", "{{namespace}}")], "Bps")
    b.ts("Top flows by bytes/s", [("topk(10, sum by (src_service, dst_service) (rate(gryvia_network_flow_bytes_total[5m])))", "{{src_service}} -> {{dst_service}}")], "Bps")
    b.ts("Active connections", [("sum by (namespace) (gryvia_network_connections_active)", "{{namespace}}")], "none")
    b.ts("Drops per second", [("sum by (namespace) (rate(gryvia_network_drops_total[5m]))", "{{namespace}}")], "ops")
    b.ts("Flow latency p99", [("histogram_quantile(0.99, sum by (le, namespace) (rate(gryvia_network_latency_seconds_bucket[5m])))", "{{namespace}}")], "s")
    b.ts("DNS latency p99", [("histogram_quantile(0.99, sum by (le, namespace) (rate(gryvia_network_dns_latency_seconds_bucket[5m])))", "{{namespace}}")], "s")
    b.ts("TCP RTT p50 / p99", [("histogram_quantile(0.5, sum by (le) (rate(gryvia_tcp_rtt_seconds_bucket[5m])))", "p50"),
                               ("histogram_quantile(0.99, sum by (le) (rate(gryvia_tcp_rtt_seconds_bucket[5m])))", "p99")], "s")
    b.ts("TCP congestion window p50", [("histogram_quantile(0.5, sum by (le) (rate(gryvia_tcp_cwnd_histogram_bucket[5m])))", "p50")], "none")
    b.write("gryvia-ebpf-network.json")


def security():
    b = Board("gryvia-security", "Gryvia / Security events", ["security"],
              "Runtime security detections from the collector's kernel probes. Detections are heuristics: expect false positives and misses.")
    b.stat("Alerts (24h)", "sum(increase(gryvia_security_alerts_total[24h]))", "none")
    b.stat("Escape attempts (24h)", "sum(increase(gryvia_security_escape_attempts_total[24h]))", "none", thresholds=GRR(1, 1))
    b.stat("Mining detections (24h)", "sum(increase(gryvia_security_mining_detections_total[24h]))", "none", thresholds=GRR(1, 1))
    b.stat("Weight-exfil signals", "sum(gryvia_fabric_exfil_events)", "none", desc="Large model-file read then connect to a non-internal address. Informational.")
    b.ts("Alerts by type and severity", [("sum by (event_type, severity) (rate(gryvia_security_alerts_total[5m]))", "{{event_type}} ({{severity}})")], "ops")
    b.ts("Escape attempts", [("rate(gryvia_security_escape_attempts_total[5m])", "escape")], "ops")
    b.ts("Mining detections", [("rate(gryvia_security_mining_detections_total[5m])", "mining")], "ops")
    b.ts("Exfil signals by job", [("gryvia_fabric_exfil_events", "{{namespace}}/{{job}}")], "none")
    b.write("gryvia-security.json")


def gateway():
    b = Board("gryvia-gateway", "Gryvia / API gateway", ["gateway"],
              "Needs apiGateway.metrics.enabled (the gateway serves /metrics only when GRYVIA_METRICS_TOKEN is set). "
              "Route labels are templates, never raw paths.")
    b.stat("Request rate", "sum(rate(gryvia_gateway_http_requests_total[5m]))", "reqps")
    b.stat("5xx ratio", 'sum(rate(gryvia_gateway_http_requests_total{status=~"5.."}[5m])) / sum(rate(gryvia_gateway_http_requests_total[5m]))', "percentunit", thresholds=GRR(0.01, 0.05))
    b.stat("Collectors reachable", "min(gryvia_gateway_collector_reachable / gryvia_gateway_collector_targets)", "percentunit", desc="Last fan-out request; empty until a collector page is opened.")
    b.stat("Rate-limited (429) per s", "sum(rate(gryvia_gateway_rate_limit_hits_total[5m]))", "reqps")
    b.ts("Requests by route and status", [("sum by (route, status) (rate(gryvia_gateway_http_requests_total[5m]))", "{{route}} {{status}}")], "reqps")
    b.ts("Latency p95 by route", [("histogram_quantile(0.95, sum by (le, route) (rate(gryvia_gateway_http_request_duration_seconds_bucket[5m])))", "{{route}}")], "s")
    b.ts("Auth attempts by method and result", [("sum by (method, result) (rate(gryvia_gateway_auth_attempts_total[5m]))", "{{method}} {{result}}")], "ops",
         desc="Method is one of session, oidc, api_key, login. No user identity is recorded.")
    b.ts("Auth failure ratio", [('sum(rate(gryvia_gateway_auth_attempts_total{result="failure"}[5m])) / sum(rate(gryvia_gateway_auth_attempts_total[5m]))', "failures")], "percentunit")
    b.ts("Collector fan-out outcomes", [("sum by (outcome) (rate(gryvia_gateway_collector_fanout_total[5m]))", "{{outcome}}")], "ops")
    b.ts("Rate limit hits by route", [("sum by (route) (rate(gryvia_gateway_rate_limit_hits_total[5m]))", "{{route}}")], "reqps")
    b.write("gryvia-gateway.json")


def operators():
    b = Board("gryvia-operators", "Gryvia / Operators", ["operators", "controller-runtime"],
              "Standard controller-runtime metrics served by every Gryvia operator, plus the quota operator's own scrape health.")
    b.ts("Reconciles per second by controller", [("sum by (controller, result) (rate(controller_runtime_reconcile_total[5m]))", "{{controller}} {{result}}")], "ops")
    b.ts("Reconcile errors per second", [("sum by (controller) (rate(controller_runtime_reconcile_errors_total[5m]))", "{{controller}}")], "ops")
    b.ts("Reconcile duration p95", [("histogram_quantile(0.95, sum by (le, controller) (rate(controller_runtime_reconcile_time_seconds_bucket[5m])))", "{{controller}}")], "s")
    b.ts("Work queue depth", [("workqueue_depth", "{{name}}")], "none")
    b.ts("Operator memory", [('process_resident_memory_bytes{job=~".*operator.*"}', "{{pod}}")], "bytes", desc="Prometheus job label depends on the PodMonitor; adjust the regex.")
    b.ts("Goroutines", [('go_goroutines{job=~".*operator.*"}', "{{pod}}")], "none")
    b.ts("Quota metrics list errors", [("gryvia_quota_metrics_list_errors", "kinds failing")], "none", desc="Non-zero: the quota operator could not list a kind at scrape time; its quota/usage series are incomplete.")
    b.write("gryvia-operators.json")


if __name__ == "__main__":
    for fn in (overview, fabric, nic, inference, collector_health, network_cost, quotas, costs, gpus, ebpf_network, security, gateway, operators):
        fn()
