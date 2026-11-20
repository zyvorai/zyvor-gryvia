// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"reflect"
	"testing"
	"time"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestInferenceHPAMetricTargets(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gpu, rps int32
		names    []string
		bad      bool
	}{
		{"cpu default", 0, 0, nil, false}, {"GPU", 70, 0, []string{InferenceGPUMetric}, false},
		{"RPS", 0, 20, []string{InferenceRPSMetric}, false}, {"both", 70, 20, []string{InferenceGPUMetric, InferenceRPSMetric}, false},
		{"negative GPU", -1, 0, nil, true}, {"GPU over 100", 101, 0, nil, true}, {"negative RPS", 0, -1, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := inferenceHPAMetrics(&gryviav1.AutoscalingConfig{TargetGPUUtilization: tc.gpu, TargetRequestsPerSecond: tc.rps})
			if (err != nil) != tc.bad {
				t.Fatalf("error %v", err)
			}
			if tc.bad {
				return
			}
			if tc.names == nil {
				if len(m) != 1 || m[0].Resource == nil || *m[0].Resource.Target.AverageUtilization != 80 {
					t.Fatal("CPU fallback changed")
				}
				return
			}
			if len(m) != len(tc.names) {
				t.Fatal("wrong metric count")
			}
			for i, name := range tc.names {
				if m[i].Pods == nil || m[i].Pods.Metric.Name != name || m[i].Pods.Target.Type != autoscalingv2.AverageValueMetricType {
					t.Fatalf("metric %+v", m[i])
				}
			}
		})
	}
}

func TestInferenceHPAMissingMetricsVisible(t *testing.T) {
	svc := newInfer("chat", func(s *gryviav1.GryviaInferenceService) {
		s.Spec.Autoscaling = &gryviav1.AutoscalingConfig{Enabled: true, MaxReplicas: 5, TargetRequestsPerSecond: 20}
	})
	c := mlClient(svc)
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")
	h := &autoscalingv2.HorizontalPodAutoscaler{}
	mustGet(t, c, "ns", "chat-inference-hpa", h)
	h.Generation = 2
	if err := c.Update(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	g := int64(2)
	h.Status.ObservedGeneration = &g
	h.Status.Conditions = []autoscalingv2.HorizontalPodAutoscalerCondition{{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse, Reason: "FailedGetPodsMetric", Message: "custom metrics adapter unavailable"}}
	if err := c.Status().Update(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	cond := findCond(getInfer(t, c, "chat").Status.Conditions, ConditionAutoscalingReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "FailedGetPodsMetric" {
		t.Fatalf("condition %+v", cond)
	}
	old := int64(1)
	h.Status.ObservedGeneration = &old
	if err := c.Status().Update(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	if findCond(getInfer(t, c, "chat").Status.Conditions, ConditionAutoscalingReady).Status != metav1.ConditionUnknown {
		t.Fatal("stale HPA status accepted")
	}
	// kube-controller-manager never sets observedGeneration: the conditions are read as they are.
	h.Status.ObservedGeneration = nil
	h.Status.Conditions[0] = autoscalingv2.HorizontalPodAutoscalerCondition{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionTrue, Reason: "ValidMetricFound"}
	if err := c.Status().Update(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	if cond := findCond(getInfer(t, c, "chat").Status.Conditions, ConditionAutoscalingReady); cond.Status != metav1.ConditionTrue || cond.Reason != "ValidMetricFound" {
		t.Fatalf("HPA without observedGeneration: %+v", cond)
	}
}

func routingTestClient(objs ...client.Object) client.Client {
	s := mlScheme()
	s.AddKnownTypeWithName(inferenceRouteGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: inferenceRouteGVK.Group, Version: inferenceRouteGVK.Version, Kind: "HTTPRouteList"}, &unstructured.UnstructuredList{})
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&gryviav1.GryviaInferenceService{}, &appsv1.Deployment{}, newInferenceRoute()).Build()
}
func routedInfer() *gryviav1.GryviaInferenceService {
	s := canarySvc()
	s.Annotations = map[string]string{AnnotationGatewayParent: "public", AnnotationGatewayHostname: "chat.example.com"}
	return s
}
func getRoute(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	u := newInferenceRoute()
	mustGet(t, c, "ns", "chat-route", u)
	return u
}
func backendWeights(t *testing.T, u *unstructured.Unstructured) map[string]int64 {
	t.Helper()
	rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "rules")
	refs := rules[0].(map[string]interface{})["backendRefs"].([]interface{})
	out := map[string]int64{}
	for _, v := range refs {
		m := v.(map[string]interface{})
		out[m["name"].(string)] = m["weight"].(int64)
	}
	return out
}
func acceptRoute(t *testing.T, c client.Client, generation int64) {
	t.Helper()
	u := getRoute(t, c)
	u.SetGeneration(generation)
	if err := c.Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	u.Object["status"] = map[string]interface{}{"parents": []interface{}{map[string]interface{}{"parentRef": map[string]interface{}{"name": "public", "namespace": "ns"}, "controllerName": "example.com/gateway", "conditions": []interface{}{
		map[string]interface{}{"type": "Accepted", "status": "True", "observedGeneration": generation}, map[string]interface{}{"type": "ResolvedRefs", "status": "True", "observedGeneration": generation},
	}}}}
	if err := c.Status().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func TestWeightedRouteLifecycle(t *testing.T) {
	c := routingTestClient(routedInfer())
	r := newInferReconciler(c, newClock())
	r.GatewayRouting = true
	reconcileOnce(t, r, "ns", "chat")
	if got := backendWeights(t, getRoute(t, c)); !reflect.DeepEqual(got, map[string]int64{"chat-route-stable": 100}) {
		t.Fatalf("cold route %+v", got)
	}
	for _, track := range []string{trackStable, trackCanary} {
		s := &corev1.Service{}
		mustGet(t, c, "ns", "chat-route-"+track, s)
		if s.Spec.Selector[labelTrack] != track {
			t.Fatalf("selector %+v", s.Spec.Selector)
		}
	}
	setReady(t, c, "chat-canary", 1)
	reconcileOnce(t, r, "ns", "chat")
	if got := backendWeights(t, getRoute(t, c)); !reflect.DeepEqual(got, map[string]int64{"chat-route-stable": 50, "chat-route-canary": 50}) {
		t.Fatalf("weighted route %+v", got)
	}
	acceptRoute(t, c, 1)
	reconcileOnce(t, r, "ns", "chat")
	if findCond(getInfer(t, c, "chat").Status.Conditions, ConditionGatewayRouting).Status != metav1.ConditionTrue {
		t.Fatal("accepted route not reflected")
	}
	setReady(t, c, "chat-canary", 0)
	reconcileOnce(t, r, "ns", "chat")
	if got := backendWeights(t, getRoute(t, c)); !reflect.DeepEqual(got, map[string]int64{"chat-route-stable": 100}) {
		t.Fatalf("unready canary receives traffic %+v", got)
	}
	s := getInfer(t, c, "chat")
	s.Annotations = nil
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	reconcileOnce(t, r, "ns", "chat")
	if mlObjectExists(c, "ns", "chat-route", newInferenceRoute()) || mlObjectExists(c, "ns", "chat-route-stable", &corev1.Service{}) {
		t.Fatal("owned route resources not cleaned up")
	}
	if !mlObjectExists(c, "ns", "chat-inference", &corev1.Service{}) {
		t.Fatal("legacy endpoint removed")
	}
}

func TestGatewayPromotionHoldWindow(t *testing.T) {
	s := routedInfer()
	s.Spec.Canary.AutoPromote = true
	s.Spec.Canary.PromoteAfterSeconds = 60
	c := routingTestClient(s)
	clk := newClock()
	r := newInferReconciler(c, clk)
	r.GatewayRouting = true
	reconcileOnce(t, r, "ns", "chat")
	setReady(t, c, "chat-canary", 1)
	clk.Add(10 * time.Minute)
	reconcileOnce(t, r, "ns", "chat")
	if !mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Fatal("promoted with no accepted route")
	}
	acceptRoute(t, c, 1)
	reconcileOnce(t, r, "ns", "chat")
	clk.Add(30 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	if !mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Fatal("route hold window ignored")
	}
	clk.Add(31 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	if mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Fatal("healthy accepted canary did not promote")
	}
	if got := backendWeights(t, getRoute(t, c)); !reflect.DeepEqual(got, map[string]int64{"chat-route-stable": 100}) {
		t.Fatalf("promotion left canary backend %+v", got)
	}
}

func TestRoutingValidationAndStaleConditions(t *testing.T) {
	for _, a := range []map[string]string{{AnnotationGatewayParent: "../other"}, {AnnotationGatewayHostname: "chat.example.com"}, {AnnotationGatewayParent: "public", AnnotationGatewaySection: "BAD"}} {
		s := newInfer("chat")
		s.Annotations = a
		if validateRoutingOptions(s) == nil {
			t.Fatal("accepted malformed routing annotations")
		}
	}
	s := routedInfer()
	c := routingTestClient(s)
	r := newInferReconciler(c, newClock())
	r.GatewayRouting = true
	reconcileOnce(t, r, "ns", "chat")
	acceptRoute(t, c, 2)
	route := getRoute(t, c)
	route.SetGeneration(3)
	if ready, _ := routeAccepted(route, s); ready {
		t.Fatal("stale observed generation accepted")
	}
}

func TestGatewayResourcesRemainOffByDefault(t *testing.T) {
	s := routedInfer()
	c := routingTestClient(s)
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")
	if mlObjectExists(c, "ns", "chat-route", newInferenceRoute()) {
		t.Fatal("routing enabled without operator flag")
	}
	if findCond(getInfer(t, c, "chat").Status.Conditions, ConditionGatewayRouting).Reason != "Disabled" {
		t.Fatal("disabled state hidden")
	}
}

func TestRouteExtremeWeights(t *testing.T) {
	for _, weight := range []int32{0, 10, 100} {
		s := routedInfer()
		s.Spec.Canary.Weight = weight
		s.Status.CanaryStatus = &gryviav1.CanaryStatus{Active: true, ReadyReplicas: 1, Health: canaryHealthHealthy}
		r := newInferReconciler(nil, newClock())
		u := newInferenceRoute()
		u.Object["spec"] = r.routeSpec(s)
		weights := backendWeights(t, u)
		if weights["chat-route-stable"] != int64(100-weight) || weights["chat-route-canary"] != int64(weight) {
			t.Fatalf("weight %d: %+v", weight, weights)
		}
	}
}

func TestRouteNameCollisionNeverAuthorizesPromotion(t *testing.T) {
	s := routedInfer()
	s.Spec.Canary.AutoPromote = true
	c := routingTestClient(s)
	r := newInferReconciler(c, newClock())
	r.GatewayRouting = true
	reconcileOnce(t, r, "ns", "chat")
	setReady(t, c, "chat-canary", 1)
	reconcileOnce(t, r, "ns", "chat")
	acceptRoute(t, c, 1)
	u := getRoute(t, c)
	u.SetOwnerReferences(nil)
	if err := c.Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	if !mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Fatal("foreign route authorized promotion")
	}
	cond := findCond(getInfer(t, c, "chat").Status.Conditions, ConditionGatewayRouting)
	if cond == nil || cond.Reason != "NameCollision" {
		t.Fatalf("condition %+v", cond)
	}
	if len(getRoute(t, c).GetOwnerReferences()) != 0 {
		t.Fatal("adopted foreign route")
	}
}

func TestGatewayReadinessLossResetsPromotionWindow(t *testing.T) {
	s := routedInfer()
	s.Spec.Canary.AutoPromote = true
	s.Spec.Canary.PromoteAfterSeconds = 60
	c := routingTestClient(s)
	clk := newClock()
	r := newInferReconciler(c, clk)
	r.GatewayRouting = true
	reconcileOnce(t, r, "ns", "chat")
	setReady(t, c, "chat-canary", 1)
	reconcileOnce(t, r, "ns", "chat")
	acceptRoute(t, c, 1)
	reconcileOnce(t, r, "ns", "chat")
	clk.Add(40 * time.Second)
	setReady(t, c, "chat-canary", 0)
	reconcileOnce(t, r, "ns", "chat")
	clk.Add(30 * time.Second)
	setReady(t, c, "chat-canary", 1)
	reconcileOnce(t, r, "ns", "chat")
	acceptRoute(t, c, 2)
	reconcileOnce(t, r, "ns", "chat")
	if !mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Fatal("promotion ignored readiness loss")
	}
	clk.Add(61 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	if mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Fatal("promotion did not resume after a new hold window")
	}
}
