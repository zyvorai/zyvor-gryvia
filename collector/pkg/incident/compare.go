// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package incident

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Window is a resolved comparison window.
type Window struct {
	Label string    `json:"label"` // the incident id or the timestamp as given
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	// Samples is how many stored samples fell in the window; Source says where the
	// values came from: "samples" (mean of the samples), "incident-peak" (no samples
	// in the window: the incident's peak values), or "none".
	Samples int    `json:"samples"`
	Source  string `json:"source"`
}

// MetricDelta compares one metric between windows a and b (Delta = B - A).
type MetricDelta struct {
	Metric string   `json:"metric"`
	A      *float64 `json:"a"`
	B      *float64 `json:"b"`
	Delta  *float64 `json:"delta"`
	// PercentChange is nil when A is zero or a side is missing.
	PercentChange *float64 `json:"percentChange"`
	Note          string   `json:"note,omitempty"`
}

// Comparison is the result of Compare.
type Comparison struct {
	Namespace string        `json:"namespace"`
	Job       string        `json:"job"`
	A         Window        `json:"a"`
	B         Window        `json:"b"`
	Metrics   []MetricDelta `json:"metrics"`
	Notes     []string      `json:"notes"`
}

// ResolveWindow interprets spec: an incident id of this job (window = its life,
// open incidents run to now), an RFC3339 time (window = [t-def, t]) or "now".
func (s *Store) ResolveWindow(ns, job, spec string, def time.Duration, now time.Time) (Window, *Incident, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || len(spec) > 64 {
		return Window{}, nil, errors.New("window spec must be an incident id or an RFC3339 time")
	}
	if strings.HasPrefix(spec, "inc-") {
		in, ok := s.Get(spec)
		if !ok || in.Namespace != ns || in.Job != job {
			return Window{}, nil, fmt.Errorf("incident %q not found for this job", spec)
		}
		to := in.End
		if in.Open || to.IsZero() {
			to = now
		}
		return Window{Label: spec, From: in.Start, To: to}, &in, nil
	}
	var at time.Time
	if spec == "now" {
		at = now
	} else {
		t, err := time.Parse(time.RFC3339, spec)
		if err != nil {
			return Window{}, nil, errors.New("window spec must be an incident id or an RFC3339 time")
		}
		at = t
	}
	return Window{Label: spec, From: at.Add(-def), To: at}, nil, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// windowValues returns the metric values of a window and fills its Samples and Source.
func (s *Store) windowValues(ns, job string, w *Window, in *Incident) map[string]float64 {
	samples := s.Samples(ns, job, w.From, w.To)
	w.Samples = len(samples)
	if len(samples) > 0 {
		sum, cnt := map[string]float64{}, map[string]int{}
		for _, sm := range samples {
			for k, v := range sm.Metrics {
				if finite(v) {
					sum[k] += v
					cnt[k]++
				}
			}
		}
		out := make(map[string]float64, len(sum))
		for k, v := range sum {
			out[k] = v / float64(cnt[k])
		}
		w.Source = "samples"
		return out
	}
	if in != nil && len(in.Peak) > 0 {
		out := make(map[string]float64, len(in.Peak))
		for k, v := range in.Peak {
			if finite(v) {
				out[k] = v
			}
		}
		w.Source = "incident-peak"
		return out
	}
	w.Source = "none"
	return nil
}

// Compare returns per-metric deltas between two windows of a job. A metric that
// was measured in only one window is listed with the other side null: it is not
// treated as zero.
func (s *Store) Compare(ns, job string, a, b Window, ia, ib *Incident) Comparison {
	va, vb := s.windowValues(ns, job, &a, ia), s.windowValues(ns, job, &b, ib)
	names := map[string]bool{}
	for k := range va {
		names[k] = true
	}
	for k := range vb {
		names[k] = true
	}
	keys := make([]string, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c := Comparison{Namespace: ns, Job: job, A: a, B: b, Metrics: []MetricDelta{}, Notes: []string{
		"sample values are the collector's rolling-window measurements taken about once a minute; a window's value is the mean of the samples inside it",
	}}
	for _, k := range keys {
		md := MetricDelta{Metric: k}
		x, okA := va[k]
		y, okB := vb[k]
		if okA {
			md.A = &x
		}
		if okB {
			md.B = &y
		}
		switch {
		case okA && okB:
			d := y - x
			md.Delta = &d
			if x != 0 {
				p := d / math.Abs(x) * 100
				if finite(p) {
					md.PercentChange = &p
				}
			}
		case okA:
			md.Note = "not measured in window b"
		default:
			md.Note = "not measured in window a"
		}
		c.Metrics = append(c.Metrics, md)
	}
	if a.Source == "none" || b.Source == "none" {
		c.Notes = append(c.Notes, "a window with source \"none\" has no stored samples (store disabled, retention passed, or the job had nothing measured then): nothing can be said about it")
	}
	if a.Source == "incident-peak" || b.Source == "incident-peak" {
		c.Notes = append(c.Notes, "\"incident-peak\" values are peaks, not means: compare with care")
	}
	return c
}
