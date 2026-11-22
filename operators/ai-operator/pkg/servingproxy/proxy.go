// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package servingproxy measures completed inference requests without buffering streams.
package servingproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Proxy struct {
	Handler http.Handler
	Metrics http.Handler
}

func New(target string, labels prometheus.Labels) (*Proxy, error) {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("invalid upstream URL")
	}
	reg := prometheus.NewRegistry()
	requests := prometheus.NewCounter(prometheus.CounterOpts{Name: "gryvia_inference_requests_total", Help: "Completed requests including errors", ConstLabels: labels})
	failures := prometheus.NewCounter(prometheus.CounterOpts{Name: "gryvia_inference_errors_total", Help: "Completed requests with upstream errors or HTTP 5xx", ConstLabels: labels})
	duration := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "gryvia_inference_request_duration_seconds", Help: "Completed request duration including stream delivery", ConstLabels: labels, Buckets: []float64{.01, .05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 300}})
	active := prometheus.NewGauge(prometheus.GaugeOpts{Name: "gryvia_inference_active_requests", Help: "Requests currently being served", ConstLabels: labels})
	reg.MustRegister(requests, failures, duration, active)
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.ModifyResponse = func(resp *http.Response) error {
		if track := labels["track"]; track != "" {
			resp.Header.Set("X-Gryvia-Track", track)
		}
		if version := labels["model_version"]; version != "" {
			resp.Header.Set("X-Gryvia-Model-Version", version)
		}
		return nil
	}
	rp.FlushInterval = -1 // flush every write, including streaming responses
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		active.Inc()
		defer active.Dec()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			abort := recover()
			requests.Inc()
			duration.Observe(time.Since(start).Seconds())
			if rw.status >= 500 || r.Context().Err() != nil || abort != nil {
				failures.Inc()
			}
			if abort != nil {
				panic(abort)
			} // net/http owns aborted-stream handling
		}()
		rp.ServeHTTP(rw, r)
	})
	return &Proxy{Handler: handler, Metrics: promhttp.HandlerFor(reg, promhttp.HandlerOpts{})}, nil
}

type responseWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *responseWriter) WriteHeader(s int) {
	if !w.wrote && s >= 200 {
		w.status = s
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(s)
}
func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Run is an operator-binary subcommand used by the injected sidecar.
func Run() error {
	labels := prometheus.Labels{}
	for _, key := range []string{"namespace", "inference", "track", "model_version", "deployment_uid", "revision", "pod"} {
		value := os.Getenv("GRYVIA_" + map[string]string{"namespace": "NAMESPACE", "inference": "INFERENCE", "track": "TRACK", "model_version": "MODEL_VERSION", "deployment_uid": "DEPLOYMENT_UID", "revision": "REVISION", "pod": "POD"}[key])
		if value == "" && key != "model_version" {
			return fmt.Errorf("missing telemetry label %s", key)
		}
		labels[key] = value
	}
	p, err := New(os.Getenv("GRYVIA_UPSTREAM"), labels)
	if err != nil {
		return err
	}
	metrics := http.NewServeMux()
	metrics.Handle("/metrics", p.Metrics)
	metrics.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	serving := &http.Server{Addr: ":18080", Handler: p.Handler, ReadHeaderTimeout: 10 * time.Second}
	monitoring := &http.Server{Addr: ":18081", Handler: metrics, ReadHeaderTimeout: 5 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- serving.ListenAndServe() }()
	go func() { errs <- monitoring.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-errs:
		cancel()
	}
	drain, c := context.WithTimeout(context.Background(), 120*time.Second)
	defer c()
	serving.Shutdown(drain)
	monitoring.Shutdown(drain)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
