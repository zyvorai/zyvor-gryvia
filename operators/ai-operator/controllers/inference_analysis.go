// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ConditionCanaryAnalysis = "CanaryAnalysisReady"
	analysisErrorKey        = "gryvia.io/canary-max-error-rate"
	analysisLatencyKey      = "gryvia.io/canary-max-p95-seconds"
	analysisSamplesKey      = "gryvia.io/canary-min-requests"
	analysisSinceKey        = "gryvia.io/analysis-healthy-since"
	analysisHashKey         = "gryvia.io/analysis-healthy-hash"
)

type canaryAnalysisConfig struct{ MaxError, MaxLatency, MinRequests float64 }

// Both thresholds enable analysis. No tenant-supplied endpoint or PromQL is accepted.
func analysisConfig(svc *gryviav1.GryviaInferenceService) (*canaryAnalysisConfig, error) {
	a := svc.Annotations
	if a[analysisErrorKey] == "" && a[analysisLatencyKey] == "" && a[analysisSamplesKey] == "" {
		return nil, nil
	}
	cfg := &canaryAnalysisConfig{MinRequests: 100}
	var err error
	cfg.MaxError, err = strconv.ParseFloat(a[analysisErrorKey], 64)
	if err != nil || math.IsNaN(cfg.MaxError) || math.IsInf(cfg.MaxError, 0) || cfg.MaxError < 0 || cfg.MaxError > 1 {
		return nil, fmt.Errorf("%s must be a finite fraction from 0 to 1", analysisErrorKey)
	}
	cfg.MaxLatency, err = strconv.ParseFloat(a[analysisLatencyKey], 64)
	if err != nil || math.IsNaN(cfg.MaxLatency) || math.IsInf(cfg.MaxLatency, 0) || cfg.MaxLatency <= 0 {
		return nil, fmt.Errorf("%s must be finite and positive", analysisLatencyKey)
	}
	if a[analysisSamplesKey] != "" {
		n, e := strconv.ParseUint(a[analysisSamplesKey], 10, 32)
		if e != nil || n == 0 {
			return nil, fmt.Errorf("%s must be a positive integer", analysisSamplesKey)
		}
		cfg.MinRequests = float64(n)
	}
	return cfg, nil
}

// The six fixed queries use a 60s window and isolate a Deployment incarnation,
// model version and pod template revision. Exporters must carry these labels.
func analysisQueries(svc *gryviav1.GryviaInferenceService, d *appsv1.Deployment) []string {
	labels := fmt.Sprintf("namespace=%s,inference=%s,track=\"canary\",model_version=%s,deployment_uid=%s,revision=%s", strconv.Quote(svc.Namespace), strconv.Quote(svc.Name), strconv.Quote(svc.Spec.Canary.ModelVersion), strconv.Quote(string(d.UID)), strconv.Quote(d.Annotations[annotationSpecHash]))
	requests := "gryvia_inference_requests_total{" + labels + "}"
	failures := "gryvia_inference_errors_total{" + labels + "}"
	buckets := "gryvia_inference_request_duration_seconds_bucket{" + labels + "}"
	count := "sum(increase(" + requests + "[60s]))"
	return []string{
		"sum(increase(" + failures + "[60s])) / (" + count + ")",
		"histogram_quantile(0.95, sum by (le) (rate(" + buckets + "[60s])))",
		count,
		"min(timestamp(" + requests + "))",
		"min(timestamp(" + failures + "))",
		"min(timestamp(" + buckets + "))",
	}
}

func prometheusSample(ctx context.Context, endpoint, query string, now time.Time) (float64, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return 0, fmt.Errorf("operator Prometheus URL must be an http(s) base URL without credentials, query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/query"
	u.RawQuery = url.Values{"query": {query}, "time": {strconv.FormatFloat(float64(now.UnixNano())/1e9, 'f', 9, 64)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	hc := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("Prometheus request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Prometheus returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(body) > 65536 {
		return 0, fmt.Errorf("invalid or oversized Prometheus response")
	}
	var result struct {
		Status   string   `json:"status"`
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
		Data     struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &result) != nil || result.Status != "success" || len(result.Warnings) > 0 || len(result.Infos) > 0 || result.Data.ResultType != "vector" || len(result.Data.Result) != 1 || len(result.Data.Result[0].Value) != 2 {
		return 0, fmt.Errorf("Prometheus must return exactly one complete vector sample")
	}
	var stamp float64
	var raw string
	if json.Unmarshal(result.Data.Result[0].Value[0], &stamp) != nil || json.Unmarshal(result.Data.Result[0].Value[1], &raw) != nil {
		return 0, fmt.Errorf("malformed Prometheus sample")
	}
	value, err := strconv.ParseFloat(raw, 64)
	age := float64(now.UnixNano())/1e9 - stamp
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || age < -1 || age > 30 {
		return 0, fmt.Errorf("non-finite, negative or stale Prometheus sample")
	}
	return value, nil
}

// Missing telemetry blocks promotion but is not an SLO violation. A measured
// breach counts through the existing health interval / failure-threshold policy.
func (r *GryviaInferenceServiceReconciler) evaluateCanary(ctx context.Context, svc *gryviav1.GryviaInferenceService, d *appsv1.Deployment, cfg *canaryAnalysisConfig, now time.Time) (healthy, failed bool) {
	report := func(status metav1.ConditionStatus, reason, msg string) {
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionCanaryAnalysis, status, reason, msg)
	}
	if r.PrometheusURL == "" {
		report(metav1.ConditionUnknown, "NotConfigured", "Set the operator inference-prometheus-url to enable evaluation")
		return
	}
	if svc.Status.CanaryStatus.ReadyReplicas == 0 {
		report(metav1.ConditionUnknown, "WarmingUp", "Waiting for a ready canary and a complete 60 second metrics window")
		return
	}
	epochHash := specHash(cfg) + ":" + specHash(svc.Spec.Canary) + ":" + d.Annotations[annotationSpecHash] + ":" + r.PrometheusURL
	epoch, err := time.Parse(time.RFC3339Nano, d.Annotations["gryvia.io/analysis-window-start"])
	if err != nil || epoch.After(now) || d.Annotations["gryvia.io/analysis-window-hash"] != epochHash {
		base := d.DeepCopy()
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations["gryvia.io/analysis-window-start"] = now.Format(time.RFC3339Nano)
		d.Annotations["gryvia.io/analysis-window-hash"] = epochHash
		if r.Patch(ctx, d, client.MergeFrom(base)) != nil {
			report(metav1.ConditionUnknown, "WarmingUp", "Could not persist the analysis window")
			return
		}
		epoch = now
	}
	if now.Sub(epoch) < 60*time.Second {
		report(metav1.ConditionUnknown, "WarmingUp", "Waiting for a complete 60 second window for the current analysis policy and revision")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	vals := make([]float64, 6)
	for i, q := range analysisQueries(svc, d) {
		v, err := prometheusSample(ctx, r.PrometheusURL, q, now)
		if err != nil {
			report(metav1.ConditionUnknown, "TelemetryUnavailable", err.Error())
			return
		}
		vals[i] = v
	}
	for _, stamp := range vals[3:] {
		age := float64(now.UnixNano())/1e9 - stamp
		if age < -1 || age > 30 {
			report(metav1.ConditionUnknown, "TelemetryUnavailable", "Underlying raw metrics are stale")
			return
		}
	}
	if vals[0] > 1 {
		report(metav1.ConditionUnknown, "TelemetryUnavailable", "Error ratio exceeds one")
		return
	}
	if vals[2] < cfg.MinRequests {
		report(metav1.ConditionUnknown, "InsufficientTraffic", fmt.Sprintf("Need at least %.0f requests in the last 60 seconds; observed %.1f", cfg.MinRequests, vals[2]))
		return
	}
	msg := fmt.Sprintf("60s error rate %.4f (limit %.4f), p95 %.4fs (limit %.4fs), %.1f requests", vals[0], cfg.MaxError, vals[1], cfg.MaxLatency, vals[2])
	if vals[0] > cfg.MaxError || vals[1] > cfg.MaxLatency {
		report(metav1.ConditionFalse, "SLOBreached", msg)
		return false, true
	}
	report(metav1.ConditionTrue, "SLOPassed", msg)
	return true, false
}

// A persisted sampled-success hold survives operator restarts and resets after
// any unavailable or failed evaluation, or changes to policy or pod template.
func (r *GryviaInferenceServiceReconciler) analysisPromotionReady(ctx context.Context, svc *gryviav1.GryviaInferenceService, d *appsv1.Deployment, cfg *canaryAnalysisConfig, healthy bool, now time.Time) (bool, error) {
	hash := specHash(cfg) + ":" + specHash(svc.Spec.Canary) + ":" + d.Annotations[annotationSpecHash] + ":" + r.PrometheusURL
	stamp, err := time.Parse(time.RFC3339Nano, d.Annotations[analysisSinceKey])
	last, lastErr := time.Parse(time.RFC3339Nano, d.Annotations["gryvia.io/analysis-last-success"])
	maxGap := 60 * time.Second
	if h := svc.Spec.HealthCheck; h != nil && time.Duration(h.IntervalSeconds)*2*time.Second > maxGap {
		maxGap = time.Duration(h.IntervalSeconds) * 2 * time.Second
	}
	continuing := healthy && err == nil && lastErr == nil && d.Annotations[analysisHashKey] == hash && !stamp.After(now) && !last.After(now) && now.Sub(last) <= maxGap

	if !healthy && d.Annotations[analysisSinceKey] == "" && d.Annotations[analysisHashKey] == "" {
		return false, nil
	}
	base := d.DeepCopy()
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	if healthy {
		if !continuing {
			d.Annotations[analysisSinceKey] = now.Format(time.RFC3339Nano)
			stamp = now
		}
		d.Annotations["gryvia.io/analysis-last-success"] = now.Format(time.RFC3339Nano)
		d.Annotations[analysisHashKey] = hash
	} else {
		delete(d.Annotations, analysisSinceKey)
		delete(d.Annotations, analysisHashKey)
		delete(d.Annotations, "gryvia.io/analysis-last-success")
	}
	if err := r.Patch(ctx, d, client.MergeFrom(base)); err != nil {
		return false, err
	}
	return healthy && now.Sub(stamp) >= time.Duration(svc.Spec.Canary.PromoteAfterSeconds)*time.Second, nil
}
