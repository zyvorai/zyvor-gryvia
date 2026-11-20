// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package dcgm

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Canned text in the shape dcgm-exporter documents (labels gpu, UUID, Hostname,
// and the Kubernetes mapping namespace/pod/container). NOT captured from real
// hardware.
const canned = `# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization (in %).
# TYPE DCGM_FI_DEV_GPU_UTIL gauge
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-aaaa",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="node-1",container="trainer",namespace="ml",pod="train-0"} 87
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-bbbb",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="node-1",container="trainer",namespace="ml",pod="train-1"} 5
# TYPE DCGM_FI_PROF_SM_ACTIVE gauge
DCGM_FI_PROF_SM_ACTIVE{gpu="0",UUID="GPU-aaaa",Hostname="node-1",namespace="ml",pod="train-0"} 0.82
DCGM_FI_PROF_SM_ACTIVE{gpu="1",UUID="GPU-bbbb",Hostname="node-1",namespace="ml",pod="train-1"} 0.03
DCGM_FI_PROF_PIPE_TENSOR_ACTIVE{gpu="0",UUID="GPU-aaaa",Hostname="node-1",namespace="ml",pod="train-0"} 0.61
DCGM_FI_PROF_DRAM_ACTIVE{gpu="0",UUID="GPU-aaaa",Hostname="node-1",namespace="ml",pod="train-0"} 0.4
DCGM_FI_PROF_NVLINK_TX_BYTES{gpu="0",UUID="GPU-aaaa",Hostname="node-1",namespace="ml",pod="train-0"} 123456789
DCGM_FI_PROF_PCIE_RX_BYTES{gpu="0",UUID="GPU-aaaa",Hostname="node-1",namespace="ml",pod="train-0"} 4096
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-aaaa"} 1000
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-aaaa"} NaN
DCGM_FI_PROF_SM_ACTIVE{gpu="9",bad="label} 1
garbage line
DCGM_FI_PROF_SM_ACTIVE{gpu="7",note="a \"quoted\" \\ value"} 0.5 1700000000000
`

func TestParse(t *testing.T) {
	ms := Parse(canned)
	byKey := map[string]float64{}
	for _, m := range ms {
		byKey[m.Name+"/"+m.Labels["gpu"]] = m.Value
	}
	want := map[string]float64{
		FieldGPUUtil + "/0": 87, FieldGPUUtil + "/1": 5, FieldSMActive + "/0": 0.82, FieldSMActive + "/1": 0.03,
		FieldTensor + "/0": 0.61, FieldDRAMActive + "/0": 0.4, FieldNVLinkTX + "/0": 123456789, FieldPCIeRX + "/0": 4096,
		FieldSMActive + "/7": 0.5, // escaped quotes and a trailing timestamp
	}
	if len(byKey) != len(want) {
		t.Errorf("parsed %d metrics, want %d: %v", len(byKey), len(want), byKey)
	}
	for k, v := range want {
		if byKey[k] != v {
			t.Errorf("%s = %v, want %v", k, byKey[k], v)
		}
	}
	for _, m := range ms {
		if m.Labels["gpu"] == "7" && m.Labels["note"] != `a "quoted" \ value` {
			t.Errorf("label unescape: %q", m.Labels["note"])
		}
	}
}

func TestSamplesGroupsPerGPUAndNormalises(t *testing.T) {
	at := time.Unix(100, 0)
	ss := Samples(at, Parse(canned))
	var g0 *GPUSample
	for i := range ss {
		if ss[i].GPU == "0" {
			g0 = &ss[i]
		}
	}
	if g0 == nil || g0.Pod != "train-0" || g0.Namespace != "ml" || len(g0.Values) != 6 {
		t.Fatalf("gpu0 sample %+v", g0)
	}
	if v, src, _ := g0.Activity(); v != 0.82 || src != FieldSMActive {
		t.Errorf("activity %v %s", v, src)
	}
	util := GPUSample{Values: map[string]float64{FieldGPUUtil: 50}}
	if v, src, _ := util.Activity(); v != 0.5 || src != FieldGPUUtil {
		t.Errorf("util fallback %v %s", v, src)
	}
	if _, _, ok := (GPUSample{Values: map[string]float64{FieldNVLinkTX: 1}}).Activity(); ok {
		t.Error("no activity field must not report activity")
	}
}

func TestStoreWindowAndCap(t *testing.T) {
	s := NewStore(10*time.Second, 5)
	t0 := time.Unix(1000, 0)
	for i := 0; i < 20; i++ {
		s.Add(t0.Add(time.Duration(i)*time.Second), []GPUSample{{At: t0.Add(time.Duration(i) * time.Second), GPU: "0"}})
	}
	if s.Len() != 5 {
		t.Errorf("len %d, want capped at 5", s.Len())
	}
	if got := s.Since(t0.Add(17*time.Second), nil); len(got) != 3 {
		t.Errorf("since: %d", len(got))
	}
}

func sm(at time.Time, v float64) GPUSample {
	return GPUSample{At: at, GPU: "0", Values: map[string]float64{FieldSMActive: v}}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestCorrelateIdleAndOverlap(t *testing.T) {
	t0 := time.Unix(1000, 0)
	// comm windows: [0,2s] and overlapping [1s,3s] (two ranks) -> union [0,3s]; then compute [3s,5s]; comm [5s,6s]
	ws := []Window{{t0, t0.Add(2 * time.Second)}, {t0.Add(time.Second), t0.Add(3 * time.Second)}, {t0.Add(5 * time.Second), t0.Add(6 * time.Second)}}
	var ss []GPUSample
	// 1 s cadence, value stands for the second before it
	vals := []float64{0.0, 0.05, 0.9, 0.95, 0.9, 0.02, 0.0} // t0..t0+6s
	for i, v := range vals {
		ss = append(ss, sm(t0.Add(time.Duration(i)*time.Second), v))
	}
	r := Correlate(ws, ss, 0.1)
	if !r.Measured {
		t.Fatalf("%+v", r)
	}
	// comm seconds = 3 + 1 = 4 ; idle: (0,1]=0.05 idle, (1,2]=0.9, (2,3]=0.95 busy, (5,6]=0.0 idle -> idle 2 of 4
	if !near(r.CommSeconds, 4) || !near(r.Coverage, 1) || !near(r.IdleDuringComm, 0.5) {
		t.Errorf("comm=%v cov=%v idle=%v", r.CommSeconds, r.Coverage, r.IdleDuringComm)
	}
	// compute = (3,5]: values 0.9 and 0.9... samples at t0+4 (0.9) covers (3,4]; t0+5 (0.02) covers (4,5]
	if !r.ComputeMeasured || !near(r.ActiveDuringCompute, (0.9+0.02)/2) {
		t.Errorf("compute %+v", r)
	}
	if r.Source != FieldSMActive {
		t.Errorf("source %q", r.Source)
	}
}

func TestCorrelateCoverageIsHonest(t *testing.T) {
	t0 := time.Unix(1000, 0)
	// a 30 s cadence (dcgm-exporter default) barely covers a 2 s window: hold is capped at 10 s
	ss := []GPUSample{sm(t0.Add(-30*time.Second), 0.9), sm(t0.Add(31*time.Second), 0.0)}
	r := Correlate([]Window{{t0, t0.Add(2 * time.Second)}}, ss, 0)
	if r.Measured || r.Coverage != 0 {
		t.Errorf("no sample interval overlaps the window: %+v", r)
	}
	// partial: sample at t0+1 with 1 s spacing covers half of a 2 s window
	ss = []GPUSample{sm(t0.Add(-time.Second), 0.5), sm(t0.Add(time.Second), 0.0)}
	r = Correlate([]Window{{t0, t0.Add(2 * time.Second)}}, ss, 0)
	// spacing 2 s: the second sample stands for (t0-1s, t0+1s], overlapping 1 s of the 2 s window
	if !r.Measured || !near(r.Coverage, 0.5) {
		t.Errorf("%+v", r)
	}
	if r := Correlate(nil, ss, 0); r.Measured {
		t.Error("no windows")
	}
	if r := Correlate([]Window{{t0, t0.Add(time.Second)}}, nil, 0); r.Measured {
		t.Error("no samples")
	}
}

func TestCorrelateAveragesGPUsAndFallsBackToUtil(t *testing.T) {
	t0 := time.Unix(1000, 0)
	// two GPUs at the same timestamps: mean of util 90% and 10% -> 0.5 (busy)
	ss := []GPUSample{
		{At: t0, GPU: "0", Values: map[string]float64{FieldGPUUtil: 90}},
		{At: t0, GPU: "1", Values: map[string]float64{FieldGPUUtil: 10}},
		{At: t0.Add(time.Second), GPU: "0", Values: map[string]float64{FieldGPUUtil: 90}},
		{At: t0.Add(time.Second), GPU: "1", Values: map[string]float64{FieldGPUUtil: 10}},
	}
	r := Correlate([]Window{{t0, t0.Add(time.Second)}}, ss, 0.1)
	if !r.Measured || r.IdleDuringComm != 0 || r.Source != FieldGPUUtil || !near(r.ActiveDuringComm, 0.5) {
		t.Errorf("%+v", r)
	}
}

func TestScrapeOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(canned))
	}))
	defer srv.Close()
	st := NewStore(time.Minute, 100)
	now := time.Unix(5000, 0)
	sc := &Scraper{URL: srv.URL, Store: st, Now: func() time.Time { return now }}
	n, err := sc.ScrapeOnce(context.Background())
	if err != nil || n == 0 || st.Len() != n {
		t.Fatalf("n=%d err=%v len=%d", n, err, st.Len())
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) }))
	defer bad.Close()
	if _, err := (&Scraper{URL: bad.URL, Store: st}).ScrapeOnce(context.Background()); err == nil {
		t.Error("HTTP 500 must be an error")
	}
	if _, err := (&Scraper{URL: "http://127.0.0.1:1/metrics", Store: st}).ScrapeOnce(context.Background()); err == nil {
		t.Error("unreachable exporter must be an error, not a panic")
	}
	if ValidateURL("file:///etc/passwd") == nil || ValidateURL("http://") == nil || ValidateURL(DefaultURL) != nil {
		t.Error("ValidateURL")
	}
}
