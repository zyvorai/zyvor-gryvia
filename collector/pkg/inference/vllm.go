// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package inference

import "time"

// vllm reads vLLM's /metrics (V0 and V1 engines). Names are those of the vllm:* families; metrics
// renamed between releases list both spellings, newest first.
type vllm struct {
	ttft, itl, queue, e2e, prefill, decode *histProbe
}

func newVLLM(w time.Duration) *vllm {
	return &vllm{
		ttft:    newHistProbe("ttft", 1e3, w, "vllm:time_to_first_token_seconds"),
		itl:     newHistProbe("itl", 1e3, w, "vllm:inter_token_latency_seconds", "vllm:time_per_output_token_seconds"),
		queue:   newHistProbe("queue_time", 1e3, w, "vllm:request_queue_time_seconds"),
		e2e:     newHistProbe("e2e", 1e3, w, "vllm:e2e_request_latency_seconds"),
		prefill: newHistProbe("prefill", 1e3, w, "vllm:request_prefill_time_seconds"),
		decode:  newHistProbe("decode", 1e3, w, "vllm:request_decode_time_seconds"),
	}
}

func (*vllm) Name() string { return EngineVLLM }

func (*vllm) Detect(f Families) bool { return anyPrefix(f, "vllm:") }

func (v *vllm) Observe(now time.Time, f Families) Reading {
	r := Reading{Engine: EngineVLLM, At: now}
	t := newTracker()
	var found bool
	found, r.TTFTP99 = v.ttft.observe(now, f)
	t.note("ttft", found)
	found, r.ITLP99 = v.itl.observe(now, f)
	t.note("itl", found)
	found, r.QueueP99 = v.queue.observe(now, f)
	t.note("queue_time", found)
	found, r.E2EP99 = v.e2e.observe(now, f)
	t.note("e2e", found)
	found, r.PrefillP99 = v.prefill.observe(now, f)
	t.note("prefill", found)
	found, r.DecodeP99 = v.decode.observe(now, f)
	t.note("decode", found)
	r.Running, found = gauge(f, false, "vllm:num_requests_running")
	t.note("requests_running", found)
	r.Waiting, found = gauge(f, false, "vllm:num_requests_waiting")
	t.note("requests_waiting", found)
	// The name says "perc" but the value is a 0..1 fraction. Renamed from gpu_cache_usage_perc.
	r.KVCache, found = gauge(f, true, "vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc")
	t.note("kv_cache", found)
	t.fill(&r)
	return r
}
