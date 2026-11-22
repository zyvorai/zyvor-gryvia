// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package servingproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsErrorsAndStreaming(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/error" {
			http.Error(w, "failed", 503)
			return
		}
		if r.URL.Path == "/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			<-release
			fmt.Fprint(w, "data: last\n\n")
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()
	p, err := New(backend.URL, prometheus.Labels{"track": "canary", "revision": "r1"})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(p.Handler)
	defer proxy.Close()
	for _, path := range []string{"/", "/error"} {
		resp, e := http.Get(proxy.URL + path)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(proxy.URL + "/stream")
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	close(release)
	if err != nil || line != "data: first\n" {
		t.Fatalf("stream was buffered: %q %v", line, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	out := httptest.NewRecorder()
	p.Metrics.ServeHTTP(out, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(out.Body.String(), `gryvia_inference_errors_total{revision="r1",track="canary"} 1`) || !strings.Contains(out.Body.String(), `gryvia_inference_requests_total{revision="r1",track="canary"} 3`) {
		t.Fatal(out.Body.String())
	}
}
func TestUpstreamFailureIsMeasured(t *testing.T) {
	p, err := New("http://127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	p.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 502 {
		t.Fatal(w.Code)
	}
	m := httptest.NewRecorder()
	p.Metrics.ServeHTTP(m, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(m.Body.String(), "gryvia_inference_errors_total 1") {
		t.Fatal(m.Body.String())
	}
}

func TestTruncatedStreamCountsFailure(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("short"))
	}))
	defer backend.Close()
	p, err := New(backend.URL, prometheus.Labels{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil).WithContext(context.WithValue(context.Background(), http.ServerContextKey, &http.Server{}))
	func() {
		defer func() {
			if v := recover(); v != http.ErrAbortHandler {
				t.Fatalf("expected stream abort, got %v", v)
			}
		}()
		p.Handler.ServeHTTP(httptest.NewRecorder(), req)
	}()
	metrics := httptest.NewRecorder()
	p.Metrics.ServeHTTP(metrics, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{"gryvia_inference_requests_total 1", "gryvia_inference_errors_total 1", "gryvia_inference_active_requests 0"} {
		if !strings.Contains(metrics.Body.String(), want) {
			t.Fatalf("missing %s", want)
		}
	}
}
