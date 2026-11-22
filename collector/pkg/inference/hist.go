// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package inference

import (
	"math"
	"sort"
	"time"
)

// Hist is a cumulative histogram: Buckets are sorted by Le and each Count includes every lower
// bucket. Total is the observation count (the +Inf bucket, or the _count sample).
type Hist struct {
	Buckets []Bucket
	Total   float64
	Sum     float64
}

// MergeSeries adds up the histogram series of a family (for example one per model_name) into one
// histogram over the union of bucket bounds. Series of one engine share their bounds; if they do
// not, each series is first extended to the union with the cumulative count of its next-lower
// bucket, which keeps the result monotonic.
func MergeSeries(series []*Series) (Hist, bool) {
	bounds := map[float64]struct{}{}
	n := 0
	for _, s := range series {
		if len(s.Buckets) == 0 {
			continue
		}
		n++
		for _, b := range s.Buckets {
			bounds[b.Le] = struct{}{}
		}
	}
	if n == 0 {
		return Hist{}, false
	}
	les := make([]float64, 0, len(bounds))
	for le := range bounds {
		les = append(les, le)
	}
	sort.Float64s(les)
	out := Hist{Buckets: make([]Bucket, len(les))}
	for i, le := range les {
		out.Buckets[i].Le = le
	}
	for _, s := range series {
		if len(s.Buckets) == 0 {
			continue
		}
		j, carry := 0, 0.0
		for i, le := range les {
			for j < len(s.Buckets) && s.Buckets[j].Le <= le {
				carry = s.Buckets[j].Count
				j++
			}
			out.Buckets[i].Count += carry
		}
		out.Sum += s.Sum
	}
	out.Total = out.Buckets[len(out.Buckets)-1].Count
	return out, true
}

// sameLayout reports whether two histograms have identical bucket bounds.
func sameLayout(a, b Hist) bool {
	if len(a.Buckets) != len(b.Buckets) {
		return false
	}
	for i := range a.Buckets {
		if a.Buckets[i].Le != b.Buckets[i].Le {
			return false
		}
	}
	return true
}

// Delta returns cur - prev. ok is false when the counters cannot be subtracted: a counter went
// backwards (the engine restarted) or the bucket layout changed. The caller must then treat cur
// as a new baseline.
func Delta(cur, prev Hist) (Hist, bool) {
	if !sameLayout(cur, prev) || cur.Total < prev.Total {
		return Hist{}, false
	}
	out := Hist{Buckets: make([]Bucket, len(cur.Buckets)), Total: cur.Total - prev.Total, Sum: cur.Sum - prev.Sum}
	for i := range cur.Buckets {
		d := cur.Buckets[i].Count - prev.Buckets[i].Count
		if d < 0 {
			return Hist{}, false
		}
		out.Buckets[i] = Bucket{Le: cur.Buckets[i].Le, Count: d}
	}
	return out, true
}

// Quantile estimates the q-quantile (0..1) by linear interpolation inside the bucket that holds
// the rank, the same method as PromQL's histogram_quantile. It returns false when the histogram
// has no observations. A rank that falls in the +Inf bucket returns the highest finite bound (the
// true value is at least that).
func (h Hist) Quantile(q float64) (float64, bool) {
	if len(h.Buckets) == 0 || h.Total <= 0 || q < 0 || q > 1 || math.IsNaN(q) {
		return 0, false
	}
	rank := q * h.Total
	i := sort.Search(len(h.Buckets), func(i int) bool { return h.Buckets[i].Count >= rank })
	if i >= len(h.Buckets) {
		i = len(h.Buckets) - 1
	}
	b := h.Buckets[i]
	if math.IsInf(b.Le, 1) {
		if i == 0 {
			return 0, false // only a +Inf bucket: no scale at all
		}
		return h.Buckets[i-1].Le, true
	}
	lower, prevCount := 0.0, 0.0
	if i > 0 {
		lower, prevCount = h.Buckets[i-1].Le, h.Buckets[i-1].Count
	} else if b.Le <= 0 {
		return b.Le, true
	}
	inBucket := b.Count - prevCount
	if inBucket <= 0 {
		return b.Le, true
	}
	return lower + (b.Le-lower)*(rank-prevCount)/inBucket, true
}

// Mean is Sum/Total of the histogram.
func (h Hist) Mean() (float64, bool) {
	if h.Total <= 0 {
		return 0, false
	}
	return h.Sum / h.Total, true
}

type deltaAt struct {
	at time.Time
	h  Hist
}

// Window turns successive cumulative scrapes of one histogram into quantiles over a sliding
// time window of per-interval deltas. The first scrape is only a baseline; a counter reset or a
// bucket layout change re-baselines (the interval is dropped rather than guessed).
type Window struct {
	span   time.Duration
	prev   *Hist
	deltas []deltaAt
}

// NewWindow creates a Window that keeps deltas for span.
func NewWindow(span time.Duration) *Window { return &Window{span: span} }

// Observe records one scrape taken at now.
func (w *Window) Observe(now time.Time, cur Hist) {
	if w.prev != nil {
		if d, ok := Delta(cur, *w.prev); ok {
			if d.Total > 0 {
				w.deltas = append(w.deltas, deltaAt{now, d})
			}
		} else {
			w.deltas = w.deltas[:0] // reset: earlier deltas belong to another engine lifetime
		}
	}
	c := cur
	w.prev = &c
	w.expire(now)
}

func (w *Window) expire(now time.Time) {
	cut := now.Add(-w.span)
	i := 0
	for i < len(w.deltas) && w.deltas[i].at.Before(cut) {
		i++
	}
	w.deltas = w.deltas[i:]
}

// Sum is the total of the deltas inside the window.
func (w *Window) Sum(now time.Time) (Hist, bool) {
	w.expire(now)
	if len(w.deltas) == 0 {
		return Hist{}, false
	}
	out := Hist{Buckets: make([]Bucket, len(w.deltas[0].h.Buckets))}
	for i, b := range w.deltas[0].h.Buckets {
		out.Buckets[i].Le = b.Le
	}
	for _, d := range w.deltas {
		if !sameLayout(out, d.h) {
			continue
		}
		out.Total += d.h.Total
		out.Sum += d.h.Sum
		for i, b := range d.h.Buckets {
			out.Buckets[i].Count += b.Count
		}
	}
	return out, out.Total > 0
}

// Quantile is the q-quantile of the observations inside the window.
func (w *Window) Quantile(now time.Time, q float64) (float64, bool) {
	h, ok := w.Sum(now)
	if !ok {
		return 0, false
	}
	return h.Quantile(q)
}

// CounterWindow does the same for a pair of cumulative counters (for example Triton's total
// duration in microseconds and its request count), yielding a mean over the window.
type CounterWindow struct {
	span         time.Duration
	prevN, prevD float64
	have         bool
	pts          []counterAt
}

type counterAt struct {
	at   time.Time
	n, d float64
}

// NewCounterWindow creates a CounterWindow that keeps deltas for span.
func NewCounterWindow(span time.Duration) *CounterWindow { return &CounterWindow{span: span} }

// Observe records the cumulative count n and cumulative total d at now.
func (c *CounterWindow) Observe(now time.Time, n, d float64) {
	if c.have {
		if n < c.prevN || d < c.prevD {
			c.pts = c.pts[:0] // reset
		} else if n > c.prevN {
			c.pts = append(c.pts, counterAt{now, n - c.prevN, d - c.prevD})
		}
	}
	c.prevN, c.prevD, c.have = n, d, true
	c.expire(now)
}

func (c *CounterWindow) expire(now time.Time) {
	cut := now.Add(-c.span)
	i := 0
	for i < len(c.pts) && c.pts[i].at.Before(cut) {
		i++
	}
	c.pts = c.pts[i:]
}

// Mean is total/count over the window; false when no request completed in it.
func (c *CounterWindow) Mean(now time.Time) (float64, bool) {
	c.expire(now)
	var n, d float64
	for _, p := range c.pts {
		n += p.n
		d += p.d
	}
	if n <= 0 {
		return 0, false
	}
	return d / n, true
}
