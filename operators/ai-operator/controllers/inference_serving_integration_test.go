// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//go:build integration

package controllers

import (
	"context"
	"os"
	"testing"
	"time"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// API-only integration: no kubelet, Gateway data plane or HPA controller exists.
// Assertions verify CRD validation, API defaults/generation, and status-driven reconciliation.
func TestInferenceServingAPIServer(t *testing.T) {
	gatewayCRD := os.Getenv("GRYVIA_GATEWAY_CRD_DIR")
	if gatewayCRD == "" {
		t.Fatal("GRYVIA_GATEWAY_CRD_DIR must contain Gateway API v1 HTTPRoute CRD YAML")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../../crds", gatewayCRD}, ErrorIfCRDPathMissing: true}
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: mlScheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}); err != nil {
		t.Fatal(err)
	}
	svc := routedInfer()
	svc.Annotations[AnnotationInferenceTelemetry] = "true"
	svc.UID = ""
	svc.ResourceVersion = ""
	svc.Spec.Autoscaling = &gryviav1.AutoscalingConfig{Enabled: true, MaxReplicas: 5, TargetGPUUtilization: 70, TargetRequestsPerSecond: 20}
	if err := c.Create(ctx, svc); err != nil {
		t.Fatal(err)
	}
	r := newInferReconciler(c, newClock())
	r.GatewayRouting = true
	r.TelemetryImage = "example/operator:test"
	reconcileOnce(t, r, "ns", "chat")
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	mustGet(t, c, "ns", "chat-inference-hpa", hpa)
	if len(hpa.Spec.Metrics) != 2 || hpa.Spec.Metrics[0].Pods.Metric.Name != InferenceGPUMetric {
		t.Fatal("API did not preserve custom metric targets")
	}
	cold := getRoute(t, c)
	if backendWeights(t, cold)["chat-route-stable"] != 100 {
		t.Fatal("cold canary receives traffic")
	}
	canary := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-canary", canary)
	if len(canary.Spec.Template.Spec.Containers) != 2 {
		t.Fatal("API did not preserve telemetry sidecar")
	}
	identity := map[string]string{}
	for _, e := range canary.Spec.Template.Spec.Containers[1].Env {
		identity[e.Name] = e.Value
	}
	if identity["GRYVIA_DEPLOYMENT_UID"] != string(canary.UID) || identity["GRYVIA_REVISION"] != canary.Annotations[annotationSpecHash] {
		t.Fatal("sidecar identity differs from analysis selector")
	}
	canary.Status.ObservedGeneration = canary.Generation
	canary.Status.Replicas = *canary.Spec.Replicas
	canary.Status.ReadyReplicas = *canary.Spec.Replicas
	canary.Status.UpdatedReplicas = *canary.Spec.Replicas
	if err := c.Status().Update(ctx, canary); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	live := getRoute(t, c)
	if live.GetGeneration() <= cold.GetGeneration() {
		t.Fatal("route update did not advance generation")
	}
	if backendWeights(t, live)["chat-route-canary"] != 50 {
		t.Fatal("API did not accept weighted backends")
	}
	// Gateway status is supplied by this test; it does not assert a working data plane.
	gen := live.GetGeneration()
	live.Object["status"] = map[string]interface{}{"parents": []interface{}{map[string]interface{}{
		"parentRef":      map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": "public", "namespace": "ns"},
		"controllerName": "example.com/test-gateway", "conditions": []interface{}{
			map[string]interface{}{"type": "Accepted", "status": "True", "reason": "Accepted", "message": "API test fixture", "lastTransitionTime": "2026-09-30T00:00:00Z", "observedGeneration": gen},
			map[string]interface{}{"type": "ResolvedRefs", "status": "True", "reason": "ResolvedRefs", "message": "API test fixture", "lastTransitionTime": "2026-09-30T00:00:00Z", "observedGeneration": gen},
		},
	}}}
	if err := c.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	if got := getRoute(t, c); got.GetGeneration() != gen {
		t.Fatal("defaulted API fields cause continuous route rewrites")
	}
	cond := findCond(getInfer(t, c, "chat").Status.Conditions, ConditionGatewayRouting)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("routing condition %+v", cond)
	}
	// Missing custom metrics propagate from a real HPA status subresource.
	mustGet(t, c, "ns", "chat-inference-hpa", hpa)
	hpa.Status.ObservedGeneration = &hpa.Generation
	hpa.Status.Conditions = []autoscalingv2.HorizontalPodAutoscalerCondition{{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse, Reason: "FailedGetPodsMetric", Message: "fixture: adapter absent", LastTransitionTime: metav1.Now()}}
	if err := c.Status().Update(ctx, hpa); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	if cond := findCond(getInfer(t, c, "chat").Status.Conditions, ConditionAutoscalingReady); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("HPA condition %+v", cond)
	}
	// SLO decisions persist through the real status subresource and Deployment patches.
	// Telemetry is an HTTP fixture, not a running Prometheus server or model workload.
	clk := newClock()
	r.Clock = clk.Now
	f := newAnalysisFixture(t)
	r.PrometheusURL = f.server.URL
	svc = getInfer(t, c, "chat")
	svc.Annotations[analysisErrorKey] = "0.01"
	svc.Annotations[analysisLatencyKey] = "0.5"
	svc.Spec.Canary.AutoPromote = false
	svc.Spec.HealthCheck = &gryviav1.HealthCheckConfig{AutoRollback: true, IntervalSeconds: 10, FailureThreshold: 2}
	if err := c.Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	step := func() { f.unix.Store(clk.Now().Unix()); reconcileOnce(t, r, "ns", "chat") }
	step()
	mustGet(t, c, "ns", "chat-canary", canary)
	canary.Status.ObservedGeneration = canary.Generation
	canary.Status.Replicas = *canary.Spec.Replicas
	canary.Status.UpdatedReplicas = *canary.Spec.Replicas
	canary.Status.ReadyReplicas = *canary.Spec.Replicas
	if err := c.Status().Update(ctx, canary); err != nil {
		t.Fatal(err)
	}
	step()
	clk.Add(60 * time.Second)
	f.mode.Store(1)
	step()
	if cond := findCond(getInfer(t, c, "chat").Status.Conditions, ConditionCanaryAnalysis); cond == nil || cond.Reason != "SLOBreached" {
		t.Fatalf("SLO condition %+v", cond)
	}
	clk.Add(10 * time.Second)
	step()
	if s := getInfer(t, c, "chat"); s.Status.CanaryStatus.Health != canaryHealthRolledBack {
		t.Fatalf("API rollback failed %+v", s.Status)
	}
	if backendWeights(t, getRoute(t, c))["chat-route-stable"] != 100 {
		t.Fatal("SLO rollback retained canary traffic")
	}

}
