// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/dcgm"
)

const (
	// DefaultWindow is how long a signal contributes to a job's status.
	DefaultWindow = 5 * time.Minute
	// maxSamples bounds the signals kept per job.
	maxSamples = 4096
	// maxCollSamples bounds the collective spans kept per job.
	maxCollSamples = 16384
)

// Score-penalty thresholds and weights (see ScoreDelta).
const (
	p99SlowMS       = 50.0
	retryRateHigh   = 0.02
	gdsHitLow       = 0.5
	stragglerHitsHi = 10
	overlapIdleHigh = 0.3  // fraction of the window spent in sync inside a collective
	cnpRateHigh     = 100  // RoCEv2 CNPs per second, averaged over the window
	pfcRateHigh     = 1000 // PFC pause frames per second, averaged over the window

	penaltyP99       = 0.25
	penaltyRDMA      = 0.35
	penaltyGDS       = 0.15
	penaltyStraggler = 0.25
	penaltyOverlap   = 0.15
	penaltyCNP       = 0.15
	penaltyPFC       = 0.10
)

// JobKey identifies the job a signal belongs to. It is filled from Bind (a
// pid -> pod -> job resolver); signals from unbound pids are grouped under
// the "_unattributed" namespace by process name.
type JobKey struct {
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
}

// Status is the folded view of one job's recent signals; it carries the
// fields of GryviaFabricSignal.status.
type Status struct {
	StragglerRank uint32  `json:"stragglerRank"`
	StragglerPID  uint32  `json:"stragglerPid"`
	StragglerHits uint64  `json:"stragglerHits"`
	NCCLP99MS     float64 `json:"ncclP99ms"`
	RDMARetryRate float64 `json:"rdmaRetryRate"`
	GDSHitRatio   float64 `json:"gdsHitRatio"`
	GDSMeasured   bool    `json:"gdsMeasured"`
	// OverlapIdleRatio is the time spent in cudaDeviceSynchronize nested inside
	// ncclAllReduce (GPU idle while communication is in flight), as a fraction
	// of the window, summed over threads and capped at 1.
	OverlapIdleRatio float64 `json:"overlapIdleRatio"`
	// CNPRate is RoCEv2 congestion notification packets per second averaged
	// over the window (node-level, reported under job _node/cnp).
	CNPRate float64 `json:"cnpRate"`
	// InferWaitP99MS is the p99 NETWORK accept -> first recv wait on inference
	// ports (infer_latency.c). It is not engine queue time, time to first token
	// or inter-token latency; those come from the engine's own metrics (see
	// Inference below). Informational: it does not feed ScoreDelta.
	InferWaitP99MS float64 `json:"inferWaitP99ms"`
	// Inference holds the serving engine's own latency figures. It is nil unless
	// the opt-in metrics scraper (-infer-metrics) delivered a fresh reading; a nil
	// field inside it means "not measured", never zero.
	Inference *Inference `json:"inference,omitempty"`
	// PFCRate is 802.1Qbb priority-flow-control pause frames per second
	// averaged over the window (node-level, reported under job _node/pfc).
	PFCRate float64 `json:"pfcRate"`
	// ExfilEvents counts weight_exfil signals (a large model-file read followed
	// by a connect to a non-internal address) in the window. Informational: it
	// never changes ScoreDelta.
	ExfilEvents uint64 `json:"exfilEvents"`
	// UCXSlowP99MS is the p99 duration of UCX tag-send calls that blocked for
	// at least the probe threshold. Informational: it does not feed ScoreDelta.
	UCXSlowP99MS float64 `json:"ucxSlowP99ms"`
	// InferTTFTP99MS is the p99 of accept -> first send on inference ports
	// (infer_ttft.c, opt-in). Informational.
	InferTTFTP99MS float64 `json:"inferTTFTP99ms"`
	// Events counts the opt-in probe signals in the window, keyed by EventName
	// (for example "gpu_oom"). Informational: it never changes ScoreDelta.
	Events map[string]uint64 `json:"events,omitempty"`

	// CollectivesCompared is the number of collectives (same communicator
	// ordinal, sequence and op) seen on at least two local ranks, and
	// CollectiveMaxSkewMS the largest slowest-minus-fastest host-side duration
	// among them. Both need ebpf/straggler.c with the identity probes attached
	// and >= 2 ranks of the job on this node; the cross-node comparison lives
	// in the gateway (see docs/nccl-rdma-gpu-correlation.md). Informational.
	CollectivesCompared uint64  `json:"collectivesCompared"`
	CollectiveMaxSkewMS float64 `json:"collectiveMaxSkewMs"`

	// NICRetryRate / NICErrorRate are RDMA NIC hardware counter events per
	// second (transport retries / sequence errors; link, symbol and discard
	// errors), reported under job _node/nic when -nic-counters is on.
	// Informational: they never change ScoreDelta.
	NICRetryRate float64 `json:"nicRetryRate"`
	NICErrorRate float64 `json:"nicErrorRate"`

	// GPU correlation (opt-in, -dcgm-url; UNVERIFIED on hardware). Measured is
	// false unless DCGM samples overlapped the job's collective windows.
	// GPUIdleDuringCommRatio is the fraction of covered communication time with
	// SM_ACTIVE (else GPU_UTIL) below the idle threshold; SMActiveDuringCompute
	// the mean SM activity outside the comm windows (ComputeMeasured says
	// whether there was any); GPUCorrelationCoverage the fraction of comm time
	// that had a DCGM sample. The comm windows are host-side NCCL call windows,
	// not on-GPU collective time. Informational: never changes ScoreDelta.
	GPUCorrelationMeasured bool    `json:"gpuCorrelationMeasured"`
	GPUIdleDuringCommRatio float64 `json:"gpuIdleDuringCommRatio"`
	SMActiveDuringComm     float64 `json:"smActiveDuringComm"`
	SMActiveDuringCompute  float64 `json:"smActiveDuringCompute"`
	GPUComputeMeasured     bool    `json:"gpuComputeMeasured"`
	GPUCorrelationCoverage float64 `json:"gpuCorrelationCoverage"`

	// SampleCount is the number of signals folded into this status (the weight the ai-operator
	// gives this node when it averages ratios across a job's nodes).
	SampleCount int `json:"sampleCount"`

	ScoreDelta float64   `json:"scoreDelta"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type sample struct {
	at  time.Time
	sig Signal
}

// Folder aggregates signals per job over a sliding window.
type Folder struct {
	window time.Duration
	now    func() time.Time

	mu        sync.Mutex
	jobs      map[JobKey][]sample
	colls     map[JobKey][]sample // SigCollective samples, kept apart so they cannot evict other signals
	pidToJob  map[uint32]JobKey
	gdsDirect bool
	inf       map[JobKey]map[string]infSample // job -> scrape target -> latest engine reading

	gpuSrc       func(k JobKey, from time.Time) []dcgm.GPUSample
	gpuThreshold float64
}

// NewFolder creates a Folder. A non-positive window means DefaultWindow.
func NewFolder(window time.Duration) *Folder {
	if window <= 0 {
		window = DefaultWindow
	}
	return &Folder{
		window:   window,
		now:      time.Now,
		jobs:     map[JobKey][]sample{},
		colls:    map[JobKey][]sample{},
		pidToJob: map[uint32]JobKey{},
	}
}

// SetGDSDirectVisible tells the folder whether the nvidia-fs kernel hook is
// attached. Without it every cuFile call is "bounce or unknown", so the direct
// ratio is meaningless and is neither reported nor penalised.
func (f *Folder) SetGDSDirectVisible(v bool) {
	f.mu.Lock()
	f.gdsDirect = v
	f.mu.Unlock()
}

// Bind attributes a pid to a job (called by the cgroup -> pod resolver).
func (f *Folder) Bind(pid uint32, ns, job string) {
	f.mu.Lock()
	f.pidToJob[pid] = JobKey{Namespace: ns, Job: job}
	f.mu.Unlock()
}

// Unbind forgets a pid.
func (f *Folder) Unbind(pid uint32) {
	f.mu.Lock()
	delete(f.pidToJob, pid)
	f.mu.Unlock()
}

// CNPJob is the node-level key under which CNP counts are folded: congestion
// notifications belong to the NIC, not to a pod.
var CNPJob = JobKey{Namespace: "_node", Job: "cnp"}

// PFCJob is the node-level key under which PFC pause frames are folded.
var PFCJob = JobKey{Namespace: "_node", Job: "pfc"}

// AddPFC records n 802.1Qbb PFC pause frames counted since the previous call
// (the collector polls pfc_pause's pause_count map). Zero is ignored.
func (f *Folder) AddPFC(n uint64) {
	if n == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addLocked(PFCJob, Signal{Type: SigPFC, Bytes: n, Comm: "pfc"})
}

// AddCNP records n RoCEv2 congestion notification packets counted since the
// previous call (the collector polls roce_cnp's cnp_count map). Zero is ignored.
func (f *Folder) AddCNP(n uint64) {
	if n == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addLocked(CNPJob, Signal{Type: SigCNP, Bytes: n, Comm: "cnp"})
}

// Add records one signal.
func (f *Folder) Add(s Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key, ok := f.pidToJob[s.PID]
	if !ok {
		key = JobKey{Namespace: "_unattributed", Job: s.Comm}
	}
	f.addLocked(key, s)
}

func (f *Folder) addLocked(key JobKey, s Signal) {
	m, limit := f.jobs, maxSamples
	if s.Type == SigCollective {
		m, limit = f.colls, maxCollSamples
	}
	list := append(m[key], sample{at: f.now(), sig: s})
	if len(list) > limit {
		list = append(list[:0], list[len(list)-limit:]...)
	}
	m[key] = list
}

// Snapshot returns the current status of every job that has signals inside
// the window. Expired signals (and jobs left with none) are dropped.
func (f *Folder) Snapshot() map[JobKey]Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	cutoff := now.Add(-f.window)
	prune := func(m map[JobKey][]sample, k JobKey) []sample {
		list := m[k]
		i := sort.Search(len(list), func(i int) bool { return !list[i].at.Before(cutoff) })
		list = list[i:]
		if len(list) == 0 {
			delete(m, k)
			return nil
		}
		m[k] = list
		return list
	}
	keys := make(map[JobKey]bool, len(f.jobs)+len(f.colls))
	for k := range f.jobs {
		keys[k] = true
	}
	for k := range f.colls {
		keys[k] = true
	}
	out := make(map[JobKey]Status, len(keys))
	for k := range keys {
		list, colls := prune(f.jobs, k), prune(f.colls, k)
		if len(list) == 0 && len(colls) == 0 {
			continue
		}
		st := fold(list, colls, f.gdsDirect, f.window)
		if f.gpuSrc != nil && len(colls) > 0 {
			f.correlateGPU(&st, k, colls, cutoff)
		}
		st.ScoreDelta = ScoreDelta(st)
		out[k] = st
	}
	f.foldInferenceLocked(out, cutoff)
	return out
}

// SetGPUSource enables the DCGM correlation: src returns the GPU samples that
// belong to a job since `from` (nil when unattributable); threshold <= 0 means
// dcgm.DefaultIdleThreshold. Not calling it leaves the correlation fields unset.
func (f *Folder) SetGPUSource(src func(k JobKey, from time.Time) []dcgm.GPUSample, threshold float64) {
	f.mu.Lock()
	f.gpuSrc, f.gpuThreshold = src, threshold
	f.mu.Unlock()
}

// correlateGPU fills the GPU fields from the job's collective call windows
// (host-side [end - duration, end] of every local rank's calls) and the job's
// DCGM samples. Called with f.mu held.
func (f *Folder) correlateGPU(st *Status, k JobKey, colls []sample, from time.Time) {
	windows := make([]dcgm.Window, 0, len(colls))
	for _, c := range colls {
		d := time.Duration(c.sig.LatencyNS)
		windows = append(windows, dcgm.Window{Start: c.at.Add(-d), End: c.at})
	}
	res := dcgm.Correlate(windows, f.gpuSrc(k, from.Add(-2*time.Second)), f.gpuThreshold)
	st.GPUCorrelationCoverage = res.Coverage
	if !res.Measured {
		return
	}
	st.GPUCorrelationMeasured = true
	st.GPUIdleDuringCommRatio = res.IdleDuringComm
	st.SMActiveDuringComm = res.ActiveDuringComm
	st.GPUComputeMeasured = res.ComputeMeasured
	st.SMActiveDuringCompute = res.ActiveDuringCompute
}

func fold(list, colls []sample, gdsDirect bool, window time.Duration) Status {
	var st Status
	st.SampleCount = len(list) + len(colls)
	var lat, inferLat, ucxLat, ttftLat []float64
	var errs, posted, direct, total, overlapNS, cnps, pfcs uint64
	var nicRetry, nicErr, nicCNP, nicPause uint64
	var nicCNPSeen, nicPauseSeen bool
	for _, sm := range list {
		s := sm.sig
		if sm.at.After(st.UpdatedAt) {
			st.UpdatedAt = sm.at
		}
		switch s.Type {
		case SigStraggler:
			st.StragglerHits++
			st.StragglerRank = s.Rank
			st.StragglerPID = s.PID
			lat = append(lat, float64(s.LatencyNS)/1e6)
		case SigRDMARetry:
			errs += uint64(s.Retries) + uint64(s.RNR)
			posted += uint64(s.WorldSize)
		case SigGDS:
			total += s.Bytes
			if s.Retries == 1 {
				direct += s.Bytes
			}
		case SigOverlap:
			overlapNS += s.LatencyNS
		case SigInferWait:
			inferLat = append(inferLat, float64(s.LatencyNS)/1e6)
		case SigCNP:
			cnps += s.Bytes
		case SigPFC:
			pfcs += s.Bytes
		case SigExfil:
			st.ExfilEvents++
		case SigUCXSlow:
			ucxLat = append(ucxLat, float64(s.LatencyNS)/1e6)
		case SigInferTTFT:
			ttftLat = append(ttftLat, float64(s.LatencyNS)/1e6)
			st.countEvent(s.Type)
		case SigNCCLTransport, SigP2PFallback, SigGPUOOM, SigGraphStall, SigGDRFail,
			SigInferGap, SigWeightMmap, SigGPUDev, SigUCXWait:
			st.countEvent(s.Type)
		case SigNICRetry:
			nicRetry += s.Bytes
		case SigNICError:
			nicErr += s.Bytes
		case SigNICCNP:
			nicCNPSeen = true
			nicCNP += s.Bytes
		case SigNICPause:
			nicPauseSeen = true
			nicPause += s.Bytes
		}
	}
	// NIC hardware counters, when the collector reads them, are authoritative
	// for CNP and pause frames: the XDP counters see a subset (pause frames are
	// usually consumed by the MAC and never reach XDP) and adding both would
	// count the same packets twice. XDP samples are used only without NIC ones.
	if nicCNPSeen {
		cnps = nicCNP
	}
	if nicPauseSeen {
		pfcs = nicPause
	}
	if t := lastAt(colls); t.After(st.UpdatedAt) {
		st.UpdatedAt = t
	}
	for _, g := range MatchCollectives(spansOf(colls)) {
		if !g.Comparable() {
			continue
		}
		st.CollectivesCompared++
		if ms := float64(g.SkewNS) / 1e6; ms > st.CollectiveMaxSkewMS {
			st.CollectiveMaxSkewMS = ms
		}
		if straggling(g) {
			st.StragglerHits++
			st.StragglerRank = g.SlowestRank
			for _, r := range g.Ranks {
				if r.Rank == g.SlowestRank {
					st.StragglerPID = r.PID
					lat = append(lat, float64(r.DurationNS)/1e6)
				}
			}
		}
	}
	st.NCCLP99MS = percentile(lat, 0.99)
	if errs > 0 {
		if posted == 0 {
			st.RDMARetryRate = 1
		} else {
			st.RDMARetryRate = math.Min(1, float64(errs)/float64(posted))
		}
	}
	if gdsDirect && total > 0 {
		st.GDSMeasured = true
		st.GDSHitRatio = float64(direct) / float64(total)
	}
	st.InferWaitP99MS = percentile(inferLat, 0.99)
	st.UCXSlowP99MS = percentile(ucxLat, 0.99)
	st.InferTTFTP99MS = percentile(ttftLat, 0.99)
	if secs := window.Seconds(); secs > 0 {
		st.OverlapIdleRatio = math.Min(1, float64(overlapNS)/1e9/secs)
		st.CNPRate = float64(cnps) / secs
		st.PFCRate = float64(pfcs) / secs
		st.NICRetryRate = float64(nicRetry) / secs
		st.NICErrorRate = float64(nicErr) / secs
	}
	st.ScoreDelta = ScoreDelta(st)
	return st
}

func (st *Status) countEvent(t uint8) {
	if st.Events == nil {
		st.Events = make(map[string]uint64)
	}
	st.Events[EventName(t)]++
}

func spansOf(colls []sample) []CollSpan {
	out := make([]CollSpan, 0, len(colls))
	for _, c := range colls {
		out = append(out, spanOf(c.sig))
	}
	return out
}

func lastAt(list []sample) time.Time {
	var t time.Time
	for _, s := range list {
		if s.at.After(t) {
			t = s.at
		}
	}
	return t
}

// percentile returns the nearest-rank percentile of v (0 for empty input).
func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	idx := int(math.Ceil(p*float64(len(v)))) - 1
	if idx < 0 {
		idx = 0
	}
	return v[idx]
}

// ScoreDelta is a penalty in [0,1] the topology scorer can subtract. ExfilEvents,
// InferWaitP99MS and UCXSlowP99MS are informational and never contribute.
// 0 = healthy fabric, 1 = do not place another gang here.
func ScoreDelta(st Status) float64 {
	d := 0.0
	if st.NCCLP99MS > p99SlowMS {
		d += penaltyP99
	}
	if st.RDMARetryRate > retryRateHigh {
		d += penaltyRDMA
	}
	if st.GDSMeasured && st.GDSHitRatio < gdsHitLow {
		d += penaltyGDS
	}
	if st.StragglerHits > stragglerHitsHi {
		d += penaltyStraggler
	}
	if st.OverlapIdleRatio > overlapIdleHigh {
		d += penaltyOverlap
	}
	if st.CNPRate > cnpRateHigh {
		d += penaltyCNP
	}
	if st.PFCRate > pfcRateHigh {
		d += penaltyPFC
	}
	return math.Max(0, math.Min(d, 1))
}

// JobStatus is one entry of the HTTP response.
type JobStatus struct {
	JobKey
	Status
}

// List returns the snapshot as a stable-ordered slice.
func (f *Folder) List() []JobStatus {
	snap := f.Snapshot()
	out := make([]JobStatus, 0, len(snap))
	for k, st := range snap {
		out = append(out, JobStatus{JobKey: k, Status: st})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Job < out[j].Job
	})
	return out
}

// ServeHTTP serves GET /api/v1/fabric: per-job fabric status and score.
func (f *Folder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(f.List()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
