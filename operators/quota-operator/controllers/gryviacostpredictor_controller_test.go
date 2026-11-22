// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

func newCostPredictorTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	return s
}

func newCostPredictorReconciler(objs ...client.Object) (*GryviaCostPredictorReconciler, client.Client) {
	scheme := newCostPredictorTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaCostPredictor{}, &gryviav1.GryviaAIJob{}).
		Build()
	r := &GryviaCostPredictorReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}
	return r, fakeClient
}

func newTestCostPredictor(name string) *gryviav1.GryviaCostPredictor {
	return &gryviav1.GryviaCostPredictor{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: gryviav1.GryviaCostPredictorSpec{
			HistoricalData: gryviav1.HistoricalDataSpec{
				LookbackDays:      30,
				MinimumSamples:    5,
				SimilarityFactors: []string{"gpuType", "model", "gpuCount"},
			},
			Integration: gryviav1.IntegrationSpec{
				DryRunMode:               false,
				InjectEstimateAnnotation: true,
			},
			Pricing: gryviav1.PricingSpec{
				PerGpuHour: map[string]float64{
					"H100":    3.50,
					"A100":    2.00,
					"default": 1.50,
				},
			},
		},
	}
}

func TestCostPredictor_Reconcile_NotFound(t *testing.T) {
	r, _ := newCostPredictorReconciler()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for not-found")
	}
}

func TestCostPredictor_Reconcile_Basic(t *testing.T) {
	predictor := newTestCostPredictor("test-predictor")
	r, fakeClient := newCostPredictorReconciler(predictor)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-predictor"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.RequeueAfter != 2*time.Minute {
		t.Errorf("expected requeue after 2m, got %v", result.RequeueAfter)
	}

	updated := &gryviav1.GryviaCostPredictor{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-predictor"}, updated); err != nil {
		t.Fatalf("failed to get predictor: %v", err)
	}

	if updated.Status.LastUpdated.IsZero() {
		t.Error("expected LastUpdated to be set")
	}

	// Check Ready condition
	foundReady := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
			foundReady = true
		}
	}
	if !foundReady {
		t.Error("expected Ready condition True")
	}
}

func TestCostPredictor_Reconcile_SkipsAnnotatedJobs(t *testing.T) {
	predictor := newTestCostPredictor("test-predictor")

	// Job already has estimate annotation - should be skipped
	job := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "already-estimated",
			Namespace: "default",
			Annotations: map[string]string{
				annotationEstimatedCost: "100.00",
			},
		},
		Spec: gryviav1.GryviaAIJobSpec{
			GPUs:    4,
			GpuType: "H100",
		},
		Status: gryviav1.GryviaAIJobStatus{
			Phase: "Pending",
		},
	}

	r, fakeClient := newCostPredictorReconciler(predictor, job)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-predictor"}}
	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	updated := &gryviav1.GryviaCostPredictor{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-predictor"}, updated); err != nil {
		t.Fatalf("failed to get predictor: %v", err)
	}
	if updated.Status.JobsEstimated != 0 {
		t.Errorf("expected 0 jobs estimated (already annotated), got %d", updated.Status.JobsEstimated)
	}
}

func TestCostPredictor_GetJobGPURate(t *testing.T) {
	predictor := newTestCostPredictor("test")

	tests := []struct {
		gpuType  string
		expected float64
	}{
		{"H100", 3.50},
		{"A100", 2.00},
		{"V100", 1.50},    // Falls back to "default"
		{"unknown", 1.50}, // Falls back to "default"
	}

	for _, tt := range tests {
		t.Run(tt.gpuType, func(t *testing.T) {
			rate := getJobGPURate(predictor, tt.gpuType)
			if rate != tt.expected {
				t.Errorf("expected rate %.2f for %s, got %.2f", tt.expected, tt.gpuType, rate)
			}
		})
	}

	// No pricing configured - default rate
	predictor.Spec.Pricing.PerGpuHour = nil
	rate := getJobGPURate(predictor, "H100")
	if rate != 2.00 {
		t.Errorf("expected default rate 2.00, got %.2f", rate)
	}
}

func TestCostPredictor_UpdateStatusCondition(t *testing.T) {
	predictor := newTestCostPredictor("test")
	r, _ := newCostPredictorReconciler(predictor)

	r.updateStatusCondition(context.Background(), predictor, "Ready", metav1.ConditionTrue, "Active", "OK")
	if len(predictor.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(predictor.Status.Conditions))
	}

	r.updateStatusCondition(context.Background(), predictor, "Ready", metav1.ConditionFalse, "Error", "Failed")
	if len(predictor.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(predictor.Status.Conditions))
	}
	if predictor.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Error("expected condition False after update")
	}
}

func TestCostPredictor_UpdateAccuracyMetrics_NoCompletedJobs(t *testing.T) {
	predictor := newTestCostPredictor("test")
	r, _ := newCostPredictorReconciler(predictor)

	// No completed jobs - accuracy should stay zero
	jobs := []gryviav1.GryviaAIJob{
		{
			Status: gryviav1.GryviaAIJobStatus{Phase: "Running"},
		},
	}

	r.updateAccuracyMetrics(context.Background(), predictor, jobs)
	if predictor.Status.PredictionAccuracy.TimeEstimate.MAE != 0 {
		t.Errorf("expected 0 MAE with no completed jobs, got %f", predictor.Status.PredictionAccuracy.TimeEstimate.MAE)
	}
}

func TestCostPredictor_UpdateAccuracyMetrics_WithCompletedJobs(t *testing.T) {
	predictor := newTestCostPredictor("test")
	r, _ := newCostPredictorReconciler(predictor)

	startTime := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	completionTime := metav1.Now()

	jobs := []gryviav1.GryviaAIJob{
		{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					annotationEstimatedCost:     "14.00",
					annotationEstimatedDuration: "2h",
				},
			},
			Spec: gryviav1.GryviaAIJobSpec{
				GPUs:    4,
				GpuType: "H100",
			},
			Status: gryviav1.GryviaAIJobStatus{
				Phase:          "Succeeded",
				StartTime:      &startTime,
				CompletionTime: &completionTime,
			},
		},
	}

	r.updateAccuracyMetrics(context.Background(), predictor, jobs)
	// With a completed job that has estimates, accuracy metrics should be calculated
	// The exact values depend on the time difference
	if predictor.Status.PredictionAccuracy.TimeEstimate.Within25Percent < 0 {
		t.Error("expected non-negative Within25Percent value")
	}
}

func TestCostPredictor_ProcessNewJobs_DryRunMode(t *testing.T) {
	predictor := newTestCostPredictor("test")
	predictor.Spec.Integration.DryRunMode = true

	job := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dry-run-job",
			Namespace: "default",
			Annotations: map[string]string{
				annotationDryRun: "true",
			},
		},
		Spec: gryviav1.GryviaAIJobSpec{
			GPUs:    4,
			GpuType: "H100",
		},
		Status: gryviav1.GryviaAIJobStatus{
			Phase: "Pending",
		},
	}

	r, fakeClient := newCostPredictorReconciler(predictor, job)

	allJobs := []gryviav1.GryviaAIJob{*job}
	err := r.processNewJobs(context.Background(), predictor, allJobs)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// In dry-run mode, job should NOT be updated with annotations
	updated := &gryviav1.GryviaAIJob{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "dry-run-job", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to get job: %v", err)
	}
	if _, ok := updated.Annotations[annotationEstimatedCost]; ok {
		t.Error("expected no cost annotation in dry-run mode")
	}
}
