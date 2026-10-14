package controllers

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func idleFixture(t *testing.T, util float64, ann map[string]string, suspend bool, now *time.Time) (*GryviaIdleReclaimReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	job := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{Name: "train", Namespace: "ns", Annotations: ann},
		Spec:       gryviav1.GryviaAIJobSpec{GPUs: 8},
		Status:     gryviav1.GryviaAIJobStatus{Phase: PhaseRunning, Metrics: &gryviav1.JobMetrics{GpuUtilization: util}},
	}
	c := fake.NewClientBuilder().WithScheme(newProfilerTestScheme()).WithObjects(job).
		WithStatusSubresource(&gryviav1.GryviaAIJob{}).Build()
	rec := record.NewFakeRecorder(10)
	return &GryviaIdleReclaimReconciler{
		Client: c, Recorder: rec, Threshold: 10, Window: 30 * time.Minute, Suspend: suspend,
		Now: func() time.Time { return *now },
	}, c, rec
}

func reconcileIdle(t *testing.T, r *GryviaIdleReclaimReconciler) *gryviav1.GryviaAIJob {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "ns", Name: "train"}}); err != nil {
		t.Fatal(err)
	}
	out := &gryviav1.GryviaAIJob{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "train"}, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIdleReclaimFlagsAfterWindowOnly(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	r, _, rec := idleFixture(t, 3, nil, false, &now)

	j := reconcileIdle(t, r)
	if j.Annotations[AnnotationIdleSince] == "" {
		t.Fatal("first idle observation should record idle-since")
	}
	if apimeta.IsStatusConditionTrue(j.Status.Conditions, ConditionIdleGPU) {
		t.Fatal("flagged before the window elapsed")
	}

	now = now.Add(31 * time.Minute)
	j = reconcileIdle(t, r)
	if !apimeta.IsStatusConditionTrue(j.Status.Conditions, ConditionIdleGPU) {
		t.Fatal("not flagged after the window")
	}
	if j.Spec.Suspend {
		t.Fatal("advisory mode must not suspend")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(rec.Events))
	}
	reconcileIdle(t, r)
	if len(rec.Events) != 1 {
		t.Fatal("event repeated on a later reconcile")
	}
}

func TestIdleReclaimSuspendMode(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	r, _, _ := idleFixture(t, 0, map[string]string{AnnotationIdleSince: now.Add(-time.Hour).Format(time.RFC3339)}, true, &now)
	if j := reconcileIdle(t, r); !j.Spec.Suspend {
		t.Fatal("suspend mode should suspend a job idle past the window")
	}
}

func TestIdleReclaimResetsWhenBusyAndHonoursOptOut(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	since := map[string]string{AnnotationIdleSince: now.Add(-time.Hour).Format(time.RFC3339)}

	r, _, _ := idleFixture(t, 80, since, true, &now)
	j := reconcileIdle(t, r)
	if _, ok := j.Annotations[AnnotationIdleSince]; ok || j.Spec.Suspend {
		t.Fatal("a busy job must lose its idle-since and not be suspended")
	}

	optOut := map[string]string{AnnotationIdleReclaim: "false", AnnotationIdleSince: since[AnnotationIdleSince]}
	r, _, _ = idleFixture(t, 0, optOut, true, &now)
	if j := reconcileIdle(t, r); j.Spec.Suspend {
		t.Fatal("opted-out job was suspended")
	}
}
