// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"reflect"
	"testing"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestStrictAdmissionMissingQueueStaysSuspended(t *testing.T) {
	j := cpuJobIn("tenant-missing", "train")
	r, c := kueueReconciler(j)
	r.KueueStrictAdmission = true
	reconcileIn(t, r, j.Namespace, j.Name, 3)
	bj := getBatchJob(t, c, j.Namespace, j.Name)
	if bj.Labels[LabelKueueQueue] != DefaultKueueQueue || bj.Spec.Suspend == nil || !*bj.Spec.Suspend {
		t.Fatalf("unprotected workload: %+v", bj.Spec)
	}
	aj := &gryviav1.GryviaAIJob{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: j.Namespace, Name: j.Name}, aj); err != nil {
		t.Fatal(err)
	}
	if aj.Status.Phase != PhaseQueued {
		t.Fatalf("phase %s", aj.Status.Phase)
	}
}

func TestQueuedGPUJobReachesKueueWithoutFreeCapacity(t *testing.T) {
	// No GPU nodes exist at all. Kueue must still see the demand, never lose it
	// behind Gryvia's free-capacity check. Actual pods remain suspended.
	j := newTestAIJob("gpu-demand", "default")
	j.Spec.QueueName = "gpu"
	r, c := kueueReconciler(j)
	reconcileN(t, r, j.Name, 3)
	bj := getBatchJob(t, c, j.Namespace, j.Name)
	if bj.Spec.Suspend == nil || !*bj.Spec.Suspend {
		t.Fatal("job started before admission")
	}
	aj := getAIJob(t, c, j.Name)
	if aj.Status.Phase != PhaseQueued || len(aj.Status.NodesAllocated) != 0 {
		t.Fatalf("status %+v", aj.Status)
	}
	if !conditionTrue(aj, ConditionScheduled) {
		t.Fatal("queue-managed scheduling condition missing")
	}
	// A later reconcile must not overwrite Kueue's unsuspend decision.
	f := false
	bj.Spec.Suspend = &f
	if err := c.Update(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, j.Name, 2)
	if *getBatchJob(t, c, j.Namespace, j.Name).Spec.Suspend {
		t.Fatal("operator resuspended admitted job")
	}
}

func TestStrictAdmissionRejectsTenantStatefulSet(t *testing.T) {
	j := cpuJobIn("tenant-a", "serve")
	j.Spec.Type = "inference"
	r, c := kueueReconciler(j)
	r.KueueStrictAdmission = true
	reconcileIn(t, r, j.Namespace, j.Name, 3)
	aj := &gryviav1.GryviaAIJob{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: j.Namespace, Name: j.Name}, aj); err != nil {
		t.Fatal(err)
	}
	if aj.Status.Phase != PhaseFailed {
		t.Fatalf("phase %s", aj.Status.Phase)
	}
	list := &batchv1.JobList{}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatal("created workload for unsupported queue kind")
	}
}

func recoveryJob() *gryviav1.GryviaAIJob {
	j := cpuJob("checkpoint")
	j.Spec.Storage = "shared"
	j.Annotations = map[string]string{AnnotationCheckpointCommand: `["python","/app/checkpoint.py"]`}
	return j
}

func TestCheckpointHookAndResumeContract(t *testing.T) {
	r, _ := newAIJobReconciler()
	j := recoveryJob()
	j.Annotations[AnnotationCheckpointGrace] = "180"
	bj, err := r.buildJob(j)
	if err != nil {
		t.Fatal(err)
	}
	p := bj.Spec.Template.Spec
	c := p.Containers[0]
	if p.TerminationGracePeriodSeconds == nil || *p.TerminationGracePeriodSeconds != 180 {
		t.Fatal("grace missing")
	}
	if c.Lifecycle == nil || !reflect.DeepEqual(c.Lifecycle.PreStop.Exec.Command, []string{"python", "/app/checkpoint.py"}) {
		t.Fatal("hook missing")
	}
	e := envMap(c.Env)
	if e["GRYVIA_CHECKPOINT_DIR"].Value != "/data/checkpoints" || e["GRYVIA_RESUME_IF_PRESENT"].Value != "true" {
		t.Fatal("resume contract missing")
	}
	if len(c.VolumeMounts) == 0 || c.VolumeMounts[0].MountPath != "/data" {
		t.Fatal("persistent volume missing")
	}
}

func TestRecoveryValidation(t *testing.T) {
	cases := []struct {
		name   string
		modify func(*gryviav1.GryviaAIJob)
	}{
		{"no storage", func(j *gryviav1.GryviaAIJob) { j.Spec.Storage = "" }},
		{"shell string", func(j *gryviav1.GryviaAIJob) { j.Annotations[AnnotationCheckpointCommand] = `"rm anything"` }},
		{"empty argv", func(j *gryviav1.GryviaAIJob) { j.Annotations[AnnotationCheckpointCommand] = `[]` }},
		{"empty executable", func(j *gryviav1.GryviaAIJob) { j.Annotations[AnnotationCheckpointCommand] = `[""]` }},
		{"nul", func(j *gryviav1.GryviaAIJob) { j.Annotations[AnnotationCheckpointCommand] = `["python","\u0000"]` }},
		{"traversal", func(j *gryviav1.GryviaAIJob) { j.Annotations[AnnotationCheckpointDirectory] = "/data/../etc" }},
		{"outside volume", func(j *gryviav1.GryviaAIJob) { j.Annotations[AnnotationCheckpointDirectory] = "/tmp/checkpoints" }},
		{"short grace", func(j *gryviav1.GryviaAIJob) { j.Annotations[AnnotationCheckpointGrace] = "1" }},
		{"long grace", func(j *gryviav1.GryviaAIJob) { j.Annotations[AnnotationCheckpointGrace] = "3601" }},
		{"serving", func(j *gryviav1.GryviaAIJob) { j.Spec.Type = "inference" }},
		{"orphan directory", func(j *gryviav1.GryviaAIJob) {
			delete(j.Annotations, AnnotationCheckpointCommand)
			j.Annotations[AnnotationCheckpointDirectory] = "/data/cp"
		}},
		{"bad spread", func(j *gryviav1.GryviaAIJob) { j.Annotations[AnnotationSpreadWorkers] = "yes" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := recoveryJob()
			tc.modify(j)
			r, _ := newAIJobReconciler()
			if _, err := r.buildJob(j); err == nil {
				t.Fatal("accepted invalid recovery configuration")
			}
		})
	}
}

func TestSpreadWorkersPreservesUserAffinity(t *testing.T) {
	j := cpuJob("distributed")
	j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2}
	j.Annotations = map[string]string{AnnotationSpreadWorkers: "true"}
	j.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "rack", LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}}}}}}
	before := j.Spec.Affinity.DeepCopy()
	r, _ := newAIJobReconciler()
	bj, err := r.buildJob(j)
	if err != nil {
		t.Fatal(err)
	}
	terms := bj.Spec.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(terms) != 2 || terms[1].TopologyKey != "kubernetes.io/hostname" || terms[1].Namespaces[0] != j.Namespace {
		t.Fatalf("spread %+v", terms)
	}
	if !reflect.DeepEqual(j.Spec.Affinity, before) {
		t.Fatal("mutated user spec")
	}
	j.Annotations = nil
	bj, err = r.buildJob(j)
	if err != nil {
		t.Fatal(err)
	}
	if len(bj.Spec.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatal("spread not opt-in")
	}
}

func TestInvalidRecoveryCreatesNoResources(t *testing.T) {
	j := recoveryJob()
	j.Annotations[AnnotationCheckpointGrace] = "1"
	r, c := newAIJobReconciler(j)
	reconcileN(t, r, j.Name, 3)
	if getAIJob(t, c, j.Name).Status.Phase != PhaseFailed {
		t.Fatal("invalid options were not rejected")
	}
	if exists(t, c, &corev1.PersistentVolumeClaim{}, j.Name+"-data") || exists(t, c, &corev1.Service{}, headlessServiceName(j)) || exists(t, c, &batchv1.Job{}, j.Name) {
		t.Fatal("resources created before validation")
	}
}

func TestRecoveryEditsDoNotChangeExistingJob(t *testing.T) {
	j := cpuJob("immutable")
	r, c := newAIJobReconciler(j)
	reconcileN(t, r, j.Name, 3)
	original := getBatchJob(t, c, j.Namespace, j.Name).Spec.Template.DeepCopy()
	aj := getAIJob(t, c, j.Name)
	aj.Annotations = map[string]string{AnnotationCheckpointGrace: "invalid"}
	if err := c.Update(context.Background(), aj); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, j.Name, 2)
	if !reflect.DeepEqual(*original, getBatchJob(t, c, j.Namespace, j.Name).Spec.Template) {
		t.Fatal("rewrote existing job template")
	}
	if getAIJob(t, c, j.Name).Status.Phase == PhaseFailed {
		t.Fatal("new annotations failed an existing job")
	}
}
