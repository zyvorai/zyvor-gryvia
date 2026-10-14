package scheduler

import (
	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"math"
	"testing"
	"time"
)

func TestFreshTelemetryBoundsAndValidity(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	labels := map[string]string{LabelGPUUtilization: "0", LabelGPUMemoryUsed: "0"}
	if got := FreshLiveTelemetryScore(labels, map[string]string{AnnotationTelemetryObservedAt: now.Format(time.RFC3339)}, now, false, false); got != 25 {
		t.Fatalf("got %d", got)
	}
	for _, stamp := range []string{"", "bad", now.Add(-61 * time.Second).Format(time.RFC3339), now.Add(time.Second).Format(time.RFC3339)} {
		if got := FreshLiveTelemetryScore(labels, map[string]string{AnnotationTelemetryObservedAt: stamp}, now, false, false); got != 0 {
			t.Fatalf("accepted %q", stamp)
		}
	}
	for _, value := range []string{"", "NaN", "+Inf", "-1", "101", "broken"} {
		labels[LabelGPUUtilization] = value
		if got := FreshLiveTelemetryScore(labels, map[string]string{AnnotationTelemetryObservedAt: now.Format(time.RFC3339)}, now, false, false); got != 0 {
			t.Fatalf("accepted %q", value)
		}
	}
	if math.IsNaN(TelemetryFromLabels(map[string]string{LabelGPUUtilization: "NaN"}).GPUUtilization) {
		t.Fatal("NaN leaked")
	}
}
func TestPlacementUsesTelemetryOnlyWhenOptedIn(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "idle", Annotations: map[string]string{AnnotationTelemetryObservedAt: now}, Labels: map[string]string{"gryvia.io/gpu-count": "1", LabelGPUUtilization: "0", LabelGPUMemoryUsed: "0"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "busy", Annotations: map[string]string{AnnotationTelemetryObservedAt: now}, Labels: map[string]string{"gryvia.io/gpu-count": "1", LabelGPUUtilization: "100", LabelGPUMemoryUsed: "100"}}},
	}
	job := &gryviav1.GryviaAIJob{}
	job.Spec.GPUs = 1
	off := scoreNodes(nodes, job, nil)
	if off[0].Score != off[1].Score {
		t.Fatal("telemetry changed default placement")
	}
	job.Annotations = map[string]string{AnnotationLiveTelemetry: "true"}
	on := scoreNodes(nodes, job, nil)
	if on[0].Score <= on[1].Score {
		t.Fatal("idle GPU was not preferred")
	}
}
