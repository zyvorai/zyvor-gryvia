// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package main is the entry point for the Gryvia flow collector.
//
// It loads compiled eBPF programs, attaches them to the appropriate
// kernel hooks, starts perf-event readers, exposes Prometheus metrics
// on an HTTP endpoint, and optionally streams enriched events to NATS.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/zyvorai/gryvia/collector/pkg/aggregator"
	"github.com/zyvorai/gryvia/collector/pkg/ai"
	"github.com/zyvorai/gryvia/collector/pkg/anomaly"
	"github.com/zyvorai/gryvia/collector/pkg/dcgm"
	"github.com/zyvorai/gryvia/collector/pkg/decoder"
	"github.com/zyvorai/gryvia/collector/pkg/diagnosis"
	"github.com/zyvorai/gryvia/collector/pkg/exporter"
	"github.com/zyvorai/gryvia/collector/pkg/fabric"
	"github.com/zyvorai/gryvia/collector/pkg/flight"
	"github.com/zyvorai/gryvia/collector/pkg/graph"
	"github.com/zyvorai/gryvia/collector/pkg/incident"
	"github.com/zyvorai/gryvia/collector/pkg/inference"
	"github.com/zyvorai/gryvia/collector/pkg/kube"
	"github.com/zyvorai/gryvia/collector/pkg/loader"
	"github.com/zyvorai/gryvia/collector/pkg/netcost"
	"github.com/zyvorai/gryvia/collector/pkg/nic"
	"github.com/zyvorai/gryvia/collector/pkg/security"
	"github.com/zyvorai/gryvia/collector/pkg/trace"
	"github.com/zyvorai/gryvia/collector/pkg/tuning"
)

func main() {
	var (
		metricsAddr     = flag.String("metrics-addr", ":9090", "Prometheus metrics listen address")
		natsURL         = flag.String("nats-url", "", "NATS server URL (optional)")
		ebpfDir         = flag.String("ebpf-dir", "/opt/gryvia/ebpf", "Directory containing compiled eBPF .o files")
		iface           = flag.String("iface", "", "Network interface for XDP/TCX attachment (empty = skip those programs)")
		enablePrograms  = flag.String("enable-programs", "", "comma-separated opt-in eBPF programs to attach, or \"all\": programs whose signals or counters nothing in the collector reads yet are skipped by default ("+strings.Join(loader.OptInPrograms(), ", ")+")")
		xdpMux          = flag.Bool("xdp-mux", false, "attach xdp_mux as the interface's only XDP program and chain roce_cnp, pfc_pause, dns_tracker, packet_filter and roce_ecn behind it, so all of them run on one interface (without it only one XDP program can attach per interface). Needs -iface. Off by default")
		cgroupPath      = flag.String("cgroup-path", "", "cgroup v2 path for sockops/sk_msg attachment (empty = skip)")
		ncclLib         = flag.String("nccl-lib", "", "path to libnccl.so for uprobes (empty = auto-discover)")
		cudaLib         = flag.String("cuda-lib", "", "path to libcudart.so for uprobes (empty = auto-discover)")
		cufileLib       = flag.String("cufile-lib", "", "path to libcufile.so for GPUDirect Storage uprobes (empty = auto-discover)")
		ucxLib          = flag.String("ucx-lib", "", "path to libucp.so (UCX) for ucx_gloo uprobes (empty = auto-discover)")
		uprobePID       = flag.Int("uprobe-pid", 0, "find NCCL/CUDA/cuFile/UCX libraries via /proc/<pid>/maps of this process")
		inferPorts      = flag.String("infer-ports", "8000,8001", "local TCP ports of inference servers for infer_latency (vLLM 8000, Triton HTTP 8001; at most 8, empty disables it)")
		flightTokenFile = flag.String("flight-token-file", "", "file containing the Flight Recorder API token; empty disables the endpoint")
		quotaPace       = flag.Bool("quota-pace", false, "attach quota_pace (the only program that changes sockets: caps SO_MAX_PACING_RATE of cgroups that hold a lease). Off by default; needs -cgroup-path. Without -quota-pace-sync nothing grants leases and pacing stays inert")
		quotaPaceSync   = flag.Bool("quota-pace-sync", false, "MUTATING, off by default: every 30 s grant a pace lease to the cgroups of pods on this node whose namespace is listed by a GryviaQuota with spec.network.maxEgressMbps (needs -quota-pace, -cgroup-path, running in a cluster and NODE_NAME). Fails open: an API error changes nothing and leases expire after 2 minutes")
		quotaPaceDry    = flag.Bool("quota-pace-dry-run", false, "with -quota-pace-sync: log what would be granted or revoked and write nothing to the pace map")
		fabricPerNode   = flag.Bool("fabric-status-per-node", false, "with -publish-fabric-status: instead of merge-patching the job's top-level status (last writer wins across nodes), server-side-apply this node's own status.nodes[] entry under field manager gryvia-collector-<NODE_NAME>; needs NODE_NAME and the ai-operator's --merge-fabric-signals to fold the entries into the top-level status (chart value ebpf.fabricStatusPerNode)")
		publishNodeFab  = flag.Bool("publish-node-fabric", false, "every 30 s write this node's fabric health (worst scoreDelta + top reasons, ttl 5 min) to the cluster-scoped GryviaNodeFabric named after NODE_NAME, for the ai-operator's opt-in fabric-aware scheduling; creates only that one object; needs a cluster, NODE_NAME and RBAC (chart value ebpf.publishNodeFabric)")
		ibverbsLib      = flag.String("ibverbs-lib", "", "path to libibverbs.so for the ibv_verbs uprobes (empty = auto-discover)")
		ibverbsProbes   = flag.Bool("ibverbs-probes", false, "attach ibv_verbs (uprobes on libibverbs ibv_create_qp/ibv_destroy_qp/ibv_reg_mr: QP and registered-memory counters). Off by default")
		nicCounters     = flag.Bool("nic-counters", false, "read RDMA NIC hardware counters from /sys/class/infiniband and fold retry/error/CNP/pause rates into the fabric status (NCCL's userspace verbs path is invisible to the kernel RDMA kprobes). Off by default; skipped when no RDMA device exists")
		nicSysfs        = flag.String("nic-sysfs", nic.DefaultRoot, "with -nic-counters: sysfs directory of RDMA devices")
		dcgmCorrelate   = flag.Bool("dcgm-correlate", false, "scrape this node's dcgm-exporter and correlate GPU activity with NCCL call windows (gpuIdleDuringCommRatio etc.; informational). Off by default; run dcgm-exporter with a short --collect-interval")
		dcgmURL         = flag.String("dcgm-url", dcgm.DefaultURL, "with -dcgm-correlate: dcgm-exporter metrics URL")
		dcgmInterval    = flag.Duration("dcgm-interval", time.Second, "with -dcgm-correlate: scrape interval")
		dcgmIdle        = flag.Float64("dcgm-idle-threshold", dcgm.DefaultIdleThreshold, "with -dcgm-correlate: SM activity (0-1) below which the GPU counts as idle")
		publishFabric   = flag.Bool("publish-fabric-status", false, "every 30 s patch the status of an existing GryviaFabricSignal (spec.jobRef = job) with the folded fabric signals; needs a cluster (KUBERNETES_SERVICE_HOST) and RBAC (chart value ebpf.publishFabricStatus)")
		tlsCertFile     = flag.String("tls-cert-file", "", "serve the HTTP listener over TLS (>= 1.2) with this certificate; re-read when the file changes (rotation). Needs -tls-key-file")
		tlsKeyFile      = flag.String("tls-key-file", "", "private key for -tls-cert-file")
		tlsClientCA     = flag.String("tls-client-ca-file", "", "enable mTLS: require and verify client certificates against this CA bundle (needs TLS); a verified client certificate authenticates every endpoint")
		apiTokenFile    = flag.String("api-token-file", "", "file with a shared token (>= 32 chars): every endpoint except /healthz, /readyz and the flight endpoint then needs an HMAC request signature (X-Gryvia-Time / X-Gryvia-Signature)")
		metricsToken    = flag.String("metrics-token-file", "", "file with a bearer token (>= 32 chars) that Prometheus may present on /metrics only")
		insecureListen  = flag.Bool("insecure-listener", false, "explicitly serve every endpoint without authentication (acknowledges the risk; contradicts the auth flags)")
		requireAuth     = flag.Bool("require-auth", false, "refuse to start unless -api-token-file, -metrics-token-file or -tls-client-ca-file is set")
		attributeNet    = flag.Bool("attribute-network", false, "attribute the bytes counted by cost_tracker (needs -iface, a cluster and NODE_NAME) to tenants and peer/zone classes; reads pods, nodes and namespaces. Off by default; see docs/network-cost-attribution.md")
		publishNetUsage = flag.Bool("publish-network-usage", false, "every 60 s write per-tenant egress as GryviaNetworkUsageRecord objects into tenant-<name> namespaces (needs -attribute-network and RBAC: chart value ebpf.publishNetworkUsage)")
		windowSec       = flag.Int("window", 300, "Aggregation sliding window in seconds")
		diagCgroup      = flag.String("flight-diagnosis-cgroup", "", "cgroup v2 root (for example /host/sys/fs/cgroup) from which /api/v1/flight/diagnosis reads cpu.stat, memory.events and *.pressure of the job's pods, read-only and userspace only; empty reports those signals as unavailable")
		diagThresholds  = flag.String("flight-diagnosis-thresholds", "", "JSON file overriding the diagnosis rule thresholds (docs/flight-diagnosis.md); empty uses the defaults")
		storeDir        = flag.String("flight-store-dir", "", "directory for the persistent incident history (append-only JSON-lines segments); empty disables it")
		storeRetention  = flag.Duration("flight-retention", 24*time.Hour, "with -flight-store-dir: drop history older than this")
		storeMaxBytes   = flag.Int64("flight-store-max-bytes", 64<<20, "with -flight-store-dir: bound on the total size of the segments (the newest segment may exceed it by one segment)")
		storeFsync      = flag.String("flight-store-fsync", "interval", "with -flight-store-dir: always, interval (incidents at once, samples every 10 s) or none")
		incidentMinDur  = flag.Duration("flight-incident-min-duration", 60*time.Second, "with -flight-store-dir: a warning-or-worse finding must persist this long to become an incident")
		inferDiscover   = flag.Bool("infer-metrics-discover", false, "opt in: also scrape http://<podIP>:<port>/metrics of pods on this node that carry a gryvia.io/job label and declare a container port from -infer-metrics-ports (needs a cluster and NODE_NAME; uses the pod-list RBAC the collector already has)")
		inferPorts2     = flag.String("infer-metrics-ports", "8000,8002,8080", "container ports probed by -infer-metrics-discover (vLLM 8000, Triton metrics 8002, TGI 8080)")
		inferInterval   = flag.Duration("infer-metrics-interval", inference.DefaultInterval, "scrape period of the serving-engine metrics (minimum 5s)")
		traceCorrelate  = flag.Bool("trace-correlate", false, "opt in: consume trace_correlator's trace_events ring (W3C traceparent of plaintext HTTP/1.x requests to job pods, IPv4), attach trace ids to the Flight Recorder timeline and serve GET /api/v1/flight/trace (HMAC-protected like the flight endpoint). Needs -iface and -flight-token-file")
	)
	var inferTargets stringList
	flag.Var(&inferTargets, "infer-metrics", "opt in: scrape a serving engine's Prometheus endpoint (vLLM, Triton, TGI); repeatable, or ';'-separated: name=vllm,url=http://10.0.0.5:8000/metrics[,engine=vllm|triton|tgi][,namespace=NS,job=JOB]")
	flag.Parse()

	// Logger.
	logger, err := zap.NewProduction()
	if err != nil {
		panic("failed to create logger: " + err.Error())
	}
	defer func() { _ = logger.Sync() }()
	log := logger.Sugar()

	log.Infow("starting Gryvia collector",
		"metrics_addr", *metricsAddr,
		"ebpf_dir", *ebpfDir,
		"iface", *iface,
	)

	sec := securityConfig{TLSCertFile: *tlsCertFile, TLSKeyFile: *tlsKeyFile, TLSClientCAFile: *tlsClientCA,
		APITokenFile: *apiTokenFile, MetricsTokenFile: *metricsToken, Insecure: *insecureListen, RequireAuth: *requireAuth}
	if err := sec.validate(); err != nil {
		log.Fatalw("invalid listener security flags", "error", err)
	}
	for _, w := range sec.warnings() {
		log.Warnw(w)
	}

	// Context with signal handling.
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *quotaPace && *cgroupPath == "" {
		log.Fatalw("-quota-pace needs -cgroup-path: pacing is only ever attached to an explicit cgroup")
	}

	if *xdpMux && *iface == "" {
		log.Fatalw("-xdp-mux needs -iface (xdp_mux attaches to it)")
	}
	if *traceCorrelate && (*iface == "" || *flightTokenFile == "") {
		log.Fatalw("-trace-correlate needs -iface (trace_correlator attaches to it) and -flight-token-file (the trace lookup is HMAC-protected)")
	}

	if (*quotaPaceSync || *quotaPaceDry) && !*quotaPace {
		log.Fatalw("-quota-pace-sync and -quota-pace-dry-run need -quota-pace (and -cgroup-path)")
	}
	if *quotaPaceDry && !*quotaPaceSync {
		log.Fatalw("-quota-pace-dry-run needs -quota-pace-sync")
	}

	ports, err := loader.ParsePorts(*inferPorts)
	if err != nil {
		log.Fatalw("invalid -infer-ports", "error", err)
	}

	// ---- Load eBPF programs ----
	optIn, err := loader.ParseEnablePrograms(*enablePrograms)
	if err != nil {
		log.Fatalw("invalid -enable-programs", "error", err)
	}
	mgr, err := loader.New(loader.Config{
		EnablePrograms: optIn,
		InferPorts:     ports,
		Dir:            *ebpfDir, Iface: *iface, CgroupPath: *cgroupPath,
		NCCLLib: *ncclLib, CUDALib: *cudaLib, CuFileLib: *cufileLib, UCXLib: *ucxLib, UprobePID: *uprobePID,
		IBVerbsLib: *ibverbsLib, IBVerbs: *ibverbsProbes,
		QuotaPace: *quotaPace, XDPMux: *xdpMux,
	}, log)
	if err != nil {
		log.Fatalw("failed to create eBPF loader", "error", err)
	}
	if err := mgr.LoadAndAttach(); err != nil {
		log.Fatalw("failed to load eBPF programs", "error", err)
	}
	defer mgr.Close()

	attachGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gryvia_ebpf_program_attached",
		Help: "1 if the eBPF program is attached, 0 otherwise.",
	}, []string{"object", "program", "kind"})
	prometheus.MustRegister(attachGauge)
	for _, s := range mgr.Status() {
		v, export := s.AttachGauge()
		if !export {
			continue // not requested by the configuration: not a failure to attach, so no series and no alert
		}
		attachGauge.WithLabelValues(s.Object, s.Program, s.Kind).Set(v)
	}

	// ---- Initialize subsystems ----
	metrics := exporter.NewMetrics()
	node := os.Getenv("NODE_NAME")
	identity := flight.NewResolver(node, "/host/proc")
	if node != "" {
		go identity.Run(ctx, func(err error) { log.Warnw("pod identity refresh failed", "error", err) })
	}
	recorder := flight.New(node)
	agg := aggregator.New(*windowSec)
	graphBuilder := graph.NewBuilder()
	detector := anomaly.NewDetector(log)

	// GPU and security decoders.
	gpuDecoder := decoder.NewGPUDecoder(4096)
	secDecoder := decoder.NewSecurityDecoder(4096)
	fabricDecoder := fabric.NewDecoder(4096)
	fabricFolder := fabric.NewFolder(fabric.DefaultWindow)
	// Attribute signals to (namespace, job) through the Flight Recorder's resolver: host PID ->
	// pod UID -> pod -> gryvia.io/job label. It fails closed, so anything it cannot resolve stays
	// under "_unattributed".
	fabricBinder := fabric.NewBinder(fabricFolder, func(pid uint32) (string, string, bool) {
		id, ok := identity.Resolve(pid)
		return id.Namespace, id.Job, ok
	})
	// GDS direct/bounce is only meaningful when the nvidia-fs kernel hook is attached.
	for _, s := range mgr.Status() {
		if s.Object == "gds_trace.o" && s.Target == "nvidia_fs_read" && s.Attached {
			fabricFolder.SetGDSDirectVisible(true)
		}
	}

	// roce_cnp keeps counters only (no ring): poll them and fold the CNP delta.
	roceAttached := false
	for _, s := range mgr.Status() {
		if s.Object == "roce_cnp.o" && s.Attached {
			roceAttached = true
		}
	}
	var lastCNP, lastRoCE uint64
	pollRoCE := func() {
		cnp, err1 := mgr.ReadCounter("roce_cnp.o", "cnp_count", fabric.CNPSlotCNP)
		roce, err2 := mgr.ReadCounter("roce_cnp.o", "cnp_count", fabric.CNPSlotRoCE)
		if err1 != nil || err2 != nil {
			log.Warnw("reading roce_cnp counters", "cnp_error", err1, "roce_error", err2)
			return
		}
		dCNP, dRoCE := cnp-lastCNP, roce-lastRoCE
		if cnp < lastCNP { // counter reset (program reloaded)
			dCNP = cnp
		}
		if roce < lastRoCE {
			dRoCE = roce
		}
		lastCNP, lastRoCE = cnp, roce
		fabricFolder.AddCNP(dCNP)
		metrics.RecordRoCE(dCNP, dRoCE)
	}

	// Opt-in extras (see fabricextras.go).
	var nicPoll *nicPoller
	if *nicCounters {
		nicPoll = newNICPoller(*nicSysfs, fabricFolder, metrics, log)
	}
	var ibvPoll *ibvPoller
	for _, s := range mgr.Status() {
		if s.Object == "ibv_verbs.o" && s.Attached {
			ibvPoll = &ibvPoller{mgr: mgr, metrics: metrics, log: log}
			break
		}
	}
	if *dcgmCorrelate {
		if err := startDCGM(ctx, log, fabricFolder, identity, *dcgmURL, *dcgmInterval, *dcgmIdle); err != nil {
			log.Warnw("-dcgm-correlate ignored", "error", err)
		}
	}

	// pfc_pause keeps per-CPU counters only (no ring): poll them, fold the
	// frame delta and export the per-priority counts.
	pfcAttached := false
	for _, s := range mgr.Status() {
		if s.Object == "pfc_pause.o" && s.Attached {
			pfcAttached = true
		}
	}
	var lastPFC [fabric.PFCSlots]uint64
	pollPFC := func() {
		var cur, delta [fabric.PFCSlots]uint64
		for slot := uint32(0); slot < fabric.PFCSlots; slot++ {
			v, err := mgr.ReadCounter("pfc_pause.o", "pause_count", slot)
			if err != nil {
				log.Warnw("reading pfc_pause counters", "slot", slot, "error", err)
				return
			}
			cur[slot] = v
			delta[slot] = v - lastPFC[slot]
			if v < lastPFC[slot] { // counter reset (program reloaded)
				delta[slot] = v
			}
		}
		lastPFC = cur
		var perPrio [fabric.PFCPriorities]uint64
		copy(perPrio[:], delta[fabric.PFCSlotPrio0:])
		fabricFolder.AddPFC(delta[fabric.PFCSlotFrames])
		metrics.RecordPFC(delta[fabric.PFCSlotFrames], delta[fabric.PFCSlotLegacy], perPrio)
	}

	// quota_pace is the only program that mutates sockets. With -quota-pace the
	// Pacer owns its pace_rate map: it deletes lapsed leases and, at shutdown,
	// every entry it wrote. Without -quota-pace-sync nothing calls Pacer.Grant, so
	// no cgroup is paced until an operator inserts an entry. With it, the
	// QuotaSyncer (below) is the only caller, and only for pods on this node in
	// namespaces listed by a GryviaQuota that sets spec.network.maxEgressMbps.
	var pacer *fabric.Pacer
	if *quotaPace {
		if pm := mgr.Map("quota_pace.o", loader.PaceRateMap); pm != nil {
			ttl := fabric.DefaultLeaseTTL
			if *quotaPaceSync {
				ttl = fabric.QuotaSyncLeaseTTL // renewed every sync; an API outage ends pacing within minutes
			}
			pacer = fabric.NewPacer(fabric.NewEBPFPaceMap(pm), ttl, log)
			go pacer.Run(ctx, 10*time.Second)
			defer func() { _ = pacer.Shutdown() }()
			log.Warnw("quota pacing enabled: the pace_rate map is empty until a lease is granted",
				"cgroup_path", *cgroupPath, "lease_ttl", ttl.String(), "quota_pace_sync", *quotaPaceSync, "dry_run", *quotaPaceDry)
			if *quotaPaceSync {
				startQuotaSync(ctx, log, pacer, node, *cgroupPath, *quotaPaceDry)
			}
		} else {
			log.Warnw("-quota-pace set but quota_pace.o is not loaded (missing object or verifier rejection); pacing stays off")
		}
	}

	// Serving-engine metrics (vLLM / Triton / TGI): opt-in, read-only HTTP GETs of the engines' own
	// /metrics endpoints. Kernel probes cannot see tokens, so TTFT, ITL and engine queue time can
	// only come from the engine.
	scraper := startInferenceScraper(ctx, log, fabricFolder, node, inferTargets, *inferDiscover, *inferPorts2, *inferInterval)

	// Publishing fabric status writes to the cluster, so it needs an explicit flag and a cluster.
	if *publishFabric {
		client, err := kube.NewInCluster()
		if err != nil {
			log.Warnw("-publish-fabric-status ignored", "error", err)
		} else {
			perNode := *fabricPerNode
			if perNode && node == "" {
				log.Warnw("-fabric-status-per-node ignored: NODE_NAME is not set; merge-patching the top-level status")
				perNode = false
			}
			go (&fabric.Publisher{API: client, Folder: fabricFolder, Log: log, Inference: scraper != nil, PerNode: perNode, Node: node}).Run(ctx)
			log.Infow("publishing fabric status to GryviaFabricSignal (existing objects only)", "interval", fabric.PublishInterval.String(), "per_node", perNode)
		}
	}

	// Per-node fabric health for fabric-aware scheduling: opt-in, writes one object named after this node.
	if *publishNodeFab {
		client, err := kube.NewInCluster()
		switch {
		case err != nil:
			log.Warnw("-publish-node-fabric ignored", "error", err)
		case node == "":
			log.Warnw("-publish-node-fabric ignored: NODE_NAME is not set")
		default:
			go (&fabric.NodePublisher{API: client, Folder: fabricFolder, Node: node, Log: log}).Run(ctx)
			log.Infow("publishing node fabric health to GryviaNodeFabric", "node", node, "interval", fabric.NodePublishInterval.String())
		}
	}

	if *publishNetUsage && !*attributeNet {
		log.Fatalw("-publish-network-usage needs -attribute-network")
	}
	if *attributeNet {
		startNetworkAttribution(ctx, log, mgr, node, *publishNetUsage)
	}

	// GPU aggregators.
	window := time.Duration(*windowSec) * time.Second
	ncclAgg := aggregator.NewNCCLAggregator(window)
	gpuMemAgg := aggregator.NewGPUMemAggregator(window)

	// Security aggregator.
	secAgg := security.NewSecurityAggregator(10000)

	// AI training analysers.
	trainingAnalyzer := ai.NewTrainingAnalyzer()
	pipelineAnalyzer := ai.NewPipelineAnalyzer()

	// TCP tuning advisor.
	tcpAdvisor := tuning.NewTCPAdvisor()

	// Trace correlation (opt-in): index the traceparent headers trace_correlator saw, keyed by
	// connection, and let the Flight Recorder attach trace ids to events of the same connection.
	var traceIndex *trace.Index
	if *traceCorrelate {
		traceIndex = trace.NewIndex()
		recorder.SetCorrelator(traceIndex)
		rings := mgr.RingBufReaders(loader.ClassTrace)
		if len(rings) == 0 {
			log.Warnw("-trace-correlate: trace_correlator is not loaded, so no trace ids will be seen (needs -iface and a loadable trace_correlator.o)")
		}
		for _, rr := range rings {
			go traceIndex.Consume(rr, identity)
		}
		log.Infow("trace correlation enabled (plaintext HTTP/1.x, IPv4, requests to gryvia.io/job pods on this node)", "trace_rings", len(rings))
	}

	// ---- Decode perf events (existing flow events) ----
	dec, err := decoder.New(mgr.PerfReaders(loader.ClassFlow), log)
	if err != nil {
		log.Warnw("no perf readers available, flow decoding disabled", "error", err)
	}

	if dec != nil {
		// Event processing pipeline for flow events.
		eventCh := dec.Events()
		go func() {
			for ev := range eventCh {
				if id, ok := identity.Resolve(ev.PID); ok {
					ev.SrcPod, ev.Namespace = id.Pod, id.Namespace
					kind := "flow"
					if ev.Verdict == 2 {
						kind = "tcp_retransmit"
					}
					observation := flight.Event{Identity: id, Source: "ebpf", Kind: kind, Bytes: uint64(ev.Bytes), DurationNs: ev.LatencyNs}
					if traceIndex != nil && ev.Protocol == 6 {
						tp := trace.TupleFromFlow(ev.SrcIP, ev.SrcPort, ev.DstIP, ev.DstPort)
						observation.Tuple = &tp
					}
					if ev.Verdict == 2 {
						observation.Retransmits, observation.Bytes = ev.Bytes, 0
					}
					recorder.Record(observation)
				}
				if ev.Verdict == 2 {
					continue // observation, not traffic or a drop
				}
				// Update aggregator.
				agg.Record(ev)
				// Update graph.
				graphBuilder.RecordFlow(ev)
				// Check for anomalies.
				detector.Observe(ev)
				// Update Prometheus metrics.
				metrics.RecordFlow(ev)
			}
		}()
		// Start perf event reading.
		go dec.Run(ctx)
	}

	// ---- Decode ring buffer events, dispatched by map class ----
	gpuRings := mgr.RingBufReaders(loader.ClassGPU)
	for _, rr := range gpuRings {
		go gpuDecoder.DecodeRingBuf(rr)
	}
	secRings := mgr.RingBufReaders(loader.ClassSecurity)
	for _, rr := range secRings {
		go secDecoder.DecodeRingBuf(rr)
	}
	fabricRings := mgr.RingBufReaders(loader.ClassFabric)
	for _, rr := range fabricRings {
		go fabricDecoder.DecodeRingBuf(rr)
	}
	log.Infow("event readers started",
		"flow_perf", len(mgr.PerfReaders(loader.ClassFlow)),
		"gpu_ringbuf", len(gpuRings), "security_ringbuf", len(secRings),
		"fabric_ringbuf", len(fabricRings))

	// ---- GPU event processing pipeline ----
	go func() {
		for ev := range gpuDecoder.Events() {
			if id, ok := identity.Resolve(ev.PID); ok {
				kind := "gpu_api"
				switch ev.EventType {
				case decoder.GPUEvtNCCLOp:
					kind = "nccl_collective"
				case decoder.GPUEvtRDMASend, decoder.GPUEvtRDMARecv:
					kind = "rdma_kernel"
				case decoder.GPUEvtPipeStall:
					kind = "pipeline_stall"
				case decoder.GPUEvtMemTransfer:
					kind = "cuda_memcpy"
				}
				observation := flight.Event{Identity: id, Source: "ebpf", Kind: kind, Bytes: ev.Bytes, DurationNs: ev.LatencyNs}
				if ev.EventType == decoder.GPUEvtNCCLOp {
					observation.Operation = decoder.NCCLOpName(ev.NCCLOp)
				}
				recorder.Record(observation)
			}
			// Update NCCL aggregator.
			ncclAgg.Process(ev)
			// Update GPU memory aggregator.
			gpuMemAgg.Process(ev)
			// Update training analyser.
			trainingAnalyzer.Process(ev)
			// Update pipeline analyser.
			pipelineAnalyzer.ProcessGPUEvent(ev)
			// Update Prometheus GPU metrics.
			metrics.RecordGPUEvent(ev)
		}
	}()

	// ---- Security event processing pipeline ----
	go func() {
		for ev := range secDecoder.Events() {
			// Update security aggregator.
			secAgg.Process(ev)
			// Update Prometheus security metrics.
			metrics.RecordSecurityEvent(ev)
		}
	}()

	// ---- Fabric signal pipeline (straggler, RDMA health, GDS, overlap, inference wait, UCX, weight exfil) ----
	go func() {
		for sig := range fabricDecoder.Events() {
			fabricBinder.Add(sig)
			if sig.Type == fabric.SigInferWait && traceIndex != nil {
				// Only with trace correlation on: the accept wait joins a request by pod, port and time.
				if id, ok := identity.Resolve(sig.PID); ok {
					recorder.Record(flight.Event{Identity: id, Source: "ebpf", Kind: "inference_accept_wait", DurationNs: sig.LatencyNS, LocalPort: uint16(sig.Rank)})
				}
			}
			if sig.Type == fabric.SigCollective {
				if id, ok := identity.Resolve(sig.PID); ok {
					ev := flight.Event{Identity: id, Source: "ebpf", Operation: decoder.NCCLOpName(sig.NCCLOp), Bytes: sig.Bytes,
						DurationNs: sig.LatencyNS, CommOrdinal: sig.CommOrdinal(), CommSeq: sig.CollSeq(), CommWorld: sig.WorldSize,
						CommLate: sig.Retries&fabric.CollFlagLate != 0}
					if sig.Rank != fabric.RankUnknown {
						r := sig.Rank
						ev.CommRank = &r
					}
					recorder.RecordCollective(ev)
				}
			}
			if sig.Type == fabric.SigWeightMmap {
				// Observe only, like SigExfil: peer_rank carries the remote IPv4 in host order.
				log.Warnw("possible model-weight exfiltration: weight file mapped, then a connect to a public address",
					"pid", sig.PID, "comm", sig.Comm, "remote_port", sig.Rank, "mmap_to_connect_ms", sig.LatencyNS/1_000_000)
			}
			if sig.Type == fabric.SigGPUDev {
				log.Warnw("GPU device opened from a cgroup outside the allowed set",
					"pid", sig.PID, "comm", sig.Comm, "cgroup_id_lo", sig.CgroupLo)
			}
			if sig.Type == fabric.SigExfil {
				// Observe only: the probe emits one signal per read burst. Never enforced here.
				log.Warnw("possible model-weight exfiltration: large model-file read followed by a connect to a non-internal address",
					"pid", sig.PID, "comm", sig.Comm, "dest", sig.ExfilDest(),
					"bytes_read", sig.Bytes, "read_to_connect_ms", sig.LatencyNS/1_000_000)
			}
		}
	}()

	// ---- Periodic straggler detection and metric updates ----
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Detect NCCL stragglers.
				ncclAgg.DetectStragglers()
				stragglers := ncclAgg.GetStragglers()
				for range stragglers {
					metrics.RecordStragglerDetected()
				}

				// Update comm/compute ratio metric.
				ratio := trainingAnalyzer.GetCommComputeRatio()
				metrics.RecordCommComputeRatio("default", ratio)

				// Update pipeline bottleneck metric.
				bottleneck, _ := pipelineAnalyzer.GetBottleneck()
				metrics.RecordPipelineBottleneck(bottleneck)

				if roceAttached {
					pollRoCE()
				}
				if pfcAttached {
					pollPFC()
				}

				if nicPoll != nil {
					nicPoll.poll(time.Now())
				}
				if ibvPoll != nil {
					ibvPoll.poll()
				}

				// Publish per-job fabric status.
				metrics.RecordFabric(fabricFolder.List())
			}
		}
	}()

	// ---- Optional NATS streaming ----
	if *natsURL != "" {
		log.Infow("NATS streaming enabled", "url", *natsURL)
		// NATS connection would be established here.
		// Events would be published to "gryvia.flows" subject.
	}

	// ---- HTTP server for metrics and API endpoints ----
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	// Existing endpoints.
	mux.HandleFunc("/api/v1/graph", graphBuilder.ServeHTTP)
	mux.HandleFunc("/api/v1/anomalies", detector.ServeHTTP)

	// GPU endpoints.
	mux.HandleFunc("/api/v1/gpu/nccl", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		stats := ncclAgg.GetOpStats()
		stragglers := ncclAgg.GetStragglers()
		resp := struct {
			OpStats    map[string]*aggregator.NCCLOpStats `json:"op_stats"`
			Stragglers []aggregator.StragglerEvent        `json:"stragglers"`
		}{
			OpStats:    stats,
			Stragglers: stragglers,
		}
		if err := encodeJSON(w, resp); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/api/v1/gpu/memory", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		stats := gpuMemAgg.GetDirectionStats()
		if err := encodeJSON(w, stats); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Fabric signals: per-job straggler / RDMA / GDS status and score penalty.
	mux.HandleFunc("/api/v1/fabric", fabricFolder.ServeHTTP)

	// Security endpoint.
	mux.HandleFunc("/api/v1/security/alerts", secAgg.ServeHTTP)

	// AI/training endpoints.
	mux.HandleFunc("/api/v1/ai/training", trainingAnalyzer.ServeHTTP)
	mux.HandleFunc("/api/v1/ai/pipeline", pipelineAnalyzer.ServeHTTP)
	mux.Handle("/api/v1/flight/diagnose", flightAuth(recorder, *flightTokenFile))
	if traceIndex != nil {
		mux.Handle("/api/v1/flight/trace", flightAuth(&trace.Handler{Index: traceIndex, Recorder: recorder, Node: node}, *flightTokenFile))
	}
	if scraper != nil {
		// Same HMAC protection as the flight endpoint: the answer lists target addresses.
		mux.Handle("/api/v1/inference", flightAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = encodeJSON(w, scraper.Status())
		}), *flightTokenFile))
	}

	// Unified diagnosis, incident history and comparison (same HMAC protection as the flight route).
	diag := &diagnosisService{node: node, recorder: recorder, identity: identity, folder: fabricFolder,
		probes: managerProbes{mgr}, now: time.Now,
		gauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gryvia_measurement_dropped_events",
			Help: "Events lost before analysis (producer ring/perf failures and consumer backlog), monotonic since start."}, []string{"source"})}
	prometheus.MustRegister(diag.gauge)
	diag.cfg, err = diagnosis.LoadConfig(*diagThresholds)
	if err != nil {
		log.Fatalw("invalid -flight-diagnosis-thresholds", "error", err)
	}
	diag.sampler = diagnosis.NewSampler(*diagCgroup, identity)
	diag.userDrops = func() []diagnosis.DropCount {
		out := []diagnosis.DropCount{{Source: "userspace/gpu-decoder", Count: gpuDecoder.Dropped()},
			{Source: "userspace/fabric-decoder", Count: fabricDecoder.Dropped()}}
		if dec != nil {
			out = append(out, diagnosis.DropCount{Source: "userspace/flow-perf-lost", Count: dec.Lost()})
		}
		return out
	}
	if *storeDir != "" {
		st, err := incident.Open(incident.Options{Dir: *storeDir, Retention: *storeRetention, MaxBytes: *storeMaxBytes, Fsync: *storeFsync})
		if err != nil {
			log.Fatalw("cannot open -flight-store-dir", "error", err)
		}
		diag.store = st
		diag.tracker = incident.NewTracker(st, incident.TrackerOptions{MinDuration: *incidentMinDur})
		log.Infow("flight incident history enabled", "dir", *storeDir, "retention", storeRetention.String(), "max_bytes", *storeMaxBytes, "fsync", *storeFsync)
	}
	diag.register(mux, *flightTokenFile)
	go diag.Run(ctx, 10*time.Second)

	// TCP tuning endpoint.
	mux.HandleFunc("/api/v1/tuning/tcp", tcpAdvisor.ServeHTTP)

	// eBPF program attach status.
	mux.HandleFunc("/api/v1/ebpf/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = encodeJSON(w, mgr.Status())
	})

	// Health check.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              *metricsAddr,
		Handler:           sec.authMiddleware(mux, time.Now),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if sec.tlsEnabled() {
		rl, err := newReloader(sec.TLSCertFile, sec.TLSKeyFile, sec.TLSClientCAFile,
			func(err error) { log.Warnw("TLS reload failed", "error", err) })
		if err != nil {
			log.Fatalw("cannot load TLS material", "error", err)
		}
		srv.TLSConfig = rl.tlsConfig()
	}

	go func() {
		log.Infow("starting HTTP server", "addr", *metricsAddr, "tls", sec.tlsEnabled(),
			"mtls", sec.TLSClientCAFile != "", "auth_enforced", sec.enforcing())
		var err error
		if sec.tlsEnabled() {
			err = srv.ListenAndServeTLS("", "")
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalw("HTTP server failed", "error", err)
		}
	}()

	// Wait for shutdown.
	<-ctx.Done()
	log.Info("shutting down collector")
	if pacer != nil {
		// Fail open first: remove every pace entry this collector wrote.
		if err := pacer.Shutdown(); err != nil {
			log.Errorw("removing pace entries at shutdown", "error", err)
		}
	}
	if err := srv.Close(); err != nil {
		log.Errorw("HTTP server close error", "error", err)
	}
}

// encodeJSON is a helper to encode JSON responses.
func encodeJSON(w http.ResponseWriter, v interface{}) error {
	enc := json.NewEncoder(w)
	return enc.Encode(v)
}

// startQuotaSync starts the GryviaQuota -> pace lease reconcile loop. Every
// prerequisite is checked and a missing one leaves pacing inert (with a log
// line): running in a cluster, NODE_NAME, the collector's own namespace (its
// pods are never paced) and a cgroup v2 root.
func startQuotaSync(ctx context.Context, log *zap.SugaredLogger, pacer *fabric.Pacer, node, cgroupPath string, dryRun bool) {
	client, err := kube.NewInCluster()
	if err != nil {
		log.Warnw("-quota-pace-sync ignored", "error", err)
		return
	}
	if node == "" {
		log.Warnw("-quota-pace-sync ignored: NODE_NAME is not set")
		return
	}
	own, err := os.ReadFile(kube.SADir + "/namespace")
	if err != nil || strings.TrimSpace(string(own)) == "" {
		log.Warnw("-quota-pace-sync ignored: cannot determine the collector's own namespace", "error", err)
		return
	}
	src := &fabric.KubeSources{API: client, Node: node}
	s := fabric.NewQuotaSyncer(src, src, &fabric.HostCgroups{Root: cgroupPath}, pacer, strings.TrimSpace(string(own)), dryRun, log)
	go s.Run(ctx, fabric.QuotaSyncInterval)
	log.Warnw("quota pace sync running", "interval", fabric.QuotaSyncInterval.String(), "dry_run", dryRun, "node", node,
		"own_namespace", strings.TrimSpace(string(own)), "cgroup_root", cgroupPath)
}

// startNetworkAttribution runs the opt-in tenant network cost pipeline: the cost_tracker byte counters are
// folded per scan into per-(hour, tenant, peer, zone) totals and, with publish, written as
// GryviaNetworkUsageRecord. Every prerequisite is checked; a missing one leaves the feature off with a log line.
func startNetworkAttribution(ctx context.Context, log *zap.SugaredLogger, mgr *loader.Manager, node string, publish bool) {
	m := mgr.Map("cost_tracker.o", "traffic_costs")
	if m == nil {
		log.Warnw("-attribute-network ignored: cost_tracker.o is not loaded (needs -iface, kernel >= 6.6 with tcx)")
		return
	}
	if node == "" {
		log.Warnw("-attribute-network ignored: NODE_NAME is not set")
		return
	}
	client, err := kube.NewInCluster()
	if err != nil {
		log.Warnw("-attribute-network ignored", "error", err)
		return
	}
	dir := netcost.NewDirectory()
	meter := netcost.NewMeter(dir, node)
	go dir.Run(ctx, client, func(err error) { log.Warnw("network attribution: directory refresh failed", "error", err) })
	go netcost.RunScan(ctx, m, mgr.Map("cost_tracker.o", "traffic_costs6"), meter, func(err error) { log.Warnw("network attribution: reading traffic_costs failed", "error", err) })
	prometheus.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "gryvia_netcost_unattributed_bytes_total",
		Help: "Bytes seen by cost_tracker that were not attributed to a tenant (unknown, non-tenant, hostNetwork, loopback, same-node).",
	}, func() float64 {
		var n uint64
		for _, v := range meter.Stats().Skipped {
			n += v
		}
		return float64(n)
	}))
	log.Infow("network attribution running (unverified on a real cluster; egress-only billing)", "node", node, "publish", publish)
	if publish {
		go (&netcost.Publisher{API: client, Meter: meter, Node: node, Log: log}).Run(ctx)
	}
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ";") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// startInferenceScraper starts the serving-engine metrics scraper when it was asked for
// (-infer-metrics and/or -infer-metrics-discover) and returns it; nil means it is off. Readings
// are folded into the fabric folder under the target's namespace/job (or "_unattributed"/name
// for a static target without attribution).
func startInferenceScraper(ctx context.Context, log *zap.SugaredLogger, folder *fabric.Folder, node string,
	specs []string, discover bool, discoverPorts string, interval time.Duration) *inference.Scraper {
	if len(specs) == 0 && !discover {
		return nil
	}
	static, err := inference.ParseTargets(strings.Join(specs, ";"))
	if err != nil {
		log.Fatalw("invalid -infer-metrics", "error", err)
	}
	var disc func(context.Context) ([]inference.Target, error)
	if discover {
		ports, err := inference.ParsePorts(discoverPorts)
		if err != nil {
			log.Fatalw("invalid -infer-metrics-ports", "error", err)
		}
		if client, err := kube.NewInCluster(); err != nil {
			log.Warnw("-infer-metrics-discover ignored", "error", err)
		} else if node == "" {
			log.Warnw("-infer-metrics-discover ignored: NODE_NAME is not set")
		} else {
			disc = func(ctx context.Context) ([]inference.Target, error) {
				return inference.Discover(ctx, client, node, ports)
			}
		}
	}
	if len(static) == 0 && disc == nil {
		return nil
	}
	sink := func(t inference.Target, r inference.Reading) {
		key := fabric.JobKey{Namespace: t.Namespace, Job: t.Job}
		if t.Namespace == "" {
			key = fabric.JobKey{Namespace: "_unattributed", Job: t.Name}
		}
		folder.SetInference(key, t.Name, r)
	}
	s := inference.NewScraper(static, disc, sink, log)
	s.Interval = interval
	go s.Run(ctx)
	log.Infow("scraping serving-engine metrics (read-only HTTP GET)", "static_targets", len(static),
		"discover", disc != nil, "interval", fmt.Sprint(interval))
	return s
}
