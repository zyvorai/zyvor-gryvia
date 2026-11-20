// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package dcgm scrapes a node's dcgm-exporter (Prometheus text exposition),
// keeps a short rolling window per GPU and correlates it with NCCL collective
// windows: how much of the communication time the GPU was idle.
//
// UNVERIFIED ON HARDWARE: everything here is tested against canned exposition
// text in the shape dcgm-exporter documents; no GPU or DCGM was available.
//
// What this is NOT: CUPTI activity tracing. It does not see kernels, streams
// or NCCL device time; it sees DCGM's sampled counters (SM_ACTIVE etc.), whose
// resolution is the exporter's collect interval (default 30 s; run
// dcgm-exporter with --collect-interval 1000 or lower for this to mean
// anything at NCCL-call scale). Correlation reports its coverage so a coarse
// interval shows up as low coverage instead of a made-up ratio.
package dcgm

import (
	"bufio"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DCGM field names read from the exporter.
const (
	FieldGPUUtil    = "DCGM_FI_DEV_GPU_UTIL"            // percent 0-100
	FieldSMActive   = "DCGM_FI_PROF_SM_ACTIVE"          // ratio 0-1
	FieldTensor     = "DCGM_FI_PROF_PIPE_TENSOR_ACTIVE" // ratio 0-1
	FieldDRAMActive = "DCGM_FI_PROF_DRAM_ACTIVE"        // ratio 0-1
	FieldNVLinkTX   = "DCGM_FI_PROF_NVLINK_TX_BYTES"    // bytes
	FieldNVLinkRX   = "DCGM_FI_PROF_NVLINK_RX_BYTES"
	FieldPCIeTX     = "DCGM_FI_PROF_PCIE_TX_BYTES"
	FieldPCIeRX     = "DCGM_FI_PROF_PCIE_RX_BYTES"
)

var wantedFields = map[string]bool{
	FieldGPUUtil: true, FieldSMActive: true, FieldTensor: true, FieldDRAMActive: true,
	FieldNVLinkTX: true, FieldNVLinkRX: true, FieldPCIeTX: true, FieldPCIeRX: true,
}

// Metric is one parsed exposition line.
type Metric struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Parse reads Prometheus text exposition. Comments, blank lines and lines it
// cannot parse are skipped; timestamps are ignored. Only lines whose name is a
// DCGM field this package uses are kept (a full exporter page is large).
func Parse(text string) []Metric {
	var out []Metric
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, rest, ok := splitLine(line)
		if !ok || !wantedFields[name] {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out = append(out, Metric{Name: name, Labels: labels, Value: v})
	}
	return out
}

func splitLine(line string) (name string, labels map[string]string, rest string, ok bool) {
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 {
		return "", nil, "", false
	}
	name = line[:i]
	if line[i] != '{' {
		return name, map[string]string{}, line[i:], true
	}
	labels = map[string]string{}
	p := i + 1
	for {
		for p < len(line) && (line[p] == ' ' || line[p] == ',') {
			p++
		}
		if p >= len(line) {
			return "", nil, "", false
		}
		if line[p] == '}' {
			return name, labels, line[p+1:], true
		}
		eq := strings.IndexByte(line[p:], '=')
		if eq < 0 || p+eq+1 >= len(line) || line[p+eq+1] != '"' {
			return "", nil, "", false
		}
		key := strings.TrimSpace(line[p : p+eq])
		p += eq + 2
		var val strings.Builder
		closed := false
		for p < len(line) {
			c := line[p]
			if c == '\\' && p+1 < len(line) {
				switch line[p+1] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(line[p+1])
				}
				p += 2
				continue
			}
			if c == '"' {
				closed = true
				p++
				break
			}
			val.WriteByte(c)
			p++
		}
		if !closed {
			return "", nil, "", false
		}
		labels[key] = val.String()
	}
}

// GPUSample is one scrape's values for one GPU.
type GPUSample struct {
	At        time.Time
	GPU       string // "gpu" label (index), else UUID, else ""
	Namespace string // dcgm-exporter Kubernetes mapping labels; empty when not mapped
	Pod       string
	Values    map[string]float64
}

// Normalized values: SMActive/Tensor/DRAM as 0..1, GPUUtil percent scaled to 0..1.
func (s GPUSample) SMActive() (float64, bool) {
	if v, ok := s.Values[FieldSMActive]; ok {
		return clamp01(v), true
	}
	return 0, false
}

// Activity is the value used as "GPU busy": SM_ACTIVE when exported, else
// GPU_UTIL/100 (coarser: it only says a kernel was resident, not how many SMs).
func (s GPUSample) Activity() (v float64, source string, ok bool) {
	if v, ok := s.SMActive(); ok {
		return v, FieldSMActive, true
	}
	if u, ok := s.Values[FieldGPUUtil]; ok {
		return clamp01(u / 100), FieldGPUUtil, true
	}
	return 0, "", false
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// Samples groups the parsed metrics of one scrape (taken at `at`) per GPU.
func Samples(at time.Time, ms []Metric) []GPUSample {
	type key struct{ gpu, ns, pod string }
	idx := map[key]*GPUSample{}
	var order []key
	for _, m := range ms {
		gpu := m.Labels["gpu"]
		if gpu == "" {
			gpu = m.Labels["UUID"]
		}
		k := key{gpu, m.Labels["namespace"], m.Labels["pod"]}
		s := idx[k]
		if s == nil {
			s = &GPUSample{At: at, GPU: gpu, Namespace: k.ns, Pod: k.pod, Values: map[string]float64{}}
			idx[k] = s
			order = append(order, k)
		}
		s.Values[m.Name] = m.Value
	}
	out := make([]GPUSample, 0, len(order))
	for _, k := range order {
		out = append(out, *idx[k])
	}
	return out
}

// Store is a bounded rolling window of samples (all GPUs of the node).
type Store struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	list   []GPUSample
}

// NewStore keeps samples for window (default 5 minutes) and at most max (default 20000).
func NewStore(window time.Duration, max int) *Store {
	if window <= 0 {
		window = 5 * time.Minute
	}
	if max <= 0 {
		max = 20000
	}
	return &Store{window: window, max: max}
}

// Add appends samples and drops those older than the window relative to `now`.
func (s *Store) Add(now time.Time, in []GPUSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, in...)
	cut := now.Add(-s.window)
	i := sort.Search(len(s.list), func(i int) bool { return !s.list[i].At.Before(cut) })
	s.list = s.list[i:]
	if len(s.list) > s.max {
		s.list = append([]GPUSample(nil), s.list[len(s.list)-s.max:]...)
	}
}

// Since returns copies of the samples with At >= from for which keep is true
// (nil keeps all).
func (s *Store) Since(from time.Time, keep func(GPUSample) bool) []GPUSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []GPUSample
	for _, x := range s.list {
		if x.At.Before(from) || (keep != nil && !keep(x)) {
			continue
		}
		out = append(out, x)
	}
	return out
}

// Len is the number of samples held.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.list)
}
