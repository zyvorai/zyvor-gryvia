// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func localQueue(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"clusterQueue": "gryvia-x"},
	}}
	u.SetGroupVersionKind(KueueGVK("LocalQueue"))
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

// workload builds a Kueue Workload owned by the batch Job with the given conditions
// (type, status, reason, message).
func workload(namespace, jobName, jobUID string, conds ...[4]string) *unstructured.Unstructured {
	cs := []interface{}{}
	for _, c := range conds {
		cs = append(cs, map[string]interface{}{"type": c[0], "status": c[1], "reason": c[2], "message": c[3]})
	}
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"conditions": cs},
	}}
	u.SetGroupVersionKind(KueueGVK("Workload"))
	u.SetNamespace(namespace)
	u.SetName("job-" + jobName + "-abcde")
	u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: jobName, UID: types.UID(jobUID)}})
	return u
}

func reconcileIn(t *testing.T, r *GryviaAIJobReconciler, namespace, name string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
}

func kueueReconciler(objs ...client.Object) (*GryviaAIJobReconciler, client.Client) {
	r, c := newAIJobReconciler(objs...)
	r.KueueIntegration = true
	return r, c
}

func cpuJobIn(namespace, name string) *gryviav1.GryviaAIJob {
	j := cpuJob(name)
	j.Namespace = namespace
	return j
}

func getBatchJob(t *testing.T, c client.Client, namespace, name string) *batchv1.Job {
	t.Helper()
	bj := &batchv1.Job{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, bj); err != nil {
		t.Fatal(err)
	}
	return bj
}

func setUID(t *testing.T, c client.Client, bj *batchv1.Job, uid string) {
	t.Helper()
	bj.UID = types.UID(uid)
	if err := c.Update(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
}

func TestPriorityBucket(t *testing.T) {
	for in, want := range map[int32]int32{-5: 0, 0: 0, 9: 0, 10: 10, 55: 50, 99: 90, 100: 100, 250: 100} {
		if got := PriorityBucket(in); got != want {
			t.Errorf("PriorityBucket(%d) = %d, want %d", in, got, want)
		}
	}
	if PriorityClassName(37) != "gryvia-priority-30" {
		t.Errorf("class name = %s", PriorityClassName(37))
	}
}

func TestKueue_QueueResolution(t *testing.T) {
	type tc struct {
		name      string
		enabled   bool
		namespace string
		mutate    func(*gryviav1.GryviaAIJob)
		objs      []client.Object
		wantQueue string
		wantNote  bool
	}
	cases := []tc{
		{name: "disabled ignores spec.queueName", enabled: false, namespace: "default",
			mutate: func(j *gryviav1.GryviaAIJob) { j.Spec.QueueName = "q" }},
		{name: "spec.queueName", enabled: true, namespace: "default",
			mutate: func(j *gryviav1.GryviaAIJob) { j.Spec.QueueName = "q" }, wantQueue: "q"},
		{name: "annotation", enabled: true, namespace: "default",
			mutate: func(j *gryviav1.GryviaAIJob) { j.Annotations = map[string]string{AnnotationQueueName: "anno"} }, wantQueue: "anno"},
		{name: "spec beats annotation", enabled: true, namespace: "default",
			mutate: func(j *gryviav1.GryviaAIJob) {
				j.Spec.QueueName = "spec"
				j.Annotations = map[string]string{AnnotationQueueName: "anno"}
			}, wantQueue: "spec"},
		{name: "kueue label passthrough", enabled: true, namespace: "default",
			mutate: func(j *gryviav1.GryviaAIJob) { j.Labels = map[string]string{LabelKueueQueue: "lbl"} }, wantQueue: "lbl"},
		{name: "plain namespace without queue", enabled: true, namespace: "default"},
		{name: "tenant namespace with default queue", enabled: true, namespace: "tenant-a",
			wantQueue: "gryvia"},
		{name: "tenant namespace without LocalQueue", enabled: true, namespace: "tenant-b", wantNote: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var objs []client.Object
			if c.namespace == "tenant-a" {
				objs = append(objs, localQueue("tenant-a", "gryvia"))
			}
			job := cpuJobIn(c.namespace, "j")
			if c.mutate != nil {
				c.mutate(job)
			}
			r, cl := newAIJobReconciler(append(objs, job)...)
			r.KueueIntegration = c.enabled
			reconcileIn(t, r, c.namespace, "j", 3)
			bj := getBatchJob(t, cl, c.namespace, "j")
			gotQueue := bj.Labels[LabelKueueQueue]
			if gotQueue != c.wantQueue {
				t.Errorf("queue label = %q, want %q", gotQueue, c.wantQueue)
			}
			suspended := bj.Spec.Suspend != nil && *bj.Spec.Suspend
			if suspended != (c.wantQueue != "") {
				t.Errorf("suspended = %v with queue %q", suspended, c.wantQueue)
			}
			hasNote := false
			aj := &gryviav1.GryviaAIJob{}
			if err := cl.Get(context.Background(), types.NamespacedName{Namespace: c.namespace, Name: "j"}, aj); err != nil {
				t.Fatal(err)
			}
			for _, cond := range aj.Status.Conditions {
				if cond.Type == ConditionKueue && cond.Reason == "LocalQueueNotFound" {
					hasNote = true
				}
			}
			if hasNote != c.wantNote {
				t.Errorf("LocalQueueNotFound condition = %v, want %v", hasNote, c.wantNote)
			}
		})
	}
}

func TestKueue_PriorityClassMapping(t *testing.T) {
	job := cpuJob("hi")
	job.Spec.QueueName = "q"
	job.Spec.Priority = 37
	low := cpuJob("lo")
	low.Spec.QueueName = "q"
	low.Spec.Priority = 5
	r, c := kueueReconciler(job, low)
	reconcileN(t, r, "hi", 3)
	reconcileN(t, r, "lo", 3)

	bj := getBatchJob(t, c, ns, "hi")
	if got := bj.Labels[LabelKueuePriorityClass]; got != "gryvia-priority-30" {
		t.Errorf("priority label = %q", got)
	}
	wpc := &unstructured.Unstructured{}
	wpc.SetGroupVersionKind(KueueGVK("WorkloadPriorityClass"))
	if err := c.Get(context.Background(), types.NamespacedName{Name: "gryvia-priority-30"}, wpc); err != nil {
		t.Fatalf("WorkloadPriorityClass not created: %v", err)
	}
	if v, _, _ := unstructured.NestedInt64(wpc.Object, "value"); v != 30 {
		t.Errorf("WPC value = %d, want 30", v)
	}
	if wpc.GetLabels()["app.kubernetes.io/managed-by"] != "gryvia-ai-operator" {
		t.Errorf("WPC labels = %v", wpc.GetLabels())
	}
	if _, ok := getBatchJob(t, c, ns, "lo").Labels[LabelKueuePriorityClass]; ok {
		t.Error("bucket 0 is the Kueue default priority: no label expected")
	}
}

func TestKueue_ExistingPriorityClassIsNotModified(t *testing.T) {
	wpc := &unstructured.Unstructured{Object: map[string]interface{}{"value": int64(999)}}
	wpc.SetGroupVersionKind(KueueGVK("WorkloadPriorityClass"))
	wpc.SetName("gryvia-priority-50")
	job := cpuJob("p")
	job.Spec.QueueName = "q"
	job.Spec.Priority = 50
	r, c := kueueReconciler(job, wpc)
	reconcileN(t, r, "p", 3)
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(KueueGVK("WorkloadPriorityClass"))
	if err := c.Get(context.Background(), types.NamespacedName{Name: "gryvia-priority-50"}, got); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := unstructured.NestedInt64(got.Object, "value"); v != 999 {
		t.Errorf("existing class was modified: value=%d", v)
	}
}

func TestKueue_DoesNotFightKueueOverSuspend(t *testing.T) {
	job := cpuJob("kq")
	job.Spec.QueueName = "q"
	r, c := kueueReconciler(job)
	reconcileN(t, r, "kq", 3)
	bj := getBatchJob(t, c, ns, "kq")
	if !*bj.Spec.Suspend {
		t.Fatal("must be created suspended")
	}
	f := false
	bj.Spec.Suspend = &f // Kueue admits
	if err := c.Update(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "kq", 3)
	if *getBatchJob(t, c, ns, "kq").Spec.Suspend {
		t.Error("controller re-suspended a Job Kueue admitted")
	}
	// spec.suspend=true on the AIJob must not re-suspend it either: Kueue owns suspend.
	aj := getAIJob(t, c, "kq")
	aj.Spec.Suspend = true
	if err := c.Update(context.Background(), aj); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "kq", 2)
	if *getBatchJob(t, c, ns, "kq").Spec.Suspend {
		t.Error("spec.suspend fought Kueue")
	}
	// Without the queue label spec.suspend still syncs (W1 behaviour).
	plain := cpuJob("plain")
	r2, c2 := kueueReconciler(plain)
	reconcileN(t, r2, "plain", 3)
	p := getAIJob(t, c2, "plain")
	p.Spec.Suspend = true
	if err := c2.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r2, "plain", 1)
	if !*getBatchJob(t, c2, ns, "plain").Spec.Suspend {
		t.Error("without a queue label spec.suspend must still be synced")
	}
}

// An elastic job under Kueue accepts partial admission down to minNodes; others are all-or-nothing.
func TestKueue_ElasticPartialAdmission(t *testing.T) {
	el := elasticJob(2, 4)
	el.Spec.QueueName = "q"
	fixed := elasticJob(3, 3)
	fixed.Name = "fixed"
	fixed.Spec.QueueName = "q"
	plain := cpuJob("plain")
	plain.Spec.QueueName = "q"
	plain.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2}
	noQueue := elasticJob(1, 2)
	noQueue.Name = "noq"
	r, c := kueueReconciler(el, fixed, plain, noQueue)
	for _, n := range []string{"el", "fixed", "plain", "noq"} {
		reconcileN(t, r, n, 3)
	}

	a := getBatchJob(t, c, ns, "el").Annotations
	if a[AnnotationKueueMinParallelism] != "2" || a[AnnotationKueueCompletionsEqualParallelism] != "true" {
		t.Errorf("elastic job annotations = %v", a)
	}
	// Kueue admits 2 of 4: the Ready condition counts the admitted workers.
	bj := getBatchJob(t, c, ns, "el")
	two, f := int32(2), false
	bj.Spec.Parallelism, bj.Spec.Completions, bj.Spec.Suspend = &two, &two, &f
	if err := c.Update(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
	bj.Status.Ready = &two
	if err := c.Status().Update(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "el", 2)
	aj := getAIJob(t, c, "el")
	if aj.Status.Phase != PhaseRunning || !conditionTrue(aj, ConditionReady) {
		t.Errorf("after partial admission: phase %s, conditions %+v", aj.Status.Phase, aj.Status.Conditions)
	}

	for _, n := range []string{"fixed", "plain", "noq"} {
		a := getBatchJob(t, c, ns, n).Annotations
		if _, ok := a[AnnotationKueueMinParallelism]; ok {
			t.Errorf("%s: unexpected partial admission: %v", n, a)
		}
		if _, ok := a[AnnotationKueueCompletionsEqualParallelism]; ok {
			t.Errorf("%s: unexpected completions annotation: %v", n, a)
		}
	}
}

func TestInterpretWorkload(t *testing.T) {
	if st := interpretWorkload(nil); st.Found {
		t.Error("nil workload must not be Found")
	}
	pending := interpretWorkload(workload(ns, "j", "u",
		[4]string{"QuotaReserved", "False", "Pending", "couldn't assign flavors: insufficient quota for cpu in flavor gryvia-default"}))
	if !pending.Found || pending.Admitted || pending.QuotaReserved || !strings.Contains(pending.PendingMsg, "insufficient quota") {
		t.Errorf("pending = %+v", pending)
	}
	adm := interpretWorkload(workload(ns, "j", "u",
		[4]string{"QuotaReserved", "True", "QuotaReserved", ""}, [4]string{"Admitted", "True", "Admitted", ""}))
	if !adm.Admitted || !adm.QuotaReserved {
		t.Errorf("admitted = %+v", adm)
	}
	ev := interpretWorkload(workload(ns, "j", "u",
		[4]string{"Evicted", "True", "Preempted", "Preempted to accommodate a workload"},
		[4]string{"QuotaReserved", "False", "Pending", "waiting"}))
	if !ev.Evicted || ev.EvictedReason != "Preempted" {
		t.Errorf("evicted = %+v", ev)
	}
	fin := interpretWorkload(workload(ns, "j", "u", [4]string{"Finished", "True", "Succeeded", ""}))
	if !fin.Finished {
		t.Errorf("finished = %+v", fin)
	}
	// Evicted=False (re-admitted) is not an eviction.
	if interpretWorkload(workload(ns, "j", "u", [4]string{"Evicted", "False", "QuotaReserved", "Previously: Preempted"})).Evicted {
		t.Error("Evicted=False must not count")
	}
}

func TestKueue_WorkloadStateDrivesPhase(t *testing.T) {
	job := cpuJob("wf")
	job.Spec.QueueName = "q"
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2}
	r, c := kueueReconciler(job)
	reconcileN(t, r, "wf", 3)
	bj := getBatchJob(t, c, ns, "wf")
	setUID(t, c, bj, "wf-uid")

	// 1. No Workload yet: Queued, message says so.
	reconcileN(t, r, "wf", 2)
	got := getAIJob(t, c, "wf")
	if got.Status.Phase != PhaseQueued || !strings.Contains(got.Status.Message, "Waiting for Kueue") {
		t.Fatalf("no workload: phase=%s msg=%q", got.Status.Phase, got.Status.Message)
	}
	if !isKueueQueued(got) {
		t.Error("Queued set for Kueue must carry the KueueAdmitted condition")
	}

	// 2. Workload pending with Kueue's reason.
	wl := workload(ns, "wf", "wf-uid", [4]string{"QuotaReserved", "False", "Pending", "couldn't assign flavors to pod set main: insufficient quota for cpu"})
	if err := c.Create(context.Background(), wl); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "wf"}})
	if err != nil {
		t.Fatal(err)
	}
	got = getAIJob(t, c, "wf")
	if got.Status.Phase != PhaseQueued || !strings.Contains(got.Status.Message, "insufficient quota for cpu") {
		t.Fatalf("pending: phase=%s msg=%q", got.Status.Phase, got.Status.Message)
	}
	if res.RequeueAfter == 0 || res.RequeueAfter > 15e9 {
		t.Errorf("queued jobs are polled every 10s, got %v", res.RequeueAfter)
	}
	for _, cd := range got.Status.Conditions {
		if cd.Type == ConditionKueue && cd.Status != metav1.ConditionFalse {
			t.Errorf("KueueAdmitted = %s while queued", cd.Status)
		}
	}

	// 3. Kueue admits: it unsuspends the Job; the phase follows the Job again.
	bj = getBatchJob(t, c, ns, "wf")
	f := false
	bj.Spec.Suspend = &f
	if err := c.Update(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "wf", 1)
	got = getAIJob(t, c, "wf")
	if got.Status.Phase != PhaseScheduling || !conditionTrue(got, ConditionKueue) {
		t.Fatalf("admitted: phase=%s conds=%v", got.Status.Phase, got.Status.Conditions)
	}
	setJobStatus(t, c, "wf", func(b *batchv1.Job) {
		n := int32(2)
		b.Status.Ready = &n
	})
	reconcileN(t, r, "wf", 1)
	if p := getAIJob(t, c, "wf").Status.Phase; p != PhaseRunning {
		t.Fatalf("pods ready: phase=%s", p)
	}

	// 4. Preempted by Kueue: Job suspended again, Evicted=True. Requeued, not terminal.
	bj = getBatchJob(t, c, ns, "wf")
	tr := true
	bj.Spec.Suspend = &tr
	if err := c.Update(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
	setJobStatus(t, c, "wf", func(b *batchv1.Job) { n := int32(0); b.Status.Ready = &n })
	got2 := &unstructured.Unstructured{}
	got2.SetGroupVersionKind(KueueGVK("Workload"))
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: wl.GetName()}, got2); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(got2.Object, []interface{}{
		map[string]interface{}{"type": "Evicted", "status": "True", "reason": "Preempted", "message": "Preempted to accommodate a workload (UID: x) due to prioritization in the ClusterQueue"},
		map[string]interface{}{"type": "QuotaReserved", "status": "False", "reason": "Pending", "message": "waiting"},
	}, "status", "conditions")
	if err := c.Update(context.Background(), got2); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "wf", 1)
	got = getAIJob(t, c, "wf")
	if got.Status.Phase != PhaseQueued || !strings.Contains(got.Status.Message, "Evicted by Kueue (Preempted), requeued") {
		t.Fatalf("evicted: phase=%s msg=%q", got.Status.Phase, got.Status.Message)
	}
	// Same without a visible Evicted condition: admitted before, suspended now => Evicted.
	_ = unstructured.SetNestedSlice(got2.Object, []interface{}{
		map[string]interface{}{"type": "QuotaReserved", "status": "False", "reason": "Pending", "message": "waiting"},
	}, "status", "conditions")
	if err := c.Update(context.Background(), got2); err != nil {
		t.Fatal(err)
	}
	// (the AIJob condition is now False from the reconcile above: simulate the admitted-then-suspended order)
	prev := getAIJob(t, c, "wf")
	prev.Status.Conditions = append(prev.Status.Conditions[:0:0], prev.Status.Conditions...)
	for i := range prev.Status.Conditions {
		if prev.Status.Conditions[i].Type == ConditionKueue {
			prev.Status.Conditions[i].Status = metav1.ConditionTrue
		}
	}
	if err := c.Status().Update(context.Background(), prev); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "wf", 1)
	if m := getAIJob(t, c, "wf").Status.Message; !strings.HasPrefix(m, "Evicted by Kueue after admission, requeued") {
		t.Errorf("admitted-then-suspended message = %q", m)
	}
	// The Job is NOT deleted (Kueue requeues it) and the reconciler keeps working on it.
	if !exists(t, c, &batchv1.Job{}, "wf") {
		t.Error("an evicted Kueue Job must be kept for Kueue to requeue")
	}

	// 5. Re-admitted, then finished: terminal phase comes from the Job.
	bj = getBatchJob(t, c, ns, "wf")
	bj.Spec.Suspend = &f
	if err := c.Update(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
	setJobStatus(t, c, "wf", func(b *batchv1.Job) {
		b.Status.Conditions = []batchv1.JobCondition{condTrue(batchv1.JobComplete, "", "")}
	})
	reconcileN(t, r, "wf", 1)
	if p := getAIJob(t, c, "wf").Status.Phase; p != PhaseSucceeded {
		t.Fatalf("finished: phase=%s", p)
	}
}

func TestKueue_QueuedPhaseGate(t *testing.T) {
	// Queued by the quota operator (no KueueAdmitted condition): held, nothing created.
	held := cpuJob("held")
	r, c := kueueReconciler(held)
	reconcileN(t, r, "held", 1) // Pending
	setPhase(t, c, "held", PhaseQueued)
	reconcileN(t, r, "held", 2)
	if exists(t, c, &batchv1.Job{}, "held") {
		t.Fatal("a job held by the quota operator must not get a workload")
	}
}

func TestKueue_IntegrationOffKeepsPassthrough(t *testing.T) {
	// Off: no Workload lookups, phase stays Scheduling for a suspended labelled Job (W1 behaviour).
	job := cpuJob("off")
	job.Labels = map[string]string{LabelKueueQueue: "q"}
	r, c := newAIJobReconciler(job)
	reconcileN(t, r, "off", 4)
	if p := getAIJob(t, c, "off").Status.Phase; p != PhaseScheduling {
		t.Errorf("phase = %s, want Scheduling", p)
	}
}
