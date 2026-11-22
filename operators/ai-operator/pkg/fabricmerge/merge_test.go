// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabricmerge

import (
	"math"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

var t0 = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

func f(v float64) *float64 { return &v }
func i32(v int32) *int32   { return &v }
func i64(v int64) *int64   { return &v }
func str(v string) *string { return &v }

func node(name string, age time.Duration, samples int64) gryviav1.FabricNodeStatus {
	return gryviav1.FabricNodeStatus{Node: name, MeasuredAt: metav1.NewTime(t0.Add(-age)), SampleCount: samples}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestMergeMaxForDegradationMetrics(t *testing.T) {
	a, b := node("a", 10*time.Second, 5), node("b", 20*time.Second, 5)
	a.ScoreDelta, b.ScoreDelta = f(0.25), f(0.5)
	a.NCCLP99Ms, b.NCCLP99Ms = f(90), f(60)
	a.RDMARetryRate = f(0.1)
	b.PFCRate, a.CNPRate = f(7), f(9)
	a.ExfilEvents, b.ExfilEvents = i64(2), i64(5)
	b.CollectiveMaxSkewMs = f(3)
	a.InferWaitP99Ms = f(4)
	a.TTFTP99Ms, b.TTFTP99Ms = f(100), f(300)
	a.ITLP99Ms = f(11)
	b.QueueTimeP99Ms, b.E2EP99Ms = f(12), f(13)
	a.RequestsWaiting, b.RequestsWaiting = i64(2), i64(9)
	a.KVCacheUsage, b.KVCacheUsage = f(0.4), f(0.8)
	got, ok := Merge(t0, []gryviav1.FabricNodeStatus{a, b})
	if !ok {
		t.Fatal("not ok")
	}
	if got.ScoreDelta != 0.5 || got.NCCLP99Ms != 90 || got.RDMARetryRate != 0.1 || got.PFCRate != 7 || got.CNPRate != 9 ||
		got.ExfilEvents != 5 || got.CollectiveMaxSkewMs != 3 || got.InferWaitP99Ms != 4 ||
		*got.TTFTP99Ms != 300 || *got.ITLP99Ms != 11 || *got.QueueTimeP99Ms != 12 || *got.E2EP99Ms != 13 ||
		*got.RequestsWaiting != 9 || *got.KVCacheUsage != 0.8 {
		t.Errorf("%+v", got)
	}
	if got.QueueTimeMeanMs != nil || got.E2EMeanMs != nil {
		t.Error("unmeasured means must stay nil")
	}
}

func TestMergePartialMeasurementKeepsOtherNodesValues(t *testing.T) {
	a := node("a", time.Second, 10)
	a.NCCLP99Ms, a.StragglerRank = f(80), i32(3)
	b := node("b", time.Second, 10) // measured nothing: must not erase a's values
	got, _ := Merge(t0, []gryviav1.FabricNodeStatus{b, a})
	if got.NCCLP99Ms != 80 || got.StragglerRank != 3 {
		t.Errorf("%+v", got)
	}
}

func TestMergeWeightedMeans(t *testing.T) {
	a, b, c := node("a", 0, 30), node("b", 0, 10), node("c", 0, 1000)
	a.GDSHitRatio, b.GDSHitRatio = f(0.2), f(1) // c did not measure it: skipped
	a.OverlapIdleRatio = f(0.5)
	b.GPUIdleDuringCommRatio, c.GPUIdleDuringCommRatio = f(0.1), f(0.9)
	a.SMActiveDuringCompute = f(0.7)
	a.GPUCorrelationCoverage, b.GPUCorrelationCoverage = f(1), f(0)
	zero := node("z", 0, 0) // sampleCount 0 weighs 1
	zero.GDSHitRatio = f(0.6)
	got, _ := Merge(t0, []gryviav1.FabricNodeStatus{a, b, c, zero})
	if want := (0.2*30 + 1*10 + 0.6*1) / 41; !near(got.GDSHitRatio, want) {
		t.Errorf("gds %v want %v", got.GDSHitRatio, want)
	}
	if !near(got.OverlapIdleRatio, 0.5) || !near(got.SMActiveDuringCompute, 0.7) {
		t.Errorf("single-node means: %+v", got)
	}
	if want := (0.1*10 + 0.9*1000) / 1010; !near(got.GPUIdleDuringCommRatio, want) {
		t.Errorf("idle %v want %v", got.GPUIdleDuringCommRatio, want)
	}
	if want := 30.0 / 40; !near(got.GPUCorrelationCoverage, want) {
		t.Errorf("coverage %v want %v", got.GPUCorrelationCoverage, want)
	}
}

func TestMergeStragglerFromWorstNCCLNode(t *testing.T) {
	a, b, c := node("a", 0, 1), node("b", 0, 1), node("c", 0, 1)
	a.StragglerRank, a.NCCLP99Ms = i32(1), f(60)
	b.StragglerRank, b.NCCLP99Ms = i32(7), f(120)
	c.StragglerRank, c.NCCLP99Ms = i32(9), f(120) // tie with b: lowest node name wins
	for _, order := range [][]gryviav1.FabricNodeStatus{{a, b, c}, {c, b, a}} {
		got, _ := Merge(t0, order)
		if got.StragglerRank != 7 || got.NCCLP99Ms != 120 {
			t.Errorf("%+v", got)
		}
	}
	// A rank without any NCCL figure falls back to the newest entry with a rank.
	d, e := node("d", 30*time.Second, 1), node("e", 5*time.Second, 1)
	d.StragglerRank, e.StragglerRank = i32(2), i32(4)
	got, _ := Merge(t0, []gryviav1.FabricNodeStatus{d, e})
	if got.StragglerRank != 4 {
		t.Errorf("fallback rank %d", got.StragglerRank)
	}
}

func TestMergeStaleNodes(t *testing.T) {
	fresh, stale := node("fresh", 100*time.Second, 1), node("stale", 400*time.Second, 1)
	fresh.NCCLP99Ms, stale.NCCLP99Ms = f(10), f(500)
	stale.ScoreDelta = f(0.9)
	got, ok := Merge(t0, []gryviav1.FabricNodeStatus{fresh, stale})
	if !ok || got.NCCLP99Ms != 10 || got.ScoreDelta != 0 {
		t.Errorf("stale entry leaked: %+v", got)
	}
	// Per-entry TTL is honoured.
	short := node("short", 30*time.Second, 1)
	short.TTLSeconds, short.NCCLP99Ms = 20, f(70)
	if _, ok := Merge(t0, []gryviav1.FabricNodeStatus{short}); ok {
		t.Error("entry past its own ttl must be stale")
	}
	long := node("long", 400*time.Second, 1)
	long.TTLSeconds, long.NCCLP99Ms = 600, f(70)
	if got, ok := Merge(t0, []gryviav1.FabricNodeStatus{long}); !ok || got.NCCLP99Ms != 70 {
		t.Error("long ttl entry must be fresh")
	}
}

func TestMergeAllStaleOrEmptyIsNotOK(t *testing.T) {
	a := node("a", time.Hour, 1)
	a.NCCLP99Ms = f(99)
	if _, ok := Merge(t0, []gryviav1.FabricNodeStatus{a}); ok {
		t.Error("all stale must not merge")
	}
	if _, ok := Merge(t0, nil); ok {
		t.Error("no nodes must not merge")
	}
	if _, ok := Merge(t0, []gryviav1.FabricNodeStatus{{Node: "x"}}); ok {
		t.Error("entry without measuredAt is stale")
	}
}

func TestMergeUpdatedAtEngineAndNodesKept(t *testing.T) {
	a, b := node("a", 40*time.Second, 1), node("b", 5*time.Second, 1)
	a.Engine, b.Engine = str("vllm"), str("vllm")
	got, _ := Merge(t0, []gryviav1.FabricNodeStatus{a, b})
	if !got.UpdatedAt.Time.Equal(t0.Add(-5*time.Second)) || got.Engine != "vllm" || len(got.Nodes) != 2 {
		t.Errorf("%+v", got)
	}
	b.Engine = str("triton")
	got, _ = Merge(t0, []gryviav1.FabricNodeStatus{a, b})
	if got.Engine != "mixed" {
		t.Errorf("engine %q", got.Engine)
	}
}

func TestExpiryAndApply(t *testing.T) {
	n := node("a", 0, 1)
	if !Expiry(n).Equal(t0.Add(DefaultTTL)) {
		t.Error("default ttl")
	}
	st := gryviav1.GryviaFabricSignalStatus{NCCLP99Ms: 5, ScoreDelta: 0.3, Nodes: []gryviav1.FabricNodeStatus{n}}
	Apply(&st, gryviav1.GryviaFabricSignalStatus{RDMARetryRate: 0.2})
	if st.NCCLP99Ms != 0 || st.ScoreDelta != 0 || st.RDMARetryRate != 0.2 || len(st.Nodes) != 1 {
		t.Errorf("%+v", st)
	}
}
