// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func newTimeMachineTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

func newTimeMachineReconciler(objs ...client.Object) (*GryviaTrainingTimeMachineReconciler, client.Client) {
	scheme := newTimeMachineTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaTrainingTimeMachine{}, &gryviav1.GryviaAIJob{}).
		Build()
	r := &GryviaTrainingTimeMachineReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Log:    ctrl.Log.WithName("test"),
	}
	return r, fakeClient
}

func newTestTimeMachine(name, namespace string) *gryviav1.GryviaTrainingTimeMachine {
	return &gryviav1.GryviaTrainingTimeMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: gryviav1.GryviaTrainingTimeMachineSpec{
			SourceJob: "source-job",
			Timeline: &gryviav1.TimelineConfig{
				Enabled:          true,
				IndexCheckpoints: true,
			},
			Retention: &gryviav1.RetentionConfig{
				KeepBest:     5,
				KeepEveryNth: 10,
				MaxStorageGi: 100,
			},
		},
	}
}

func TestTimeMachine_Reconcile_NotFound(t *testing.T) {
	r, _ := newTimeMachineReconciler()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for not-found")
	}
}

func TestTimeMachine_Reconcile_DeletionTimestamp(t *testing.T) {
	now := metav1.Now()
	tm := newTestTimeMachine("test-tm", "default")
	tm.DeletionTimestamp = &now
	tm.Finalizers = []string{"keep-for-test"}

	r, _ := newTimeMachineReconciler(tm)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-tm", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error during deletion, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for deleted resource")
	}
}

func TestTimeMachine_Reconcile_SourceJobNotFound(t *testing.T) {
	tm := newTestTimeMachine("test-tm", "default")
	r, fakeClient := newTimeMachineReconciler(tm)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-tm", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err == nil {
		t.Error("expected error when source job not found")
	}
	if result.RequeueAfter != timeMachineReconcileInterval {
		t.Errorf("expected requeue after %v, got %v", timeMachineReconcileInterval, result.RequeueAfter)
	}

	updated := &gryviav1.GryviaTrainingTimeMachine{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-tm", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to get updated tm: %v", err)
	}
	// Should have SourceJobFound condition set to False
	foundCond := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == ConditionSourceJobFound && cond.Status == metav1.ConditionFalse {
			foundCond = true
		}
	}
	if !foundCond {
		t.Error("expected SourceJobFound condition to be False")
	}
}

func TestTimeMachine_GetSourceJob(t *testing.T) {
	sourceJob := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "source-job",
			Namespace: "default",
		},
		Spec: gryviav1.GryviaAIJobSpec{
			Type:  "training",
			Image: "pytorch:latest",
			GPUs:  4,
		},
	}

	tm := newTestTimeMachine("test-tm", "default")
	r, _ := newTimeMachineReconciler(tm, sourceJob)

	job, err := r.getSourceJob(context.Background(), tm)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if job.Name != "source-job" {
		t.Errorf("expected source-job, got %s", job.Name)
	}
}

func TestTimeMachine_ResolveCheckpoint_ByStep(t *testing.T) {
	tm := newTestTimeMachine("test-tm", "default")
	r, _ := newTimeMachineReconciler()

	step := 1000
	resolved, err := r.resolveCheckpoint(tm, gryviav1.CheckpointSelector{
		Step: &step,
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resolved != 1000 {
		t.Errorf("expected step 1000, got %d", resolved)
	}
}

func TestTimeMachine_ResolveCheckpoint_ByEpoch(t *testing.T) {
	tm := newTestTimeMachine("test-tm", "default")
	tm.Status.CheckpointTimeline = &gryviav1.CheckpointTimelineStatus{
		LatestCheckpoint: &gryviav1.CheckpointInfo{
			Step:  5000,
			Epoch: 10,
		},
	}
	r, _ := newTimeMachineReconciler()

	epoch := 5
	resolved, err := r.resolveCheckpoint(tm, gryviav1.CheckpointSelector{
		Epoch: &epoch,
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	// stepsPerEpoch = 5000/10 = 500, resolved = 500 * 5 = 2500
	if resolved != 2500 {
		t.Errorf("expected step 2500, got %d", resolved)
	}
}

func TestTimeMachine_ResolveCheckpoint_ByMetric(t *testing.T) {
	tm := newTestTimeMachine("test-tm", "default")
	tm.Status.CheckpointTimeline = &gryviav1.CheckpointTimelineStatus{
		BestCheckpoint: &gryviav1.CheckpointInfo{
			Step:        3000,
			MetricName:  "loss",
			MetricValue: 0.1,
		},
	}
	r, _ := newTimeMachineReconciler()

	resolved, err := r.resolveCheckpoint(tm, gryviav1.CheckpointSelector{
		Metric: &gryviav1.MetricSelector{
			Name:     "val_loss",
			Selector: "min",
		},
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resolved != 3000 {
		t.Errorf("expected step 3000, got %d", resolved)
	}
}

func TestTimeMachine_ResolveCheckpoint_NoSelector(t *testing.T) {
	tm := newTestTimeMachine("test-tm", "default")
	r, _ := newTimeMachineReconciler()

	_, err := r.resolveCheckpoint(tm, gryviav1.CheckpointSelector{})
	if err == nil {
		t.Error("expected error with no selector")
	}
}

func TestTimeMachine_SetForkStatus(t *testing.T) {
	tm := newTestTimeMachine("test-tm", "default")
	r, _ := newTimeMachineReconciler()

	// Add new fork status
	r.setForkStatus(tm, "fork-1", 1000, "forked-job-1", ForkStatusRunning, 0.5)
	if len(tm.Status.Forks) != 1 {
		t.Fatalf("expected 1 fork, got %d", len(tm.Status.Forks))
	}
	if tm.Status.Forks[0].Name != "fork-1" {
		t.Errorf("expected fork name fork-1, got %s", tm.Status.Forks[0].Name)
	}
	if tm.Status.Forks[0].Status != ForkStatusRunning {
		t.Errorf("expected status Running, got %s", tm.Status.Forks[0].Status)
	}

	// Update existing fork status
	r.setForkStatus(tm, "fork-1", 1000, "forked-job-1", ForkStatusSucceeded, 0.01)
	if len(tm.Status.Forks) != 1 {
		t.Fatalf("expected still 1 fork, got %d", len(tm.Status.Forks))
	}
	if tm.Status.Forks[0].Status != ForkStatusSucceeded {
		t.Errorf("expected status Succeeded, got %s", tm.Status.Forks[0].Status)
	}

	// Add another fork
	r.setForkStatus(tm, "fork-2", 2000, "forked-job-2", ForkStatusPending, 0)
	if len(tm.Status.Forks) != 2 {
		t.Fatalf("expected 2 forks, got %d", len(tm.Status.Forks))
	}
}

func TestTimeMachine_TrackForkedJobs(t *testing.T) {
	forkedJob := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "forked-job-1",
			Namespace: "default",
		},
		Status: gryviav1.GryviaAIJobStatus{
			Phase: PhaseSucceeded,
			Metrics: &gryviav1.JobMetrics{
				Loss: 0.05,
			},
		},
	}

	tm := newTestTimeMachine("test-tm", "default")
	tm.Status.Forks = []gryviav1.ForkStatus{
		{Name: "fork-1", ForkedJob: "forked-job-1", Status: ForkStatusRunning},
	}

	r, _ := newTimeMachineReconciler(tm, forkedJob)
	r.trackForkedJobs(context.Background(), tm)

	if tm.Status.Forks[0].Status != ForkStatusSucceeded {
		t.Errorf("expected fork status Succeeded, got %s", tm.Status.Forks[0].Status)
	}
	if tm.Status.Forks[0].CurrentMetric != 0.05 {
		t.Errorf("expected current metric 0.05, got %f", tm.Status.Forks[0].CurrentMetric)
	}
}

func TestTimeMachine_UpdateStorageUsage(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "source-job-data",
			Namespace: "default",
			Labels:    map[string]string{"gryvia.io/job": "source-job"},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("50Gi"),
			},
		},
	}

	tm := newTestTimeMachine("test-tm", "default")
	r, _ := newTimeMachineReconciler(tm, pvc)
	r.updateStorageUsage(context.Background(), tm)

	if tm.Status.StorageUsed != "50Gi" {
		t.Errorf("expected 50Gi storage used, got %s", tm.Status.StorageUsed)
	}
}

func TestTimeMachine_UpdateCondition(t *testing.T) {
	r, _ := newTimeMachineReconciler()
	tm := newTestTimeMachine("test", "default")

	r.updateCondition(tm, ConditionSourceJobFound, metav1.ConditionTrue, "Found", "Source job found")
	if len(tm.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(tm.Status.Conditions))
	}

	r.updateCondition(tm, ConditionSourceJobFound, metav1.ConditionFalse, "NotFound", "Source job missing")
	if len(tm.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(tm.Status.Conditions))
	}
	if tm.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Error("expected condition False after update")
	}

	r.updateCondition(tm, ConditionTimelineIndexed, metav1.ConditionTrue, "OK", "Timeline indexed")
	if len(tm.Status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(tm.Status.Conditions))
	}
}
