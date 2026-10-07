package webhook

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/preflight"
)

func gpuNode(name, gpuType, mem string, gpus int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			preflight.LabelGPU: gpuType, preflight.LabelGPUMemory: mem}},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			"nvidia.com/gpu": *resource.NewQuantity(gpus, resource.DecimalSI)}},
	}
}

func TestPreflightAnnotationsAreValidated(t *testing.T) {
	job := validJob()
	job.Annotations = map[string]string{preflight.AnnotationParams: "abc"}
	errs := ValidateJob(job)
	if len(errs) != 1 || !strings.Contains(errs[0], preflight.AnnotationParams) {
		t.Fatalf("errs = %v", errs)
	}
}

func TestPreflightWarnsButNeverDenies(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(gpuNode("n1", "A100", "40", 8)).Build()
	v := NewGryviaAIJobValidator(c)

	job := validJob()
	job.Spec.GpuType = "A100"
	job.Annotations = map[string]string{preflight.AnnotationParams: "70"} // 130 GiB per GPU on 40 GiB GPUs
	w := v.clusterWarnings(context.Background(), job)
	if len(w) != 1 || !strings.Contains(w[0], "preflight Blocked") || !strings.Contains(w[0], "memory") {
		t.Fatalf("warnings = %v", w)
	}

	job.Annotations = map[string]string{preflight.AnnotationParams: "7", preflight.AnnotationExtraGiB: "4"}
	if w := v.clusterWarnings(context.Background(), job); len(w) != 0 {
		t.Fatalf("a fitting job gets no warning: %v", w)
	}
}
