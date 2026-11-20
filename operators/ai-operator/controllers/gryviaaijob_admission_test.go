// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func gryviaGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: kind}
}

func gateScheme() *runtime.Scheme {
	s := newAIJobTestScheme()
	for _, k := range []string{"GryviaQuota", "GryviaBudget", "GryviaUsageRecord", "GryviaGpuSku", "GryviaTenant", "GryviaReservation"} {
		s.AddKnownTypeWithName(gryviaGVK(k), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gryviaGVK(k+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

func uobj(kind, name string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	u.SetGroupVersionKind(gryviaGVK(kind))
	u.SetName(name)
	return u
}

func gateReconciler(gate bool, wrap func(client.WithWatch) client.Client, objs ...client.Object) (*GryviaAIJobReconciler, client.Client, *record.FakeRecorder) {
	s := gateScheme()
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaAIJob{}, &appsv1.StatefulSet{}, &batchv1.Job{}).Build()
	var c client.Client = base
	if wrap != nil {
		c = wrap(base)
	}
	rec := record.NewFakeRecorder(20)
	return &GryviaAIJobReconciler{Client: c, Scheme: s, Log: ctrl.Log.WithName("test"), Recorder: rec, AdmissionGate: gate}, c, rec
}

func tightBudget() *unstructured.Unstructured {
	return uobj("GryviaQuota", "q", map[string]interface{}{
		"team": "a", "namespaces": []interface{}{ns},
		"budget": map[string]interface{}{"monthlyBudget": float64(5), "hardLimit": true},
	})
}

func noWorkload(t *testing.T, c client.Client, name string) {
	t.Helper()
	for n, o := range map[string]client.Object{
		name: &batchv1.Job{}, name + "-training": &appsv1.StatefulSet{},
		name + "-headless": &corev1.Service{}, name + "-data": &corev1.PersistentVolumeClaim{},
	} {
		if exists(t, c, o, n) {
			t.Errorf("%s was created for a rejected job", n)
		}
	}
}

func TestAdmissionGate_RejectsOverBudgetJobWithoutCreatingAnything(t *testing.T) {
	job := newTestAIJob("big", ns) // 4 x H100 x 1h x 8.00 = 32 > 5
	job.Spec.Storage = "std"
	r, c, rec := gateReconciler(true, nil, job, gpuNode("n1"), tightBudget())
	reconcileN(t, r, "big", 4)

	got := getAIJob(t, c, "big")
	if got.Status.Phase != PhaseRejected || !strings.Contains(got.Status.Message, "forecast 32.00") {
		t.Fatalf("status = %+v", got.Status)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, "Rejected")
	if cond == nil || cond.Reason != "BudgetExceeded" || cond.Status != "True" {
		t.Errorf("condition = %+v", cond)
	}
	if len(got.Status.NodesAllocated) != 0 {
		t.Errorf("a rejected job must not be scheduled: %+v", got.Status.NodesAllocated)
	}
	noWorkload(t, c, "big")
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "AdmissionRejected") {
			t.Errorf("event = %s", e)
		}
	default:
		t.Error("expected an AdmissionRejected event")
	}
}

func TestAdmissionGate_RejectedStaysRejectedAndIsNotReevaluated(t *testing.T) {
	job := newTestAIJob("big", ns)
	r, c, _ := gateReconciler(true, nil, job, gpuNode("n1"), tightBudget())
	reconcileN(t, r, "big", 3)
	if getAIJob(t, c, "big").Status.Phase != PhaseRejected {
		t.Fatal("setup")
	}
	// The budget is raised afterwards: the job stays Rejected (terminal and sticky).
	q := &unstructured.Unstructured{}
	q.SetGroupVersionKind(gryviaGVK("GryviaQuota"))
	if err := c.Get(context.Background(), client.ObjectKey{Name: "q"}, q); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(q.Object, float64(1e6), "spec", "budget", "monthlyBudget")
	if err := c.Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "big", 3)
	if getAIJob(t, c, "big").Status.Phase != PhaseRejected {
		t.Error("Rejected must be sticky")
	}
	noWorkload(t, c, "big")
}

func TestAdmissionGate_AffordableJobProceeds(t *testing.T) {
	job := newTestAIJob("ok", ns)
	q := uobj("GryviaQuota", "q", map[string]interface{}{
		"team": "a", "namespaces": []interface{}{ns},
		"budget": map[string]interface{}{"monthlyBudget": float64(1000), "hardLimit": true},
	})
	r, c, _ := gateReconciler(true, nil, job, gpuNode("n1"), q)
	reconcileN(t, r, "ok", 4)
	if got := getAIJob(t, c, "ok"); got.Status.Phase == PhaseRejected || got.Status.Phase == PhasePending {
		t.Errorf("phase = %s", got.Status.Phase)
	}
	if !exists(t, c, &batchv1.Job{}, "ok") {
		t.Error("the workload should exist")
	}
}

func TestAdmissionGate_OffByDefault(t *testing.T) {
	job := newTestAIJob("big", ns)
	r, c, _ := gateReconciler(false, nil, job, gpuNode("n1"), tightBudget())
	reconcileN(t, r, "big", 4)
	if got := getAIJob(t, c, "big"); got.Status.Phase == PhaseRejected {
		t.Error("the gate must do nothing unless enabled")
	}
	if !exists(t, c, &batchv1.Job{}, "big") {
		t.Error("workload expected with the gate off")
	}
}

func TestAdmissionGate_FailsOpenOnLookupError(t *testing.T) {
	job := newTestAIJob("j", ns)
	failing := func(c client.WithWatch) client.Client {
		return interceptor.NewClient(c, interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
				if u, ok := l.(*unstructured.UnstructuredList); ok && strings.HasPrefix(u.GroupVersionKind().Kind, "Gryvia") {
					return errors.New("forbidden: cannot list gryviaquotas")
				}
				return cl.List(ctx, l, opts...)
			},
		})
	}
	r, c, rec := gateReconciler(true, failing, job, gpuNode("n1"), tightBudget())
	reconcileN(t, r, "j", 4)
	got := getAIJob(t, c, "j")
	if got.Status.Phase == PhaseRejected {
		t.Fatal("must fail open")
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionAdmissionUnchecked); cond == nil || cond.Status != "True" {
		t.Errorf("AdmissionUnchecked condition missing: %+v", got.Status.Conditions)
	}
	if !exists(t, c, &batchv1.Job{}, "j") {
		t.Error("the workload should be created")
	}
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "LookupFailed") {
			t.Errorf("event = %s", e)
		}
	default:
		t.Error("expected a Warning event")
	}
}

func TestAdmissionGate_SoftBudgetWarnsOnce(t *testing.T) {
	job := newTestAIJob("soft", ns)
	soft := uobj("GryviaQuota", "q", map[string]interface{}{
		"team": "a", "namespaces": []interface{}{ns},
		"budget": map[string]interface{}{"monthlyBudget": float64(5), "hardLimit": false},
	})
	// No node: the job stays Pending, so the gate runs on every reconcile.
	r, c, rec := gateReconciler(true, nil, job, soft)
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(job)})
	}
	got := getAIJob(t, c, "soft")
	if got.Status.Phase == PhaseRejected {
		t.Fatal("a soft budget must not reject")
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionBudgetWarning); cond == nil || !strings.Contains(cond.Message, "soft limit") {
		t.Errorf("BudgetWarning = %+v", cond)
	}
	n := 0
	for len(rec.Events) > 0 {
		if e := <-rec.Events; strings.Contains(e, "BudgetWarning") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("warning events = %d, want 1 (deduplicated)", n)
	}
}

func TestAdmissionGate_QuotaGPULimitRejects(t *testing.T) {
	job := newTestAIJob("wide", ns)
	job.Spec.GPUs = 4
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, GpusPerNode: 4}
	q := uobj("GryviaQuota", "q", map[string]interface{}{
		"team": "a", "namespaces": []interface{}{ns},
		"gpuQuota": map[string]interface{}{"maxGPUs": int64(64), "maxGPUsPerJob": int64(8)},
	})
	r, c, _ := gateReconciler(true, nil, job, gpuNode("n1"), q)
	reconcileN(t, r, "wide", 3)
	got := getAIJob(t, c, "wide")
	if got.Status.Phase != PhaseRejected || !strings.Contains(got.Status.Message, "16 GPUs exceeds the per-job limit of 8") {
		t.Errorf("status = %+v", got.Status)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, "Rejected"); cond == nil || cond.Reason != "QuotaExceeded" {
		t.Errorf("condition = %+v", cond)
	}
	noWorkload(t, c, "wide")
}

func TestAdmissionGate_DoesNotReevaluateStartedJobs(t *testing.T) {
	// A job past Pending already got its workload: a budget that filled up since must not touch it.
	job := newTestAIJob("run", ns)
	r, c, _ := gateReconciler(true, nil, job, gpuNode("n1"))
	reconcileN(t, r, "run", 4)
	if got := getAIJob(t, c, "run"); got.Status.Phase == PhasePending || got.Status.Phase == PhaseRejected {
		t.Fatalf("setup: phase %s", got.Status.Phase)
	}
	if err := c.Create(context.Background(), tightBudget()); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "run", 3)
	if got := getAIJob(t, c, "run"); got.Status.Phase == PhaseRejected {
		t.Error("a started job must not be rejected by the gate")
	}
}
