package controllers

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func TestGuardObservesCommittedSteps(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	r, _ := newCheckpointGuardReconciler()
	r.Enabled, r.Now = true, func() time.Time { return now }
	guard := newTestCheckpointGuard("g", "default")
	at := metav1.NewTime(now.Add(-5 * time.Minute))
	start := metav1.NewTime(now.Add(-2 * time.Hour))
	jobs := []gryviav1.GryviaAIJob{
		{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Status: gryviav1.GryviaAIJobStatus{Phase: PhaseRunning, StartTime: &start,
			Checkpoint: &gryviav1.AIJobCheckpoint{Guard: "g", LastCommittedStep: 100, LastCommittedAt: &at}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: gryviav1.GryviaAIJobSpec{Type: "training"}, Status: gryviav1.GryviaAIJobStatus{Phase: PhaseRunning}},
	}
	r.observeCheckpoints(guard, jobs)
	if guard.Status.TotalCheckpoints != 1 || guard.Status.LastValidCheckpoint != "a/step-100" || !guard.Status.Jobs[0].Injected {
		t.Fatalf("status %+v", guard.Status)
	}
	if guard.Status.Jobs[1].Injected || guard.Status.Jobs[1].Message == "" {
		t.Fatal("job b has no storage: not injected, with a reason")
	}
	r.observeCheckpoints(guard, jobs)
	if guard.Status.TotalCheckpoints != 1 {
		t.Fatal("the same step must not be counted twice")
	}

	jobs[0].Status.Checkpoint.LastCommittedAt = &metav1.Time{Time: now.Add(-90 * time.Minute)}
	jobs[0].Status.Checkpoint.LastCommittedStep = 100
	r.observeCheckpoints(guard, jobs)
	for _, c := range guard.Status.Conditions {
		if c.Type == ConditionCheckpointValid && (c.Status != metav1.ConditionFalse || c.Reason != "Stale") {
			t.Fatalf("no commit in 2x the 30m interval must be Stale, got %s/%s", c.Status, c.Reason)
		}
	}

	r.Enabled = false
	r.observeCheckpoints(guard, jobs[1:])
	if guard.Status.Jobs[0].Message != "checkpoint injection is off (ai-operator --checkpoint-guard)" {
		t.Fatalf("message %q", guard.Status.Jobs[0].Message)
	}
}

func TestGuardRequestsAnEarlyCheckpointOnce(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a-checkpoint-status", Namespace: "default"},
		Data: map[string]string{"committedAt": "2026-10-07T11:00:00Z"}}
	r, c := newCheckpointGuardReconciler(cm)
	r.Now = func() time.Time { return now }
	guard := newTestCheckpointGuard("g", "default")
	jobs := []gryviav1.GryviaAIJob{{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}, Status: gryviav1.GryviaAIJobStatus{Phase: PhaseRunning,
		Checkpoint: &gryviav1.AIJobCheckpoint{Guard: "g", StatusConfigMap: "a-checkpoint-status"}}}}

	if n := r.requestCheckpoints(context.Background(), guard, jobs, "GPU degraded"); n != 1 {
		t.Fatalf("requests %d", n)
	}
	got := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "a-checkpoint-status"}, got)
	if got.Data["requestedAt"] != "2026-10-07T12:00:00Z" || got.Data["requestReason"] != "GPU degraded" {
		t.Fatalf("request %v", got.Data)
	}
	if n := r.requestCheckpoints(context.Background(), guard, jobs, "again"); n != 0 {
		t.Fatal("an open request must not be repeated")
	}
	got.Data["committedAt"] = "2026-10-07T12:01:00Z"
	_ = c.Update(context.Background(), got)
	r.Now = func() time.Time { return now.Add(5 * time.Minute) }
	if n := r.requestCheckpoints(context.Background(), guard, jobs, "again"); n != 1 {
		t.Fatal("after a commit answered the request a new one may be written")
	}
}
