package controllers

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func newCheckpointGuardTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	return s
}

func newCheckpointGuardReconciler(objs ...client.Object) (*GryviaCheckpointGuardReconciler, client.Client) {
	scheme := newCheckpointGuardTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaCheckpointGuard{}, &gryviav1.GryviaAIJob{}).
		Build()
	r := &GryviaCheckpointGuardReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Log:    ctrl.Log.WithName("test"),
	}
	return r, fakeClient
}

func newTestCheckpointGuard(name, namespace string) *gryviav1.GryviaCheckpointGuard {
	return &gryviav1.GryviaCheckpointGuard{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: gryviav1.GryviaCheckpointGuardSpec{
			JobSelector: gryviav1.JobSelector{
				MatchLabels: map[string]string{"team": "ml"},
			},
			CheckpointPolicy: gryviav1.CheckpointPolicy{
				IntervalMinutes: 30,
				EmergencyCheckpoint: &gryviav1.EmergencyCheckpointConfig{
					Triggers: []string{TriggerGpuHealthDegraded, TriggerMemoryPressure},
				},
			},
			Validation: &gryviav1.CheckpointValidation{
				ChecksumVerify:  true,
				RetentionCount:  5,
				RetainValidOnly: true,
			},
		},
	}
}

func TestCheckpointGuard_Reconcile_NotFound(t *testing.T) {
	r, _ := newCheckpointGuardReconciler()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for not-found")
	}
}

func TestCheckpointGuard_Reconcile_DeletionTimestamp(t *testing.T) {
	now := metav1.Now()
	guard := newTestCheckpointGuard("test-guard", "default")
	guard.DeletionTimestamp = &now
	guard.Finalizers = []string{"keep-for-test"}

	r, _ := newCheckpointGuardReconciler(guard)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-guard", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error during deletion, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for deleted resource")
	}
}

func TestCheckpointGuard_Reconcile_InitializesPhase(t *testing.T) {
	guard := newTestCheckpointGuard("test-guard", "default")
	r, fakeClient := newCheckpointGuardReconciler(guard)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-guard", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after phase initialization")
	}

	updated := &gryviav1.GryviaCheckpointGuard{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-guard", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to get updated guard: %v", err)
	}
	if updated.Status.Phase != PhaseIdle {
		t.Errorf("expected phase %s, got %s", PhaseIdle, updated.Status.Phase)
	}
}

func TestCheckpointGuard_Reconcile_NoMatchingJobs(t *testing.T) {
	guard := newTestCheckpointGuard("test-guard", "default")
	guard.Status.Phase = PhaseIdle
	r, fakeClient := newCheckpointGuardReconciler(guard)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-guard", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after for no matching jobs")
	}

	updated := &gryviav1.GryviaCheckpointGuard{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-guard", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to get updated guard: %v", err)
	}
	if updated.Status.Phase != PhaseIdle {
		t.Errorf("expected phase %s when no jobs match, got %s", PhaseIdle, updated.Status.Phase)
	}
	if updated.Status.MatchedJobs != 0 {
		t.Errorf("expected 0 matched jobs, got %d", updated.Status.MatchedJobs)
	}
}

func TestCheckpointGuard_IsEmergencyTrigger(t *testing.T) {
	guard := newTestCheckpointGuard("test-guard", "default")
	r, _ := newCheckpointGuardReconciler()

	if !r.isEmergencyTrigger(guard, TriggerGpuHealthDegraded) {
		t.Error("expected GpuHealthDegraded to be an emergency trigger")
	}
	if !r.isEmergencyTrigger(guard, TriggerMemoryPressure) {
		t.Error("expected MemoryPressure to be an emergency trigger")
	}
	if r.isEmergencyTrigger(guard, TriggerSpotPreemptionSignal) {
		t.Error("expected SpotPreemptionSignal to not be an emergency trigger")
	}
	if r.isEmergencyTrigger(guard, TriggerLossDivergence) {
		t.Error("expected LossDivergence to not be an emergency trigger")
	}

	// No emergency checkpoint config
	guard.Spec.CheckpointPolicy.EmergencyCheckpoint = nil
	if r.isEmergencyTrigger(guard, TriggerGpuHealthDegraded) {
		t.Error("expected false when no emergency checkpoint config")
	}
}

func TestCheckpointGuard_DetectLossDivergence(t *testing.T) {
	r, _ := newCheckpointGuardReconciler()

	job := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-job",
			Namespace: "default",
		},
	}

	// No metrics - no divergence
	if r.detectLossDivergence(job) {
		t.Error("expected no divergence when no metrics")
	}

	// Normal loss
	job.Status.Metrics = &gryviav1.JobMetrics{Loss: 0.5}
	if r.detectLossDivergence(job) {
		t.Error("expected no divergence for normal loss")
	}

	// Very high loss (divergence)
	job.Status.Metrics.Loss = 1e7
	if !r.detectLossDivergence(job) {
		t.Error("expected divergence for very high loss")
	}
}

func TestCheckpointGuard_GetRequeueInterval(t *testing.T) {
	r, _ := newCheckpointGuardReconciler()

	guard := newTestCheckpointGuard("test-guard", "default")
	guard.Spec.CheckpointPolicy.IntervalMinutes = 10

	interval := r.getRequeueInterval(guard)
	expected := 5 * time.Minute // Half of 10 minutes
	if interval != expected {
		t.Errorf("expected %v, got %v", expected, interval)
	}

	// Very small interval should floor at 30 seconds
	guard.Spec.CheckpointPolicy.IntervalMinutes = 0
	interval = r.getRequeueInterval(guard)
	if interval != 15*time.Minute { // Half of default 30 min
		t.Errorf("expected 15m for default interval, got %v", interval)
	}
}

func TestCheckpointGuard_UpdateGuardCondition(t *testing.T) {
	r, _ := newCheckpointGuardReconciler()
	guard := newTestCheckpointGuard("test-guard", "default")

	r.updateGuardCondition(guard, ConditionGuardActive, metav1.ConditionTrue, "Active", "Monitoring")
	if len(guard.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(guard.Status.Conditions))
	}

	r.updateGuardCondition(guard, ConditionGuardActive, metav1.ConditionFalse, "Inactive", "No jobs")
	if len(guard.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(guard.Status.Conditions))
	}
	if guard.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Error("expected condition to be False")
	}

	r.updateGuardCondition(guard, ConditionCheckpointValid, metav1.ConditionTrue, "Valid", "Checkpoint OK")
	if len(guard.Status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(guard.Status.Conditions))
	}
}

func TestCheckJobPodHealth_ByLabelForBothWorkloadKinds(t *testing.T) {
	pod := func(name string, phase corev1.PodPhase, ready corev1.ConditionStatus) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"gryvia.io/job": "j"}},
			Status: corev1.PodStatus{Phase: phase,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready, Message: "m"}}},
		}
	}
	job := newTestAIJob("j", "default")
	cases := []struct {
		name string
		pods []client.Object
		want bool
	}{
		{"no pods and no StatefulSet", nil, true},
		{"Job pods ready (no StatefulSet exists)", []client.Object{pod("j-0-abc", corev1.PodRunning, corev1.ConditionTrue)}, true},
		{"an unready pod is reported", []client.Object{pod("j-0-abc", corev1.PodRunning, corev1.ConditionFalse)}, false},
		{"completed Job pods are not unhealthy", []client.Object{pod("j-0-abc", corev1.PodSucceeded, corev1.ConditionFalse)}, true},
		{"failed pods replaced by the Job controller are skipped", []client.Object{pod("j-0-abc", corev1.PodFailed, corev1.ConditionFalse)}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newCheckpointGuardReconciler(c.pods...)
			ok, reason := r.checkJobPodHealth(context.Background(), job)
			if ok != c.want {
				t.Errorf("healthy = %v (%s), want %v", ok, reason, c.want)
			}
		})
	}
}
