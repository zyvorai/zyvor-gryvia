// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package diagnosis

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/fabric"
	"github.com/zyvorai/gryvia/collector/pkg/flight"
)

// Non-cgroup signal names, and the eBPF object that produces each.
const (
	SigTCPRetransmit = "flight.tcp_retransmit"
	SigPipeStall     = "flight.pipeline_stall"
	SigNCCLCall      = "flight.nccl_collective"
	SigStraggler     = "fabric.straggler"
	SigRDMARetry     = "fabric.rdma_retry"
	SigOverlap       = "fabric.overlap"
	SigGDS           = "fabric.gds"
	SigCNP           = "fabric.cnp"
	SigPFC           = "fabric.pfc"
	SigExfil         = "fabric.weight_exfil"
)

// SignalProbe maps each eBPF-derived signal to the object that must be attached
// for it to be measured. An attached probe that observed nothing yields a
// measured zero: the report cannot tell "the workload did not do that" from
// "the workload did it and the hook did not fire" (see docs/flight-diagnosis.md).
var SignalProbe = map[string]string{
	SigTCPRetransmit: "tcp_trace.o",
	SigPipeStall:     "datapipe_bottleneck.o",
	SigNCCLCall:      "nccl_trace.o",
	SigStraggler:     "straggler.o",
	SigRDMARetry:     "rdma_health.o",
	SigOverlap:       "overlap.o",
	SigGDS:           "gds_trace.o",
	SigCNP:           "roce_cnp.o",
	SigPFC:           "pfc_pause.o",
	SigExfil:         "weight_exfil.o",
}

// kindSignals lists, per finding kind, the signals whose absence limits a conclusion.
var kindSignals = map[string][]string{
	KindNetwork:   {SigTCPRetransmit, SigCNP, SigPFC},
	KindStorage:   {SigPipeStall, SigIOPressure, SigGDS},
	KindCPU:       {SigCPUStat, SigCPUPressure},
	KindMemory:    {SigMemEvents, SigMemUsage, SigMemPressure},
	KindGPUComm:   {SigStraggler, SigOverlap, SigNCCLCall},
	KindStraggler: {SigStraggler},
	KindRDMA:      {SigRDMARetry},
	KindExfil:     {SigExfil},
}

// kindCaveats are always attached to a finding of that kind: what this tool
// cannot see even when every probe is attached.
var kindCaveats = map[string]string{
	KindNetwork:   "NIC and switch counters are not read; retransmits are counted when a connection closes",
	KindStorage:   "per-job block I/O latency is not measured (the block-layer counters in datapipe_bottleneck.o are node-wide); a GPU-idle gap does not prove the input pipeline is the cause",
	KindCPU:       "host-wide CPU contention outside the job's cgroups is not read",
	KindMemory:    "GPU (HBM) memory is not read",
	KindGPUComm:   "GPU utilisation (DCGM/CUPTI) and NCCL internals are not read; durations are host-side API spans",
	KindStraggler: "rank skew is seen at the NCCL API; the GPU-side or network cause is not measured",
	KindRDMA:      "NIC hardware counters are not read (unverified on hardware: no RDMA device was available to test)",
	KindExfil:     "heuristic on a large model-file read followed by a connect; payload is not inspected",
}

// relevantObjects are the objects whose absence makes a report incomplete.
var relevantObjects = func() map[string]bool {
	m := map[string]bool{"gpu_mem_trace.o": true}
	for _, o := range SignalProbe {
		m[o] = true
	}
	return m
}()

// Input is everything Evaluate needs; it performs no I/O.
type Input struct {
	Now       time.Time
	Namespace string
	Job       string
	Node      string

	// Events are the flight events of the job inside the window (oldest first);
	// EventsTruncated says the bounded ring evicted events still inside it.
	Events          []flight.Event
	EventsTruncated bool
	// Fabric is the folded status of the job; CNPRate and PFCRate are node-level.
	Fabric  fabric.Status
	CNPRate float64
	PFCRate float64

	// Cgroup is the derived cgroup view (nil is treated as no access).
	Cgroup *CgroupWindow

	// Probes is the loader's per-program result; nil means "unknown".
	Probes []ProbeStatus
	// Drops are loss counters (producer maps and userspace decoders).
	Drops []DropCount
	// DropsNotCounted names attached relevant objects that expose no `drops` map.
	DropsNotCounted []string
}

type hit struct {
	kind     string
	sev      string
	base     string // base confidence before adjustment
	marginal bool
	signal   string
	ev       []Evidence
	summary  string
}

func sevFor(v float64, t Threshold) string {
	switch {
	case v > t.Crit:
		return SevCritical
	case v > t.Warn:
		return SevWarning
	}
	return ""
}

func down(c string) string {
	if c == ConfHigh {
		return ConfMedium
	}
	return ConfLow
}

func up(c string) string {
	if c == ConfLow {
		return ConfMedium
	}
	return ConfHigh
}

// marginal reports a value in the lowest quarter of the warn..crit band: barely
// over the threshold, so the finding is less trustworthy.
func marginal(v float64, t Threshold) bool { return t.Crit > t.Warn && v < t.Warn+0.25*(t.Crit-t.Warn) }

// Evaluate applies the rules. It is deterministic and side-effect free.
func Evaluate(in Input, cfg Config) Diagnosis {
	win := cfg.Window()
	winLabel := win.String()
	d := Diagnosis{
		Namespace: in.Namespace, Job: in.Job, Node: in.Node,
		Scope:       "node-local; measured signals only",
		GeneratedAt: in.Now.UTC(), Window: winLabel,
		Findings: []Finding{}, Unavailable: []Unavailable{}, Measured: []string{},
		Metrics: map[string]float64{},
	}
	unavail := map[string]string{}
	measured := map[string]bool{}
	setUnavail := func(sig, why string) { unavail[sig] = why }
	metric := func(sig, name string, v float64) {
		measured[sig] = true
		d.Metrics[name] = v
	}

	// Probe availability per signal.
	attached := map[string]bool{}
	skipReason := map[string]string{}
	for _, p := range in.Probes {
		if p.Attached {
			attached[p.Object] = true
		} else if _, have := skipReason[p.Object]; !have {
			r := p.Reason
			if r == "" {
				r = "not attached"
			}
			if p.Program != "" {
				r = p.Program + ": " + r
			}
			skipReason[p.Object] = r
		}
	}
	probeOK := func(sig string) bool {
		obj := SignalProbe[sig]
		if in.Probes == nil {
			setUnavail(sig, "eBPF probe status unknown (the loader reported nothing)")
			return false
		}
		if !attached[obj] {
			why := skipReason[obj]
			if why == "" {
				why = "object not loaded"
			}
			setUnavail(sig, obj+" not attached: "+why)
			return false
		}
		return true
	}

	var hits []hit
	add := func(h hit) { hits = append(hits, h) }
	ev := func(source, metricName string, v float64) Evidence {
		return Evidence{Source: source, Metric: metricName, Value: round(v), Window: winLabel}
	}

	// ---- flight events ----
	var retrans uint64
	stallByPod := map[string]uint64{}
	var stalls int
	var ncclNs []float64
	for _, e := range in.Events {
		switch e.Kind {
		case "tcp_retransmit":
			retrans += uint64(e.Retransmits)
		case "pipeline_stall":
			stalls++
			stallByPod[e.Identity.Pod] += e.DurationNs
		case "nccl_collective":
			if e.DurationNs > 0 {
				ncclNs = append(ncclNs, float64(e.DurationNs)/1e6)
			}
		}
	}
	if probeOK(SigTCPRetransmit) {
		metric(SigTCPRetransmit, "tcp_retransmits", float64(retrans))
		if s := sevFor(float64(retrans), cfg.TCPRetransmits); s != "" {
			add(hit{kind: KindNetwork, sev: s, base: ConfMedium, marginal: marginal(float64(retrans), cfg.TCPRetransmits), signal: SigTCPRetransmit,
				ev:      []Evidence{ev("flight", "tcp_retransmits", float64(retrans))},
				summary: fmt.Sprintf("%d TCP retransmissions observed on closed connections in %s", retrans, winLabel)})
		}
	}
	if probeOK(SigPipeStall) {
		var worst uint64
		for _, ns := range stallByPod {
			if ns > worst {
				worst = ns
			}
		}
		ratio := math.Min(1, float64(worst)/float64(win.Nanoseconds()))
		metric(SigPipeStall, "pipeline_stall_ratio", ratio)
		d.Metrics["pipeline_stall_events"] = float64(stalls)
		if stalls >= cfg.PipelineStallMin {
			if s := sevFor(ratio, cfg.PipelineStallRatio); s != "" {
				add(hit{kind: KindStorage, sev: s, base: ConfLow, marginal: marginal(ratio, cfg.PipelineStallRatio), signal: SigPipeStall,
					ev:      []Evidence{ev("flight", "pipeline_stall_ratio", ratio), ev("flight", "pipeline_stall_events", float64(stalls))},
					summary: fmt.Sprintf("GPU was idle between kernel launches for %.0f%% of %s in the worst pod (%d gaps over 1 ms); the data pipeline is one possible cause, not established", ratio*100, winLabel, stalls)})
			}
		}
	}
	if probeOK(SigNCCLCall) {
		sort.Float64s(ncclNs)
		p99 := 0.0
		if n := len(ncclNs); n > 0 {
			p99 = ncclNs[int(math.Ceil(0.99*float64(n)))-1]
		}
		metric(SigNCCLCall, "nccl_call_p99_ms", p99) // informational; no rule: API span includes compute overlap
	}

	// ---- fabric status ----
	f := in.Fabric
	if probeOK(SigStraggler) {
		metric(SigStraggler, "straggler_hits", float64(f.StragglerHits))
		d.Metrics["nccl_p99_ms"] = f.NCCLP99MS
		if s := sevFor(float64(f.StragglerHits), cfg.Straggler); s != "" {
			add(hit{kind: KindStraggler, sev: s, base: ConfMedium, marginal: marginal(float64(f.StragglerHits), cfg.Straggler), signal: SigStraggler,
				ev:      []Evidence{ev("fabric", "straggler_hits", float64(f.StragglerHits)), ev("fabric", "straggler_rank", float64(f.StragglerRank))},
				summary: fmt.Sprintf("rank %d was the slowest participant in %d allreduce spans in the window", f.StragglerRank, f.StragglerHits)})
		}
		if s := sevFor(f.NCCLP99MS, cfg.NCCLP99MS); s != "" {
			add(hit{kind: KindGPUComm, sev: s, base: ConfMedium, marginal: marginal(f.NCCLP99MS, cfg.NCCLP99MS), signal: SigStraggler,
				ev:      []Evidence{ev("fabric", "nccl_p99_ms", f.NCCLP99MS)},
				summary: fmt.Sprintf("allreduce span p99 is %.1f ms", f.NCCLP99MS)})
		}
	}
	if probeOK(SigOverlap) {
		metric(SigOverlap, "overlap_idle_ratio", f.OverlapIdleRatio)
		if s := sevFor(f.OverlapIdleRatio, cfg.OverlapIdle); s != "" {
			add(hit{kind: KindGPUComm, sev: s, base: ConfMedium, marginal: marginal(f.OverlapIdleRatio, cfg.OverlapIdle), signal: SigOverlap,
				ev:      []Evidence{ev("fabric", "overlap_idle_ratio", f.OverlapIdleRatio)},
				summary: fmt.Sprintf("host was blocked in cudaDeviceSynchronize inside an in-flight allreduce for %.0f%% of the window", f.OverlapIdleRatio*100)})
		}
	}
	if probeOK(SigRDMARetry) {
		metric(SigRDMARetry, "rdma_retry_rate", f.RDMARetryRate)
		if s := sevFor(f.RDMARetryRate, cfg.RDMARetry); s != "" {
			add(hit{kind: KindRDMA, sev: s, base: ConfMedium, marginal: marginal(f.RDMARetryRate, cfg.RDMARetry), signal: SigRDMARetry,
				ev:      []Evidence{ev("fabric", "rdma_retry_rate", f.RDMARetryRate)},
				summary: fmt.Sprintf("RDMA queue pairs reported retry/RNR-exceeded completions at %.1f%% of posted work", f.RDMARetryRate*100)})
		}
	}
	if probeOK(SigGDS) {
		if f.GDSMeasured {
			metric(SigGDS, "gds_direct_ratio", f.GDSHitRatio)
			if f.GDSHitRatio < cfg.GDSHitRatioLow {
				add(hit{kind: KindStorage, sev: SevWarning, base: ConfMedium, signal: SigGDS,
					ev:      []Evidence{ev("fabric", "gds_direct_ratio", f.GDSHitRatio)},
					summary: fmt.Sprintf("only %.0f%% of cuFile bytes took the direct GPUDirect Storage path (bounce buffer otherwise)", f.GDSHitRatio*100)})
			}
		} else {
			setUnavail(SigGDS, "gds_trace.o attached but no direct/bounce classification (nvidia_fs hook not attached, or no cuFile reads in the window)")
		}
	}
	if probeOK(SigCNP) {
		metric(SigCNP, "cnp_rate", in.CNPRate)
		if s := sevFor(in.CNPRate, cfg.CNPRate); s != "" {
			add(hit{kind: KindNetwork, sev: s, base: ConfMedium, marginal: marginal(in.CNPRate, cfg.CNPRate), signal: SigCNP,
				ev:      []Evidence{ev("fabric", "cnp_rate", in.CNPRate)},
				summary: fmt.Sprintf("RoCEv2 congestion notifications at %.0f/s on this node's RDMA interface (node-level, not specific to this job)", in.CNPRate)})
		}
	}
	if probeOK(SigPFC) {
		metric(SigPFC, "pfc_rate", in.PFCRate)
		if s := sevFor(in.PFCRate, cfg.PFCRate); s != "" {
			add(hit{kind: KindNetwork, sev: s, base: ConfMedium, marginal: marginal(in.PFCRate, cfg.PFCRate), signal: SigPFC,
				ev:      []Evidence{ev("fabric", "pfc_rate", in.PFCRate)},
				summary: fmt.Sprintf("PFC pause frames at %.0f/s on this node (node-level, not specific to this job)", in.PFCRate)})
		}
	}
	if probeOK(SigExfil) {
		metric(SigExfil, "exfil_events", float64(f.ExfilEvents))
		if s := sevFor(float64(f.ExfilEvents), cfg.Exfil); s != "" {
			add(hit{kind: KindExfil, sev: s, base: ConfMedium, signal: SigExfil,
				ev:      []Evidence{ev("fabric", "exfil_events", float64(f.ExfilEvents))},
				summary: fmt.Sprintf("%d large model-file read(s) followed by a connect to a non-internal address", f.ExfilEvents)})
		}
	}

	// ---- cgroup ----
	cg := in.Cgroup
	if cg == nil {
		cg = &CgroupWindow{Measured: map[string]bool{}}
		for _, sig := range AllCgroupSignals {
			setUnavail(sig, "no cgroup access")
		}
	}
	for _, u := range cg.Unavailable {
		setUnavail(u.Signal, u.Reason)
	}
	if cg.Measured[SigCPUStat] {
		metric(SigCPUStat, "cpu_throttle_ratio", cg.ThrottleRatio)
		d.Metrics["cpu_throttled_seconds"] = round(float64(cg.ThrottledUsec) / 1e6)
		if s := sevFor(cg.ThrottleRatio, cfg.CPUThrottleRatio); s != "" {
			add(hit{kind: KindCPU, sev: s, base: ConfHigh, marginal: marginal(cg.ThrottleRatio, cfg.CPUThrottleRatio), signal: SigCPUStat,
				ev: []Evidence{ev("cgroup", "cpu_throttle_ratio", cg.ThrottleRatio), ev("cgroup", "cpu_throttled_seconds", float64(cg.ThrottledUsec)/1e6)},
				summary: fmt.Sprintf("CFS quota throttled %.0f%% of periods in the worst container (%.1f s of throttled time in %s): the CPU limit is binding",
					cg.ThrottleRatio*100, float64(cg.ThrottledUsec)/1e6, winLabel)})
		}
	}
	if cg.Measured[SigCPUPressure] {
		metric(SigCPUPressure, "cpu_psi_some_avg10", cg.CPUPSI)
		if s := sevFor(cg.CPUPSI, cfg.CPUPressure); s != "" {
			add(hit{kind: KindCPU, sev: s, base: ConfMedium, marginal: marginal(cg.CPUPSI, cfg.CPUPressure), signal: SigCPUPressure,
				ev:      []Evidence{ev("cgroup", "cpu_psi_some_avg10", cg.CPUPSI)},
				summary: fmt.Sprintf("tasks were runnable but waiting for CPU up to %.0f%% of a 10 s interval (PSI)", cg.CPUPSI)})
		}
	}
	if cg.Measured[SigMemEvents] {
		metric(SigMemEvents, "mem_oom_kills", float64(cg.OOMKill))
		d.Metrics["mem_high_max_events"] = float64(cg.MemHighMax)
		if cg.OOMKill > 0 {
			add(hit{kind: KindMemory, sev: SevCritical, base: ConfHigh, signal: SigMemEvents,
				ev:      []Evidence{ev("cgroup", "mem_oom_kills", float64(cg.OOMKill))},
				summary: fmt.Sprintf("the kernel OOM-killed %d process(es) of this job in %s", cg.OOMKill, winLabel)})
		}
		if s := sevFor(float64(cg.MemHighMax), cfg.MemEvents); s != "" {
			add(hit{kind: KindMemory, sev: s, base: ConfHigh, signal: SigMemEvents,
				ev:      []Evidence{ev("cgroup", "mem_high_max_events", float64(cg.MemHighMax))},
				summary: fmt.Sprintf("memory.high/memory.max limit was hit %d time(s): allocations were throttled or failed", cg.MemHighMax)})
		}
	}
	if cg.Measured[SigMemUsage] {
		metric(SigMemUsage, "mem_usage_ratio", cg.MemUsageRatio)
		if s := sevFor(cg.MemUsageRatio, cfg.MemUsageRatio); s != "" {
			add(hit{kind: KindMemory, sev: s, base: ConfMedium, marginal: marginal(cg.MemUsageRatio, cfg.MemUsageRatio), signal: SigMemUsage,
				ev:      []Evidence{ev("cgroup", "mem_usage_ratio", cg.MemUsageRatio)},
				summary: fmt.Sprintf("memory use is %.0f%% of the limit of the fullest container", cg.MemUsageRatio*100)})
		}
	}
	if cg.Measured[SigMemPressure] {
		metric(SigMemPressure, "mem_psi_some_avg10", cg.MemPSI)
		if s := sevFor(cg.MemPSI, cfg.MemPressure); s != "" {
			add(hit{kind: KindMemory, sev: s, base: ConfMedium, marginal: marginal(cg.MemPSI, cfg.MemPressure), signal: SigMemPressure,
				ev:      []Evidence{ev("cgroup", "mem_psi_some_avg10", cg.MemPSI)},
				summary: fmt.Sprintf("tasks stalled on memory (reclaim/swap) up to %.0f%% of a 10 s interval (PSI)", cg.MemPSI)})
		}
	}
	if cg.Measured[SigIOPressure] {
		metric(SigIOPressure, "io_psi_some_avg10", cg.IOPSI)
		if s := sevFor(cg.IOPSI, cfg.IOPressure); s != "" {
			add(hit{kind: KindStorage, sev: s, base: ConfMedium, marginal: marginal(cg.IOPSI, cfg.IOPressure), signal: SigIOPressure,
				ev:      []Evidence{ev("cgroup", "io_psi_some_avg10", cg.IOPSI)},
				summary: fmt.Sprintf("tasks stalled on block I/O up to %.0f%% of a 10 s interval (PSI)", cg.IOPSI)})
		}
	}

	// ---- combine hits into one finding per kind ----
	byKind := map[string][]hit{}
	for _, h := range hits {
		byKind[h.kind] = append(byKind[h.kind], h)
	}
	for kind, hs := range byKind {
		fd := Finding{Kind: kind, Severity: SevInfo, Confidence: ConfLow, Evidence: []Evidence{}, WhatWasNotMeasured: []string{}}
		signals := map[string]bool{}
		conf := ConfLow
		var sums []string
		allMarginal := true
		for _, h := range hs {
			if SevRank(h.sev) > SevRank(fd.Severity) {
				fd.Severity = h.sev
			}
			if ConfRank(h.base) > ConfRank(conf) {
				conf = h.base
			}
			if !h.marginal {
				allMarginal = false
			}
			signals[h.signal] = true
			fd.Evidence = append(fd.Evidence, h.ev...)
			sums = append(sums, h.summary)
		}
		if len(signals) >= 2 {
			conf = up(conf) // independent signals agree
		}
		if allMarginal {
			conf = down(conf) // barely above the threshold
		}
		fd.Confidence = conf
		fd.Summary = strings.Join(sums, "; ")
		for _, sig := range kindSignals[kind] {
			if why, ok := unavail[sig]; ok {
				fd.WhatWasNotMeasured = append(fd.WhatWasNotMeasured, sig+": "+why)
			}
		}
		if c := kindCaveats[kind]; c != "" {
			fd.WhatWasNotMeasured = append(fd.WhatWasNotMeasured, c)
		}
		if in.EventsTruncated && hasFlightEvidence(fd.Evidence) {
			fd.WhatWasNotMeasured = append(fd.WhatWasNotMeasured, "older flight events in the window were evicted by the bounded ring; event counts are lower bounds")
		}
		d.Findings = append(d.Findings, fd)
	}
	sort.SliceStable(d.Findings, func(i, j int) bool {
		a, b := d.Findings[i], d.Findings[j]
		if SevRank(a.Severity) != SevRank(b.Severity) {
			return SevRank(a.Severity) > SevRank(b.Severity)
		}
		if ConfRank(a.Confidence) != ConfRank(b.Confidence) {
			return ConfRank(a.Confidence) > ConfRank(b.Confidence)
		}
		return a.Kind < b.Kind
	})

	for sig := range measured {
		d.Measured = append(d.Measured, sig)
	}
	sort.Strings(d.Measured)
	for sig, why := range unavail {
		if !measured[sig] {
			d.Unavailable = append(d.Unavailable, Unavailable{sig, why})
		}
	}
	sort.Slice(d.Unavailable, func(i, j int) bool { return d.Unavailable[i].Signal < d.Unavailable[j].Signal })
	d.Completeness = completeness(in, d, attached, skipReason)
	d.Summary = summarize(d)
	return d
}

func hasFlightEvidence(evs []Evidence) bool {
	for _, e := range evs {
		if e.Source == "flight" {
			return true
		}
	}
	return false
}

func round(v float64) float64 { return math.Round(v*1000) / 1000 }

func summarize(d Diagnosis) string {
	switch {
	case len(d.Findings) > 0:
		f := d.Findings[0]
		return fmt.Sprintf("%d finding(s); most significant: %s (%s, %s confidence)", len(d.Findings), f.Kind, f.Severity, f.Confidence)
	case len(d.Measured) == 0:
		return "nothing could be measured; no conclusion is possible (unavailable is not healthy)"
	default:
		s := "no bottleneck detected in measured signals: " + strings.Join(d.Measured, ", ")
		if len(d.Unavailable) > 0 {
			s += fmt.Sprintf(" (%d signal(s) unavailable, which is not evidence of health)", len(d.Unavailable))
		}
		return s
	}
}

func completeness(in Input, d Diagnosis, attached map[string]bool, skipReason map[string]string) Completeness {
	c := Completeness{
		ProbesSkipped: []ProbeStatus{}, MissingNodes: []string{}, Reasons: []string{},
		DroppedEvents: DroppedEvents{Counts: []DropCount{}},
		Sampling: Sampling{Ratio: 1, Note: "no probe samples events: every event that reaches a ring or perf buffer is decoded; losses are counted under droppedEvents",
			Filters: []string{
				"overlap: cudaDeviceSynchronize spans under 1 ms are not emitted",
				"datapipe_bottleneck: GPU idle gaps under 1 ms are not emitted",
				"tcp_trace: retransmits are counted when a connection closes",
			}},
		NodesExpected:  Count{},
		NodesReporting: 1,
	}
	attachedObjs := map[string]bool{}
	for _, p := range in.Probes {
		if p.Attached {
			c.ProbesAttached++
			attachedObjs[p.Object] = true
		} else if len(c.ProbesSkipped) < 100 {
			c.ProbesSkipped = append(c.ProbesSkipped, p)
		}
	}
	for _, dc := range in.Drops {
		c.DroppedEvents.Total += dc.Count
		c.DroppedEvents.Counts = append(c.DroppedEvents.Counts, dc)
	}
	c.DroppedEvents.NotCounted = append([]string(nil), in.DropsNotCounted...)
	if in.Probes == nil {
		c.Reasons = append(c.Reasons, "eBPF probe status unknown")
	} else {
		var missing []string
		for obj := range relevantObjects {
			if !attachedObjs[obj] {
				r := skipReason[obj]
				if r == "" {
					r = "not loaded"
				}
				missing = append(missing, obj+" ("+r+")")
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			c.Reasons = append(c.Reasons, "diagnosis probes not attached: "+strings.Join(missing, "; "))
		}
	}
	if c.DroppedEvents.Total > 0 {
		c.Reasons = append(c.Reasons, fmt.Sprintf("%d event(s) were dropped before analysis (ring or perf buffer full, or a consumer behind); counts are lower bounds", c.DroppedEvents.Total))
	}
	if len(c.DroppedEvents.NotCounted) > 0 {
		c.Reasons = append(c.Reasons, "ring losses are not counted for: "+strings.Join(c.DroppedEvents.NotCounted, ", "))
	}
	if in.EventsTruncated {
		c.Reasons = append(c.Reasons, "the flight ring evicted events still inside the window")
	}
	if len(d.Unavailable) > 0 {
		names := make([]string, 0, len(d.Unavailable))
		for _, u := range d.Unavailable {
			names = append(names, u.Signal)
		}
		c.Reasons = append(c.Reasons, fmt.Sprintf("%d signal(s) unavailable: %s", len(names), strings.Join(names, ", ")))
	}
	c.Reasons = append(c.Reasons, "cluster coverage is not evaluated in a node-local report (nodesExpected unknown); the gateway computes it from the job's pods")
	c.Complete = len(c.Reasons) == 0
	return c
}
