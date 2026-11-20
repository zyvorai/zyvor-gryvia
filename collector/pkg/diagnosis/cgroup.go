// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package diagnosis

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/fabric"
	"github.com/zyvorai/gryvia/collector/pkg/flight"
)

// Bounds on the sampler's memory.
const (
	maxSamplesPerJob = 90 // 15 minutes at the default 10 s interval
	maxSampledJobs   = 128
	maxCgroupFile    = 64 << 10
)

// Cgroup signal names (used in Unavailable and Measured).
const (
	SigCPUStat     = "cgroup.cpu.stat"
	SigCPUPressure = "cgroup.cpu.pressure"
	SigMemEvents   = "cgroup.memory.events"
	SigMemUsage    = "cgroup.memory.current+max"
	SigMemPressure = "cgroup.memory.pressure"
	SigIOPressure  = "cgroup.io.pressure"
)

// AllCgroupSignals lists every signal the sampler tries to read.
var AllCgroupSignals = []string{SigCPUStat, SigCPUPressure, SigMemEvents, SigMemUsage, SigMemPressure, SigIOPressure}

// PSI is one pressure-stall line ("some" or "full").
type PSI struct {
	Avg10 float64 // percent of the last 10 s that tasks were stalled
	Total uint64  // microseconds, monotonic
}

// groupRead is what is read from one cgroup directory (a container's, or the
// pod's own). Pointers/flags say which files existed.
type groupRead struct {
	Name string

	HasCPUStat                        bool
	NrPeriods, NrThrottled            uint64
	ThrottledUsec                     uint64
	HasMemEvents                      bool
	OOM, OOMKill, High, MaxEvents     uint64
	HasMemUsage                       bool
	MemCurrent                        uint64
	MemMax                            uint64 // 0 with MemLimited false = "max" (unlimited)
	MemLimited                        bool
	CPUSome, MemSome, IOSome          PSI
	HasCPUPSI, HasMemPSI, HasIOPSI    bool
	CPUFull, MemFull, IOFull          PSI
	HasCPUFull, HasMemFull, HasIOFull bool
}

// podRead is one pod's cgroup tree at one instant.
type podRead struct {
	Pod    string
	Groups []groupRead // Groups[0] is the pod cgroup itself
	// Missing maps a signal to why it could not be read from every group.
	Missing map[string]string
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCgroupFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxCgroupFile {
		return nil, errors.New("file larger than expected")
	}
	return b, nil
}

// parseKV parses "key value" lines (cpu.stat, memory.events).
func parseKV(b []byte) map[string]uint64 {
	out := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			out[f[0]] = v
		}
	}
	return out
}

// parsePSI parses a pressure file. It returns the "some" and "full" lines when
// present; a kernel older than 5.13 has no "full" line for cpu.pressure.
func parsePSI(b []byte) (some, full PSI, hasSome, hasFull bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || (f[0] != "some" && f[0] != "full") {
			continue
		}
		var p PSI
		ok := false
		for _, kv := range f[1:] {
			k, v, found := strings.Cut(kv, "=")
			if !found {
				continue
			}
			switch k {
			case "avg10":
				if x, err := strconv.ParseFloat(v, 64); err == nil && !math.IsNaN(x) && !math.IsInf(x, 0) && x >= 0 {
					p.Avg10, ok = x, true
				}
			case "total":
				if x, err := strconv.ParseUint(v, 10, 64); err == nil {
					p.Total = x
				}
			}
		}
		if !ok {
			continue
		}
		if f[0] == "some" {
			some, hasSome = p, true
		} else {
			full, hasFull = p, true
		}
	}
	return
}

// readGroup reads the files of one cgroup directory. Read errors are recorded per
// signal in missing (first error wins) and never abort the read.
func readGroup(dir, name string, missing map[string]string) groupRead {
	g := groupRead{Name: name}
	note := func(sig string, err error) {
		if _, dup := missing[sig]; !dup {
			reason := "cannot read"
			switch {
			case errors.Is(err, os.ErrNotExist):
				reason = "file not present (controller not enabled for this cgroup, or PSI disabled)"
			case errors.Is(err, os.ErrPermission):
				reason = "permission denied"
			default:
				reason = err.Error()
			}
			missing[sig] = reason
		}
	}
	if b, err := readSmall(filepath.Join(dir, "cpu.stat")); err != nil {
		note(SigCPUStat, err)
	} else if kv := parseKV(b); kv["nr_periods"] > 0 || hasKey(kv, "nr_periods") {
		g.HasCPUStat = true
		g.NrPeriods, g.NrThrottled, g.ThrottledUsec = kv["nr_periods"], kv["nr_throttled"], kv["throttled_usec"]
	} else {
		note(SigCPUStat, errors.New("cpu.stat has no CFS bandwidth counters (nr_periods)"))
	}
	if b, err := readSmall(filepath.Join(dir, "memory.events")); err != nil {
		note(SigMemEvents, err)
	} else {
		kv := parseKV(b)
		g.HasMemEvents = true
		g.OOM, g.OOMKill, g.High, g.MaxEvents = kv["oom"], kv["oom_kill"], kv["high"], kv["max"]
	}
	cur, errCur := readSmall(filepath.Join(dir, "memory.current"))
	lim, errLim := readSmall(filepath.Join(dir, "memory.max"))
	switch {
	case errCur != nil:
		note(SigMemUsage, errCur)
	case errLim != nil:
		note(SigMemUsage, errLim)
	default:
		c, err1 := strconv.ParseUint(strings.TrimSpace(string(cur)), 10, 64)
		if err1 != nil {
			note(SigMemUsage, errors.New("memory.current is not a number"))
			break
		}
		g.HasMemUsage, g.MemCurrent = true, c
		if l := strings.TrimSpace(string(lim)); l != "max" {
			if m, err := strconv.ParseUint(l, 10, 64); err == nil && m > 0 {
				g.MemMax, g.MemLimited = m, true
			}
		}
	}
	for _, p := range []struct {
		file, sig  string
		some, full *PSI
		hs, hf     *bool
	}{
		{"cpu.pressure", SigCPUPressure, &g.CPUSome, &g.CPUFull, &g.HasCPUPSI, &g.HasCPUFull},
		{"memory.pressure", SigMemPressure, &g.MemSome, &g.MemFull, &g.HasMemPSI, &g.HasMemFull},
		{"io.pressure", SigIOPressure, &g.IOSome, &g.IOFull, &g.HasIOPSI, &g.HasIOFull},
	} {
		b, err := readSmall(filepath.Join(dir, p.file))
		if err != nil {
			note(p.sig, err)
			continue
		}
		s, f, hs, hf := parsePSI(b)
		if !hs {
			note(p.sig, errors.New("no PSI 'some' line"))
			continue
		}
		*p.some, *p.full, *p.hs, *p.hf = s, f, hs, hf
	}
	return g
}

func hasKey(m map[string]uint64, k string) bool { _, ok := m[k]; return ok }

// DirResolver finds a pod's cgroup directories (pod first, then containers).
type DirResolver interface {
	Dirs(p fabric.PodRef) ([]string, error)
}

// readPod reads the pod's cgroup tree. Signals missing from every group are
// reported in Missing; a signal present in at least one group counts as read.
func readPod(r DirResolver, pod flight.PodInfo) (podRead, error) {
	if pod.QOS == "" {
		return podRead{}, errors.New("pod QoS class unknown (needed to locate its cgroup)")
	}
	dirs, err := r.Dirs(fabric.PodRef{Namespace: pod.Namespace, Name: pod.Pod, UID: pod.UID, QOS: pod.QOS})
	if err != nil {
		return podRead{}, err
	}
	out := podRead{Pod: pod.Pod, Missing: map[string]string{}}
	perGroup := make([]map[string]string, len(dirs))
	for i, d := range dirs {
		perGroup[i] = map[string]string{}
		name := "pod"
		if i > 0 {
			name = filepath.Base(d)
		}
		out.Groups = append(out.Groups, readGroup(d, name, perGroup[i]))
	}
	// A signal is missing for the pod only if no group had it.
	present := map[string]bool{}
	for _, g := range out.Groups {
		if g.HasCPUStat {
			present[SigCPUStat] = true
		}
		if g.HasCPUPSI {
			present[SigCPUPressure] = true
		}
		if g.HasMemEvents {
			present[SigMemEvents] = true
		}
		if g.HasMemUsage {
			present[SigMemUsage] = true
		}
		if g.HasMemPSI {
			present[SigMemPressure] = true
		}
		if g.HasIOPSI {
			present[SigIOPressure] = true
		}
	}
	for _, sig := range AllCgroupSignals {
		if present[sig] {
			continue
		}
		for _, m := range perGroup {
			if why, ok := m[sig]; ok {
				out.Missing[sig] = why
				break
			}
		}
		if _, ok := out.Missing[sig]; !ok {
			out.Missing[sig] = "not read"
		}
	}
	return out, nil
}

// Sample is one poll of a job's pods on this node.
type Sample struct {
	At           time.Time
	PodsExpected int
	Pods         []podRead
	// PodErrors maps pod name to why its cgroup could not be read.
	PodErrors map[string]string
}

// Sampler polls the cgroups of the jobs' pods and keeps a bounded history.
type Sampler struct {
	Root string // cgroup v2 root; empty means no cgroup access is configured
	Res  DirResolver
	Pods PodSource
	Now  func() time.Time

	mu      sync.Mutex
	history map[string][]Sample
}

// PodSource is the resolver view the sampler needs (flight.Resolver implements it).
type PodSource interface {
	Pods(ns, job string) []flight.PodInfo
	Jobs() [][2]string
}

// NewSampler builds a sampler over a cgroup v2 root ("" disables it).
func NewSampler(root string, pods PodSource) *Sampler {
	s := &Sampler{Root: root, Pods: pods, Now: time.Now, history: map[string][]Sample{}}
	if root != "" {
		s.Res = &fabric.HostCgroups{Root: root}
	}
	return s
}

// Enabled reports whether cgroup access is configured.
func (s *Sampler) Enabled() bool { return s != nil && s.Root != "" && s.Res != nil }

func jobKey(ns, job string) string { return ns + "/" + job }

// Poll reads every known job once. Jobs that no longer have pods are dropped.
func (s *Sampler) Poll() {
	if !s.Enabled() || s.Pods == nil {
		return
	}
	now := s.Now()
	jobs := s.Pods.Jobs()
	if len(jobs) > maxSampledJobs {
		jobs = jobs[:maxSampledJobs]
	}
	live := map[string]bool{}
	for _, j := range jobs {
		live[jobKey(j[0], j[1])] = true
		pods := s.Pods.Pods(j[0], j[1])
		smp := Sample{At: now, PodsExpected: len(pods), PodErrors: map[string]string{}}
		for _, pod := range pods {
			pr, err := readPod(s.Res, pod)
			if err != nil {
				smp.PodErrors[pod.Pod] = err.Error()
				continue
			}
			smp.Pods = append(smp.Pods, pr)
		}
		s.mu.Lock()
		h := append(s.history[jobKey(j[0], j[1])], smp)
		if len(h) > maxSamplesPerJob {
			h = append(h[:0], h[len(h)-maxSamplesPerJob:]...)
		}
		s.history[jobKey(j[0], j[1])] = h
		s.mu.Unlock()
	}
	s.mu.Lock()
	for k := range s.history {
		if !live[k] {
			delete(s.history, k)
		}
	}
	s.mu.Unlock()
}

// Run polls every interval until ctx ends.
func (s *Sampler) Run(ctx context.Context, interval time.Duration) {
	if !s.Enabled() {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	s.Poll()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Poll()
		}
	}
}

// CgroupWindow is the per-job cgroup view derived from the samples in the window.
type CgroupWindow struct {
	Span         time.Duration
	Samples      int
	PodsExpected int
	PodsRead     int

	// Counter deltas over the span; valid only when CountersOK.
	CountersOK      bool
	NrPeriods       uint64
	NrThrottled     uint64
	ThrottledUsec   uint64
	ThrottleRatio   float64 // max over cgroups that had >= minPeriods periods (see ratioValid)
	ThrottleRatioOK bool
	OOMKill         uint64
	OOM             uint64
	MemHighMax      uint64 // increments of memory.events high + max

	// Gauges from the newest sample; PSI values are the max avg10 seen in the window.
	MemUsageRatio                     float64
	MemUsageRatioOK                   bool // false when no cgroup has a finite memory.max
	CPUPSI, MemPSI, IOPSI             float64
	CPUPSIFull, MemPSIFull, IOPSIFull float64

	// Measured names the signals that have values; Unavailable the rest with reasons.
	Measured    map[string]bool
	Unavailable []Unavailable
}

// Window derives the cgroup view for a job. minPeriods is Config.MinCPUPeriods.
// It never returns nil: without cgroup access the result is all-unavailable.
func (s *Sampler) Window(ns, job string, window, minSpan time.Duration, minPeriods int) *CgroupWindow {
	w := &CgroupWindow{Measured: map[string]bool{}}
	unavailAll := func(reason string) *CgroupWindow {
		for _, sig := range AllCgroupSignals {
			w.Unavailable = append(w.Unavailable, Unavailable{sig, reason})
		}
		return w
	}
	if !s.Enabled() {
		return unavailAll("no cgroup access: -flight-diagnosis-cgroup is not set")
	}
	s.mu.Lock()
	hist := append([]Sample(nil), s.history[jobKey(ns, job)]...)
	s.mu.Unlock()
	cut := s.Now().Add(-window)
	i := sort.Search(len(hist), func(i int) bool { return !hist[i].At.Before(cut) })
	hist = hist[i:]
	if len(hist) == 0 {
		return unavailAll("no cgroup samples for this job yet (no Running pod of the job on this node, or the pod list is stale)")
	}
	first, last := hist[0], hist[len(hist)-1]
	w.Samples, w.Span = len(hist), last.At.Sub(first.At)
	w.PodsExpected, w.PodsRead = last.PodsExpected, len(last.Pods)
	if len(last.Pods) == 0 {
		reasons := make([]string, 0, len(last.PodErrors))
		for p, e := range last.PodErrors {
			reasons = append(reasons, p+": "+e)
		}
		sort.Strings(reasons)
		return unavailAll("no pod cgroup could be read: " + strings.Join(reasons, "; "))
	}

	// Gauges and PSI from the samples.
	for _, sm := range hist {
		for _, p := range sm.Pods {
			for _, g := range p.Groups {
				w.CPUPSI, w.CPUPSIFull = math.Max(w.CPUPSI, g.CPUSome.Avg10), math.Max(w.CPUPSIFull, g.CPUFull.Avg10)
				w.MemPSI, w.MemPSIFull = math.Max(w.MemPSI, g.MemSome.Avg10), math.Max(w.MemPSIFull, g.MemFull.Avg10)
				w.IOPSI, w.IOPSIFull = math.Max(w.IOPSI, g.IOSome.Avg10), math.Max(w.IOPSIFull, g.IOFull.Avg10)
			}
		}
	}
	// A signal is measured when some pod of the newest sample had it.
	seen := map[string]bool{}
	missing := map[string]string{}
	for _, p := range last.Pods {
		for _, sig := range AllCgroupSignals {
			if _, miss := p.Missing[sig]; !miss {
				seen[sig] = true
			} else if _, dup := missing[sig]; !dup {
				missing[sig] = p.Pod + ": " + p.Missing[sig]
			}
		}
	}
	for _, p := range last.Pods {
		for _, g := range p.Groups {
			if g.HasMemUsage && g.MemLimited {
				w.MemUsageRatio = math.Max(w.MemUsageRatio, float64(g.MemCurrent)/float64(g.MemMax))
				w.MemUsageRatioOK = true
			}
		}
	}
	// Counter deltas per (pod, group), tolerant of restarts (a counter that went
	// down is treated as reset and contributes its current value).
	counters := w.Span >= minSpan && len(hist) >= 2
	if counters {
		w.CountersOK = true
		type gk struct{ pod, group string }
		base := map[gk]groupRead{}
		for _, p := range first.Pods {
			for _, g := range p.Groups {
				base[gk{p.Pod, g.Name}] = g
			}
		}
		delta := func(cur, old uint64) uint64 {
			if cur < old {
				return cur
			}
			return cur - old
		}
		for _, p := range last.Pods {
			for _, g := range p.Groups {
				b, had := base[gk{p.Pod, g.Name}]
				if !had {
					continue // appeared inside the window: no baseline
				}
				if g.HasCPUStat && b.HasCPUStat {
					dp, dt := delta(g.NrPeriods, b.NrPeriods), delta(g.NrThrottled, b.NrThrottled)
					w.NrPeriods += dp
					w.NrThrottled += dt
					w.ThrottledUsec += delta(g.ThrottledUsec, b.ThrottledUsec)
					if dp >= uint64(minPeriods) {
						w.ThrottleRatio = math.Max(w.ThrottleRatio, float64(dt)/float64(dp))
						w.ThrottleRatioOK = true
					}
				}
				if g.HasMemEvents && b.HasMemEvents {
					w.OOMKill += delta(g.OOMKill, b.OOMKill)
					w.OOM += delta(g.OOM, b.OOM)
					w.MemHighMax += delta(g.High, b.High) + delta(g.MaxEvents, b.MaxEvents)
				}
			}
		}
	}
	span := fmt.Sprintf("needs two samples at least %ds apart (have %d over %ds)", int(minSpan/time.Second), len(hist), int(w.Span/time.Second))
	for _, sig := range AllCgroupSignals {
		counterSig := sig == SigCPUStat || sig == SigMemEvents
		switch {
		case !seen[sig]:
			why := missing[sig]
			if why == "" {
				why = "not readable"
			}
			w.Unavailable = append(w.Unavailable, Unavailable{sig, why})
		case counterSig && !counters:
			w.Unavailable = append(w.Unavailable, Unavailable{sig, span})
		case sig == SigCPUStat && !w.ThrottleRatioOK:
			// Counters read but the CPU cgroup saw too few periods (no CPU limit set gives nr_periods 0).
			w.Unavailable = append(w.Unavailable, Unavailable{sig, fmt.Sprintf("fewer than %d CFS periods in the window (no CPU limit, or an idle pod)", minPeriods)})
		case sig == SigMemUsage && !w.MemUsageRatioOK:
			w.Unavailable = append(w.Unavailable, Unavailable{sig, "memory.current read but no cgroup of the job has a finite memory.max (usage ratio undefined)"})
		default:
			w.Measured[sig] = true
		}
	}
	if len(last.PodErrors) > 0 {
		names := make([]string, 0, len(last.PodErrors))
		for p := range last.PodErrors {
			names = append(names, p)
		}
		sort.Strings(names)
		w.Unavailable = append(w.Unavailable, Unavailable{"cgroup.pods", fmt.Sprintf("%d of %d pods of the job on this node could not be read (%s): %s", len(last.PodErrors), last.PodsExpected, strings.Join(names, ","), last.PodErrors[names[0]])})
	}
	return w
}
