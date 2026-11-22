// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package inference

import (
	"math"
	"os"
	"strings"
	"testing"
)

func mustParseFile(t *testing.T, name string) Families {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fams, err := ParseText(f)
	if err != nil {
		t.Fatal(err)
	}
	return fams
}

func TestParseTextHistogramAndLabels(t *testing.T) {
	fams := mustParseFile(t, "vllm_v0_t1.txt")
	f := fams["vllm:time_to_first_token_seconds"]
	if f == nil || f.Type != "histogram" || len(f.Series) != 1 {
		t.Fatalf("ttft family = %+v", f)
	}
	s := f.Series[0]
	if s.Labels["model_name"] != "meta-llama/Llama-3.1-8B-Instruct" || s.Labels["engine"] != "0" {
		t.Errorf("labels = %v", s.Labels)
	}
	if _, has := s.Labels["le"]; has {
		t.Error("le must not remain a series label")
	}
	if len(s.Buckets) != 17 || !math.IsInf(s.Buckets[16].Le, 1) || s.Count != 600 || s.Buckets[16].Count != 600 {
		t.Errorf("buckets = %d last=%+v count=%v", len(s.Buckets), s.Buckets[len(s.Buckets)-1], s.Count)
	}
	if g := fams["vllm:num_requests_waiting"]; g == nil || g.Type != "gauge" || g.Series[0].Value != 3 {
		t.Errorf("gauge = %+v", g)
	}
}

func TestParseTextSuffixOnlyForDeclaredHistograms(t *testing.T) {
	// nv_inference_exec_count is a counter whose name ends in _count, not the count of a
	// histogram called nv_inference_exec.
	fams := mustParseFile(t, "triton_t1.txt")
	if f := fams["nv_inference_exec_count"]; f == nil || f.Type != "counter" || len(f.Series) != 2 {
		t.Fatalf("exec_count = %+v", f)
	}
	if fams["nv_inference_exec"] != nil {
		t.Error("nv_inference_exec must not exist")
	}
}

func TestParseTextSummary(t *testing.T) {
	fams := mustParseFile(t, "triton_summary_t1.txt")
	f := fams["nv_inference_request_summary_us"]
	if f == nil || f.Type != "summary" || len(f.Series) != 2 {
		t.Fatalf("summary = %+v", f)
	}
	for _, s := range f.Series {
		if len(s.Quantiles) != 5 || s.Count == 0 || s.Labels["model"] == "" {
			t.Errorf("series %v quantiles=%v count=%v", s.Labels, s.Quantiles, s.Count)
		}
	}
}

func TestParseTextTolerance(t *testing.T) {
	in := "# TYPE x gauge\n" +
		"x{a=\"quo\\\"te\\\\ \\n\"} 1 1700000000000\n" + // escapes and a timestamp
		"garbage line without value\n" +
		"y{broken=\"x} 2\n" + // unterminated label value
		"z 3e-3\n" +
		"w NaN\n" +
		"v +Inf\n" +
		"# EOF\n"
	fams, err := ParseText(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := fams["x"].Series[0].Labels["a"]; got != "quo\"te\\ \n" {
		t.Errorf("escapes: %q", got)
	}
	if fams["y"] != nil || fams["garbage"] != nil {
		t.Error("malformed lines must be skipped")
	}
	if fams["z"].Series[0].Value != 0.003 || !math.IsNaN(fams["w"].Series[0].Value) || !math.IsInf(fams["v"].Series[0].Value, 1) {
		t.Error("special values")
	}
}

func TestParseTextOpenMetricsCreatedIgnored(t *testing.T) {
	in := "# TYPE r counter\nr_total 5\nr_created 1.7e9\n# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_bucket{le=\"+Inf\"} 2\nh_sum 3\nh_count 2\nh_created 1.7e9\n"
	fams, _ := ParseText(strings.NewReader(in))
	if fams["r_created"] != nil || fams["h_created"] != nil {
		t.Error("_created samples must be dropped")
	}
	if s := fams["h"].Series[0]; s.Count != 2 || s.Sum != 3 || len(s.Buckets) != 2 {
		t.Errorf("h = %+v", s)
	}
}

func TestParseTextSampleCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxSamples+50; i++ {
		b.WriteString("m 1\n")
	}
	if _, err := ParseText(strings.NewReader(b.String())); err != nil {
		t.Fatal(err)
	}
}
