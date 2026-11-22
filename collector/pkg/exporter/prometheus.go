// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package exporter provides Prometheus metrics derived from eBPF flow
// events, GPU/NCCL events, security events, and AI training analysis.
package exporter

import (
	"fmt"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/zyvorai/gryvia/collector/pkg/decoder"
	"github.com/zyvorai/gryvia/collector/pkg/fabric"
	"github.com/zyvorai/gryvia/collector/pkg/nic"
)

// Metrics holds all Prometheus metric handles.
type Metrics struct {
	// Network metrics.
	flowBytes      *prometheus.CounterVec
	latency        *prometheus.HistogramVec
	connectionsAct *prometheus.GaugeVec
	drops          *prometheus.CounterVec
	dnsLatency     *prometheus.HistogramVec

	// GPU / NCCL metrics.
	ncclOpDuration        *prometheus.HistogramVec
	ncclBytesTotal        *prometheus.CounterVec
	ncclStragglerTotal    prometheus.Counter
	gpuMemcpyBytes        *prometheus.CounterVec
	gpuMemcpyDuration     *prometheus.HistogramVec
	rdmaSendBytesTotal    prometheus.Counter
	rdmaRecvBytesTotal    prometheus.Counter
	rdmaCompletionLatency prometheus.Histogram

	// Security metrics.
	securityAlertsTotal      *prometheus.CounterVec
	securityEscapeAttempts   prometheus.Counter
	securityMiningDetections prometheus.Counter

	// AI training metrics.
	trainingCommComputeRatio *prometheus.GaugeVec
	trainingStragglerEvents  prometheus.Counter
	pipelineBottleneck       *prometheus.GaugeVec

	// Fabric signal metrics (per job, from pkg/fabric).
	fabricScoreDelta    *prometheus.GaugeVec
	fabricStragglerRank *prometheus.GaugeVec
	fabricNCCLP99       *prometheus.GaugeVec
	fabricRDMARetryRate *prometheus.GaugeVec
	fabricGDSHitRatio   *prometheus.GaugeVec
	fabricOverlapIdle   *prometheus.GaugeVec
	fabricCNPRate       *prometheus.GaugeVec
	fabricInferWaitP99  *prometheus.GaugeVec
	fabricPFCRate       *prometheus.GaugeVec
	fabricExfilEvents   *prometheus.GaugeVec
	fabricSignalEvents  *prometheus.GaugeVec
	fabricInferTTFTP99  *prometheus.GaugeVec
	fabricUCXSlowP99    *prometheus.GaugeVec
	engineLatency       *prometheus.GaugeVec // gryvia_inference_latency_seconds{namespace,job,engine,metric}
	engineRequests      *prometheus.GaugeVec // gryvia_inference_requests{namespace,job,engine,state}
	engineKVCache       *prometheus.GaugeVec
	fabricCollCompared  *prometheus.GaugeVec
	fabricCollSkew      *prometheus.GaugeVec
	fabricNICRetryRate  *prometheus.GaugeVec
	fabricNICErrorRate  *prometheus.GaugeVec
	fabricGPUIdleComm   *prometheus.GaugeVec
	fabricSMActiveComp  *prometheus.GaugeVec
	fabricGPUCorrCov    *prometheus.GaugeVec
	nicCounterRate      *prometheus.GaugeVec
	ibvQPCreated        prometheus.Counter
	ibvQPDestroyed      prometheus.Counter
	ibvMRRegistered     prometheus.Counter
	ibvMRBytes          prometheus.Counter
	pfcFrames           prometheus.Counter
	pfcLegacyFrames     prometheus.Counter
	pfcPriorityFrames   *prometheus.CounterVec
	roceCNPPackets      prometheus.Counter
	roceRoCEPackets     prometheus.Counter

	// Performance / TCP metrics.
	tcpCwndHistogram prometheus.Histogram
	tcpRTTHistogram  prometheus.Histogram
	networkCostBytes *prometheus.CounterVec
}

// NewMetrics registers and returns all collector metrics.
func NewMetrics() *Metrics {
	labels := []string{"src_service", "dst_service", "protocol", "namespace"}
	fabricLabels := []string{"namespace", "job"}

	return &Metrics{
		// ---- Network metrics ----
		flowBytes: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "gryvia_network_flow_bytes_total",
			Help: "Total bytes transferred between services.",
		}, labels),

		latency: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gryvia_network_latency_seconds",
			Help:    "Network latency between services in seconds.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0, 5.0},
		}, labels),

		connectionsAct: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_network_connections_active",
			Help: "Number of currently active connections between services.",
		}, labels),

		drops: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "gryvia_network_drops_total",
			Help: "Total dropped packets/connections between services.",
		}, labels),

		dnsLatency: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gryvia_network_dns_latency_seconds",
			Help:    "DNS resolution latency in seconds.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0},
		}, []string{"namespace"}),

		// ---- GPU / NCCL metrics ----
		ncclOpDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gryvia_nccl_operation_duration_seconds",
			Help:    "Duration of NCCL collective operations in seconds.",
			Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0},
		}, []string{"op_type", "rank"}),

		ncclBytesTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "gryvia_nccl_bytes_total",
			Help: "Total bytes transferred by NCCL operations.",
		}, []string{"op_type"}),

		ncclStragglerTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_nccl_stragglers_detected_total",
			Help: "Total number of NCCL straggler events detected.",
		}),

		gpuMemcpyBytes: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "gryvia_gpu_memcpy_bytes_total",
			Help: "Total bytes transferred by GPU memcpy operations.",
		}, []string{"direction"}),

		gpuMemcpyDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gryvia_gpu_memcpy_duration_seconds",
			Help:    "Duration of GPU memcpy operations in seconds.",
			Buckets: []float64{0.00001, 0.0001, 0.001, 0.01, 0.1, 1.0},
		}, []string{"direction"}),

		rdmaSendBytesTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_rdma_send_bytes_total",
			Help: "Total bytes sent via RDMA.",
		}),

		rdmaRecvBytesTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_rdma_recv_bytes_total",
			Help: "Total bytes received via RDMA.",
		}),

		rdmaCompletionLatency: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "gryvia_rdma_completion_latency_seconds",
			Help:    "RDMA completion latency in seconds.",
			Buckets: []float64{0.000001, 0.00001, 0.0001, 0.001, 0.01},
		}),

		// ---- Security metrics ----
		securityAlertsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "gryvia_security_alerts_total",
			Help: "Total security alerts by event type and severity.",
		}, []string{"event_type", "severity"}),

		securityEscapeAttempts: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_security_escape_attempts_total",
			Help: "Total container escape attempts detected.",
		}),

		securityMiningDetections: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_security_mining_detections_total",
			Help: "Total crypto mining activity detections.",
		}),

		// ---- AI training metrics ----
		trainingCommComputeRatio: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_training_comm_compute_ratio",
			Help: "Ratio of communication time to compute time for training jobs.",
		}, []string{"job"}),

		trainingStragglerEvents: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_training_straggler_events_total",
			Help: "Total straggler events detected during training.",
		}),

		pipelineBottleneck: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_pipeline_bottleneck",
			Help: "Current pipeline bottleneck indicator (1.0 = bottleneck).",
		}, []string{"phase"}),

		// ---- Fabric signal metrics ----
		fabricScoreDelta: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_score_delta",
			Help: "Fabric health penalty in [0,1] for the topology scorer (0 = healthy).",
		}, fabricLabels),
		fabricStragglerRank: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_straggler_rank",
			Help: "Rank of the most recent NCCL straggler (0 when the rank is unknown).",
		}, fabricLabels),
		fabricNCCLP99: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_nccl_p99_seconds",
			Help: "p99 latency of straggler-flagged NCCL collectives in the window.",
		}, fabricLabels),
		fabricRDMARetryRate: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_rdma_retry_rate",
			Help: "RDMA retry/RNR error completions per posted send in the window.",
		}, fabricLabels),
		fabricGDSHitRatio: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_gds_hit_ratio",
			Help: "Fraction of cuFile bytes confirmed direct (GPUDirect Storage); absent when not measurable.",
		}, fabricLabels),
		fabricOverlapIdle: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_overlap_idle_ratio",
			Help: "Fraction of the window spent in cudaDeviceSynchronize inside an in-flight ncclAllReduce (GPU idle while communicating).",
		}, fabricLabels),
		fabricCNPRate: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_cnp_rate",
			Help: "RoCEv2 congestion notification packets per second over the window (job _node/cnp).",
		}, fabricLabels),
		fabricInferWaitP99: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_infer_wait_p99_seconds",
			Help: "p99 NETWORK wait from accept to first recv on inference ports (vLLM/Triton). Not engine queue time, TTFT or ITL: see gryvia_inference_latency_seconds.",
		}, fabricLabels),
		fabricPFCRate: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_pfc_rate",
			Help: "802.1Qbb PFC pause frames per second over the window (job _node/pfc).",
		}, fabricLabels),
		fabricExfilEvents: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_exfil_events",
			Help: "weight_exfil signals in the window: large model-file read then connect to a non-internal address. Informational, never changes the score.",
		}, fabricLabels),
		fabricSignalEvents: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_signal_events",
			Help: "Opt-in probe signals in the window by signal (nccl_transport, p2p_fallback, gpu_oom, graph_stall, gdr_fail, infer_gap, weight_mmap, gpu_dev, ucx_wait). Informational, never changes the score; gpu_oom is routine under PyTorch's caching allocator.",
		}, []string{"namespace", "job", "signal"}),
		fabricInferTTFTP99: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_infer_ttft_p99_seconds",
			Help: "p99 of accept to first send on inference ports (infer_ttft, opt-in).",
		}, fabricLabels),
		fabricUCXSlowP99: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_ucx_slow_p99_seconds",
			Help: "p99 duration of UCX tag-send calls that blocked for at least 5 ms.",
		}, fabricLabels),
		engineLatency: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_inference_latency_seconds",
			Help: "Latency read from the serving engine's own metrics (opt-in -infer-metrics). metric is ttft_p99, itl_p99, queue_time_p99, e2e_p99, prefill_p99, decode_p99 (quantiles over a 5 minute window of per-interval histogram deltas) or queue_time_mean, e2e_mean, compute_mean (engines that export only duration counters). Absent when not measured.",
		}, []string{"namespace", "job", "engine", "metric"}),
		engineRequests: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_inference_requests",
			Help: "Requests the serving engine reports as running or waiting (state label), summed over the job's scraped replicas on this node.",
		}, []string{"namespace", "job", "engine", "state"}),
		engineKVCache: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_inference_kv_cache_usage_ratio",
			Help: "KV cache usage in [0,1] reported by the engine (worst replica); absent when the engine does not export it.",
		}, []string{"namespace", "job", "engine"}),
		fabricCollCompared: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_collectives_compared",
			Help: "NCCL collectives (same communicator ordinal, sequence, op) seen on >= 2 local ranks in the window.",
		}, fabricLabels),
		fabricCollSkew: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_collective_max_skew_seconds",
			Help: "Largest slowest-minus-fastest host-side NCCL call duration among matched local collectives (not GPU time).",
		}, fabricLabels),
		fabricNICRetryRate: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_nic_retry_rate",
			Help: "RDMA NIC transport retry / sequence error events per second (hardware counters; job _node/nic). Informational.",
		}, fabricLabels),
		fabricNICErrorRate: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_nic_error_rate",
			Help: "RDMA NIC link/symbol/discard error events per second (hardware counters; job _node/nic). Informational.",
		}, fabricLabels),
		fabricGPUIdleComm: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_gpu_idle_during_comm_ratio",
			Help: "Fraction of covered NCCL communication time with DCGM SM_ACTIVE (else GPU_UTIL) below the idle threshold. Absent unless -dcgm-correlate measured it. Informational.",
		}, fabricLabels),
		fabricSMActiveComp: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_sm_active_during_compute",
			Help: "Mean DCGM SM activity outside NCCL call windows (between the job's first and last collective). Absent unless measured. Informational.",
		}, fabricLabels),
		fabricGPUCorrCov: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_fabric_gpu_correlation_coverage",
			Help: "Fraction of NCCL communication time covered by a DCGM sample (low = exporter collect interval too coarse).",
		}, fabricLabels),
		nicCounterRate: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gryvia_nic_counter_rate",
			Help: "RDMA NIC hardware counter rate per second from /sys/class/infiniband (port_*_bytes derived from 4-byte data units).",
		}, []string{"device", "port", "counter"}),
		ibvQPCreated: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_ibverbs_qp_created_total",
			Help: "Queue pairs created via libibverbs ibv_create_qp on this node (ibv_verbs uprobes, -ibverbs-probes).",
		}),
		ibvQPDestroyed: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_ibverbs_qp_destroyed_total",
			Help: "Queue pairs destroyed via libibverbs ibv_destroy_qp on this node.",
		}),
		ibvMRRegistered: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_ibverbs_mr_registered_total",
			Help: "Memory regions registered via ibv_reg_mr / ibv_reg_mr_iova2 on this node.",
		}),
		ibvMRBytes: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_ibverbs_mr_registered_bytes_total",
			Help: "Bytes registered via ibv_reg_mr / ibv_reg_mr_iova2 on this node.",
		}),
		pfcFrames: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_pfc_pause_frames_total",
			Help: "802.1Qbb priority flow control pause frames seen by the pfc_pause XDP program.",
		}),
		pfcLegacyFrames: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_pfc_legacy_pause_frames_total",
			Help: "802.3x link-level pause frames seen by the pfc_pause XDP program.",
		}),
		pfcPriorityFrames: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "gryvia_pfc_priority_pause_frames_total",
			Help: "PFC pause frames that pause the given priority (enabled, non-zero quanta).",
		}, []string{"priority"}),
		roceCNPPackets: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_roce_cnp_packets_total",
			Help: "RoCEv2 congestion notification packets seen by the roce_cnp XDP program.",
		}),
		roceRoCEPackets: promauto.NewCounter(prometheus.CounterOpts{
			Name: "gryvia_roce_packets_total",
			Help: "RoCEv2 (UDP 4791) packets seen by the roce_cnp XDP program.",
		}),

		// ---- Performance / TCP metrics ----
		tcpCwndHistogram: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "gryvia_tcp_cwnd_histogram",
			Help:    "Distribution of TCP congestion window sizes.",
			Buckets: []float64{1, 5, 10, 20, 50, 100, 200, 500, 1000},
		}),

		tcpRTTHistogram: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "gryvia_tcp_rtt_seconds",
			Help:    "Distribution of TCP round-trip times in seconds.",
			Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5},
		}),

		networkCostBytes: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "gryvia_network_cost_bytes_total",
			Help: "Total network bytes by zone type and namespace for cost analysis.",
		}, []string{"zone_type", "namespace"}),
	}
}

// RecordFlow updates Prometheus metrics from a decoded flow event.
func (m *Metrics) RecordFlow(ev decoder.FlowEvent) {
	src := ev.SrcService
	if src == "" {
		src = decoder.IPToString(ev.SrcIP)
	}
	dst := ev.DstService
	if dst == "" {
		dst = decoder.IPToString(ev.DstIP)
	}
	proto := protoLabel(ev.Protocol)
	ns := ev.Namespace

	labelValues := []string{src, dst, proto, ns}

	// Bytes transferred.
	if ev.Bytes > 0 {
		m.flowBytes.WithLabelValues(labelValues...).Add(float64(ev.Bytes))
	}

	// Latency.
	if ev.LatencyNs > 0 {
		m.latency.WithLabelValues(labelValues...).Observe(
			float64(ev.LatencyNs) / 1e9)
	}

	// Drops (verdict == 1).
	if ev.Verdict == 1 {
		m.drops.WithLabelValues(labelValues...).Inc()
	}

	// Active connections (increment on new, would decrement on close
	// with a more stateful approach -- for now, bump on every event).
	if ev.Verdict == 0 && ev.Protocol == 6 {
		m.connectionsAct.WithLabelValues(labelValues...).Inc()
	}
}

// RecordDNSLatency records a DNS resolution latency observation.
func (m *Metrics) RecordDNSLatency(ns string, latencyNs uint64) {
	if latencyNs > 0 {
		m.dnsLatency.WithLabelValues(ns).Observe(float64(latencyNs) / 1e9)
	}
}

// RecordGPUEvent updates GPU/NCCL Prometheus metrics from a decoded GPU event.
func (m *Metrics) RecordGPUEvent(ev decoder.GPUEvent) {
	switch ev.EventType {
	case decoder.GPUEvtNCCLOp:
		opType := decoder.NCCLOpName(ev.NCCLOp)
		rank := fmt.Sprintf("%d", ev.SrcRank)
		m.ncclOpDuration.WithLabelValues(opType, rank).Observe(float64(ev.LatencyNs) / 1e9)
		m.ncclBytesTotal.WithLabelValues(opType).Add(float64(ev.Bytes))

	case decoder.GPUEvtMemTransfer:
		dir := decoder.MemDirectionName(ev.Direction)
		m.gpuMemcpyBytes.WithLabelValues(dir).Add(float64(ev.Bytes))
		m.gpuMemcpyDuration.WithLabelValues(dir).Observe(float64(ev.LatencyNs) / 1e9)

	case decoder.GPUEvtRDMASend:
		m.rdmaSendBytesTotal.Add(float64(ev.Bytes))
		m.rdmaCompletionLatency.Observe(float64(ev.LatencyNs) / 1e9)

	case decoder.GPUEvtRDMARecv:
		m.rdmaRecvBytesTotal.Add(float64(ev.Bytes))
		m.rdmaCompletionLatency.Observe(float64(ev.LatencyNs) / 1e9)
	}
}

// RecordSecurityEvent updates security Prometheus metrics.
func (m *Metrics) RecordSecurityEvent(ev decoder.SecurityEvent) {
	eventType := decoder.SecurityEventTypeName(ev.EventType)
	severity := decoder.SeverityName(ev.Severity)
	m.securityAlertsTotal.WithLabelValues(eventType, severity).Inc()

	switch ev.EventType {
	case decoder.SecEvtContainerEscape:
		m.securityEscapeAttempts.Inc()
	case decoder.SecEvtCryptoMining:
		m.securityMiningDetections.Inc()
	}
}

// RecordStragglerDetected increments the straggler counter.
func (m *Metrics) RecordStragglerDetected() {
	m.ncclStragglerTotal.Inc()
	m.trainingStragglerEvents.Inc()
}

// RecordCommComputeRatio sets the communication/compute ratio gauge.
func (m *Metrics) RecordCommComputeRatio(job string, ratio float64) {
	m.trainingCommComputeRatio.WithLabelValues(job).Set(ratio)
}

// RecordPipelineBottleneck sets the bottleneck gauge for a given phase.
func (m *Metrics) RecordPipelineBottleneck(bottleneck string) {
	// Reset all phases.
	for _, phase := range []string{"data_loading", "preprocessing", "compute", "communication"} {
		if phase == bottleneck {
			m.pipelineBottleneck.WithLabelValues(phase).Set(1.0)
		} else {
			m.pipelineBottleneck.WithLabelValues(phase).Set(0.0)
		}
	}
}

// RecordFabric publishes the per-job fabric status. The vectors are reset
// first so jobs whose signals aged out of the window disappear.
func (m *Metrics) RecordFabric(jobs []fabric.JobStatus) {
	m.fabricScoreDelta.Reset()
	m.fabricStragglerRank.Reset()
	m.fabricNCCLP99.Reset()
	m.fabricRDMARetryRate.Reset()
	m.fabricGDSHitRatio.Reset()
	m.fabricOverlapIdle.Reset()
	m.fabricCNPRate.Reset()
	m.fabricInferWaitP99.Reset()
	m.fabricPFCRate.Reset()
	m.fabricExfilEvents.Reset()
	m.fabricSignalEvents.Reset()
	m.fabricInferTTFTP99.Reset()
	m.fabricUCXSlowP99.Reset()
	m.engineLatency.Reset()
	m.engineRequests.Reset()
	m.engineKVCache.Reset()
	m.fabricCollCompared.Reset()
	m.fabricCollSkew.Reset()
	m.fabricNICRetryRate.Reset()
	m.fabricNICErrorRate.Reset()
	m.fabricGPUIdleComm.Reset()
	m.fabricSMActiveComp.Reset()
	m.fabricGPUCorrCov.Reset()
	for _, j := range jobs {
		m.recordInference(j)
		m.fabricScoreDelta.WithLabelValues(j.Namespace, j.Job).Set(j.ScoreDelta)
		m.fabricStragglerRank.WithLabelValues(j.Namespace, j.Job).Set(float64(j.StragglerRank))
		m.fabricNCCLP99.WithLabelValues(j.Namespace, j.Job).Set(j.NCCLP99MS / 1e3)
		m.fabricRDMARetryRate.WithLabelValues(j.Namespace, j.Job).Set(j.RDMARetryRate)
		m.fabricOverlapIdle.WithLabelValues(j.Namespace, j.Job).Set(j.OverlapIdleRatio)
		m.fabricCNPRate.WithLabelValues(j.Namespace, j.Job).Set(j.CNPRate)
		m.fabricInferWaitP99.WithLabelValues(j.Namespace, j.Job).Set(j.InferWaitP99MS / 1e3)
		m.fabricPFCRate.WithLabelValues(j.Namespace, j.Job).Set(j.PFCRate)
		m.fabricExfilEvents.WithLabelValues(j.Namespace, j.Job).Set(float64(j.ExfilEvents))
		m.fabricUCXSlowP99.WithLabelValues(j.Namespace, j.Job).Set(j.UCXSlowP99MS / 1e3)
		for name, n := range j.Events {
			m.fabricSignalEvents.WithLabelValues(j.Namespace, j.Job, name).Set(float64(n))
		}
		if j.InferTTFTP99MS > 0 {
			m.fabricInferTTFTP99.WithLabelValues(j.Namespace, j.Job).Set(j.InferTTFTP99MS / 1e3)
		}
		if j.GDSMeasured {
			m.fabricGDSHitRatio.WithLabelValues(j.Namespace, j.Job).Set(j.GDSHitRatio)
		}
		if j.CollectivesCompared > 0 {
			m.fabricCollCompared.WithLabelValues(j.Namespace, j.Job).Set(float64(j.CollectivesCompared))
			m.fabricCollSkew.WithLabelValues(j.Namespace, j.Job).Set(j.CollectiveMaxSkewMS / 1e3)
		}
		if j.Namespace == fabric.NICJob.Namespace && j.Job == fabric.NICJob.Job {
			m.fabricNICRetryRate.WithLabelValues(j.Namespace, j.Job).Set(j.NICRetryRate)
			m.fabricNICErrorRate.WithLabelValues(j.Namespace, j.Job).Set(j.NICErrorRate)
		}
		if j.GPUCorrelationCoverage > 0 || j.GPUCorrelationMeasured {
			m.fabricGPUCorrCov.WithLabelValues(j.Namespace, j.Job).Set(j.GPUCorrelationCoverage)
		}
		if j.GPUCorrelationMeasured {
			m.fabricGPUIdleComm.WithLabelValues(j.Namespace, j.Job).Set(j.GPUIdleDuringCommRatio)
			if j.GPUComputeMeasured {
				m.fabricSMActiveComp.WithLabelValues(j.Namespace, j.Job).Set(j.SMActiveDuringCompute)
			}
		}
	}
}

// recordInference publishes the engine figures of one job. Unmeasured figures are simply absent.
func (m *Metrics) recordInference(j fabric.JobStatus) {
	in := j.Inference
	if in == nil {
		return
	}
	lat := func(metric string, ms *float64) {
		if ms != nil {
			m.engineLatency.WithLabelValues(j.Namespace, j.Job, in.Engine, metric).Set(*ms / 1e3)
		}
	}
	lat("ttft_p99", in.TTFTP99MS)
	lat("itl_p99", in.ITLP99MS)
	lat("queue_time_p99", in.QueueP99MS)
	lat("e2e_p99", in.E2EP99MS)
	lat("prefill_p99", in.PrefillP99MS)
	lat("decode_p99", in.DecodeP99MS)
	lat("queue_time_mean", in.QueueMeanMS)
	lat("e2e_mean", in.E2EMeanMS)
	lat("compute_mean", in.ComputeMeanMS)
	if in.RequestsRunning != nil {
		m.engineRequests.WithLabelValues(j.Namespace, j.Job, in.Engine, "running").Set(*in.RequestsRunning)
	}
	if in.RequestsWaiting != nil {
		m.engineRequests.WithLabelValues(j.Namespace, j.Job, in.Engine, "waiting").Set(*in.RequestsWaiting)
	}
	if in.KVCacheUsage != nil {
		m.engineKVCache.WithLabelValues(j.Namespace, j.Job, in.Engine).Set(*in.KVCacheUsage)
	}
}

// RecordNIC publishes the per-port NIC counter rates (whitelisted counters
// only: label cardinality stays bounded whatever the driver exports). The
// gauge is reset first so ports that disappear vanish.
func (m *Metrics) RecordNIC(r nic.Rates) {
	m.nicCounterRate.Reset()
	for _, k := range r.SortedPorts() {
		rates := r.Ports[k]
		for _, name := range nic.GaugeNames {
			v, ok := rates[name]
			if !ok {
				continue
			}
			switch name {
			case "port_xmit_data":
				name, v = "port_xmit_bytes", v*nic.BytesPerDataUnit
			case "port_rcv_data":
				name, v = "port_rcv_bytes", v*nic.BytesPerDataUnit
			}
			m.nicCounterRate.WithLabelValues(k.Device, k.Port, name).Set(v)
		}
	}
}

// RecordIBVerbs adds the libibverbs control-path counts seen since the previous poll.
func (m *Metrics) RecordIBVerbs(qpCreated, qpDestroyed, mrRegistered, mrBytes uint64) {
	m.ibvQPCreated.Add(float64(qpCreated))
	m.ibvQPDestroyed.Add(float64(qpDestroyed))
	m.ibvMRRegistered.Add(float64(mrRegistered))
	m.ibvMRBytes.Add(float64(mrBytes))
}

// RecordRoCE adds the RoCEv2 packet counts seen since the previous poll.
func (m *Metrics) RecordRoCE(cnp, roce uint64) {
	m.roceCNPPackets.Add(float64(cnp))
	m.roceRoCEPackets.Add(float64(roce))
}

// RecordPFC adds the pause frame counts seen since the previous poll: PFC
// frames, 802.3x legacy pause frames and the per-priority pause counts.
func (m *Metrics) RecordPFC(frames, legacy uint64, perPriority [fabric.PFCPriorities]uint64) {
	m.pfcFrames.Add(float64(frames))
	m.pfcLegacyFrames.Add(float64(legacy))
	for p, n := range perPriority {
		if n > 0 {
			m.pfcPriorityFrames.WithLabelValues(strconv.Itoa(p)).Add(float64(n))
		}
	}
}

// RecordTCPStats records TCP connection statistics for Prometheus.
func (m *Metrics) RecordTCPStats(cwnd float64, rttUs float64) {
	m.tcpCwndHistogram.Observe(cwnd)
	m.tcpRTTHistogram.Observe(rttUs / 1e6) // convert us to seconds
}

// RecordNetworkCost records network cost bytes by zone type and namespace.
func (m *Metrics) RecordNetworkCost(zoneType, namespace string, bytes float64) {
	m.networkCostBytes.WithLabelValues(zoneType, namespace).Add(bytes)
}

func protoLabel(proto uint8) string {
	switch proto {
	case 6:
		return "tcp"
	case 17:
		return "udp"
	default:
		return "other"
	}
}
