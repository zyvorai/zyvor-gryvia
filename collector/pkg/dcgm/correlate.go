// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package dcgm

import (
	"sort"
	"time"
)

// Window is one interval during which an NCCL collective call was in flight on
// a rank of the job, as observed by the host-side uprobes (not GPU time).
type Window struct{ Start, End time.Time }

// DefaultIdleThreshold: activity (SM_ACTIVE, or GPU_UTIL/100) below this counts as idle.
const DefaultIdleThreshold = 0.10

// maxHold bounds how far back a sample is considered representative: a sample
// at t stands for (t - min(spacing to the previous sample, maxHold), t].
const maxHold = 10 * time.Second

// Result of correlating comm windows with the job's GPU samples.
type Result struct {
	// Measured is false when there were no windows, no samples or zero overlap.
	Measured bool
	// Source is the DCGM field used as activity ("DCGM_FI_PROF_SM_ACTIVE" or "DCGM_FI_DEV_GPU_UTIL").
	Source string
	// CommSeconds is the union of the in-flight windows; CoveredSeconds the
	// part of it that lies inside a sample's representative interval.
	CommSeconds    float64
	CoveredSeconds float64
	// Coverage = CoveredSeconds / CommSeconds.
	Coverage float64
	// IdleDuringComm is the fraction of covered comm time with activity below
	// the threshold (GPU idle while waiting on communication). Its complement
	// is the share of communication time overlapped with GPU work.
	IdleDuringComm float64
	// ActiveDuringComm / ActiveDuringCompute are time-weighted mean activity
	// inside / outside the comm windows (compute = the rest of the span from
	// the first to the last window). ComputeMeasured says the latter had data.
	ActiveDuringComm    float64
	ActiveDuringCompute float64
	ComputeMeasured     bool
}

type interval struct{ a, b time.Time }

// union merges overlapping/adjacent windows (sorted output).
func union(ws []Window) []interval {
	iv := make([]interval, 0, len(ws))
	for _, w := range ws {
		if w.End.After(w.Start) {
			iv = append(iv, interval{w.Start, w.End})
		}
	}
	sort.Slice(iv, func(i, j int) bool { return iv[i].a.Before(iv[j].a) })
	var out []interval
	for _, x := range iv {
		if n := len(out); n > 0 && !x.a.After(out[n-1].b) {
			if x.b.After(out[n-1].b) {
				out[n-1].b = x.b
			}
			continue
		}
		out = append(out, x)
	}
	return out
}

func overlap(a, b interval) time.Duration {
	lo, hi := a.a, a.b
	if b.a.After(lo) {
		lo = b.a
	}
	if b.b.Before(hi) {
		hi = b.b
	}
	if hi.After(lo) {
		return hi.Sub(lo)
	}
	return 0
}

// point is the per-timestamp mean activity across the job's GPUs.
type point struct {
	at  time.Time
	act float64
}

func meanByTime(samples []GPUSample) ([]point, string) {
	type acc struct {
		sum float64
		n   int
	}
	by := map[time.Time]*acc{}
	src := ""
	for _, s := range samples {
		v, from, ok := s.Activity()
		if !ok {
			continue
		}
		if src == "" || from == FieldSMActive {
			src = from // prefer SM_ACTIVE if any sample has it
		}
		a := by[s.At]
		if a == nil {
			a = &acc{}
			by[s.At] = a
		}
		a.sum += v
		a.n++
	}
	pts := make([]point, 0, len(by))
	for t, a := range by {
		pts = append(pts, point{t, a.sum / float64(a.n)})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].at.Before(pts[j].at) })
	return pts, src
}

// Correlate computes the Result for one job. samples are the job's GPUs only
// (attribute them first); windows may overlap (several ranks). threshold <= 0
// means DefaultIdleThreshold.
func Correlate(windows []Window, samples []GPUSample, threshold float64) Result {
	if threshold <= 0 {
		threshold = DefaultIdleThreshold
	}
	comm := union(windows)
	pts, src := meanByTime(samples)
	if len(comm) == 0 || len(pts) == 0 {
		return Result{}
	}
	var commTotal time.Duration
	for _, c := range comm {
		commTotal += c.b.Sub(c.a)
	}
	span := interval{comm[0].a, comm[len(comm)-1].b}

	var covered, idle, actComm, computeCovered, actCompute float64
	for i, p := range pts {
		hold := maxHold
		if i > 0 {
			if d := p.at.Sub(pts[i-1].at); d < hold {
				hold = d
			}
		}
		si := interval{p.at.Add(-hold), p.at}
		var c time.Duration
		for _, w := range comm {
			c += overlap(si, w)
		}
		if c > 0 {
			covered += c.Seconds()
			actComm += p.act * c.Seconds()
			if p.act < threshold {
				idle += c.Seconds()
			}
		}
		if inSpan := overlap(si, span); inSpan > c {
			comp := (inSpan - c).Seconds()
			computeCovered += comp
			actCompute += p.act * comp
		}
	}
	r := Result{Source: src, CommSeconds: commTotal.Seconds(), CoveredSeconds: covered}
	if commTotal > 0 {
		r.Coverage = covered / commTotal.Seconds()
	}
	if covered <= 0 {
		return r
	}
	r.Measured = true
	r.IdleDuringComm = idle / covered
	r.ActiveDuringComm = actComm / covered
	if computeCovered > 0 {
		r.ComputeMeasured = true
		r.ActiveDuringCompute = actCompute / computeCovered
	}
	return r
}
