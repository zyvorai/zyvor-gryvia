// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func clientReturning(code int, body string, seen *int) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if seen != nil {
			*seen++
		}
		return response(code, body), nil
	})}
}

const partialBody = `{"namespace":"ml","job":"train","coverage":{"total":3,"reachable":2,"reporting":1,"complete":false},"truncated":false,"counts":{"tcp_retransmit":2},"findings":[{"node":"n1","code":"c","evidence":"e\u001b[31m"}],"events":[{}]}`
const completeBody = `{"namespace":"ml","job":"train","coverage":{"total":1,"reachable":1,"reporting":1,"complete":true},"truncated":false,"counts":{},"findings":[],"events":[]}`

func TestFetchRequestShape(t *testing.T) {
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer jwt" || r.URL.Path != "/api/flight/jobs/train" || r.URL.Query().Get("namespace") != "ml" || r.Method != http.MethodGet {
			t.Errorf("bad request: %v", r)
		}
		return response(200, completeBody), nil
	})}
	if _, err := fetch(context.Background(), client, "https://gateway.example/", "ml", "train", "jwt\n"); err != nil {
		t.Fatal(err)
	}
}

func TestFetchRejectsUnsafeInput(t *testing.T) {
	calls := 0
	c := clientReturning(200, completeBody, &calls)
	for _, tc := range []struct{ gw, ns, job, tok string }{
		{"http://example.com", "ml", "train", "t"},
		{"https://u:p@example.com", "ml", "train", "t"},
		{"https://example.com?x=1", "ml", "train", "t"},
		{"ftp://example.com", "ml", "train", "t"},
		{"https://example.com", "ml", "a/b", "t"},
		{"https://example.com", "ml", "a?b", "t"},
		{"https://example.com", "ML", "train", "t"},
		{"https://example.com", "ml", "train", "  "},
	} {
		if _, err := fetch(context.Background(), c, tc.gw, tc.ns, tc.job, tc.tok); err == nil {
			t.Errorf("accepted %+v", tc)
		}
	}
	if calls != 0 {
		t.Fatal("request sent for rejected input")
	}
	if _, err := fetch(context.Background(), c, "http://localhost:8080", "ml", "train", "t"); err != nil {
		t.Fatalf("localhost tunnel rejected: %v", err)
	}
}

func TestFetchDoesNotFollowRedirect(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		res := response(302, "")
		res.Header.Set("Location", "https://other.example")
		return res, nil
	})}
	if _, err := fetch(context.Background(), client, "https://gateway.example", "ml", "train", "jwt"); err == nil || calls != 1 {
		t.Fatalf("followed redirect: %v calls=%d", err, calls)
	}
}

func TestFetchStatusErrors(t *testing.T) {
	for code, want := range map[int]string{
		401: "rejected", 403: "may not read that namespace", 404: "route not found", 503: "token unset",
	} {
		_, err := fetch(context.Background(), clientReturning(code, `{"detail":"why\nnow"}`, nil), "https://g.example", "ml", "train", "jwt")
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "why now") {
			t.Errorf("%d: %v", code, err)
		}
	}
}

func TestFetchRejectsMalformedAndOversize(t *testing.T) {
	if _, err := fetch(context.Background(), clientReturning(200, "not json", nil), "https://g.example", "ml", "train", "jwt"); err == nil {
		t.Fatal("accepted malformed")
	}
	if _, err := fetch(context.Background(), clientReturning(200, "["+strings.Repeat("0,", 3<<20)+"0]", nil), "https://g.example", "ml", "train", "jwt"); err == nil {
		t.Fatal("accepted oversize or non-object")
	}
}

func runCLI(t *testing.T, client *http.Client, env map[string]string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb, func(k string) string { return env[k] }, client)
	return code, out.String(), errb.String()
}

func TestRunExitCodesAndPartialCoverage(t *testing.T) {
	tok := map[string]string{tokenEnv: "secret-token"}
	base := []string{"-gateway", "https://g.example", "-namespace", "ml", "-job", "train"}

	code, out, errs := runCLI(t, clientReturning(200, partialBody, nil), tok, "", base...)
	if code != 0 || !strings.Contains(out, `"complete":false`) || !strings.Contains(errs, "partial") {
		t.Fatalf("partial json: %d %q %q", code, out, errs)
	}
	code, _, _ = runCLI(t, clientReturning(200, partialBody, nil), tok, "", append(base, "-require-complete")...)
	if code != 3 {
		t.Fatalf("require-complete on partial: %d", code)
	}
	code, out, _ = runCLI(t, clientReturning(200, completeBody, nil), tok, "", append(base, "-require-complete")...)
	if code != 0 || !strings.Contains(out, "complete") {
		t.Fatalf("complete: %d", code)
	}
	trunc := strings.Replace(completeBody, `"truncated":false`, `"truncated":true`, 1)
	if code, _, _ = runCLI(t, clientReturning(200, trunc, nil), tok, "", append(base, "-require-complete")...); code != 3 {
		t.Fatalf("truncated must count as partial: %d", code)
	}
	code, out, _ = runCLI(t, clientReturning(200, partialBody, nil), tok, "", append(base, "-o", "text")...)
	if code != 0 || !strings.Contains(out, "PARTIAL VIEW") || strings.Contains(out, "\x1b") {
		t.Fatalf("text: %d %q", code, out)
	}
	if code, _, errs = runCLI(t, clientReturning(403, `{"detail":"Namespace is not available to this user"}`, nil), tok, "", base...); code != 1 || !strings.Contains(errs, "not available") {
		t.Fatalf("403: %d %q", code, errs)
	}
}

func TestRunUsageAndTokenHandling(t *testing.T) {
	c := clientReturning(200, completeBody, nil)
	if code, _, _ := runCLI(t, c, nil, "", "-namespace", "ml"); code != 2 {
		t.Fatalf("missing job: %d", code)
	}
	if code, _, errs := runCLI(t, c, nil, "", "-namespace", "ml", "-job", "t"); code != 2 || !strings.Contains(errs, "no token") {
		t.Fatalf("missing token: %d %q", code, errs)
	}
	if code, _, _ := runCLI(t, c, nil, "", "-token", "x", "-namespace", "ml", "-job", "t"); code != 2 {
		t.Fatal("a -token flag must not exist")
	}
	if code, _, _ := runCLI(t, c, nil, "", "-namespace", "ml", "-job", "t", "-o", "yaml"); code != 2 {
		t.Fatal("bad -o accepted")
	}
	// stdin token; the token must never appear in output.
	var seenAuth string
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		seenAuth = r.Header.Get("Authorization")
		return response(200, completeBody), nil
	})}
	code, out, errs := runCLI(t, client, nil, "stdin-secret\n", "-gateway", "https://g.example", "-namespace", "ml", "-job", "t", "-token-file", "-")
	if code != 0 || seenAuth != "Bearer stdin-secret" || strings.Contains(out+errs, "stdin-secret") {
		t.Fatalf("stdin token: %d %q", code, seenAuth)
	}
}

const diagBody = `{"namespace":"ml","job":"train","summary":"1 finding(s)","partial":true,
"findings":[{"kind":"cpu","severity":"critical","confidence":"high","nodes":["n1"],"summary":"throttled\u001b[31m","whatWasNotMeasured":["host CPU"],
"evidence":[{"node":"n1","source":"cgroup","metric":"cpu_throttle_ratio","value":0.75,"window":"5m0s"}]}],
"unavailable":[{"node":"n1","signal":"cgroup.io.pressure","reason":"PSI disabled"}],
"measurementCompleteness":{"probesAttached":4,"probesSkipped":[],"droppedEvents":{"total":3},"sampling":{"ratio":1},
"nodesExpected":"unknown","nodesReporting":1,"missingNodes":[],"complete":false,"reasons":["expected nodes unknown"]}}`

func TestDiagnosisFlag(t *testing.T) {
	tok := map[string]string{tokenEnv: "secret-token"}
	base := []string{"-gateway", "https://g.example", "-namespace", "ml", "-job", "train", "-diagnosis"}
	var path string
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		path = r.URL.Path
		return response(200, diagBody), nil
	})}
	code, out, errs := runCLI(t, client, tok, "", base...)
	if code != 0 || path != "/api/flight/jobs/train/diagnosis" || !strings.Contains(out, `"measurementCompleteness"`) || !strings.Contains(errs, "incomplete measurement") {
		t.Fatalf("json: %d %q %q %q", code, path, out, errs)
	}
	if code, _, _ = runCLI(t, client, tok, "", append(base, "-require-complete")...); code != 3 {
		t.Fatalf("require-complete on incomplete: %d", code)
	}
	code, out, _ = runCLI(t, client, tok, "", append(base, "-o", "text")...)
	for _, want := range []string{"INCOMPLETE MEASUREMENT", "absence of findings is not evidence of health", "of unknown expected", "cpu_throttle_ratio", "not measured", "Unavailable (not healthy): 1", "expected nodes unknown"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("text missing %q: %d %q", want, code, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatal("control characters must not reach the terminal")
	}
	complete := strings.Replace(strings.Replace(diagBody, `"partial":true`, `"partial":false`, 1), `"complete":false`, `"complete":true`, 1)
	ok := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return response(200, complete), nil })}
	if code, _, _ = runCLI(t, ok, tok, "", append(base, "-require-complete")...); code != 0 {
		t.Fatalf("complete: %d", code)
	}
}

const vllmInference = `{"namespace":"ml","job":"train","available":true,"engine":"vllm","updatedAt":"2026-01-01T00:00:00Z","ttftP99ms":475,"itlP99ms":72.2,"queueTimeP99ms":50,"e2eP99ms":2250,"requestsWaiting":3,"kvCacheUsage":0.42,"inferWaitP99ms":4}`
const tritonInference = `{"namespace":"ml","job":"train","available":true,"engine":"triton","queueTimeMeanMs":0.86,"e2eMeanMs":7.4}`

// routeClient answers the flight route and the inference route with different bodies.
func routeClient(flightBody, inferenceBody string, inferenceCode int) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/api/flight/inference/") {
			return response(inferenceCode, inferenceBody), nil
		}
		return response(200, flightBody), nil
	})}
}

func TestInferenceFlag(t *testing.T) {
	tok := map[string]string{tokenEnv: "secret-token"}
	base := []string{"-gateway", "https://g.example", "-namespace", "ml", "-job", "train", "-inference"}

	code, out, _ := runCLI(t, routeClient(completeBody, vllmInference, 200), tok, "", append(base, "-o", "text")...)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{`engine "vllm"`, "time to first token p99   475 ms", "inter-token latency p99   72.2 ms",
		"engine queue time p99     50 ms", "end-to-end latency p99    2.25 s", "requests waiting          3", "KV cache usage            42%",
		"network accept wait p99   4 ms", "not queue time"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}

	// Counter-only engine: means are labelled as means, unexported metrics read "not measured".
	_, out, _ = runCLI(t, routeClient(completeBody, tritonInference, 200), tok, "", append(base, "-o", "text")...)
	for _, want := range []string{"time to first token p99   not measured", "engine queue time MEAN    0.86 ms", "end-to-end latency MEAN   7.4 ms", "KV cache usage            not measured"} {
		if !strings.Contains(out, want) {
			t.Errorf("triton output lacks %q:\n%s", want, out)
		}
	}

	// JSON: a combined object, valid JSON.
	_, out, _ = runCLI(t, routeClient(completeBody, vllmInference, 200), tok, "", base...)
	var combined struct {
		Flight    map[string]interface{} `json:"flight"`
		Inference map[string]interface{} `json:"inference"`
	}
	if err := json.Unmarshal([]byte(out), &combined); err != nil || combined.Flight["job"] != "train" || combined.Inference["engine"] != "vllm" {
		t.Errorf("combined json: %v %q", err, out)
	}

	// Not published: says so instead of printing zeros.
	_, out, _ = runCLI(t, routeClient(completeBody, `{"namespace":"ml","job":"train","available":false}`, 200), tok, "", append(base, "-o", "text")...)
	if !strings.Contains(out, "no engine metrics published") || strings.Contains(out, "475") {
		t.Errorf("unavailable output: %s", out)
	}

	// A failing inference route is an error, not silence.
	if code, _, errs := runCLI(t, routeClient(completeBody, `{"detail":"nope"}`, 403), tok, "", base...); code != 1 || !strings.Contains(errs, "inference") {
		t.Errorf("inference failure: %d %q", code, errs)
	}

	// Without the flag the output is exactly the flight report.
	_, out, _ = runCLI(t, routeClient(completeBody, vllmInference, 200), tok, "", base[:len(base)-1]...)
	if strings.Contains(out, `"inference"`) || !strings.HasPrefix(out, `{"namespace"`) {
		t.Errorf("default output changed: %s", out)
	}

	// Control characters in engine text are escaped.
	evil := strings.Replace(vllmInference, `"engine":"vllm"`, `"engine":"a\u001b[31mb"`, 1)
	_, out, _ = runCLI(t, routeClient(completeBody, evil, 200), tok, "", append(base, "-o", "text")...)
	if strings.Contains(out, "\x1b") {
		t.Error("escape sequence reached the terminal")
	}
}
