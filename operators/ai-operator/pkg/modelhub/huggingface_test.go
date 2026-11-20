// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package modelhub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestList(t *testing.T) {
	var gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[
		 {"id":"Qwen/Qwen3-8B","sha":"abc1234def","lastModified":"2026-09-30T10:00:00.000Z","downloads":1200,
		  "tags":["license:apache-2.0","safetensors"],"pipeline_tag":"text-generation","gated":false,
		  "safetensors":{"total":8190735360}},
		 {"id":"meta-llama/Llama-4-70B","sha":"0123456789","gated":"manual","tags":["license:llama4"]}
		]`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	models, err := c.List(context.Background(), Query{Author: "Qwen", PipelineTag: "text-generation", Limit: 5}, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth header %q", gotAuth)
	}
	for _, want := range []string{"author=Qwen", "limit=5", "pipeline_tag=text-generation", "sort=lastModified"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q lacks %q", gotQuery, want)
		}
	}
	if len(models) != 2 {
		t.Fatalf("got %d models", len(models))
	}
	q := models[0]
	if q.SHA != "abc1234def" || q.Params != 8190735360 || q.License() != "apache-2.0" || q.Gated || q.LastModified.IsZero() {
		t.Errorf("first model: %+v", q)
	}
	if !models[1].Gated || models[1].License() != "llama4" {
		t.Errorf("second model: %+v", models[1])
	}
}

func TestListErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if _, err := (&Client{BaseURL: srv.URL}).List(context.Background(), Query{Author: "x"}, ""); err == nil {
		t.Error("a 429 must be an error")
	}
}

func TestEstimateParams(t *testing.T) {
	cases := map[string]int64{
		"Qwen/Qwen2.5-7B-Instruct":       7e9,
		"Qwen/Qwen2.5-0.5B":              5e8,
		"HuggingFaceTB/SmolLM-135M":      135e6,
		"mistralai/Mixtral-8x7B-v0.1":    56e9,
		"meta-llama/Llama-3.1-70B":       70e9,
		"google/gemma-3-27b-it":          27e9,
		"someone/no-size-in-name":        0,
		"org/model-v2.5":                 0,
		"deepseek-ai/DeepSeek-V3.1-Base": 0,
	}
	for id, want := range cases {
		if got := (Model{ID: id}).EstimateParams(); got != want {
			t.Errorf("%s: got %d want %d", id, got, want)
		}
	}
	if got := (Model{ID: "a/b-7B", Params: 42}).EstimateParams(); got != 42 {
		t.Errorf("hub metadata must win: %d", got)
	}
}

func TestValidators(t *testing.T) {
	for _, id := range []string{"Qwen/Qwen3-8B", "a/b", "meta-llama/Llama-3.1_x"} {
		if !ValidID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range []string{"", "noslash", "a/b/c", "a/{{x}}", "-a/b", "a/b c"} {
		if ValidID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
	if !ValidSHA("abc1234") || ValidSHA("main") || ValidSHA("") {
		t.Error("sha validation")
	}
}
