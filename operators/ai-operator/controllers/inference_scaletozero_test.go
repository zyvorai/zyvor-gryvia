// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func withScaleToZero(idle int32) func(*gryviav1.GryviaInferenceService) {
	return func(s *gryviav1.GryviaInferenceService) {
		s.Spec.ScaleToZero = &gryviav1.ScaleToZeroConfig{Enabled: true, IdleSeconds: idle}
	}
}

func replicasOf(t *testing.T, c client.Client) int32 {
	t.Helper()
	d := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", d)
	return *d.Spec.Replicas
}

func annotate(t *testing.T, c client.Client, key, value string) {
	t.Helper()
	s := getInfer(t, c, "chat")
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations[key] = value
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func TestScaleToZero_IdleScalesDownAndWakeRestores(t *testing.T) {
	c := mlClient(newInfer("chat", withScaleToZero(120)))
	clk := newClock()
	r := newInferReconciler(c, clk)

	res := reconcileOnce(t, r, "ns", "chat")
	setReady(t, c, "chat-inference", 2)
	reconcileOnce(t, r, "ns", "chat")
	s := getInfer(t, c, "chat")
	if s.Status.Phase != PhaseReady || replicasOf(t, c) != 2 {
		t.Fatalf("phase %s replicas %d", s.Status.Phase, replicasOf(t, c))
	}
	if s.Status.LastWakeAt == nil || !s.Status.LastWakeAt.Time.Equal(clk.Now()) {
		t.Fatalf("the idle clock starts when scale-to-zero is on: %v", s.Status.LastWakeAt)
	}
	if res.RequeueAfter == 0 || res.RequeueAfter > 30*time.Second {
		t.Errorf("requeue %v", res.RequeueAfter)
	}

	// A request a minute in keeps it up past the original deadline.
	clk.Add(time.Minute)
	annotate(t, c, gryviav1.AnnotationLastRequest, clk.Now().Format(time.RFC3339))
	clk.Add(90 * time.Second)
	res = reconcileOnce(t, r, "ns", "chat")
	if replicasOf(t, c) != 2 {
		t.Fatal("scaled down although a request came 90s ago (idle 120s)")
	}
	if res.RequeueAfter < 29*time.Second || res.RequeueAfter > 31*time.Second {
		t.Errorf("requeue at the idle deadline: got %v, want about 31s", res.RequeueAfter)
	}

	clk.Add(31 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	s = getInfer(t, c, "chat")
	if replicasOf(t, c) != 0 || s.Status.Phase != PhaseScaledToZero || s.Status.ScaledToZeroAt == nil {
		t.Fatalf("idle: replicas %d phase %s at %v", replicasOf(t, c), s.Status.Phase, s.Status.ScaledToZeroAt)
	}
	if cond := meta.FindStatusCondition(s.Status.Conditions, ConditionScaleToZero); cond == nil || cond.Reason != "Idle" {
		t.Errorf("condition %+v", cond)
	}
	if s.Status.Endpoint == "" {
		t.Error("the endpoint stays so the gateway keeps the route")
	}

	// It stays at zero without a wake request, however long.
	setReady(t, c, "chat-inference", 0)
	clk.Add(time.Hour)
	reconcileOnce(t, r, "ns", "chat")
	if replicasOf(t, c) != 0 || getInfer(t, c, "chat").Status.Phase != PhaseScaledToZero {
		t.Fatal("left zero without a wake request")
	}

	annotate(t, c, gryviav1.AnnotationWakeRequested, clk.Now().Format(time.RFC3339))
	reconcileOnce(t, r, "ns", "chat")
	s = getInfer(t, c, "chat")
	if replicasOf(t, c) != 2 || s.Status.Phase != PhaseDeployingInfer || s.Status.ScaledToZeroAt != nil {
		t.Fatalf("wake: replicas %d phase %s", replicasOf(t, c), s.Status.Phase)
	}
	if _, ok := s.Annotations[gryviav1.AnnotationWakeRequested]; ok {
		t.Error("the wake annotation is consumed")
	}
	if !s.Status.LastWakeAt.Time.Equal(clk.Now()) {
		t.Errorf("lastWakeAt %v", s.Status.LastWakeAt)
	}
	setReady(t, c, "chat-inference", 2)
	reconcileOnce(t, r, "ns", "chat")
	if getInfer(t, c, "chat").Status.Phase != PhaseReady {
		t.Error("not Ready after the pods are")
	}
}

func TestScaleToZero_SlowStartGetsTheColdStartTime(t *testing.T) {
	c := mlClient(newInfer("chat", withScaleToZero(60)))
	clk := newClock()
	r := newInferReconciler(c, clk)
	reconcileOnce(t, r, "ns", "chat")
	clk.Add(2 * time.Minute) // idle passed, but no replica was ever ready
	reconcileOnce(t, r, "ns", "chat")
	if replicasOf(t, c) != 2 {
		t.Fatal("a service still starting was scaled down inside the cold-start time")
	}
	clk.Add(5 * time.Minute) // past idle + the default 300s cold start
	reconcileOnce(t, r, "ns", "chat")
	if replicasOf(t, c) != 0 {
		t.Fatal("a service that never became ready was kept past idle + cold start")
	}
}

func TestScaleToZero_LeavesTheHPAAloneAndWakesIntoItsRange(t *testing.T) {
	c := mlClient(newInfer("chat", withScaleToZero(60), func(s *gryviav1.GryviaInferenceService) {
		s.Spec.Replicas = 1
		s.Spec.Autoscaling = &gryviav1.AutoscalingConfig{Enabled: true, MinReplicas: 2, MaxReplicas: 5}
	}))
	clk := newClock()
	r := newInferReconciler(c, clk)
	reconcileOnce(t, r, "ns", "chat")
	setReady(t, c, "chat-inference", 2)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	mustGet(t, c, "ns", "chat-inference-hpa", hpa)

	// The HPA scaled up meanwhile; the controller does not touch the count while running.
	d := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", d)
	four := int32(4)
	d.Spec.Replicas = &four
	if err := c.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	if replicasOf(t, c) != 4 {
		t.Fatalf("the HPA owns the count while running: %d", replicasOf(t, c))
	}

	clk.Add(2 * time.Minute)
	reconcileOnce(t, r, "ns", "chat")
	if replicasOf(t, c) != 0 {
		t.Fatal("not scaled to zero under an HPA")
	}
	// Changing the autoscaling spec while at zero must not touch the HPA.
	s := getInfer(t, c, "chat")
	s.Spec.Autoscaling.MaxReplicas = 9
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	mustGet(t, c, "ns", "chat-inference-hpa", hpa)
	if hpa.Spec.MaxReplicas != 5 {
		t.Errorf("HPA reconciled at zero: max %d", hpa.Spec.MaxReplicas)
	}

	annotate(t, c, gryviav1.AnnotationWakeRequested, clk.Now().Format(time.RFC3339))
	reconcileOnce(t, r, "ns", "chat")
	if replicasOf(t, c) != 2 {
		t.Fatalf("woken to %d, want max(1, minReplicas) = 2", replicasOf(t, c))
	}
	reconcileOnce(t, r, "ns", "chat")
	mustGet(t, c, "ns", "chat-inference-hpa", hpa)
	if hpa.Spec.MaxReplicas != 9 {
		t.Errorf("HPA not reconciled after the wake: max %d", hpa.Spec.MaxReplicas)
	}
}

func TestScaleToZero_DisablingAtZeroRestoresAndCanaryRulesItOut(t *testing.T) {
	c := mlClient(newInfer("chat", withScaleToZero(60), func(s *gryviav1.GryviaInferenceService) {
		s.Spec.Autoscaling = &gryviav1.AutoscalingConfig{Enabled: true, MinReplicas: 1, MaxReplicas: 3}
	}))
	clk := newClock()
	r := newInferReconciler(c, clk)
	reconcileOnce(t, r, "ns", "chat")
	setReady(t, c, "chat-inference", 2)
	reconcileOnce(t, r, "ns", "chat")
	clk.Add(2 * time.Minute)
	reconcileOnce(t, r, "ns", "chat")
	if replicasOf(t, c) != 0 {
		t.Fatal("not at zero")
	}
	s := getInfer(t, c, "chat")
	s.Spec.ScaleToZero = nil
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	s = getInfer(t, c, "chat")
	if replicasOf(t, c) != 2 || s.Status.ScaledToZeroAt != nil || meta.FindStatusCondition(s.Status.Conditions, ConditionScaleToZero) != nil {
		t.Fatalf("turning it off at zero: replicas %d status %+v", replicasOf(t, c), s.Status.ScaledToZeroAt)
	}

	c2 := mlClient(newInfer("chat", withScaleToZero(60), func(s *gryviav1.GryviaInferenceService) {
		s.Spec.Canary = &gryviav1.CanaryConfig{Enabled: true, Weight: 10, ModelVersion: "llama-v2"}
	}))
	r2 := newInferReconciler(c2, clk)
	reconcileOnce(t, r2, "ns", "chat")
	clk.Add(time.Hour)
	reconcileOnce(t, r2, "ns", "chat")
	s = getInfer(t, c2, "chat")
	if replicasOf(t, c2) != 2 {
		t.Fatal("scaled to zero with a canary")
	}
	if cond := meta.FindStatusCondition(s.Status.Conditions, ConditionScaleToZero); cond == nil || cond.Reason != "Unsupported" {
		t.Errorf("condition %+v", cond)
	}
}

func TestScaleToZero_WakeWhileRunningCountsAsActivity(t *testing.T) {
	c := mlClient(newInfer("chat", withScaleToZero(60)))
	clk := newClock()
	r := newInferReconciler(c, clk)
	reconcileOnce(t, r, "ns", "chat")
	setReady(t, c, "chat-inference", 2)
	clk.Add(50 * time.Second)
	annotate(t, c, gryviav1.AnnotationWakeRequested, "anything")
	reconcileOnce(t, r, "ns", "chat")
	if _, ok := getInfer(t, c, "chat").Annotations[gryviav1.AnnotationWakeRequested]; ok {
		t.Error("a stale wake request is consumed while running")
	}
	clk.Add(50 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	if replicasOf(t, c) != 2 {
		t.Fatal("the wake request did not restart the idle clock")
	}
	annotate(t, c, gryviav1.AnnotationLastRequest, "not-a-time")
	clk.Add(11 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	if replicasOf(t, c) != 0 {
		t.Fatal("a malformed last-request annotation must not keep the service up")
	}
}
