// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"sort"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/inference"
)

// Inference is the serving engine's own view of one job, folded from the readings of every
// scraped target of the job on this node. Latencies are milliseconds. A nil pointer means not
// measured: the engine version does not export the metric, or no request finished inside the
// window yet.
//
// Several targets of one job (replicas on this node) are combined pessimistically: latency and
// KV-cache figures take the worst replica, request counts add up. Percentiles of different
// replicas cannot be averaged, so the result is "the slowest replica's p99", not the p99 of all
// requests.
type Inference struct {
	Engine string `json:"engine"` // vllm, triton, tgi or mixed

	TTFTP99MS    *float64 `json:"ttftP99ms,omitempty"` // time to first token
	ITLP99MS     *float64 `json:"itlP99ms,omitempty"`  // inter-token latency
	QueueP99MS   *float64 `json:"queueTimeP99ms,omitempty"`
	E2EP99MS     *float64 `json:"e2eP99ms,omitempty"`
	PrefillP99MS *float64 `json:"prefillP99ms,omitempty"`
	DecodeP99MS  *float64 `json:"decodeP99ms,omitempty"`

	// Means, for engines that export only cumulative duration counters (Triton without
	// summary latencies). Never a substitute for a p99.
	QueueMeanMS   *float64 `json:"queueTimeMeanMs,omitempty"`
	E2EMeanMS     *float64 `json:"e2eMeanMs,omitempty"`
	ComputeMeanMS *float64 `json:"computeMeanMs,omitempty"`

	RequestsRunning *float64 `json:"requestsRunning,omitempty"`
	RequestsWaiting *float64 `json:"requestsWaiting,omitempty"`
	KVCacheUsage    *float64 `json:"kvCacheUsage,omitempty"` // 0..1

	Targets   int       `json:"targets"`
	Notes     []string  `json:"notes,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type infSample struct {
	at time.Time
	r  inference.Reading
}

// SetInference records the latest engine reading of one scrape target for a job. Readings older
// than the folder window stop counting, so a target that disappears ages out.
func (f *Folder) SetInference(k JobKey, target string, r inference.Reading) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inf == nil {
		f.inf = map[JobKey]map[string]infSample{}
	}
	m := f.inf[k]
	if m == nil {
		m = map[string]infSample{}
		f.inf[k] = m
	}
	m[target] = infSample{at: f.now(), r: r}
}

// foldInferenceLocked adds the fresh engine readings to out, creating an entry for a job that has
// engine metrics but no fabric signals. Expired readings are dropped.
func (f *Folder) foldInferenceLocked(out map[JobKey]Status, cutoff time.Time) {
	for k, targets := range f.inf {
		var fresh []infSample
		for name, s := range targets {
			if s.at.Before(cutoff) {
				delete(targets, name)
				continue
			}
			fresh = append(fresh, s)
		}
		if len(fresh) == 0 {
			delete(f.inf, k)
			continue
		}
		st := out[k]
		st.Inference = mergeReadings(fresh)
		if st.Inference.UpdatedAt.After(st.UpdatedAt) {
			st.UpdatedAt = st.Inference.UpdatedAt
		}
		out[k] = st
	}
}

func maxPtr(dst **float64, v *float64) {
	if v != nil && (*dst == nil || *v > **dst) {
		c := *v
		*dst = &c
	}
}

func addPtr(dst **float64, v *float64) {
	if v == nil {
		return
	}
	if *dst == nil {
		c := *v
		*dst = &c
		return
	}
	s := **dst + *v
	*dst = &s
}

func mergeReadings(rs []infSample) *Inference {
	sort.Slice(rs, func(i, j int) bool { return rs[i].r.Engine < rs[j].r.Engine })
	out := &Inference{Targets: len(rs)}
	notes := map[string]bool{}
	for i, s := range rs {
		r := s.r
		switch {
		case i == 0:
			out.Engine = r.Engine
		case out.Engine != r.Engine:
			out.Engine = "mixed"
		}
		if s.at.After(out.UpdatedAt) {
			out.UpdatedAt = s.at
		}
		maxPtr(&out.TTFTP99MS, r.TTFTP99)
		maxPtr(&out.ITLP99MS, r.ITLP99)
		maxPtr(&out.QueueP99MS, r.QueueP99)
		maxPtr(&out.E2EP99MS, r.E2EP99)
		maxPtr(&out.PrefillP99MS, r.PrefillP99)
		maxPtr(&out.DecodeP99MS, r.DecodeP99)
		maxPtr(&out.QueueMeanMS, r.QueueMean)
		maxPtr(&out.E2EMeanMS, r.E2EMean)
		maxPtr(&out.ComputeMeanMS, r.ComputeMean)
		maxPtr(&out.KVCacheUsage, r.KVCache)
		addPtr(&out.RequestsRunning, r.Running)
		addPtr(&out.RequestsWaiting, r.Waiting)
		for _, n := range r.Notes {
			notes[n] = true
		}
	}
	for n := range notes {
		out.Notes = append(out.Notes, n)
	}
	sort.Strings(out.Notes)
	return out
}
