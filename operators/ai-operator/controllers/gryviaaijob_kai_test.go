package controllers

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func TestKAI_DistributedJobIsGangScheduledInItsQueue(t *testing.T) {
	r, _ := newAIJobReconciler()
	r.KAIScheduler = true
	job := newTestAIJob("train", ns)
	job.Spec.QueueName = "research"
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, GpusPerNode: 8, Framework: "pytorch"}

	bj, err := r.buildJob(job)
	if err != nil {
		t.Fatal(err)
	}
	if got := bj.Annotations[AnnotationKAIBatchMinMember]; got != "4" {
		t.Errorf("batch-min-member = %q, want 4", got)
	}
	tpl := bj.Spec.Template
	if tpl.Spec.SchedulerName != KAISchedulerName {
		t.Errorf("schedulerName = %q", tpl.Spec.SchedulerName)
	}
	if tpl.Labels[LabelKAIQueue] != "research" {
		t.Errorf("queue label = %q", tpl.Labels[LabelKAIQueue])
	}
	if bj.Spec.Suspend != nil && *bj.Spec.Suspend {
		t.Error("a KAI job must not be suspended: KAI gates placement, not admission")
	}
	if q := tpl.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]; q.Value() != 8 {
		t.Errorf("gpu limit = %v, want 8", q.Value())
	}
}

func TestKAI_DefaultQueueAndSingletonJob(t *testing.T) {
	r, _ := newAIJobReconciler()
	r.KAIScheduler = true
	bj, err := r.buildJob(cpuJob("one"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bj.Annotations[AnnotationKAIBatchMinMember]; ok {
		t.Error("a single-pod job needs no gang annotation")
	}
	if got := bj.Spec.Template.Labels[LabelKAIQueue]; got != DefaultKAIQueue {
		t.Errorf("queue = %q, want %q", got, DefaultKAIQueue)
	}
	r.KAIDefaultQueue = "team-a"
	bj, _ = r.buildJob(cpuJob("two"))
	if got := bj.Spec.Template.Labels[LabelKAIQueue]; got != "team-a" {
		t.Errorf("queue = %q, want team-a", got)
	}
}

func TestKAI_ElasticJobGangsItsMinimum(t *testing.T) {
	job := newTestAIJob("elastic", ns)
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 6, GpusPerNode: 1,
		Elastic: &gryviav1.ElasticConfig{MinNodes: 2}}
	if got := kaiMinMember(job, 6); got != 2 {
		t.Errorf("min member = %d, want 2", got)
	}
	if got := kaiMinMember(newTestAIJob("plain", ns), 3); got != 3 {
		t.Errorf("min member = %d, want 3", got)
	}
}

func TestKAI_FractionalGPU(t *testing.T) {
	r, _ := newAIJobReconciler()
	r.KAIScheduler = true
	job := newTestAIJob("half", ns)
	job.Spec.GPUs = 1
	job.Annotations = map[string]string{AnnotationGPUShare: "0.5", "gryvia.io/gpu-isolation": "mps"}

	tpl := r.buildPodTemplate(job, map[string]string{"gryvia.io/job": "half"}, 1, gryviav1.WorkloadKindJob, envVersionCurrent)
	if tpl.Annotations[AnnotationKAIGPUFraction] != "0.5" {
		t.Errorf("gpu-fraction = %q", tpl.Annotations[AnnotationKAIGPUFraction])
	}
	if _, ok := tpl.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]; ok {
		t.Error("a KAI fraction must not also request a whole nvidia.com/gpu")
	}
	for _, e := range tpl.Spec.Containers[0].Env {
		if strings.HasPrefix(e.Name, "CUDA_MPS_") {
			t.Errorf("MPS env %s set under KAI", e.Name)
		}
	}

	job.Annotations["gryvia.io/gpu-memory"] = "20480"
	tpl = r.buildPodTemplate(job, map[string]string{}, 1, gryviav1.WorkloadKindJob, envVersionCurrent)
	if tpl.Annotations[AnnotationKAIGPUMemory] != "20480" || tpl.Annotations[AnnotationKAIGPUFraction] != "" {
		t.Errorf("want only gpu-memory=20480, got %v", tpl.Annotations)
	}

	job.Annotations = map[string]string{AnnotationGPUShare: "2"}
	tpl = r.buildPodTemplate(job, map[string]string{}, 2, gryviav1.WorkloadKindJob, envVersionCurrent)
	if tpl.Annotations[AnnotationKAIGPUFraction] != "" {
		t.Error("whole GPUs are not a fraction")
	}
}

func TestKAI_StatefulSetSelectorIsUntouched(t *testing.T) {
	r, _ := newAIJobReconciler()
	r.KAIScheduler = true
	job := newTestAIJob("serve", ns)
	job.Spec.Type = "inference"
	sts := r.buildStatefulSet(job)
	if _, ok := sts.Spec.Selector.MatchLabels[LabelKAIQueue]; ok {
		t.Error("the queue label must not join the immutable selector")
	}
	if sts.Spec.Template.Labels[LabelKAIQueue] != DefaultKAIQueue || sts.Spec.Template.Spec.SchedulerName != KAISchedulerName {
		t.Errorf("template not handed to KAI: %v %q", sts.Spec.Template.Labels, sts.Spec.Template.Spec.SchedulerName)
	}
}

func TestKAI_OffChangesNothing(t *testing.T) {
	r, _ := newAIJobReconciler()
	job := newTestAIJob("plain", ns)
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2, GpusPerNode: 1}
	bj, _ := r.buildJob(job)
	if bj.Spec.Template.Spec.SchedulerName != "" || bj.Annotations != nil || bj.Spec.Template.Labels[LabelKAIQueue] != "" {
		t.Errorf("KAI fields set without --kai-scheduler: %v %v", bj.Annotations, bj.Spec.Template.Labels)
	}
}

func TestKAI_ReconcileDelegatesPlacement(t *testing.T) {
	job := newTestAIJob("deleg", ns)
	job.Spec.GPUs = 8
	r, c := newAIJobReconciler(job) // no GPU nodes: the operator's own placement would fail
	r.KAIScheduler = true
	reconcileN(t, r, "deleg", 4)

	got := getAIJob(t, c, "deleg")
	if !conditionTrue(got, ConditionScheduled) {
		t.Fatalf("job not handed to KAI: phase %s, conditions %v", got.Status.Phase, got.Status.Conditions)
	}
	bj := &batchv1.Job{}
	if !exists(t, c, bj, "deleg") {
		t.Fatal("batch Job not created")
	}
	if bj.Spec.Template.Spec.SchedulerName != KAISchedulerName {
		t.Errorf("schedulerName = %q", bj.Spec.Template.Spec.SchedulerName)
	}
	if exists(t, c, &appsv1.StatefulSet{}, "deleg") {
		t.Error("unexpected StatefulSet")
	}
}
