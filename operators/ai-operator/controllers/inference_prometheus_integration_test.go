// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//go:build integration

package controllers

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/servingproxy"
	appsv1 "k8s.io/api/apps/v1"
)

// Real proxy/exporter, Prometheus scrape and PromQL drive controller rollback.
// Object persistence uses a fake client; envtest separately validates real APIs.
func TestProxyPrometheusSLO(t *testing.T) {
	binary := os.Getenv("GRYVIA_PROMETHEUS_BINARY")
	if binary == "" {
		t.Skip("set GRYVIA_PROMETHEUS_BINARY for real Prometheus integration")
	}
	svc := analyzedCanary()
	svc.Spec.Canary.AutoPromote = false
	svc.Spec.HealthCheck.IntervalSeconds = 1
	svc.Annotations[analysisSamplesKey] = "10"
	c := mlClient(svc)
	clk := newClock()
	clk.t = time.Now().Add(-61 * time.Second)
	r := newInferReconciler(c, clk)
	r.PrometheusURL = "http://placeholder"
	reconcileOnce(t, r, "ns", "chat")
	setReady(t, c, "chat-canary", 2)
	d := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-canary", d)
	var failing atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if failing.Load() {
			http.Error(w, "injected failure", 503)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()
	p, err := servingproxy.New(backend.URL, prometheus.Labels{"namespace": "ns", "inference": "chat", "track": "canary", "model_version": "v2", "deployment_uid": string(d.UID), "revision": d.Annotations[annotationSpecHash]})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(p.Handler)
	defer proxy.Close()
	metrics := httptest.NewServer(p.Metrics)
	defer metrics.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	root := t.TempDir()
	config := filepath.Join(root, "prometheus.yaml")
	content := "global:\n  scrape_interval: 1s\n  scrape_timeout: 1s\nscrape_configs:\n- job_name: gryvia\n  static_configs:\n  - targets: ['" + strings.TrimPrefix(metrics.URL, "http://") + "']\n"
	if err := os.WriteFile(config, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config.file="+config, "--storage.tsdb.path="+filepath.Join(root, "data"), "--web.listen-address=127.0.0.1:"+strconv.Itoa(port))
	logfile, err := os.Create(filepath.Join(root, "prometheus.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logfile.Close()
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	r.PrometheusURL = "http://127.0.0.1:" + strconv.Itoa(port)
	reconcileOnce(t, r, "ns", "chat") // persist warmup referencing the real endpoint
	r.Clock = time.Now
	waitFor := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("timed out waiting for real telemetry")
	}
	waitFor(func() bool {
		_, e := prometheusSample(context.Background(), r.PrometheusURL, "gryvia_inference_requests_total", time.Now())
		return e == nil
	})
	send := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			resp, e := http.Get(proxy.URL + "/v1/completions")
			if e != nil {
				t.Fatal(e)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	send(100)
	waitFor(func() bool {
		reconcileOnce(t, r, "ns", "chat")
		s := getInfer(t, c, "chat")
		cond := findCond(s.Status.Conditions, ConditionCanaryAnalysis)
		return cond != nil && cond.Reason == "SLOPassed"
	})
	failing.Store(true)
	send(100)
	waitFor(func() bool {
		reconcileOnce(t, r, "ns", "chat")
		return getInfer(t, c, "chat").Status.CanaryStatus.Health == canaryHealthRolledBack
	})
	primary := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", primary)
	if primary.Annotations[annotationRolledBack] != svc.Spec.Canary.ModelVersion {
		t.Fatal("real failures did not reject canary")
	}
}
