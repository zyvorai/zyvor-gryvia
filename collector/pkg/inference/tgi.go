// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package inference

import "time"

// tgi reads HuggingFace text-generation-inference's /metrics.
//
// TGI exports no time-to-first-token. tgi_request_mean_time_per_token_duration is, per request,
// the mean time per generated token; its p99 is the slowest 1% of request means, which is
// reported as ITL with a note, not as a per-token p99.
type tgi struct {
	queue, itl, e2e *histProbe
}

func newTGI(w time.Duration) *tgi {
	return &tgi{
		queue: newHistProbe("queue_time", 1e3, w, "tgi_request_queue_duration"),
		itl:   newHistProbe("itl", 1e3, w, "tgi_request_mean_time_per_token_duration"),
		e2e:   newHistProbe("e2e", 1e3, w, "tgi_request_duration"),
	}
}

func (*tgi) Name() string { return EngineTGI }

func (*tgi) Detect(f Families) bool { return anyPrefix(f, "tgi_") }

func (g *tgi) Observe(now time.Time, f Families) Reading {
	r := Reading{Engine: EngineTGI, At: now}
	t := newTracker()
	var found bool
	found, r.QueueP99 = g.queue.observe(now, f)
	t.note("queue_time", found)
	found, r.ITLP99 = g.itl.observe(now, f)
	t.note("itl", found)
	if found {
		r.Notes = append(r.Notes, "itl: p99 of the per-request mean time per token (tgi_request_mean_time_per_token_duration)")
	}
	found, r.E2EP99 = g.e2e.observe(now, f)
	t.note("e2e", found)
	r.Waiting, found = gauge(f, false, "tgi_queue_size")
	t.note("requests_waiting", found)
	r.Running, found = gauge(f, false, "tgi_batch_current_size")
	t.note("requests_running", found)
	// Not exported by TGI: listed so the caller can say "not measured".
	t.note("ttft", false)
	t.note("kv_cache", false)
	t.fill(&r)
	return r
}
