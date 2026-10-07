package controllers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConditionSLO = "SLOControl"
	labelJob     = "gryvia.io/job"

	sloStateHealthy      = "Healthy"
	sloStateBreached     = "Breached"
	sloStateScaledUp     = "ScaledUp"
	sloStateScaledDown   = "ScaledDown"
	sloStateLowTraffic   = "InsufficientTraffic"
	sloStateUnknown      = "Unknown"
	sloKVCacheOverloaded = 0.9
	// Engine gauges come from the collector (15 s scrape) through Prometheus; older samples are treated as missing.
	sloMaxSampleAge = 90.0
)

type sloConfig struct {
	ttft, itl   float64 // seconds; 0 = no objective
	maxErr      *float64
	minRequests float64
	window      time.Duration
	step        int32
	downAfter   int32
	job         string
}

type sloMeasurement struct {
	requests, errorRate, ttft, itl float64
	waiting, kvCache               float64
	hasWaiting, hasKVCache         bool
}

func (m sloMeasurement) overload() (overloaded, known bool) {
	if m.hasWaiting && m.waiting > 0 {
		return true, true
	}
	if m.hasKVCache && m.kvCache >= sloKVCacheOverloaded {
		return true, true
	}
	return false, m.hasWaiting || m.hasKVCache
}

type sloDecision struct {
	floor, healthy int32
	state, msg     string
	breached       bool
}

func sloActive(svc *gryviav1.GryviaInferenceService) bool {
	return svc.Spec.SLO != nil && svc.Spec.SLO.Enabled
}

func sloMetricsJob(svc *gryviav1.GryviaInferenceService) string {
	if svc.Spec.SLO != nil && svc.Spec.SLO.MetricsJob != "" {
		return svc.Spec.SLO.MetricsJob
	}
	return svc.Name
}

func sloSettings(svc *gryviav1.GryviaInferenceService) (*sloConfig, error) {
	s := svc.Spec.SLO
	cfg := &sloConfig{
		ttft: float64(s.MaxTTFTMilliseconds) / 1000, itl: float64(s.MaxInterTokenMilliseconds) / 1000, maxErr: s.MaxErrorRate,
		minRequests: 100, window: 60 * time.Second, step: 1, downAfter: 10, job: sloMetricsJob(svc),
	}
	if cfg.ttft <= 0 && cfg.itl <= 0 && cfg.maxErr == nil {
		return nil, fmt.Errorf("spec.slo needs at least one of maxTTFTMilliseconds, maxInterTokenMilliseconds or maxErrorRate")
	}
	if cfg.maxErr != nil && (*cfg.maxErr < 0 || *cfg.maxErr > 1) {
		return nil, fmt.Errorf("spec.slo.maxErrorRate must be between 0 and 1")
	}
	if s.MinRequests > 0 {
		cfg.minRequests = float64(s.MinRequests)
	}
	if s.WindowSeconds > 0 {
		cfg.window = time.Duration(min(max(s.WindowSeconds, 30), 600)) * time.Second
	}
	if s.ScaleUpStep > 0 {
		cfg.step = min(s.ScaleUpStep, 10)
	}
	if s.ScaleDownAfterWindows > 0 {
		cfg.downAfter = min(max(s.ScaleDownAfterWindows, 3), 1000)
	}
	return cfg, nil
}

// sloFloor is the HPA minimum: the spec minimum, raised to the SLO floor while SLO control holds one.
func sloFloor(svc *gryviav1.GryviaInferenceService, lo, hi int32) int32 {
	if !sloActive(svc) || svc.Status.SLO == nil || svc.Status.SLO.FloorReplicas <= lo {
		return lo
	}
	return min(svc.Status.SLO.FloorReplicas, hi)
}

// decideSLO evaluates one window. The floor only rises when an objective is breached and the engine reports
// overload (requests waiting or KV cache nearly full): a breach without overload is not fixed by more replicas.
// It falls by one after downAfter consecutive healthy windows. A window with too little traffic holds everything.
func decideSLO(cfg *sloConfig, m sloMeasurement, prev gryviav1.InferenceSLOStatus, observed, lo, hi int32) sloDecision {
	cur := min(max(prev.FloorReplicas, lo), hi)
	d := sloDecision{floor: cur, healthy: prev.HealthyWindows}
	if m.requests < cfg.minRequests {
		d.state = sloStateLowTraffic
		d.msg = fmt.Sprintf("%.0f requests in the window, %.0f needed; floor held at %d", m.requests, cfg.minRequests, cur)
		return d
	}
	var breaches, measured []string
	if cfg.maxErr != nil {
		measured = append(measured, fmt.Sprintf("error rate %.4f (max %.4f)", m.errorRate, *cfg.maxErr))
		if m.errorRate > *cfg.maxErr {
			breaches = append(breaches, "error rate")
		}
	}
	if cfg.ttft > 0 {
		measured = append(measured, fmt.Sprintf("TTFT p99 %.0fms (max %.0fms)", m.ttft*1000, cfg.ttft*1000))
		if m.ttft > cfg.ttft {
			breaches = append(breaches, "TTFT")
		}
	}
	if cfg.itl > 0 {
		measured = append(measured, fmt.Sprintf("ITL p99 %.0fms (max %.0fms)", m.itl*1000, cfg.itl*1000))
		if m.itl > cfg.itl {
			breaches = append(breaches, "ITL")
		}
	}
	summary := strings.Join(measured, ", ")
	if len(breaches) == 0 {
		d.state, d.healthy = sloStateHealthy, prev.HealthyWindows+1
		d.msg = summary
		if d.healthy >= cfg.downAfter && cur > lo {
			d.floor, d.healthy, d.state = cur-1, 0, sloStateScaledDown
			d.msg = fmt.Sprintf("%s; %d healthy windows, floor lowered to %d", summary, cfg.downAfter, d.floor)
		}
		return d
	}
	d.breached, d.healthy, d.state = true, 0, sloStateBreached
	overloaded, known := m.overload()
	switch {
	case !known:
		d.msg = fmt.Sprintf("%s breached (%s); no queue or KV cache metrics to confirm overload, floor held at %d", strings.Join(breaches, ", "), summary, cur)
	case !overloaded:
		d.msg = fmt.Sprintf("%s breached (%s) but the engine is not overloaded; more replicas would not help, floor held at %d", strings.Join(breaches, ", "), summary, cur)
	default:
		target := min(max(cur, observed)+cfg.step, hi)
		if target <= cur {
			d.msg = fmt.Sprintf("%s breached (%s) and overloaded, already at maxReplicas %d", strings.Join(breaches, ", "), summary, hi)
			return d
		}
		d.floor, d.state = target, sloStateScaledUp
		d.msg = fmt.Sprintf("%s breached (%s) and overloaded; floor raised to %d", strings.Join(breaches, ", "), summary, target)
	}
	return d
}

func sloQueries(svc *gryviav1.GryviaInferenceService, cfg *sloConfig) map[string]string {
	w := strconv.Itoa(int(cfg.window.Seconds())) + "s"
	proxy := fmt.Sprintf("namespace=%s,inference=%s,track=%q", strconv.Quote(svc.Namespace), strconv.Quote(svc.Name), trackStable)
	engine := fmt.Sprintf("namespace=%s,job=%s", strconv.Quote(svc.Namespace), strconv.Quote(cfg.job))
	requests := "gryvia_inference_requests_total{" + proxy + "}"
	count := "sum(increase(" + requests + "[" + w + "]))"
	return map[string]string{
		"requests":       count,
		"requestsFresh":  "min(timestamp(" + requests + "))",
		"errorRate":      "sum(increase(gryvia_inference_errors_total{" + proxy + "}[" + w + "])) / (" + count + ")",
		"ttft":           "max(gryvia_inference_latency_seconds{" + engine + `,metric="ttft_p99"})`,
		"ttftFresh":      "min(timestamp(gryvia_inference_latency_seconds{" + engine + `,metric="ttft_p99"}))`,
		"itl":            "max(gryvia_inference_latency_seconds{" + engine + `,metric="itl_p99"})`,
		"itlFresh":       "min(timestamp(gryvia_inference_latency_seconds{" + engine + `,metric="itl_p99"}))`,
		"waiting":        "sum(gryvia_inference_requests{" + engine + `,state="waiting"})`,
		"waitingFresh":   "min(timestamp(gryvia_inference_requests{" + engine + `,state="waiting"}))`,
		"kvCache":        "max(gryvia_inference_kv_cache_usage_ratio{" + engine + "})",
		"kvCacheFresh":   "min(timestamp(gryvia_inference_kv_cache_usage_ratio{" + engine + "}))",
		"errorRateFresh": "min(timestamp(gryvia_inference_errors_total{" + proxy + "}))",
	}
}

// measureSLO reads one window. Every configured objective needs a fresh sample; overload signals are optional.
func measureSLO(ctx context.Context, endpoint string, svc *gryviav1.GryviaInferenceService, cfg *sloConfig, now time.Time) (sloMeasurement, error) {
	q := sloQueries(svc, cfg)
	nowUnix := float64(now.UnixNano()) / 1e9
	fresh := func(name string) (float64, error) {
		v, err := prometheusSample(ctx, endpoint, q[name], now)
		if err != nil {
			return 0, fmt.Errorf("%s: %v", name, err)
		}
		stamp, err := prometheusSample(ctx, endpoint, q[name+"Fresh"], now)
		if err != nil {
			return 0, fmt.Errorf("%s: %v", name, err)
		}
		if age := nowUnix - stamp; age < -1 || age > sloMaxSampleAge {
			return 0, fmt.Errorf("%s: newest sample is %.0fs old", name, age)
		}
		return v, nil
	}
	var m sloMeasurement
	var err error
	if m.requests, err = fresh("requests"); err != nil {
		return m, err
	}
	if m.requests < cfg.minRequests {
		return m, nil
	}
	if cfg.maxErr != nil {
		if m.errorRate, err = fresh("errorRate"); err != nil {
			return m, err
		}
	}
	if cfg.ttft > 0 {
		if m.ttft, err = fresh("ttft"); err != nil {
			return m, err
		}
	}
	if cfg.itl > 0 {
		if m.itl, err = fresh("itl"); err != nil {
			return m, err
		}
	}
	if v, e := fresh("waiting"); e == nil {
		m.waiting, m.hasWaiting = v, true
	}
	if v, e := fresh("kvCache"); e == nil {
		m.kvCache, m.hasKVCache = v, true
	}
	return m, nil
}

// reconcileSLO evaluates spec.slo once per window and records the floor that reconcileHPA applies. It returns how
// long until the next window is due (0 when SLO control is off).
func (r *GryviaInferenceServiceReconciler) reconcileSLO(ctx context.Context, svc *gryviav1.GryviaInferenceService, primary *appsv1.Deployment, now time.Time) time.Duration {
	if !sloActive(svc) {
		svc.Status.SLO = nil
		removeCondition(&svc.Status.Conditions, ConditionSLO)
		return 0
	}
	off := func(reason, msg string) time.Duration {
		svc.Status.SLO = &gryviav1.InferenceSLOStatus{State: sloStateUnknown, Message: msg}
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionSLO, metav1.ConditionFalse, reason, msg)
		return 0
	}
	cfg, err := sloSettings(svc)
	if err != nil {
		return off("InvalidConfig", err.Error())
	}
	if scaleToZeroEnabled(svc) {
		return off("ScaleToZero", "SLO control is not applied together with scaleToZero")
	}
	if !autoscalingEnabled(svc) {
		return off("RequiresAutoscaling", "SLO control raises the HPA minimum; enable spec.autoscaling")
	}
	lo, hi, ok := autoscalingRange(svc)
	if !ok {
		return off("RequiresAutoscaling", "SLO control needs a valid autoscaling range")
	}
	if svc.Status.SLO == nil {
		svc.Status.SLO = &gryviav1.InferenceSLOStatus{}
	}
	st := svc.Status.SLO
	if r.PrometheusURL == "" {
		st.State, st.Message = sloStateUnknown, "Set the operator inference-prometheus-url to enable SLO control"
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionSLO, metav1.ConditionUnknown, "NotConfigured", st.Message)
		return 0
	}
	if st.LastEvaluated != nil {
		if wait := st.LastEvaluated.Add(cfg.window).Sub(now); wait > 0 {
			return wait
		}
	}
	st.LastEvaluated = &metav1.Time{Time: now}
	m, err := measureSLO(ctx, r.PrometheusURL, svc, cfg, now)
	if err != nil {
		st.State, st.HealthyWindows = sloStateUnknown, 0
		st.Message = fmt.Sprintf("metrics unavailable, floor held: %v", err)
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionSLO, metav1.ConditionUnknown, "MetricsUnavailable", st.Message)
		return cfg.window
	}
	observed := int32(0)
	if primary != nil {
		observed = primary.Status.ReadyReplicas
	}
	d := decideSLO(cfg, m, *st, observed, lo, hi)
	st.State, st.HealthyWindows, st.Message = d.state, d.healthy, d.msg
	st.FloorReplicas = d.floor
	if d.floor <= lo {
		st.FloorReplicas = 0
	}
	if d.breached {
		st.Breaches++
	}
	status := metav1.ConditionTrue
	if d.breached {
		status = metav1.ConditionFalse
	}
	setCondition(&svc.Status.Conditions, svc.Generation, ConditionSLO, status, d.state, d.msg)
	return cfg.window
}
