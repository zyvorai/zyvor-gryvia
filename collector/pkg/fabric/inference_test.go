// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/inference"
)

func f64(v float64) *float64 { return &v }

var jobKey = JobKey{Namespace: "ml", Job: "serve"}

func TestSnapshotHasNoInferenceByDefault(t *testing.T) {
	f, _ := newTestFolder()
	f.Bind(1, "ml", "serve")
	f.Add(Signal{PID: 1, Type: SigInferWait, LatencyNS: 5_000_000, Comm: "python"})
	st := only(t, f)
	if st.Inference != nil {
		t.Errorf("no scraper ran but Inference = %+v", st.Inference)
	}
	if st.InferWaitP99MS != 5 {
		t.Errorf("network accept wait must be untouched: %v", st.InferWaitP99MS)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "inference\"") {
		t.Errorf("inference key leaked into the default JSON: %s", b)
	}
}

func TestInferenceOnlyJobAppearsAndKeepsNetworkWaitSeparate(t *testing.T) {
	f, c := newTestFolder()
	f.SetInference(jobKey, "vllm-0:8000", inference.Reading{Engine: "vllm", TTFTP99: f64(475), Waiting: f64(3)})
	st := only(t, f)
	if st.Inference == nil || *st.Inference.TTFTP99MS != 475 || st.InferWaitP99MS != 0 {
		t.Fatalf("status = %+v", st)
	}
	if st.Inference.ITLP99MS != nil {
		t.Error("an unreported metric must stay nil, not 0")
	}
	if !st.UpdatedAt.Equal(c.t) {
		t.Errorf("UpdatedAt = %v", st.UpdatedAt)
	}
	// Ages out with the window.
	c.t = c.t.Add(2 * time.Minute)
	if got := f.Snapshot(); len(got) != 0 {
		t.Errorf("stale reading survived: %+v", got)
	}
}

func TestInferenceMergeWorstReplica(t *testing.T) {
	f, _ := newTestFolder()
	f.SetInference(jobKey, "a", inference.Reading{Engine: "vllm", TTFTP99: f64(100), KVCache: f64(0.3), Waiting: f64(2), Running: f64(5), Notes: []string{"n"}})
	f.SetInference(jobKey, "b", inference.Reading{Engine: "vllm", TTFTP99: f64(250), KVCache: f64(0.9), Waiting: f64(4), ITLP99: f64(30)})
	in := only(t, f).Inference
	if in.Engine != "vllm" || in.Targets != 2 {
		t.Errorf("%+v", in)
	}
	if *in.TTFTP99MS != 250 || *in.KVCacheUsage != 0.9 || *in.RequestsWaiting != 6 || *in.RequestsRunning != 5 || *in.ITLP99MS != 30 {
		t.Errorf("merge = ttft %v kv %v waiting %v running %v itl %v", *in.TTFTP99MS, *in.KVCacheUsage, *in.RequestsWaiting, *in.RequestsRunning, *in.ITLP99MS)
	}
	f.SetInference(jobKey, "c", inference.Reading{Engine: "tgi"})
	if in := only(t, f).Inference; in.Engine != "mixed" {
		t.Errorf("engine = %q", in.Engine)
	}
	// Re-setting a target replaces its reading rather than accumulating.
	f.SetInference(jobKey, "a", inference.Reading{Engine: "vllm", TTFTP99: f64(10)})
	if in := only(t, f).Inference; in.Targets != 3 {
		t.Errorf("targets = %d", in.Targets)
	}
}

func inferenceStatus() Status {
	return Status{UpdatedAt: time.Unix(1_700_000_000, 0), Inference: &Inference{
		Engine: "vllm", TTFTP99MS: f64(475), ITLP99MS: f64(72.5), QueueP99MS: f64(50), E2EP99MS: f64(2250),
		RequestsWaiting: f64(2.6), KVCacheUsage: f64(1.7), QueueMeanMS: f64(math.NaN()),
	}}
}

func decodeStatus(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var got struct {
		Status map[string]interface{} `json:"status"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	return got.Status
}

func TestStatusPatchWithoutInferenceIsUnchanged(t *testing.T) {
	body, err := StatusPatchBody(inferenceStatus())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ttftP99ms", "itlP99ms", "queueTimeP99ms", "e2eP99ms", "requestsWaiting", "kvCacheUsage", "engine"} {
		if _, has := decodeStatus(t, body)[k]; has {
			t.Errorf("%s present although the scraper is off", k)
		}
	}
}

func TestStatusPatchWithInference(t *testing.T) {
	body, err := statusPatchBody(inferenceStatus(), true)
	if err != nil {
		t.Fatal(err)
	}
	s := decodeStatus(t, body)
	if s["ttftP99ms"].(float64) != 475 || s["itlP99ms"].(float64) != 72.5 || s["queueTimeP99ms"].(float64) != 50 ||
		s["e2eP99ms"].(float64) != 2250 || s["engine"].(string) != "vllm" {
		t.Errorf("patch = %s", body)
	}
	if s["requestsWaiting"].(float64) != 3 {
		t.Errorf("requestsWaiting rounds to an integer: %v", s["requestsWaiting"])
	}
	if s["kvCacheUsage"].(float64) != 1 {
		t.Errorf("kvCacheUsage is clamped to 1: %v", s["kvCacheUsage"])
	}
	if v, has := s["queueTimeMeanMs"]; has {
		t.Errorf("NaN is not a measurement and must be omitted, got %v", v)
	}
	// Nothing measured: every engine field is omitted (never null/zero: another node may have measured it).
	body, _ = statusPatchBody(Status{UpdatedAt: time.Unix(1_700_000_000, 0)}, true)
	s = decodeStatus(t, body)
	for _, k := range []string{"ttftP99ms", "itlP99ms", "queueTimeP99ms", "e2eP99ms", "requestsWaiting", "kvCacheUsage", "engine"} {
		if v, has := s[k]; has {
			t.Errorf("%s = %v, want omitted", k, v)
		}
	}
}

func TestPublisherSendsInferenceOnlyWhenEnabled(t *testing.T) {
	for _, on := range []bool{false, true} {
		api := newFakeAPI()
		api.lists[gfsBase+"ml/gryviafabricsignals"] = `{"items":[{"metadata":{"name":"serve-fabric"},"spec":{"jobRef":"serve"}}]}`
		f, _ := newTestFolder()
		f.SetInference(jobKey, "t", inference.Reading{Engine: "vllm", TTFTP99: f64(475)})
		p := &Publisher{API: api, Folder: f, Inference: on}
		if n := p.PublishOnce(context.Background()); n != 1 {
			t.Fatalf("patched %d", n)
		}
		s := decodeStatus(t, api.patches[gfsBase+"ml/gryviafabricsignals/serve-fabric/status"])
		_, has := s["ttftP99ms"]
		if has != on {
			t.Errorf("inference=%v: ttftP99ms present=%v", on, has)
		}
	}
}

// The engine fields (inference) and the collective/GPU fields (collective identity, DCGM correlation) live in one
// merge-patch: neither may drop the other.
func TestStatusPatchCarriesInferenceAndCollectiveFields(t *testing.T) {
	st := inferenceStatus()
	st.CollectiveMaxSkewMS = 12.5
	st.GPUCorrelationMeasured, st.GPUIdleDuringCommRatio, st.GPUCorrelationCoverage = true, 0.25, 0.5
	body, err := statusPatchBody(st, true)
	if err != nil {
		t.Fatal(err)
	}
	s := decodeStatus(t, body)
	if s["ttftP99ms"].(float64) != 475 || s["engine"].(string) != "vllm" {
		t.Errorf("engine fields lost: %s", body)
	}
	if s["collectiveMaxSkewMs"].(float64) != 12.5 || s["gpuIdleDuringCommRatio"].(float64) != 0.25 || s["gpuCorrelationCoverage"].(float64) != 0.5 {
		t.Errorf("collective/GPU fields lost: %s", body)
	}
	if v, has := s["smActiveDuringCompute"]; has {
		t.Errorf("unmeasured compute activity must be omitted: %v", v)
	}
}
