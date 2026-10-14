package controllers

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func TestNativeAllocationCountsAdmittedPending(t *testing.T) {
	job := &gryviav1.GryviaAIJob{}
	job.Name, job.Namespace = "j", "ns"
	job.Spec.GPUs = 2
	job.Status.Phase = PhasePending
	if _, ok, _ := NativeAllocationFromAIJob(job); ok {
		t.Fatal("pending job without NativeAdmitted must not count")
	}
	meta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{Type: ConditionNativeAdmitted, Status: metav1.ConditionTrue, Reason: "Admitted"})
	a, ok, err := NativeAllocationFromAIJob(job)
	if err != nil || !ok || a.GPU != 2000 {
		t.Fatalf("admitted pending job must count 2000 milli-GPU, got %+v ok=%v err=%v", a, ok, err)
	}
}
