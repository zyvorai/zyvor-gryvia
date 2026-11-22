// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package inference

import "time"

// triton reads NVIDIA Triton Inference Server's metrics endpoint (port 8002 by default).
//
// Triton's latency figures are cumulative counters of microseconds (nv_inference_*_duration_us)
// next to request counts, so over an interval only a MEAN can be derived. When the server runs
// with summary latencies enabled (--metrics-config summary_latencies=true) it also exports
// summaries with quantiles; those give a real p99. Triton does not know about tokens, so time to
// first token, inter-token latency and KV cache are never reported for it.
type triton struct {
	e2eMean, queueMean, computeMean *CounterWindow
}

func newTriton(w time.Duration) *triton {
	return &triton{e2eMean: NewCounterWindow(w), queueMean: NewCounterWindow(w), computeMean: NewCounterWindow(w)}
}

func (*triton) Name() string { return EngineTriton }

func (*triton) Detect(f Families) bool { return anyPrefix(f, "nv_inference_") }

// sumSeries adds Value over all series of the family; false when the family is absent.
func sumSeries(f Families, name string) (float64, bool) {
	fam := f[name]
	if fam == nil || len(fam.Series) == 0 {
		return 0, false
	}
	var v float64
	for _, s := range fam.Series {
		v += s.Value
	}
	return v, true
}

// summaryP99 is the worst (largest) 0.99 quantile over the series (models) of a summary.
func summaryP99(f Families, name string) (float64, bool) {
	fam := f[name]
	if fam == nil || fam.Type != "summary" {
		return 0, false
	}
	best, ok := 0.0, false
	for _, s := range fam.Series {
		if q, has := s.Quantiles[0.99]; has && (!ok || q > best) {
			best, ok = q, true
		}
	}
	return best, ok
}

func (tr *triton) Observe(now time.Time, f Families) Reading {
	r := Reading{Engine: EngineTriton, At: now}
	t := newTracker()

	reqs, haveReqs := sumSeries(f, "nv_inference_request_success")
	execs, haveExecs := sumSeries(f, "nv_inference_exec_count")
	e2eUS, haveE2E := sumSeries(f, "nv_inference_request_duration_us")
	queueUS, haveQueue := sumSeries(f, "nv_inference_queue_duration_us")
	computeUS, haveCompute := sumSeries(f, "nv_inference_compute_infer_duration_us")

	e2eP99, haveE2ESum := summaryP99(f, "nv_inference_request_summary_us")
	queueP99, haveQueueSum := summaryP99(f, "nv_inference_queue_summary_us")
	if haveE2ESum {
		r.E2EP99 = ptr(e2eP99 / 1e3)
	}
	if haveQueueSum {
		r.QueueP99 = ptr(queueP99 / 1e3)
	}

	if haveReqs && haveE2E {
		tr.e2eMean.Observe(now, reqs, e2eUS)
		if m, ok := tr.e2eMean.Mean(now); ok {
			r.E2EMean = ptr(m / 1e3)
		}
	}
	if haveReqs && haveQueue {
		tr.queueMean.Observe(now, reqs, queueUS)
		if m, ok := tr.queueMean.Mean(now); ok {
			r.QueueMean = ptr(m / 1e3)
		}
	}
	if haveExecs && haveCompute {
		tr.computeMean.Observe(now, execs, computeUS)
		if m, ok := tr.computeMean.Mean(now); ok {
			r.ComputeMean = ptr(m / 1e3)
			r.Notes = append(r.Notes, "compute: mean per model execution (batch), not per request")
		}
	}
	t.note("e2e", (haveReqs && haveE2E) || haveE2ESum)
	t.note("queue_time", (haveReqs && haveQueue) || haveQueueSum)
	t.note("compute", haveExecs && haveCompute)
	if (haveReqs && (haveE2E || haveQueue)) && !haveE2ESum && !haveQueueSum {
		r.Notes = append(r.Notes, "latencies are means over the window: Triton exports cumulative duration counters; enable summary_latencies for quantiles")
	}
	var found bool
	r.Waiting, found = gauge(f, false, "nv_inference_pending_request_count")
	t.note("requests_waiting", found)
	// Triton has no notion of tokens or a KV cache.
	t.note("ttft", false)
	t.note("itl", false)
	t.note("kv_cache", false)
	t.fill(&r)
	return r
}
