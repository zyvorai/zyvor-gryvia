// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package exporter

import (
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zyvorai/gryvia/collector/pkg/fabric"
)

func gatherValue(t *testing.T, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if want, ok := labels[lp.GetName()]; ok && want != lp.GetValue() {
					continue next
				}
			}
			if m.GetGauge() != nil {
				return m.GetGauge().GetValue(), true
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

var testMetrics = sync.OnceValue(NewMetrics) // promauto registers on the default registry: once per test binary

func TestRecordPFCAndFabricGauges(t *testing.T) {
	m := testMetrics()
	var perPrio [fabric.PFCPriorities]uint64
	perPrio[3], perPrio[5] = 7, 2
	m.RecordPFC(9, 4, perPrio)
	m.RecordPFC(1, 0, [fabric.PFCPriorities]uint64{3: 1})
	for name, want := range map[string]float64{
		"gryvia_pfc_pause_frames_total":        10,
		"gryvia_pfc_legacy_pause_frames_total": 4,
	} {
		if got, ok := gatherValue(t, name, nil); !ok || got != want {
			t.Errorf("%s = %v ok=%v, want %v", name, got, ok, want)
		}
	}
	if got, ok := gatherValue(t, "gryvia_pfc_priority_pause_frames_total", map[string]string{"priority": "3"}); !ok || got != 8 {
		t.Errorf("priority 3 = %v ok=%v, want 8", got, ok)
	}
	if _, ok := gatherValue(t, "gryvia_pfc_priority_pause_frames_total", map[string]string{"priority": "0"}); ok {
		t.Error("a priority with no pauses must not create a series")
	}

	m.RecordFabric([]fabric.JobStatus{{
		JobKey: fabric.PFCJob,
		Status: fabric.Status{PFCRate: 1500, ExfilEvents: 2, UCXSlowP99MS: 12},
	}})
	labels := map[string]string{"namespace": "_node", "job": "pfc"}
	for name, want := range map[string]float64{
		"gryvia_fabric_pfc_rate": 1500, "gryvia_fabric_exfil_events": 2, "gryvia_fabric_ucx_slow_p99_seconds": 0.012,
	} {
		if got, ok := gatherValue(t, name, labels); !ok || got != want {
			t.Errorf("%s = %v ok=%v, want %v", name, got, ok, want)
		}
	}
	m.RecordFabric(nil)
	if _, ok := gatherValue(t, "gryvia_fabric_pfc_rate", labels); ok {
		t.Error("jobs that aged out must disappear")
	}
}

func TestRecordInferenceGauges(t *testing.T) {
	m := testMetrics()
	f := func(v float64) *float64 { return &v }
	m.RecordFabric([]fabric.JobStatus{{
		JobKey: fabric.JobKey{Namespace: "ml", Job: "serve"},
		Status: fabric.Status{InferWaitP99MS: 4, Inference: &fabric.Inference{
			Engine: "vllm", TTFTP99MS: f(475), QueueP99MS: f(50), RequestsWaiting: f(3), KVCacheUsage: f(0.42),
		}},
	}})
	base := map[string]string{"namespace": "ml", "job": "serve", "engine": "vllm"}
	with := func(k, v string) map[string]string {
		out := map[string]string{k: v}
		for a, b := range base {
			out[a] = b
		}
		return out
	}
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"gryvia_inference_latency_seconds", with("metric", "ttft_p99"), 0.475},
		{"gryvia_inference_latency_seconds", with("metric", "queue_time_p99"), 0.05},
		{"gryvia_inference_requests", with("state", "waiting"), 3},
		{"gryvia_inference_kv_cache_usage_ratio", base, 0.42},
		{"gryvia_fabric_infer_wait_p99_seconds", map[string]string{"namespace": "ml", "job": "serve"}, 0.004},
	} {
		if got, ok := gatherValue(t, c.name, c.labels); !ok || got != c.want {
			t.Errorf("%s %v = %v ok=%v, want %v", c.name, c.labels, got, ok, c.want)
		}
	}
	// Not measured means no series, not a zero.
	if _, ok := gatherValue(t, "gryvia_inference_latency_seconds", with("metric", "itl_p99")); ok {
		t.Error("an unmeasured ITL must have no series")
	}
	if _, ok := gatherValue(t, "gryvia_inference_requests", with("state", "running")); ok {
		t.Error("unreported running count must have no series")
	}
	m.RecordFabric(nil)
	if _, ok := gatherValue(t, "gryvia_inference_kv_cache_usage_ratio", base); ok {
		t.Error("aged-out jobs must disappear")
	}
}
