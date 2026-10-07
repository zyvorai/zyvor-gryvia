package controllers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/meta"
)

func sloCfg() *sloConfig {
	e := 0.01
	return &sloConfig{ttft: 0.5, itl: 0.05, maxErr: &e, minRequests: 100, window: time.Minute, step: 1, downAfter: 3, job: "chat"}
}

func TestDecideSLO(t *testing.T) {
	ok := sloMeasurement{requests: 500, errorRate: 0.001, ttft: 0.2, itl: 0.02, hasWaiting: true}
	slow := ok
	slow.ttft = 0.9
	slowQueued := slow
	slowQueued.waiting = 4
	slowKV := slow
	slowKV.hasWaiting, slowKV.hasKVCache, slowKV.kvCache = false, true, 0.97
	slowBlind := slow
	slowBlind.hasWaiting = false
	errs := ok
	errs.errorRate, errs.waiting = 0.2, 1
	quiet := ok
	quiet.requests = 10

	for _, tc := range []struct {
		name           string
		m              sloMeasurement
		prev           gryviav1.InferenceSLOStatus
		observed       int32
		floor, healthy int32
		state          string
		breached       bool
	}{
		{"healthy counts up", ok, gryviav1.InferenceSLOStatus{HealthyWindows: 1}, 2, 2, 2, sloStateHealthy, false},
		{"healthy run lowers floor", ok, gryviav1.InferenceSLOStatus{FloorReplicas: 5, HealthyWindows: 2}, 5, 4, 0, sloStateScaledDown, false},
		{"never below spec minimum", ok, gryviav1.InferenceSLOStatus{HealthyWindows: 9}, 2, 2, 10, sloStateHealthy, false},
		{"breach without overload holds", slow, gryviav1.InferenceSLOStatus{HealthyWindows: 2}, 3, 2, 0, sloStateBreached, true},
		{"breach without overload signal holds", slowBlind, gryviav1.InferenceSLOStatus{}, 3, 2, 0, sloStateBreached, true},
		{"breach with queue raises above observed", slowQueued, gryviav1.InferenceSLOStatus{}, 3, 4, 0, sloStateScaledUp, true},
		{"breach with KV cache raises", slowKV, gryviav1.InferenceSLOStatus{FloorReplicas: 4}, 2, 5, 0, sloStateScaledUp, true},
		{"error breach with queue raises", errs, gryviav1.InferenceSLOStatus{}, 2, 3, 0, sloStateScaledUp, true},
		{"capped at max", slowQueued, gryviav1.InferenceSLOStatus{FloorReplicas: 6}, 6, 6, 0, sloStateBreached, true},
		{"too little traffic holds", quiet, gryviav1.InferenceSLOStatus{FloorReplicas: 4, HealthyWindows: 2}, 4, 4, 2, sloStateLowTraffic, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := decideSLO(sloCfg(), tc.m, tc.prev, tc.observed, 2, 6)
			if d.floor != tc.floor || d.healthy != tc.healthy || d.state != tc.state || d.breached != tc.breached {
				t.Fatalf("got floor %d healthy %d state %s breached %v (%s)", d.floor, d.healthy, d.state, d.breached, d.msg)
			}
		})
	}
}

func TestSLOSettings(t *testing.T) {
	svc := newInfer("chat", func(s *gryviav1.GryviaInferenceService) { s.Spec.SLO = &gryviav1.InferenceSLO{Enabled: true} })
	if _, err := sloSettings(svc); err == nil {
		t.Fatal("an SLO without objectives must be rejected")
	}
	svc.Spec.SLO = &gryviav1.InferenceSLO{Enabled: true, MaxTTFTMilliseconds: 800, WindowSeconds: 5, ScaleDownAfterWindows: 1, MetricsJob: "chat-engine"}
	cfg, err := sloSettings(svc)
	if err != nil || cfg.ttft != 0.8 || cfg.window != 30*time.Second || cfg.downAfter != 3 || cfg.job != "chat-engine" || cfg.minRequests != 100 {
		t.Fatalf("settings %+v err %v", cfg, err)
	}
}

// fakeSLOProm answers the SLO queries from a mutable table; timestamp queries return the current time unless stale.
type fakeSLOProm struct {
	mu     sync.Mutex
	now    func() time.Time
	values map[string]float64
	stale  map[string]bool
	server *httptest.Server
}

func newFakeSLOProm(t *testing.T, now func() time.Time) *fakeSLOProm {
	f := &fakeSLOProm{now: now, values: map[string]float64{}, stale: map[string]bool{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		q := r.URL.Query().Get("query")
		var key string
		switch {
		case strings.Contains(q, "gryvia_inference_errors_total"):
			key = "errorRate"
		case strings.Contains(q, "gryvia_inference_requests_total"):
			key = "requests"
		case strings.Contains(q, `metric="ttft_p99"`):
			key = "ttft"
		case strings.Contains(q, `metric="itl_p99"`):
			key = "itl"
		case strings.Contains(q, `state="waiting"`):
			key = "waiting"
		case strings.Contains(q, "kv_cache_usage_ratio"):
			key = "kvCache"
		}
		if !strings.Contains(q, `namespace="ns"`) {
			http.Error(w, "bad query", 400)
			return
		}
		v, present := f.values[key]
		if !present {
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			return
		}
		ts := f.now().Unix()
		if strings.HasPrefix(q, "min(timestamp(") {
			v = float64(ts)
			if f.stale[key] {
				v -= 600
			}
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"value":[%d,"%g"]}]}}`, ts, v)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSLOProm) set(kv map[string]float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range kv {
		f.values[k] = v
	}
}

func TestMeasureSLO(t *testing.T) {
	clk := newClock()
	prom := newFakeSLOProm(t, clk.Now)
	svc := newInfer("chat")
	cfg := sloCfg()
	prom.set(map[string]float64{"requests": 400, "errorRate": 0.002, "ttft": 0.7, "itl": 0.03, "kvCache": 0.95})
	m, err := measureSLO(t.Context(), prom.server.URL, svc, cfg, clk.Now())
	if err != nil || m.ttft != 0.7 || m.hasWaiting || !m.hasKVCache {
		t.Fatalf("measurement %+v err %v", m, err)
	}
	if over, known := m.overload(); !over || !known {
		t.Fatal("KV cache at 95% must count as overload")
	}
	prom.stale["ttft"] = true
	if _, err := measureSLO(t.Context(), prom.server.URL, svc, cfg, clk.Now()); err == nil || !strings.Contains(err.Error(), "ttft") {
		t.Fatalf("stale TTFT must make the window unavailable, got %v", err)
	}
	prom.stale["ttft"] = false
	delete(prom.values, "itl")
	if _, err := measureSLO(t.Context(), prom.server.URL, svc, cfg, clk.Now()); err == nil {
		t.Fatal("a missing configured objective must make the window unavailable")
	}
}

func sloSvc() *gryviav1.GryviaInferenceService {
	return newInfer("chat", func(s *gryviav1.GryviaInferenceService) {
		s.Spec.Autoscaling = &gryviav1.AutoscalingConfig{Enabled: true, MinReplicas: 2, MaxReplicas: 5}
		s.Spec.SLO = &gryviav1.InferenceSLO{Enabled: true, MaxTTFTMilliseconds: 500, ScaleDownAfterWindows: 3}
	})
}

func hpaMin(t *testing.T, r *GryviaInferenceServiceReconciler) int32 {
	t.Helper()
	h := &autoscalingv2.HorizontalPodAutoscaler{}
	mustGet(t, r.Client, "ns", inferHPAName(sloSvc()), h)
	return *h.Spec.MinReplicas
}

func TestInference_SLORaisesAndLowersHPAFloor(t *testing.T) {
	c := mlClient(sloSvc())
	clk := newClock()
	r := newInferReconciler(c, clk)
	prom := newFakeSLOProm(t, clk.Now)
	r.PrometheusURL = prom.server.URL

	prom.set(map[string]float64{"requests": 500, "ttft": 0.9, "waiting": 3})
	reconcileOnce(t, r, "ns", "chat")
	svc := getInfer(t, c, "chat")
	if svc.Status.SLO == nil || svc.Status.SLO.State != sloStateScaledUp || hpaMin(t, r) != 3 {
		t.Fatalf("breach with queue: status %+v hpa min %d", svc.Status.SLO, hpaMin(t, r))
	}
	primary := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", primary)
	if primary.Spec.Template.Labels[labelJob] != "chat" {
		t.Errorf("SLO pods need the collector job label, got %v", primary.Spec.Template.Labels)
	}

	reconcileOnce(t, r, "ns", "chat")
	if hpaMin(t, r) != 3 {
		t.Fatal("the floor must not move again before the next window")
	}

	prom.set(map[string]float64{"ttft": 0.2, "waiting": 0})
	for i := 0; i < 3; i++ {
		clk.Add(61 * time.Second)
		reconcileOnce(t, r, "ns", "chat")
	}
	svc = getInfer(t, c, "chat")
	if svc.Status.SLO.State != sloStateScaledDown || hpaMin(t, r) != 2 || svc.Status.SLO.FloorReplicas != 0 {
		t.Fatalf("after healthy windows: status %+v hpa min %d", svc.Status.SLO, hpaMin(t, r))
	}
	if cond := meta.FindStatusCondition(svc.Status.Conditions, ConditionSLO); cond == nil || cond.Reason != sloStateScaledDown {
		t.Fatalf("condition %+v", cond)
	}

	prom.set(map[string]float64{"ttft": 0.9, "waiting": 3})
	prom.stale["ttft"] = true
	clk.Add(61 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	svc = getInfer(t, c, "chat")
	if svc.Status.SLO.State != sloStateUnknown || hpaMin(t, r) != 2 {
		t.Fatalf("stale metrics must hold: status %+v hpa min %d", svc.Status.SLO, hpaMin(t, r))
	}
}

func TestInference_SLOGates(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		mutate       func(*gryviav1.GryviaInferenceService)
	}{
		{"needs autoscaling", "RequiresAutoscaling", func(s *gryviav1.GryviaInferenceService) { s.Spec.Autoscaling = nil }},
		{"not with scale to zero", "ScaleToZero", func(s *gryviav1.GryviaInferenceService) {
			s.Spec.ScaleToZero = &gryviav1.ScaleToZeroConfig{Enabled: true, IdleSeconds: 600}
		}},
		{"needs an objective", "InvalidConfig", func(s *gryviav1.GryviaInferenceService) { s.Spec.SLO.MaxTTFTMilliseconds = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sloSvc()
			tc.mutate(s)
			c := mlClient(s)
			r := newInferReconciler(c, newClock())
			r.PrometheusURL = "http://prometheus.invalid"
			reconcileOnce(t, r, "ns", "chat")
			cond := meta.FindStatusCondition(getInfer(t, c, "chat").Status.Conditions, ConditionSLO)
			if cond == nil || cond.Reason != tc.reason {
				t.Fatalf("condition %+v", cond)
			}
		})
	}

	c := mlClient(sloSvc())
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")
	if cond := meta.FindStatusCondition(getInfer(t, c, "chat").Status.Conditions, ConditionSLO); cond == nil || cond.Reason != "NotConfigured" {
		t.Fatalf("without Prometheus: %+v", cond)
	}
	if hpaMin(t, r) != 2 {
		t.Fatal("without Prometheus the HPA keeps the spec minimum")
	}
}
