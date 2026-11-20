// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestFolder() (*Folder, *clock) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	f := NewFolder(time.Minute)
	f.now = c.now
	return f, c
}

func only(t *testing.T, f *Folder) Status {
	t.Helper()
	snap := f.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 job, got %d", len(snap))
	}
	for _, st := range snap {
		return st
	}
	return Status{}
}

func TestScoreDeltaFolding(t *testing.T) {
	cases := []struct {
		name string
		st   Status
		want float64
	}{
		{"healthy", Status{}, 0},
		{"slow p99", Status{NCCLP99MS: 60}, 0.25},
		{"p99 at threshold", Status{NCCLP99MS: 50}, 0},
		{"rdma retries", Status{RDMARetryRate: 0.05}, 0.35},
		{"gds bounce measured", Status{GDSMeasured: true, GDSHitRatio: 0.2}, 0.15},
		{"gds ratio 0 but unmeasured", Status{GDSHitRatio: 0}, 0},
		{"gds healthy", Status{GDSMeasured: true, GDSHitRatio: 0.9}, 0},
		{"stragglers", Status{StragglerHits: 11}, 0.25},
		{"10 stragglers is not enough", Status{StragglerHits: 10}, 0},
		{"gpu idle while comm", Status{OverlapIdleRatio: 0.31}, 0.15},
		{"idle at threshold", Status{OverlapIdleRatio: 0.3}, 0},
		{"cnp storm", Status{CNPRate: 101}, 0.15},
		{"cnp at threshold", Status{CNPRate: 100}, 0},
		{"inference wait is informational", Status{InferWaitP99MS: 5000}, 0},
		{"pfc storm", Status{PFCRate: 1001}, 0.10},
		{"pfc at threshold", Status{PFCRate: 1000}, 0},
		{"exfil is informational", Status{ExfilEvents: 50}, 0},
		{"ucx slow sends are informational", Status{UCXSlowP99MS: 5000}, 0},
		{"sum", Status{NCCLP99MS: 60, RDMARetryRate: 0.05}, 0.6},
		{"clamped", Status{NCCLP99MS: 60, RDMARetryRate: 0.05, GDSMeasured: true, StragglerHits: 20}, 1},
		{"everything bad stays <= 1", Status{NCCLP99MS: 60, RDMARetryRate: 1, GDSMeasured: true, StragglerHits: 20, OverlapIdleRatio: 1, CNPRate: 1e6}, 1},
	}
	for _, c := range cases {
		if got := ScoreDelta(c.st); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestFoldStraggler(t *testing.T) {
	f, _ := newTestFolder()
	for i := 1; i <= 100; i++ {
		f.Add(Signal{Type: SigStraggler, PID: 42, Rank: 3, LatencyNS: uint64(i) * 1_000_000, Comm: "train"})
	}
	st := only(t, f)
	if st.StragglerRank != 3 || st.StragglerPID != 42 || st.StragglerHits != 100 {
		t.Errorf("straggler fields: %+v", st)
	}
	if st.NCCLP99MS != 99 {
		t.Errorf("p99 = %v, want 99 (nearest rank over 1..100 ms)", st.NCCLP99MS)
	}
	if st.ScoreDelta != 0.5 { // slow p99 + >10 hits
		t.Errorf("score = %v, want 0.5", st.ScoreDelta)
	}
}

func TestFoldRDMARate(t *testing.T) {
	f, _ := newTestFolder()
	f.Add(Signal{Type: SigRDMARetry, PID: 1, Comm: "nfsd", Rank: 7, WorldSize: 400, Retries: 8})
	f.Add(Signal{Type: SigRDMARetry, PID: 1, Comm: "nfsd", Rank: 7, WorldSize: 100, RNR: 4})
	st := only(t, f)
	if want := 12.0 / 500.0; st.RDMARetryRate != want {
		t.Errorf("rate = %v, want %v", st.RDMARetryRate, want)
	}
	if st.ScoreDelta != 0.35 {
		t.Errorf("score = %v, want 0.35", st.ScoreDelta)
	}
	// Errors with no post count cap at 1 instead of dividing by zero.
	f2, _ := newTestFolder()
	f2.Add(Signal{Type: SigRDMARetry, Retries: 3})
	if got := only(t, f2).RDMARetryRate; got != 1 {
		t.Errorf("no-post rate = %v, want 1", got)
	}
}

func TestFoldGDS(t *testing.T) {
	sigs := []Signal{
		{Type: SigGDS, Comm: "loader", Bytes: 100, Retries: 1},
		{Type: SigGDS, Comm: "loader", Bytes: 300, Retries: 0},
	}
	f, _ := newTestFolder()
	for _, s := range sigs {
		f.Add(s)
	}
	st := only(t, f)
	if st.GDSMeasured || st.GDSHitRatio != 0 || st.ScoreDelta != 0 {
		t.Errorf("without the nvidia-fs hook the ratio must stay unmeasured: %+v", st)
	}

	f, _ = newTestFolder()
	f.SetGDSDirectVisible(true)
	for _, s := range sigs {
		f.Add(s)
	}
	st = only(t, f)
	if !st.GDSMeasured || st.GDSHitRatio != 0.25 || st.ScoreDelta != 0.15 {
		t.Errorf("with the hook: %+v", st)
	}
}

func TestWindowExpiryAndAttribution(t *testing.T) {
	f, c := newTestFolder()
	f.Bind(10, "ml", "llama")
	f.Add(Signal{Type: SigStraggler, PID: 10, Rank: 1, LatencyNS: 9e6})
	f.Add(Signal{Type: SigStraggler, PID: 11, Rank: 2, LatencyNS: 9e6, Comm: "other"})
	snap := f.Snapshot()
	if _, ok := snap[JobKey{"ml", "llama"}]; !ok {
		t.Errorf("bound pid not attributed: %v", snap)
	}
	if _, ok := snap[JobKey{"_unattributed", "other"}]; !ok {
		t.Errorf("unbound pid not grouped by comm: %v", snap)
	}
	c.t = c.t.Add(30 * time.Second)
	f.Add(Signal{Type: SigStraggler, PID: 10, Rank: 5, LatencyNS: 9e6})
	c.t = c.t.Add(45 * time.Second) // first signals are now 75s old, the last 45s
	snap = f.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expired job must vanish: %v", snap)
	}
	if st := snap[JobKey{"ml", "llama"}]; st.StragglerHits != 1 || st.StragglerRank != 5 {
		t.Errorf("expired signals still counted: %+v", st)
	}
	c.t = c.t.Add(time.Hour)
	if len(f.Snapshot()) != 0 {
		t.Error("everything must expire")
	}
}

func TestSampleCap(t *testing.T) {
	f, _ := newTestFolder()
	for i := 0; i < maxSamples+50; i++ {
		f.Add(Signal{Type: SigStraggler, Comm: "x", LatencyNS: 1})
	}
	if st := only(t, f); st.StragglerHits != maxSamples {
		t.Errorf("hits = %d, want cap %d", st.StragglerHits, maxSamples)
	}
}

func TestServeHTTP(t *testing.T) {
	f, _ := newTestFolder()
	f.Add(Signal{Type: SigStraggler, PID: 1, Rank: 4, LatencyNS: 60e6, Comm: "train"})

	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/fabric", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["job"] != "train" || got[0]["stragglerRank"] != float64(4) || got[0]["scoreDelta"] != 0.25 {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/fabric", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status %d", rec.Code)
	}
}

func TestFoldOverlap(t *testing.T) {
	f, _ := newTestFolder() // 60 s window
	for i := 0; i < 4; i++ {
		f.Add(Signal{Type: SigOverlap, PID: 7, Comm: "train", LatencyNS: 5e9, Retries: 1})
	}
	st := only(t, f)
	if want := 20.0 / 60.0; st.OverlapIdleRatio < want-1e-9 || st.OverlapIdleRatio > want+1e-9 {
		t.Errorf("idle ratio = %v, want %v", st.OverlapIdleRatio, want)
	}
	if st.ScoreDelta != 0.15 {
		t.Errorf("score = %v, want 0.15", st.ScoreDelta)
	}
	// Many threads in sync at once cannot push the ratio past 1.
	f2, _ := newTestFolder()
	for i := 0; i < 50; i++ {
		f2.Add(Signal{Type: SigOverlap, Comm: "train", LatencyNS: 10e9})
	}
	if got := only(t, f2).OverlapIdleRatio; got != 1 {
		t.Errorf("capped ratio = %v, want 1", got)
	}
}

func TestFoldCNP(t *testing.T) {
	f, c := newTestFolder() // 60 s window
	f.AddCNP(0)             // ignored
	if len(f.Snapshot()) != 0 {
		t.Fatal("zero CNPs must not create a job")
	}
	f.AddCNP(3000)
	c.t = c.t.Add(10 * time.Second)
	f.AddCNP(3000)
	snap := f.Snapshot()
	st, ok := snap[CNPJob]
	if !ok || len(snap) != 1 {
		t.Fatalf("CNPs must fold under %v: %v", CNPJob, snap)
	}
	if st.CNPRate != 100 { // 6000 packets / 60 s window: at, not above, the threshold
		t.Errorf("rate = %v, want 100", st.CNPRate)
	}
	if st.ScoreDelta != 0 {
		t.Errorf("score at threshold = %v, want 0", st.ScoreDelta)
	}
	f.AddCNP(60)
	if st = f.Snapshot()[CNPJob]; st.CNPRate != 101 || st.ScoreDelta != 0.15 {
		t.Errorf("above threshold: %+v", st)
	}
	c.t = c.t.Add(2 * time.Minute)
	if len(f.Snapshot()) != 0 {
		t.Error("CNP samples must expire with the window")
	}
}

func TestFoldInferWait(t *testing.T) {
	f, _ := newTestFolder()
	for i := 1; i <= 100; i++ {
		f.Add(Signal{Type: SigInferWait, PID: 9, Rank: 8000, LatencyNS: uint64(i) * 1_000_000, Comm: "vllm"})
	}
	st := only(t, f)
	if st.InferWaitP99MS != 99 {
		t.Errorf("infer p99 = %v, want 99", st.InferWaitP99MS)
	}
	if st.ScoreDelta != 0 || st.StragglerHits != 0 || st.NCCLP99MS != 0 {
		t.Errorf("inference wait must not leak into the fabric score: %+v", st)
	}
}

func TestFoldPFC(t *testing.T) {
	f, c := newTestFolder() // 60 s window
	f.AddPFC(0)
	if len(f.Snapshot()) != 0 {
		t.Fatal("zero pause frames must not create a job")
	}
	f.AddPFC(30000)
	c.t = c.t.Add(10 * time.Second)
	f.AddPFC(30000)
	snap := f.Snapshot()
	st, ok := snap[PFCJob]
	if !ok || len(snap) != 1 {
		t.Fatalf("PFC must fold under %v: %v", PFCJob, snap)
	}
	if st.PFCRate != 1000 || st.ScoreDelta != 0 { // 60000 / 60 s: at, not above, the threshold
		t.Errorf("at threshold: %+v", st)
	}
	f.AddPFC(60)
	if st = f.Snapshot()[PFCJob]; st.PFCRate != 1001 || st.ScoreDelta != 0.10 {
		t.Errorf("above threshold: %+v", st)
	}
	if st.CNPRate != 0 || st.ExfilEvents != 0 {
		t.Errorf("PFC must not leak into other fields: %+v", st)
	}
	c.t = c.t.Add(2 * time.Minute)
	if len(f.Snapshot()) != 0 {
		t.Error("PFC samples must expire with the window")
	}
}

func TestFoldExfilDoesNotChangeScore(t *testing.T) {
	f, _ := newTestFolder()
	f.Bind(7, "ml", "train")
	for i := 0; i < 20; i++ {
		f.Add(Signal{Type: SigExfil, PID: 7, Bytes: 9 << 20, LatencyNS: 1e9, Rank: 443, PeerRank: 0xC0000201, Comm: "python3"})
	}
	st := f.Snapshot()[JobKey{Namespace: "ml", Job: "train"}]
	if st.ExfilEvents != 20 {
		t.Errorf("exfil events = %d, want 20", st.ExfilEvents)
	}
	if st.ScoreDelta != 0 || st.StragglerHits != 0 || st.NCCLP99MS != 0 || st.RDMARetryRate != 0 || st.CNPRate != 0 {
		t.Errorf("exfil must not change the score or any other field: %+v", st)
	}
}

func TestFoldUCXSlow(t *testing.T) {
	f, _ := newTestFolder()
	for i := 1; i <= 100; i++ {
		f.Add(Signal{Type: SigUCXSlow, PID: 9, LatencyNS: uint64(i) * 1_000_000, Comm: "ucx"})
	}
	st := only(t, f)
	if st.UCXSlowP99MS != 99 {
		t.Errorf("ucx p99 = %v, want 99", st.UCXSlowP99MS)
	}
	if st.ScoreDelta != 0 || st.StragglerHits != 0 || st.NCCLP99MS != 0 {
		t.Errorf("ucx must not leak into the NCCL straggler fields or score: %+v", st)
	}
}
