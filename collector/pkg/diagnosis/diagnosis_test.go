// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package diagnosis

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/fabric"
	"github.com/zyvorai/gryvia/collector/pkg/flight"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func allProbes() []ProbeStatus {
	var out []ProbeStatus
	for _, o := range []string{"tcp_trace.o", "datapipe_bottleneck.o", "nccl_trace.o", "straggler.o", "rdma_health.o", "overlap.o", "gds_trace.o", "roce_cnp.o", "pfc_pause.o", "weight_exfil.o", "gpu_mem_trace.o"} {
		out = append(out, ProbeStatus{Object: o, Program: "p", Attached: true})
	}
	return out
}

func healthyCgroup() *CgroupWindow {
	m := map[string]bool{}
	for _, s := range AllCgroupSignals {
		m[s] = true
	}
	return &CgroupWindow{Measured: m, MemUsageRatio: 0.3, MemUsageRatioOK: true, CountersOK: true, ThrottleRatioOK: true}
}

func base() Input {
	return Input{Now: t0, Namespace: "ml", Job: "train", Node: "n1", Probes: allProbes(), Cgroup: healthyCgroup()}
}

func kinds(d Diagnosis) []string {
	var k []string
	for _, f := range d.Findings {
		k = append(k, f.Kind+"/"+f.Severity+"/"+f.Confidence)
	}
	return k
}

func TestEvaluateRules(t *testing.T) {
	cfg := DefaultConfig()
	stall := func(n int, each time.Duration) []flight.Event {
		var ev []flight.Event
		for i := 0; i < n; i++ {
			ev = append(ev, flight.Event{Kind: "pipeline_stall", DurationNs: uint64(each), Identity: flight.Identity{Pod: "p0"}})
		}
		return ev
	}
	cases := []struct {
		name   string
		mutate func(*Input)
		want   []string // exact ranked kind/severity/confidence
	}{
		{"healthy", func(*Input) {}, nil},
		{"retransmits warning marginal", func(in *Input) {
			in.Events = []flight.Event{{Kind: "tcp_retransmit", Retransmits: 11}}
		}, []string{"network/warning/low"}},
		{"retransmits critical", func(in *Input) {
			in.Events = []flight.Event{{Kind: "tcp_retransmit", Retransmits: 500}}
		}, []string{"network/critical/medium"}},
		{"retransmits at threshold is not a finding", func(in *Input) {
			in.Events = []flight.Event{{Kind: "tcp_retransmit", Retransmits: 10}}
		}, nil},
		{"pipeline stall alone is low confidence", func(in *Input) { in.Events = stall(20, 6*time.Second) },
			[]string{"storage/critical/low"}}, // 120s/300s = 40% > 30%
		{"pipeline stall corroborated by io pressure", func(in *Input) {
			in.Events = stall(20, 5*time.Second) // 100/300 = 33% critical
			in.Cgroup.IOPSI = 40
		}, []string{"storage/critical/high"}},
		{"too few stalls ignored", func(in *Input) { in.Events = stall(3, 60*time.Second) }, nil},
		{"cpu throttling", func(in *Input) { in.Cgroup.ThrottleRatio, in.Cgroup.ThrottledUsec = 0.6, 12_000_000 }, []string{"cpu/critical/high"}},
		{"cpu psi only", func(in *Input) { in.Cgroup.CPUPSI = 30 }, []string{"cpu/warning/medium"}},
		{"cpu throttle plus psi stays high", func(in *Input) { in.Cgroup.ThrottleRatio, in.Cgroup.CPUPSI = 0.3, 60 }, []string{"cpu/critical/high"}},
		{"oom kill", func(in *Input) { in.Cgroup.OOMKill = 1 }, []string{"memory/critical/high"}},
		{"memory near limit", func(in *Input) { in.Cgroup.MemUsageRatio = 0.95 }, []string{"memory/warning/medium"}},
		{"memory high events", func(in *Input) { in.Cgroup.MemHighMax = 3 }, []string{"memory/warning/high"}},
		{"straggler and comm", func(in *Input) {
			in.Fabric = fabric.Status{StragglerHits: 60, StragglerRank: 3, NCCLP99MS: 300}
		}, []string{"gpu-comm/critical/medium", "straggler/critical/medium"}},
		{"overlap idle", func(in *Input) { in.Fabric.OverlapIdleRatio = 0.5 }, []string{"gpu-comm/warning/medium"}},
		{"rdma retry", func(in *Input) { in.Fabric.RDMARetryRate = 0.5 }, []string{"rdma/critical/medium"}},
		{"exfil", func(in *Input) { in.Fabric.ExfilEvents = 1 }, []string{"exfil/warning/medium"}},
		{"cnp and pfc corroborate network", func(in *Input) { in.CNPRate, in.PFCRate = 500, 5000 }, []string{"network/warning/high"}},
		{"gds bounce", func(in *Input) { in.Fabric.GDSMeasured, in.Fabric.GDSHitRatio = true, 0.1 }, []string{"storage/warning/medium"}},
	}
	for _, c := range cases {
		in := base()
		c.mutate(&in)
		d := Evaluate(in, cfg)
		got := kinds(d)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
		for _, f := range d.Findings {
			if len(f.Evidence) == 0 || f.Summary == "" || len(f.WhatWasNotMeasured) == 0 {
				t.Errorf("%s: finding lacks evidence/summary/caveats: %+v", c.name, f)
			}
		}
	}
}

func TestNoBottleneckAndUnavailableIsNotHealthy(t *testing.T) {
	cfg := DefaultConfig()
	d := Evaluate(base(), cfg)
	if len(d.Findings) != 0 || !strings.HasPrefix(d.Summary, "no bottleneck detected in measured signals") || len(d.Measured) == 0 {
		t.Fatalf("healthy summary: %+v", d.Summary)
	}
	if d.Completeness.Complete {
		t.Fatal("node-local report must never claim complete (nodesExpected unknown)")
	}
	b, _ := json.Marshal(d.Completeness)
	if !strings.Contains(string(b), `"nodesExpected":"unknown"`) || !strings.Contains(string(b), `"complete":false`) {
		t.Fatalf("completeness json: %s", b)
	}

	// Nothing measured: no conclusion.
	in := Input{Now: t0, Namespace: "ml", Job: "train"}
	d = Evaluate(in, cfg)
	if len(d.Findings) != 0 || len(d.Measured) != 0 || !strings.Contains(d.Summary, "nothing could be measured") {
		t.Fatalf("empty: %s", d.Summary)
	}
	if len(d.Unavailable) < 10 {
		t.Fatalf("all signals must be unavailable: %+v", d.Unavailable)
	}

	// A missing probe is unavailable with the loader's reason, and a finding mentions it.
	in = base()
	in.Probes = nil
	for _, p := range allProbes() {
		if p.Object == "tcp_trace.o" {
			in.Probes = append(in.Probes, ProbeStatus{Object: p.Object, Program: "kprobe_x", Reason: "skipped: kernel symbol missing"})
			continue
		}
		in.Probes = append(in.Probes, p)
	}
	in.Cgroup.OOMKill = 0
	in.Cgroup.CPUPSI = 90 // a cpu finding whose notMeasured must not mention tcp
	d = Evaluate(in, cfg)
	var found bool
	for _, u := range d.Unavailable {
		if u.Signal == SigTCPRetransmit && strings.Contains(u.Reason, "kernel symbol missing") {
			found = true
		}
	}
	if !found {
		t.Fatalf("tcp_retransmit not reported unavailable with reason: %+v", d.Unavailable)
	}
	if !strings.Contains(strings.Join(d.Completeness.Reasons, "|"), "tcp_trace.o") || len(d.Completeness.ProbesSkipped) != 1 {
		t.Fatalf("completeness: %+v", d.Completeness)
	}
	if !strings.Contains(d.Summary, "finding") {
		t.Fatalf("summary: %s", d.Summary)
	}
	// Healthy-but-unavailable summary says so.
	in.Cgroup.CPUPSI = 0
	d = Evaluate(in, cfg)
	if !strings.Contains(d.Summary, "unavailable, which is not evidence of health") {
		t.Fatalf("summary: %s", d.Summary)
	}
}

func TestDropsAndTruncationAffectCompleteness(t *testing.T) {
	in := base()
	in.Drops = []DropCount{{"straggler.o/drops", 7}, {"userspace/gpu-decoder", 3}}
	in.DropsNotCounted = []string{"weight_exfil.o"}
	in.EventsTruncated = true
	in.Events = []flight.Event{{Kind: "tcp_retransmit", Retransmits: 50}}
	d := Evaluate(in, DefaultConfig())
	c := d.Completeness
	if c.DroppedEvents.Total != 10 || c.Complete || len(c.Reasons) < 4 {
		t.Fatalf("%+v", c)
	}
	if c.Sampling.Ratio != 1 {
		t.Fatal("sampling")
	}
	joined := strings.Join(d.Findings[0].WhatWasNotMeasured, "|")
	if !strings.Contains(joined, "lower bounds") {
		t.Fatalf("finding must say counts are lower bounds: %s", joined)
	}
}

func TestConfigLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "t.json")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, err := LoadConfig(write(`{"tcpRetransmits":{"warn":1,"crit":2}}`))
	if err != nil || c.TCPRetransmits.Warn != 1 || c.CPUPressure.Warn != DefaultConfig().CPUPressure.Warn {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{`{"tcpRetransmit":{}}`, `{"tcpRetransmits":{"warn":5,"crit":1}}`, `{"windowSeconds":1}`, `not json`} {
		if _, err := LoadConfig(write(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if c, err := LoadConfig(""); err != nil || c.WindowSeconds != 300 {
		t.Fatal("defaults")
	}
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParsePSIAndKV(t *testing.T) {
	some, full, hs, hf := parsePSI([]byte("some avg10=12.50 avg60=1.00 avg300=0.10 total=123456\nfull avg10=3.00 avg60=0.00 avg300=0.00 total=99\n"))
	if !hs || !hf || some.Avg10 != 12.5 || some.Total != 123456 || full.Avg10 != 3 {
		t.Fatalf("%v %v", some, full)
	}
	_, _, hs, hf = parsePSI([]byte("some avg10=1.00 avg60=0 avg300=0 total=1\n"))
	if !hs || hf {
		t.Fatal("older kernels have no full line")
	}
	if _, _, hs, _ = parsePSI([]byte("garbage\nsome avg10=NaN total=1\n")); hs {
		t.Fatal("NaN accepted")
	}
	kv := parseKV([]byte("nr_periods 10\nnr_throttled 3\nbad line here\nthrottled_usec 77\n"))
	if kv["nr_periods"] != 10 || kv["nr_throttled"] != 3 || kv["throttled_usec"] != 77 {
		t.Fatalf("%v", kv)
	}
}

// ---- fake cgroup trees ----

const podUID = "12345678-1234-1234-1234-123456789abc"

type fakeGroup struct {
	cpuStat, memEvents, memCurrent, memMax string
	cpuP, memP, ioP                        string
}

func psi(some float64) string {
	return fmt.Sprintf("some avg10=%.2f avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", some)
}

func writeGroup(t *testing.T, dir string, g fakeGroup) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"cpu.stat": g.cpuStat, "memory.events": g.memEvents, "memory.current": g.memCurrent, "memory.max": g.memMax,
		"cpu.pressure": g.cpuP, "memory.pressure": g.memP, "io.pressure": g.ioP} {
		if body == "" {
			continue // file absent
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newRoot(t *testing.T) (root, podDir, ctrDir string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory io\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	us := strings.ReplaceAll(podUID, "-", "_")
	podDir = filepath.Join(root, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+us+".slice")
	ctrDir = filepath.Join(podDir, "cri-containerd-abc.scope")
	return
}

type fakePods struct{ pods []flight.PodInfo }

func (f fakePods) Pods(ns, job string) []flight.PodInfo { return f.pods }
func (f fakePods) Jobs() [][2]string {
	if len(f.pods) == 0 {
		return nil
	}
	return [][2]string{{"ml", "train"}}
}

func fakePod(qos string) fakePods {
	return fakePods{[]flight.PodInfo{{Identity: flight.Identity{Namespace: "ml", Job: "train", Pod: "w-0", Node: "n1"}, UID: podUID, QOS: qos}}}
}

func TestSamplerWindowThrottleOOMAndPSI(t *testing.T) {
	root, podDir, ctrDir := newRoot(t)
	writeGroup(t, podDir, fakeGroup{cpuP: psi(5), memP: psi(2), ioP: psi(1), memCurrent: "100\n", memMax: "max\n"})
	g := fakeGroup{
		cpuStat:   "usage_usec 1\nnr_periods 100\nnr_throttled 10\nthrottled_usec 1000000\n",
		memEvents: "low 0\nhigh 1\nmax 0\noom 0\noom_kill 0\n", memCurrent: "900\n", memMax: "1000\n",
	}
	writeGroup(t, ctrDir, g)
	now := t0
	s := NewSampler(root, fakePod("Burstable"))
	s.Now = func() time.Time { return now }
	s.Poll()

	// 60 s later: 200 more periods, 150 throttled; an OOM kill; PSI rose.
	now = now.Add(60 * time.Second)
	writeGroup(t, podDir, fakeGroup{cpuP: psi(25), memP: psi(2), ioP: psi(12), memCurrent: "100\n", memMax: "max\n"})
	g.cpuStat = "usage_usec 9\nnr_periods 300\nnr_throttled 160\nthrottled_usec 9000000\n"
	g.memEvents = "low 0\nhigh 4\nmax 2\noom 1\noom_kill 1\n"
	g.memCurrent = "980\n"
	writeGroup(t, ctrDir, g)
	s.Poll()

	w := s.Window("ml", "train", 5*time.Minute, 20*time.Second, 20)
	if !w.CountersOK || w.NrPeriods != 200 || w.NrThrottled != 150 || w.ThrottledUsec != 8_000_000 || w.ThrottleRatio != 0.75 {
		t.Fatalf("cpu: %+v", w)
	}
	if w.OOMKill != 1 || w.MemHighMax != 5 || w.MemUsageRatio != 0.98 {
		t.Fatalf("mem: %+v", w)
	}
	if w.CPUPSI != 25 || w.IOPSI != 12 {
		t.Fatalf("psi: %+v", w)
	}
	if len(w.Unavailable) != 0 {
		t.Fatalf("everything was readable: %+v", w.Unavailable)
	}
	// Feed through the rules: cpu critical/high, memory critical/high (oom) + more.
	in := base()
	in.Cgroup = w
	d := Evaluate(in, DefaultConfig())
	got := strings.Join(kinds(d), ",")
	if !strings.Contains(got, "cpu/critical/high") || !strings.Contains(got, "memory/critical/high") || !strings.Contains(got, "storage/warning/low") {
		t.Fatalf("got %s", got)
	}
}

func TestSamplerCounterResetAndSpan(t *testing.T) {
	root, podDir, ctrDir := newRoot(t)
	writeGroup(t, podDir, fakeGroup{cpuP: psi(0), memP: psi(0), ioP: psi(0)})
	g := fakeGroup{cpuStat: "nr_periods 1000\nnr_throttled 900\nthrottled_usec 5\n", memEvents: "oom_kill 4\n", memCurrent: "1\n", memMax: "10\n"}
	writeGroup(t, ctrDir, g)
	now := t0
	s := NewSampler(root, fakePod("Burstable"))
	s.Now = func() time.Time { return now }
	s.Poll()
	w := s.Window("ml", "train", 5*time.Minute, 20*time.Second, 20)
	if w.CountersOK {
		t.Fatal("one sample cannot give deltas")
	}
	un := map[string]string{}
	for _, u := range w.Unavailable {
		un[u.Signal] = u.Reason
	}
	if !strings.Contains(un[SigCPUStat], "two samples") || !strings.Contains(un[SigMemEvents], "two samples") {
		t.Fatalf("counter signals must say why: %+v", un)
	}
	if !w.Measured[SigCPUPressure] || !w.Measured[SigMemUsage] {
		t.Fatalf("gauges and PSI need only one sample: %+v", w.Measured)
	}
	// Container restarted: counters restart from a small value.
	now = now.Add(30 * time.Second)
	g.cpuStat = "nr_periods 50\nnr_throttled 5\nthrottled_usec 1\n"
	g.memEvents = "oom_kill 0\n"
	writeGroup(t, ctrDir, g)
	s.Poll()
	w = s.Window("ml", "train", 5*time.Minute, 20*time.Second, 20)
	if w.NrPeriods != 50 || w.NrThrottled != 5 || w.OOMKill != 0 || w.ThrottleRatio != 0.1 {
		t.Fatalf("reset handling: %+v", w)
	}
}

func TestSamplerUnavailableCases(t *testing.T) {
	s := NewSampler("", fakePod("Burstable"))
	w := s.Window("ml", "train", time.Minute, time.Second, 1)
	if len(w.Measured) != 0 || len(w.Unavailable) != len(AllCgroupSignals) || !strings.Contains(w.Unavailable[0].Reason, "no cgroup access") {
		t.Fatalf("%+v", w)
	}

	root, podDir, ctrDir := newRoot(t)
	// PSI files absent (PSI disabled) and no memory.max: reported, not zero.
	writeGroup(t, podDir, fakeGroup{})
	writeGroup(t, ctrDir, fakeGroup{cpuStat: "nr_periods 0\nnr_throttled 0\nthrottled_usec 0\n", memEvents: "oom_kill 0\n", memCurrent: "5\n", memMax: "max\n"})
	now := t0
	s = NewSampler(root, fakePod("Burstable"))
	s.Now = func() time.Time { return now }
	s.Poll()
	now = now.Add(30 * time.Second)
	s.Poll()
	w = s.Window("ml", "train", 5*time.Minute, 20*time.Second, 20)
	un := map[string]string{}
	for _, u := range w.Unavailable {
		un[u.Signal] = u.Reason
	}
	for _, sig := range []string{SigCPUPressure, SigMemPressure, SigIOPressure} {
		if !strings.Contains(un[sig], "not present") {
			t.Errorf("%s: %q", sig, un[sig])
		}
	}
	if !strings.Contains(un[SigCPUStat], "fewer than 20 CFS periods") || !strings.Contains(un[SigMemUsage], "no cgroup of the job has a finite memory.max") {
		t.Fatalf("%+v", un)
	}
	if !w.Measured[SigMemEvents] {
		t.Fatalf("memory.events was readable: %+v", w.Measured)
	}
	// And none of that is turned into a finding.
	in := base()
	in.Cgroup = w
	if d := Evaluate(in, DefaultConfig()); len(d.Findings) != 0 {
		t.Fatalf("%v", kinds(d))
	}

	// Missing QoS and missing cgroup dir are per-pod errors, not silence.
	s = NewSampler(root, fakePod(""))
	s.Now = func() time.Time { return t0 }
	s.Poll()
	w = s.Window("ml", "train", time.Minute, time.Second, 1)
	if len(w.Measured) != 0 || !strings.Contains(w.Unavailable[0].Reason, "QoS") {
		t.Fatalf("%+v", w.Unavailable)
	}
	s = NewSampler(root, fakePods{[]flight.PodInfo{{Identity: flight.Identity{Namespace: "ml", Job: "train", Pod: "gone"}, UID: "aaaaaaaa-1234-1234-1234-123456789abc", QOS: "Guaranteed"}}})
	s.Now = func() time.Time { return t0 }
	s.Poll()
	w = s.Window("ml", "train", time.Minute, time.Second, 1)
	if len(w.Measured) != 0 || !strings.Contains(w.Unavailable[0].Reason, "no cgroup for pod") {
		t.Fatalf("%+v", w.Unavailable)
	}
	// Unknown job: no samples.
	w = NewSampler(root, fakePod("Burstable")).Window("ml", "other", time.Minute, time.Second, 1)
	if !strings.Contains(w.Unavailable[0].Reason, "no cgroup samples") {
		t.Fatalf("%+v", w.Unavailable)
	}
}

func TestSamplerNeverEscapesRoot(t *testing.T) {
	root, podDir, _ := newRoot(t)
	outside := t.TempDir()
	writeGroup(t, outside, fakeGroup{memCurrent: "5\n", memMax: "10\n"})
	if err := os.MkdirAll(filepath.Dir(podDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, podDir); err != nil {
		t.Fatal(err)
	}
	s := NewSampler(root, fakePod("Burstable"))
	s.Now = func() time.Time { return t0 }
	s.Poll()
	w := s.Window("ml", "train", time.Minute, time.Second, 1)
	if len(w.Measured) != 0 {
		t.Fatalf("read through a symlink out of the root: %+v", w.Measured)
	}
}
