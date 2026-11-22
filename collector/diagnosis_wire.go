// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zyvorai/gryvia/collector/pkg/diagnosis"
	"github.com/zyvorai/gryvia/collector/pkg/fabric"
	"github.com/zyvorai/gryvia/collector/pkg/flight"
	"github.com/zyvorai/gryvia/collector/pkg/incident"
	"github.com/zyvorai/gryvia/collector/pkg/loader"
)

// dropObjects are the eBPF objects that declare a `drops` per-CPU counter map
// (GRYVIA_DECLARE_DROPS in ebpf/headers/gryvia_core.h) and the slot that counts
// their output failures: 0 = bpf_ringbuf_reserve failed, 1 = perf output failed.
var dropObjects = map[string]uint32{
	"tcp_trace.o": 1, "nccl_trace.o": 0, "gpu_mem_trace.o": 0, "datapipe_bottleneck.o": 0,
	"straggler.o": 0, "rdma_health.o": 0, "gds_trace.o": 0, "overlap.o": 0,
	"infer_latency.o": 0, "ucx_gloo.o": 0, "weight_exfil.o": 0, "trace_correlator.o": 0,
}

// probeSource is the part of loader.Manager the diagnosis reads.
type probeSource interface {
	Status() []loader.ProgramStatus
	ReadCounter(object, name string, key uint32) (uint64, error)
	HasMap(object, name string) bool
}

type managerProbes struct{ *loader.Manager }

func (m managerProbes) HasMap(object, name string) bool { return m.Map(object, name) != nil }

type diagnosisService struct {
	node     string
	cfg      diagnosis.Config
	recorder *flight.Recorder
	identity *flight.Resolver
	folder   *fabric.Folder
	probes   probeSource
	sampler  *diagnosis.Sampler
	store    *incident.Store   // nil = history disabled
	tracker  *incident.Tracker // nil = history disabled
	// userDrops returns consumer-side loss counters (decoder channels, perf lost samples).
	userDrops func() []diagnosis.DropCount
	now       func() time.Time
	gauge     *prometheus.GaugeVec
}

var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validLabel(s string) bool { return len(s) > 0 && len(s) <= 63 && dnsLabelRe.MatchString(s) }

// probeStatuses converts the loader's per-program results.
func probeStatuses(p probeSource) []diagnosis.ProbeStatus {
	st := p.Status()
	out := make([]diagnosis.ProbeStatus, 0, len(st))
	for _, s := range st {
		out = append(out, diagnosis.ProbeStatus{Object: s.Object, Program: s.Program, Kind: s.Kind, Attached: s.Attached, Reason: s.Reason})
	}
	return out
}

// collectDrops reads every producer-side drop counter of the loaded objects and
// names the attached objects that have none.
func (s *diagnosisService) collectDrops(probes []diagnosis.ProbeStatus) (drops []diagnosis.DropCount, notCounted []string) {
	attached := map[string]bool{}
	for _, p := range probes {
		if p.Attached {
			attached[p.Object] = true
		}
	}
	objs := make([]string, 0, len(dropObjects))
	for o := range dropObjects {
		objs = append(objs, o)
	}
	sort.Strings(objs)
	for _, o := range objs {
		if !attached[o] {
			continue
		}
		if !s.probes.HasMap(o, "drops") {
			notCounted = append(notCounted, o)
			continue
		}
		v, err := s.probes.ReadCounter(o, "drops", dropObjects[o])
		if err != nil {
			notCounted = append(notCounted, o)
			continue
		}
		kind := "ringbuf"
		if dropObjects[o] == 1 {
			kind = "perf"
		}
		drops = append(drops, diagnosis.DropCount{Source: o + "/drops:" + kind, Count: v})
	}
	if s.userDrops != nil {
		drops = append(drops, s.userDrops()...)
	}
	return drops, notCounted
}

// evaluate builds the diagnosis of one job; ok is false when the node knows nothing of it.
func (s *diagnosisService) evaluate(ns, job string) (diagnosis.Diagnosis, bool) {
	now := s.now()
	events, truncated, haveEvents := s.recorder.Window(ns, job, now.Add(-s.cfg.Window()))
	pods := s.identity.Pods(ns, job)
	snap := s.folder.Snapshot()
	st, haveFabric := snap[fabric.JobKey{Namespace: ns, Job: job}]
	if !haveEvents && len(pods) == 0 && !haveFabric {
		return diagnosis.Diagnosis{}, false
	}
	probes := probeStatuses(s.probes)
	if len(probes) == 0 {
		probes = nil // the loader reported nothing: unknown, not "no probes needed"
	}
	drops, notCounted := s.collectDrops(probes)
	in := diagnosis.Input{
		Now: now, Namespace: ns, Job: job, Node: s.node,
		Events: events, EventsTruncated: truncated,
		Fabric: st, CNPRate: snap[fabric.CNPJob].CNPRate, PFCRate: snap[fabric.PFCJob].PFCRate,
		Cgroup: s.sampler.Window(ns, job, s.cfg.Window(), s.cfg.MinSpan(), s.cfg.MinCPUPeriods),
		Probes: probes, Drops: drops, DropsNotCounted: notCounted,
	}
	return diagnosis.Evaluate(in, s.cfg), true
}

// ServeHTTP serves GET /api/v1/flight/diagnosis?namespace=&job= (mounted behind flightAuth).
func (s *diagnosisService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ns, job := r.URL.Query().Get("namespace"), r.URL.Query().Get("job")
	if !validLabel(ns) || !validLabel(job) {
		http.Error(w, "namespace and job required (DNS-1123 labels)", http.StatusBadRequest)
		return
	}
	d, ok := s.evaluate(ns, job)
	if !ok {
		http.Error(w, "job unknown on this node", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
}

// cycle evaluates every job this node knows and feeds the incident tracker.
func (s *diagnosisService) cycle() {
	seen := map[[2]string]bool{}
	var jobs [][2]string
	for _, j := range append(s.recorder.Jobs(), s.identity.Jobs()...) {
		if !seen[j] {
			seen[j] = true
			jobs = append(jobs, j)
		}
	}
	if len(jobs) > 128 {
		jobs = jobs[:128]
	}
	now := s.now()
	for _, j := range jobs {
		if d, ok := s.evaluate(j[0], j[1]); ok && s.tracker != nil {
			s.tracker.Observe(now, d)
		}
	}
	if s.tracker != nil {
		s.tracker.Expire(now)
	}
	if s.store != nil {
		s.store.Tick()
	}
	if s.gauge != nil && s.probes != nil {
		drops, _ := s.collectDrops(probeStatuses(s.probes))
		for _, d := range drops {
			s.gauge.WithLabelValues(d.Source).Set(float64(d.Count))
		}
	}
}

// Run drives the sampler and the incident loop until ctx ends.
func (s *diagnosisService) Run(ctx context.Context, every time.Duration) {
	if s.sampler.Enabled() {
		go s.sampler.Run(ctx, every)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if s.store != nil {
				_ = s.store.Close()
			}
			return
		case <-t.C:
			s.cycle()
		}
	}
}

// register mounts the endpoints behind the Flight Recorder's HMAC check.
func (s *diagnosisService) register(mux *http.ServeMux, tokenFile string) {
	h := &incident.Handler{Store: s.store, Node: s.node}
	mux.Handle("/api/v1/flight/diagnosis", flightAuth(s, tokenFile))
	mux.Handle("/api/v1/flight/incidents", flightAuth(http.HandlerFunc(h.ServeIncidents), tokenFile))
	mux.Handle("/api/v1/flight/incidents/export", flightAuth(http.HandlerFunc(h.ServeExport), tokenFile))
	mux.Handle("/api/v1/flight/compare", flightAuth(http.HandlerFunc(h.ServeCompare), tokenFile))
}
