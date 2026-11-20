// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package inference

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Engine names.
const (
	EngineVLLM   = "vllm"
	EngineTriton = "triton"
	EngineTGI    = "tgi"
)

// DefaultWindow is how long per-interval deltas contribute to a quantile.
const DefaultWindow = 5 * time.Minute

// Reading is one engine's view after a scrape. A nil pointer means "not measured": the metric is
// missing from this engine version, or (for quantiles) no request completed inside the window
// yet. It is never a zero that could be mistaken for a fast engine.
//
// Latencies are milliseconds. P99 fields are quantiles of the engine's own histograms (or of a
// Triton summary); the Mean fields exist for engines that only export cumulative counters
// (Triton without summary_latencies) and are means, never to be read as a p99.
type Reading struct {
	Engine string
	At     time.Time

	TTFTP99    *float64 // time to first token
	ITLP99     *float64 // inter-token latency (time per output token)
	QueueP99   *float64 // engine queue time (admission to start of execution)
	E2EP99     *float64 // end-to-end request latency inside the engine
	PrefillP99 *float64
	DecodeP99  *float64

	QueueMean   *float64 // mean over the window; counter-only engines
	E2EMean     *float64
	ComputeMean *float64

	Running   *float64 // requests executing
	Waiting   *float64 // requests queued
	KVCache   *float64 // KV cache usage, 0..1
	Available []string // logical metrics found in the exposition
	Missing   []string // logical metrics this engine could export but did not
	Notes     []string // caveats about how a figure was derived
}

// Engine turns successive expositions of one target into Readings. An Engine holds per-target
// state (the previous cumulative histograms) so it must not be shared between targets.
type Engine interface {
	Name() string
	// Detect reports whether the exposition looks like this engine's.
	Detect(Families) bool
	// Observe folds one scrape taken at now.
	Observe(now time.Time, f Families) Reading
}

// NewEngine builds an engine by name. window <= 0 means DefaultWindow.
func NewEngine(name string, window time.Duration) (Engine, error) {
	if window <= 0 {
		window = DefaultWindow
	}
	switch strings.ToLower(name) {
	case EngineVLLM:
		return newVLLM(window), nil
	case EngineTriton:
		return newTriton(window), nil
	case EngineTGI:
		return newTGI(window), nil
	}
	return nil, fmt.Errorf("unknown engine %q (want vllm, triton or tgi)", name)
}

// DetectEngine guesses the engine from metric name prefixes; "" when unknown.
func DetectEngine(f Families) string {
	for _, name := range []string{EngineVLLM, EngineTriton, EngineTGI} {
		e, _ := NewEngine(name, 0)
		if e.Detect(f) {
			return name
		}
	}
	return ""
}

func anyPrefix(f Families, prefix string) bool {
	for n := range f {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// first returns the first family of names that exists and has series.
func first(f Families, names ...string) (*Family, string) {
	for _, n := range names {
		if fam := f[n]; fam != nil && len(fam.Series) > 0 {
			return fam, n
		}
	}
	return nil, ""
}

func ptr(v float64) *float64 { return &v }

// histProbe follows one histogram metric across scrapes.
type histProbe struct {
	key   string   // logical name reported in Available/Missing
	names []string // candidate family names, newest engine version first
	scale float64  // multiply a value to get milliseconds
	win   *Window
}

func newHistProbe(key string, scale float64, window time.Duration, names ...string) *histProbe {
	return &histProbe{key: key, names: names, scale: scale, win: NewWindow(window)}
}

// observe feeds the current scrape and returns (found, p99 in ms, measured).
func (p *histProbe) observe(now time.Time, f Families) (bool, *float64) {
	fam, _ := first(f, p.names...)
	if fam == nil || fam.Type == "summary" {
		return false, nil
	}
	h, ok := MergeSeries(fam.Series)
	if !ok {
		return false, nil
	}
	p.win.Observe(now, h)
	if v, ok := p.win.Quantile(now, 0.99); ok {
		return true, ptr(v * p.scale)
	}
	return true, nil
}

// gauge sums (or maxes) the series of the first existing family.
func gauge(f Families, max bool, names ...string) (*float64, bool) {
	fam, _ := first(f, names...)
	if fam == nil {
		return nil, false
	}
	var v float64
	for i, s := range fam.Series {
		switch {
		case max && (i == 0 || s.Value > v):
			v = s.Value
		case !max:
			v += s.Value
		}
	}
	return ptr(v), true
}

// tracker collects the Available/Missing bookkeeping.
type tracker struct{ avail, miss map[string]bool }

func newTracker() *tracker { return &tracker{avail: map[string]bool{}, miss: map[string]bool{}} }
func (t *tracker) note(key string, found bool) {
	if found {
		t.avail[key] = true
	} else {
		t.miss[key] = true
	}
}
func (t *tracker) fill(r *Reading) {
	for k := range t.avail {
		r.Available = append(r.Available, k)
	}
	for k := range t.miss {
		r.Missing = append(r.Missing, k)
	}
	sort.Strings(r.Available)
	sort.Strings(r.Missing)
}
