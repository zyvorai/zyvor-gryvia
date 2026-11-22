// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func analyzedCanary() *gryviav1.GryviaInferenceService {
	return canarySvc(func(s *gryviav1.GryviaInferenceService) {
		s.Annotations = map[string]string{analysisErrorKey: "0.01", analysisLatencyKey: "0.5"}
		s.Spec.Canary.AutoPromote = true
		s.Spec.Canary.PromoteAfterSeconds = 40
		s.Spec.HealthCheck = &gryviav1.HealthCheckConfig{AutoRollback: true, FailureThreshold: 2, IntervalSeconds: 10}
	})
}

type analysisFixture struct {
	mode   atomic.Int32
	unix   atomic.Int64
	calls  atomic.Int32
	server *httptest.Server
}

func newAnalysisFixture(t *testing.T) *analysisFixture {
	f := &analysisFixture{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		q := r.URL.Query().Get("query")
		if r.URL.Path != "/api/v1/query" || r.URL.Query().Get("time") == "" || !strings.Contains(q, `namespace="ns",inference="chat",track="canary",model_version="v2",deployment_uid=`) || !strings.Contains(q, `revision="`) {
			http.Error(w, "invalid query", 400)
			return
		}
		if f.mode.Load() == 2 || (f.mode.Load() == 6 && strings.HasPrefix(q, "min(timestamp(gryvia_inference_errors_total")) {
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			return
		}
		value := 0.005
		switch {
		case strings.HasPrefix(q, "histogram_quantile"):
			value = 0.2
			if f.mode.Load() == 4 {
				value = 2
			}
		case strings.HasPrefix(q, "sum(increase(gryvia_inference_requests_total"):
			value = 200
			if f.mode.Load() == 5 {
				value = 1
			}
		case strings.HasPrefix(q, "min(timestamp"):
			value = float64(f.unix.Load())
			if f.mode.Load() == 3 {
				value -= 31
			}
		default:
			if f.mode.Load() == 1 {
				value = 0.2
			}
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"value":[%d,"%g"]}]}}`, f.unix.Load(), value)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestAnalysisConfig(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{analysisErrorKey, "NaN"}, {analysisErrorKey, "Inf"}, {analysisErrorKey, "-0.1"}, {analysisErrorKey, "1.1"},
		{analysisLatencyKey, "0"}, {analysisLatencyKey, "NaN"}, {analysisLatencyKey, "-1"}, {analysisSamplesKey, "0"}, {analysisSamplesKey, "1.5"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			s := analyzedCanary()
			s.Annotations[tc.key] = tc.value
			if _, err := analysisConfig(s); err == nil {
				t.Fatal("accepted invalid policy")
			}
		})
	}
	s := analyzedCanary()
	delete(s.Annotations, analysisLatencyKey)
	if _, err := analysisConfig(s); err == nil {
		t.Fatal("accepted partial policy")
	}
	s.Annotations = nil
	if cfg, err := analysisConfig(s); cfg != nil || err != nil {
		t.Fatal("default must remain disabled")
	}
}

func TestPrometheusResponseValidation(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		name, body string
		code       int
		ok         bool
	}{
		{"valid", `{"status":"success","data":{"resultType":"vector","result":[{"value":[1000,"0.1"]}]}}`, 200, true},
		{"missing", `{"status":"success","data":{"resultType":"vector","result":[]}}`, 200, false},
		{"multiple", `{"status":"success","data":{"resultType":"vector","result":[{"value":[1000,"1"]},{"value":[1000,"2"]}]}}`, 200, false},
		{"nan", `{"status":"success","data":{"resultType":"vector","result":[{"value":[1000,"NaN"]}]}}`, 200, false},
		{"stale", `{"status":"success","data":{"resultType":"vector","result":[{"value":[969,"1"]}]}}`, 200, false},
		{"future", `{"status":"success","data":{"resultType":"vector","result":[{"value":[1002,"1"]}]}}`, 200, false},
		{"partial", `{"status":"success","warnings":["partial data"],"data":{"resultType":"vector","result":[{"value":[1000,"1"]}]}}`, 200, false},
		{"oversized", strings.Repeat("x", 65537), 200, false}, {"server error", "", 503, false}, {"redirect", "", 302, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code); fmt.Fprint(w, tc.body) }))
			defer ts.Close()
			_, err := prometheusSample(context.Background(), ts.URL, "x", now)
			if (err == nil) != tc.ok {
				t.Fatalf("error %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := prometheusSample(ctx, "http://127.0.0.1:1", "x", now); err == nil {
		t.Fatal("ignored cancellation")
	}
	for _, endpoint := range []string{"file:///tmp/metrics", "http://user:pass@localhost", "http://localhost/?token=secret", "http://localhost/#x"} {
		if _, err := prometheusSample(context.Background(), endpoint, "x", now); err == nil {
			t.Fatal("accepted invalid endpoint")
		}
	}
}

func TestCanaryAnalysisPromotionHold(t *testing.T) {
	c := mlClient(analyzedCanary())
	clk := newClock()
	r := newInferReconciler(c, clk)
	f := newAnalysisFixture(t)
	r.PrometheusURL = f.server.URL
	step := func() { f.unix.Store(clk.Now().Unix()); reconcileOnce(t, r, "ns", "chat") }
	step()
	setReady(t, c, "chat-canary", 2)
	step()
	if f.calls.Load() != 0 {
		t.Fatal("queried incomplete window")
	}
	clk.Add(60 * time.Second)
	step()
	if cond := findCond(getInfer(t, c, "chat").Status.Conditions, ConditionCanaryAnalysis); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition %+v", cond)
	}
	clk.Add(20 * time.Second)
	f.mode.Store(1)
	step()
	step()
	s := getInfer(t, c, "chat")
	if s.Status.ConsecutiveFailures != 1 || !s.Status.CanaryStatus.Active {
		t.Fatalf("repeated reconcile counted failure: %+v", s.Status)
	}
	f.mode.Store(2)
	step()
	s = getInfer(t, c, "chat")
	if s.Status.ConsecutiveFailures != 0 || s.Status.CanaryStatus.Health != canaryHealthPending || routeWeight(s) != 50 {
		t.Fatal("missing telemetry must block promotion, clear consecutive breaches and retain analysis traffic")
	}
	f.mode.Store(0)
	step()
	clk.Add(39 * time.Second)
	step()
	if !getInfer(t, c, "chat").Status.CanaryStatus.Active {
		t.Fatal("promoted before new success hold")
	}
	clk.Add(time.Second)
	step()
	s = getInfer(t, c, "chat")
	if s.Status.CanaryStatus.Health != canaryHealthPromoted {
		t.Fatalf("did not promote: %+v", s.Status)
	}
}

func TestCanaryAnalysisRollback(t *testing.T) {
	for _, mode := range []int32{1, 4} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			c := mlClient(analyzedCanary())
			clk := newClock()
			r := newInferReconciler(c, clk)
			f := newAnalysisFixture(t)
			r.PrometheusURL = f.server.URL
			step := func() { f.unix.Store(clk.Now().Unix()); reconcileOnce(t, r, "ns", "chat") }
			step()
			setReady(t, c, "chat-canary", 2)
			step()
			clk.Add(60 * time.Second)
			f.mode.Store(mode)
			step()
			clk.Add(10 * time.Second)
			step()
			s := getInfer(t, c, "chat")
			if s.Status.CanaryStatus.Health != canaryHealthRolledBack || routeWeight(s) != 0 {
				t.Fatalf("no rollback: %+v", s.Status)
			}
			primary := &appsv1.Deployment{}
			mustGet(t, c, "ns", "chat-inference", primary)
			if primary.Annotations[annotationRolledBack] != "v2" {
				t.Fatal("rejected version not persisted")
			}
			step()
			if getInfer(t, c, "chat").Status.CanaryStatus.Active {
				t.Fatal("recreated rejected version")
			}
		})
	}
}

func TestCanaryAnalysisMissingStaleLowTraffic(t *testing.T) {
	for _, mode := range []int32{2, 3, 5, 6} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			c := mlClient(analyzedCanary())
			clk := newClock()
			r := newInferReconciler(c, clk)
			f := newAnalysisFixture(t)
			r.PrometheusURL = f.server.URL
			step := func() { f.unix.Store(clk.Now().Unix()); reconcileOnce(t, r, "ns", "chat") }
			step()
			setReady(t, c, "chat-canary", 2)
			step()
			clk.Add(60 * time.Second)
			f.mode.Store(mode)
			step()
			clk.Add(time.Hour)
			step()
			s := getInfer(t, c, "chat")
			if !s.Status.CanaryStatus.Active || s.Status.ConsecutiveFailures != 0 || findCond(s.Status.Conditions, ConditionCanaryAnalysis).Status != metav1.ConditionUnknown {
				t.Fatalf("unsafe missing-data decision %+v", s.Status)
			}
		})
	}
}

func TestAnalysisPolicyChangeRestartsWindow(t *testing.T) {
	c := mlClient(analyzedCanary())
	clk := newClock()
	r := newInferReconciler(c, clk)
	f := newAnalysisFixture(t)
	r.PrometheusURL = f.server.URL
	step := func() { f.unix.Store(clk.Now().Unix()); reconcileOnce(t, r, "ns", "chat") }
	step()
	setReady(t, c, "chat-canary", 2)
	step()
	clk.Add(60 * time.Second)
	step()
	clk.Add(39 * time.Second)
	s := getInfer(t, c, "chat")
	s.Annotations[analysisLatencyKey] = "0.4"
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	step()
	s = getInfer(t, c, "chat")
	if !s.Status.CanaryStatus.Active || findCond(s.Status.Conditions, ConditionCanaryAnalysis).Reason != "WarmingUp" {
		t.Fatal("policy change reused previous evidence")
	}
	d := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-canary", d)
	if d.Annotations[analysisSinceKey] != "" {
		t.Fatal("old promotion hold retained")
	}
}

func TestAnalysisRestartAndObservationGap(t *testing.T) {
	c := mlClient(analyzedCanary())
	clk := newClock()
	r := newInferReconciler(c, clk)
	f := newAnalysisFixture(t)
	r.PrometheusURL = f.server.URL
	step := func() { f.unix.Store(clk.Now().Unix()); reconcileOnce(t, r, "ns", "chat") }
	step()
	setReady(t, c, "chat-canary", 2)
	step()
	clk.Add(60 * time.Second)
	step()
	clk.Add(20 * time.Second)
	// A new reconciler uses the persisted hold; it must not restart a short hold.
	r = newInferReconciler(c, clk)
	r.PrometheusURL = f.server.URL
	step()
	clk.Add(20 * time.Second)
	step()
	if getInfer(t, c, "chat").Status.CanaryStatus.Health != canaryHealthPromoted {
		t.Fatal("restart lost persisted hold")
	}
	// Evaluate the gap rule directly on a persisted canary, without promotion/deletion.
	svc := analyzedCanary()
	d := &appsv1.Deployment{ObjectMeta: objMeta("ns", "gap-canary")}
	if err := c.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	cfg, _ := analysisConfig(svc)
	if ready, err := r.analysisPromotionReady(context.Background(), svc, d, cfg, true, clk.Now()); ready || err != nil {
		t.Fatalf("initial hold: %v %v", ready, err)
	}
	clk.Add(time.Hour)
	if ready, err := r.analysisPromotionReady(context.Background(), svc, d, cfg, true, clk.Now()); ready || err != nil {
		t.Fatalf("observation gap reused old success: %v %v", ready, err)
	}
}

func TestAnalysisNoEndpointBlocksPromotion(t *testing.T) {
	c := mlClient(analyzedCanary())
	clk := newClock()
	r := newInferReconciler(c, clk)
	reconcileOnce(t, r, "ns", "chat")
	setReady(t, c, "chat-canary", 2)
	clk.Add(time.Hour)
	reconcileOnce(t, r, "ns", "chat")
	s := getInfer(t, c, "chat")
	cond := findCond(s.Status.Conditions, ConditionCanaryAnalysis)
	if !s.Status.CanaryStatus.Active || cond == nil || cond.Reason != "NotConfigured" || s.Status.ConsecutiveFailures != 0 {
		t.Fatalf("missing endpoint permitted decision %+v", s.Status)
	}
}
