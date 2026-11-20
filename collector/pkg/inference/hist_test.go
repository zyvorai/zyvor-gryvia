// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package inference

import (
	"math"
	"testing"
	"time"
)

func close2(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func mk(bounds []float64, cum []float64) Hist {
	h := Hist{}
	for i := range bounds {
		h.Buckets = append(h.Buckets, Bucket{Le: bounds[i], Count: cum[i]})
	}
	h.Total = cum[len(cum)-1]
	return h
}

var inf = math.Inf(1)

func TestQuantileInterpolation(t *testing.T) {
	// 100 observations: 50 in (0,1], 30 in (1,2], 20 in (2,4].
	h := mk([]float64{1, 2, 4, inf}, []float64{50, 80, 100, 100})
	cases := []struct {
		q, want float64
	}{
		{0.5, 1},    // rank 50 is the top of the first bucket
		{0.25, 0.5}, // rank 25: halfway through (0,1]
		{0.65, 1.5}, // rank 65: halfway through (1,2]
		{0.9, 3},    // rank 90: halfway through (2,4]
		{0.99, 3.9}, // rank 99
		{1, 4},      // rank 100
		{0.8, 2},    // rank 80 lands on a bucket edge
	}
	for _, c := range cases {
		got, ok := h.Quantile(c.q)
		if !ok || !close2(got, c.want) {
			t.Errorf("q=%v got %v ok=%v want %v", c.q, got, ok, c.want)
		}
	}
}

func TestQuantileInfBucketAndEdges(t *testing.T) {
	// 10% of observations are above the last finite bound: the answer is that bound.
	h := mk([]float64{1, 2, inf}, []float64{50, 90, 100})
	if got, ok := h.Quantile(0.99); !ok || got != 2 {
		t.Errorf("inf bucket: got %v ok=%v want 2", got, ok)
	}
	if _, ok := (Hist{}).Quantile(0.5); ok {
		t.Error("empty histogram must not report a quantile")
	}
	if _, ok := mk([]float64{1, inf}, []float64{0, 0}).Quantile(0.5); ok {
		t.Error("zero observations must not report a quantile")
	}
	if _, ok := mk([]float64{inf}, []float64{5}).Quantile(0.5); ok {
		t.Error("only a +Inf bucket has no scale")
	}
	if _, ok := h.Quantile(1.5); ok {
		t.Error("q out of range")
	}
}

func TestDeltaAndResets(t *testing.T) {
	prev := mk([]float64{1, 2, inf}, []float64{10, 20, 30})
	cur := mk([]float64{1, 2, inf}, []float64{15, 40, 60})
	d, ok := Delta(cur, prev)
	if !ok || d.Total != 30 || d.Buckets[0].Count != 5 || d.Buckets[1].Count != 20 || d.Buckets[2].Count != 30 {
		t.Fatalf("delta = %+v ok=%v", d, ok)
	}
	// Counter reset: the total went down.
	if _, ok := Delta(mk([]float64{1, 2, inf}, []float64{1, 2, 3}), prev); ok {
		t.Error("reset must be reported")
	}
	// One bucket went backwards while the total did not.
	if _, ok := Delta(mk([]float64{1, 2, inf}, []float64{5, 40, 60}), prev); ok {
		t.Error("bucket regression must be reported")
	}
	// Layout change (engine upgraded with new buckets).
	if _, ok := Delta(mk([]float64{1, 3, inf}, []float64{15, 40, 60}), prev); ok {
		t.Error("layout change must be reported")
	}
}

func TestMergeSeries(t *testing.T) {
	a := &Series{Buckets: []Bucket{{1, 1}, {2, 3}, {inf, 4}}, Sum: 5}
	b := &Series{Buckets: []Bucket{{1, 2}, {2, 2}, {inf, 2}}, Sum: 1}
	h, ok := MergeSeries([]*Series{a, b, {}})
	if !ok || h.Total != 6 || h.Buckets[0].Count != 3 || h.Buckets[1].Count != 5 || h.Sum != 6 {
		t.Fatalf("merged = %+v", h)
	}
	// Different bounds: b has an extra 1.5 bucket; a's cumulative count carries over.
	c := &Series{Buckets: []Bucket{{1, 2}, {1.5, 4}, {2, 4}, {inf, 4}}}
	h, _ = MergeSeries([]*Series{a, c})
	// le=1: 1+2; le=1.5: a carries its le=1 count (1) + 4; le=2: 3+4; +Inf: 4+4.
	want := []float64{3, 5, 7, 8}
	for i, w := range want {
		if h.Buckets[i].Count != w {
			t.Errorf("bucket %d = %v want %v (%+v)", i, h.Buckets[i].Count, w, h.Buckets)
		}
	}
	if _, ok := MergeSeries(nil); ok {
		t.Error("no series")
	}
}

func TestWindowBaselineDeltaAndExpiry(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	w := NewWindow(5 * time.Minute)
	bounds := []float64{1, 2, inf}
	// First sample is only a baseline, even though it already holds 1000 observations.
	w.Observe(t0, mk(bounds, []float64{1000, 1000, 1000}))
	if _, ok := w.Quantile(t0, 0.99); ok {
		t.Fatal("baseline must not produce a quantile")
	}
	// 100 new observations, 99 fast and 1 slow.
	w.Observe(t0.Add(15*time.Second), mk(bounds, []float64{1099, 1100, 1100}))
	q, ok := w.Quantile(t0.Add(15*time.Second), 0.99)
	if !ok || !close2(q, 1.0+(0.99*100-99)/1*1) { // rank 99 sits at the top of the first bucket (99/99)
		t.Errorf("q = %v ok=%v", q, ok)
	}
	// A reset (counters back to small numbers) drops history and re-baselines.
	w.Observe(t0.Add(30*time.Second), mk(bounds, []float64{2, 3, 3}))
	if _, ok := w.Quantile(t0.Add(30*time.Second), 0.99); ok {
		t.Error("after a reset there is no data until the next interval")
	}
	w.Observe(t0.Add(45*time.Second), mk(bounds, []float64{2, 13, 13}))
	if q, ok := w.Quantile(t0.Add(45*time.Second), 0.5); !ok || !close2(q, 1.5) {
		t.Errorf("post-reset q = %v ok=%v", q, ok)
	}
	// The interval ages out of the window.
	if _, ok := w.Quantile(t0.Add(45*time.Second+6*time.Minute), 0.5); ok {
		t.Error("expired deltas must be dropped")
	}
}

func TestWindowIdleIntervalsKeepEarlierDeltas(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	w := NewWindow(5 * time.Minute)
	b := []float64{1, inf}
	w.Observe(t0, mk(b, []float64{0, 0}))
	w.Observe(t0.Add(15*time.Second), mk(b, []float64{10, 10}))
	// No traffic for two intervals: the window still answers from the first delta.
	w.Observe(t0.Add(30*time.Second), mk(b, []float64{10, 10}))
	w.Observe(t0.Add(45*time.Second), mk(b, []float64{10, 10}))
	if q, ok := w.Quantile(t0.Add(45*time.Second), 0.5); !ok || !close2(q, 0.5) {
		t.Errorf("q = %v ok=%v", q, ok)
	}
}

func TestCounterWindowMean(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	c := NewCounterWindow(time.Minute)
	c.Observe(t0, 100, 1000)
	if _, ok := c.Mean(t0); ok {
		t.Fatal("baseline")
	}
	c.Observe(t0.Add(10*time.Second), 110, 1300) // 10 requests, 300 us
	c.Observe(t0.Add(20*time.Second), 110, 1300) // idle
	if m, ok := c.Mean(t0.Add(20 * time.Second)); !ok || m != 30 {
		t.Errorf("mean = %v ok=%v", m, ok)
	}
	c.Observe(t0.Add(30*time.Second), 5, 50) // reset
	if _, ok := c.Mean(t0.Add(30 * time.Second)); ok {
		t.Error("reset drops history")
	}
	c.Observe(t0.Add(40*time.Second), 15, 150)
	if m, ok := c.Mean(t0.Add(40 * time.Second)); !ok || m != 10 {
		t.Errorf("mean = %v ok=%v", m, ok)
	}
}
