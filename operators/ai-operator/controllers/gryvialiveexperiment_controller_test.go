// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func newLiveExperimentTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

func newLiveExperimentReconciler(objs ...client.Object) (*GryviaLiveExperimentReconciler, client.Client) {
	scheme := newLiveExperimentTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaLiveExperiment{}, &gryviav1.GryviaAIJob{}).
		Build()
	r := &GryviaLiveExperimentReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Log:    ctrl.Log.WithName("test"),
	}
	return r, fakeClient
}

func newTestLiveExperiment(name, namespace string) *gryviav1.GryviaLiveExperiment {
	return &gryviav1.GryviaLiveExperiment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: gryviav1.GryviaLiveExperimentSpec{
			Description: "Test experiment",
			Jobs: []gryviav1.ExperimentJob{
				{Name: "baseline", JobRef: "job-baseline"},
				{Name: "candidate", JobRef: "job-candidate"},
			},
			Comparison: gryviav1.ComparisonConfig{
				PrimaryMetric: "loss",
				Direction:     "minimize",
			},
		},
	}
}

func TestLiveExperiment_Reconcile_NotFound(t *testing.T) {
	r, _ := newLiveExperimentReconciler()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for not-found")
	}
}

func TestLiveExperiment_Reconcile_DeletionTimestamp(t *testing.T) {
	now := metav1.Now()
	exp := newTestLiveExperiment("test-exp", "default")
	exp.DeletionTimestamp = &now
	exp.Finalizers = []string{"keep-for-test"}

	r, _ := newLiveExperimentReconciler(exp)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-exp", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error during deletion, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for deleted resource")
	}
}

func TestLiveExperiment_Reconcile_InitializesPhase(t *testing.T) {
	exp := newTestLiveExperiment("test-exp", "default")
	r, fakeClient := newLiveExperimentReconciler(exp)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-exp", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after phase initialization")
	}

	updated := &gryviav1.GryviaLiveExperiment{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-exp", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to get updated experiment: %v", err)
	}
	if updated.Status.Phase != ExperimentPhasePending {
		t.Errorf("expected phase %s, got %s", ExperimentPhasePending, updated.Status.Phase)
	}
}

func TestLiveExperiment_Reconcile_CompletedNoRequeue(t *testing.T) {
	exp := newTestLiveExperiment("test-exp", "default")
	exp.Status.Phase = ExperimentPhaseCompleted

	r, _ := newLiveExperimentReconciler(exp)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-exp", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for completed experiment")
	}
}

func TestLiveExperiment_Reconcile_FailedNoRequeue(t *testing.T) {
	exp := newTestLiveExperiment("test-exp", "default")
	exp.Status.Phase = ExperimentPhaseFailed

	r, _ := newLiveExperimentReconciler(exp)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-exp", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for failed experiment")
	}
}

func TestLiveExperiment_GetMetricPatterns_CustomPatterns(t *testing.T) {
	r, _ := newLiveExperimentReconciler()

	exp := newTestLiveExperiment("test", "default")
	exp.Spec.Comparison.MetricSource = &gryviav1.MetricSourceConfig{
		Type: "log-pattern",
		Patterns: map[string]string{
			"loss":     `train_loss: ([\d.]+)`,
			"accuracy": `val_acc: ([\d.]+)`,
		},
	}

	patterns := r.getMetricPatterns(exp)
	if patterns["loss"] != `train_loss: ([\d.]+)` {
		t.Errorf("expected custom loss pattern, got %s", patterns["loss"])
	}
}

func TestLiveExperiment_GetMetricPatterns_DefaultPatterns(t *testing.T) {
	r, _ := newLiveExperimentReconciler()

	exp := newTestLiveExperiment("test", "default")
	exp.Spec.Comparison.PrimaryMetric = "loss"

	patterns := r.getMetricPatterns(exp)
	if _, ok := patterns["loss"]; !ok {
		t.Error("expected default loss pattern")
	}
	if _, ok := patterns["accuracy"]; !ok {
		t.Error("expected default accuracy pattern")
	}
}

func TestLiveExperiment_GetMetricPatterns_SecondaryMetrics(t *testing.T) {
	r, _ := newLiveExperimentReconciler()

	exp := newTestLiveExperiment("test", "default")
	exp.Spec.Comparison.SecondaryMetrics = []gryviav1.SecondaryMetric{
		{Name: "f1_score", Weight: 0.3},
	}

	patterns := r.getMetricPatterns(exp)
	if _, ok := patterns["f1_score"]; !ok {
		t.Error("expected pattern for secondary metric f1_score")
	}
}

func TestLiveExperiment_TerminateJob(t *testing.T) {
	exp := newTestLiveExperiment("test-exp", "default")

	job := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-baseline",
			Namespace: "default",
		},
		Spec: gryviav1.GryviaAIJobSpec{
			Type:  "training",
			Image: "pytorch:latest",
			GPUs:  4,
		},
		Status: gryviav1.GryviaAIJobStatus{
			Phase: PhaseRunning,
		},
	}

	r, fakeClient := newLiveExperimentReconciler(exp, job)

	err := r.terminateJob(context.Background(), exp, "baseline", "underperforming")
	if err != nil {
		t.Fatalf("expected no error terminating job, got %v", err)
	}

	updated := &gryviav1.GryviaAIJob{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "job-baseline", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to get updated job: %v", err)
	}
	if updated.Status.Phase != PhaseFailed {
		t.Errorf("expected phase Failed, got %s", updated.Status.Phase)
	}
	if updated.Status.CompletionTime == nil {
		t.Error("expected CompletionTime to be set")
	}
}

func TestLiveExperiment_TerminateJob_NotFound(t *testing.T) {
	exp := newTestLiveExperiment("test-exp", "default")
	r, _ := newLiveExperimentReconciler(exp)

	// Job name not in spec
	err := r.terminateJob(context.Background(), exp, "nonexistent", "test")
	if err == nil {
		t.Error("expected error for unknown job name")
	}
}
