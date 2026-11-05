package controllers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func TestApplyVolcano(t *testing.T) {
	r := &GryviaAIJobReconciler{VolcanoScheduler: true, VolcanoDefaultQueue: "team-a"}
	job := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
		Spec:       gryviav1.GryviaAIJobSpec{},
	}
	ann := map[string]string{}
	spec := &corev1.PodSpec{}
	r.applyVolcano(job, ann, spec)
	if spec.SchedulerName != VolcanoSchedulerName {
		t.Fatalf("schedulerName = %q", spec.SchedulerName)
	}
	if ann[AnnotationVolcanoQueue] != "team-a" {
		t.Fatalf("queue = %q", ann[AnnotationVolcanoQueue])
	}
}
