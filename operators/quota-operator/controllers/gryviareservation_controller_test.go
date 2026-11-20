// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

var resNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func gpuNode(name, gpuType string, gpus int, ready bool) *corev1.Node {
	st := corev1.ConditionTrue
	if !ready {
		st = corev1.ConditionFalse
	}
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{LabelGPUType: gpuType}},
		Status: corev1.NodeStatus{
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}},
			Allocatable: corev1.ResourceList{GPUResource: *resource.NewQuantity(int64(gpus), resource.DecimalSI)},
		},
	}
	return n
}

func mkRes(name, owner, gpuType string, gpus int32) *gryviav1.GryviaReservation {
	return &gryviav1.GryviaReservation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{reservationFinalizer}},
		Spec: gryviav1.GryviaReservationSpec{
			Owner:     gryviav1.ReservationOwner{Type: "team", Name: owner},
			Resources: gryviav1.ReservationResources{GpuType: gpuType, GpuCount: gpus},
			Schedule:  gryviav1.ReservationSchedule{Type: "immediate"},
		},
	}
}

func resReconciler(objs ...client.Object) (*GryviaReservationReconciler, client.Client) {
	s := newQuotaTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaReservation{}, &gryviav1.GryviaAIJob{}).Build()
	return &GryviaReservationReconciler{Client: c, Scheme: s, Now: func() time.Time { return resNow }}, c
}

func reconcileRes(t *testing.T, r *GryviaReservationReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

func getNode(t *testing.T, c client.Client, name string) *corev1.Node {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, n); err != nil {
		t.Fatal(err)
	}
	return n
}

func getRes(t *testing.T, c client.Client, name string) *gryviav1.GryviaReservation {
	t.Helper()
	x := &gryviav1.GryviaReservation{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, x); err != nil {
		t.Fatal(err)
	}
	return x
}

func hasReservedTaint(n *corev1.Node, value string) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == ReservedTaintKey && t.Value == value && t.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

func TestReservation_ClaimsReadyMatchingNodesAndTaints(t *testing.T) {
	r, c := resReconciler(mkRes("r1", "ml-team", "H100", 16),
		gpuNode("n3", "H100", 8, true), gpuNode("n1", "H100", 8, true), gpuNode("n2", "H100", 8, true),
		gpuNode("down", "H100", 8, false), gpuNode("other", "A100", 8, true))
	reconcileRes(t, r, "r1")
	st := getRes(t, c, "r1").Status
	// Deterministic: lowest names first, Ready and matching only.
	if st.State != "active" || len(st.AllocatedNodes) != 2 || st.AllocatedNodes[0] != "n1" || st.AllocatedNodes[1] != "n2" || st.AllocatedGPUs != 16 {
		t.Fatalf("status = %+v", st)
	}
	for _, name := range []string{"n1", "n2"} {
		n := getNode(t, c, name)
		if !hasReservedTaint(n, "ml-team") || n.Labels[LabelReservedFor] != "r1" || n.Labels[LabelReservedBy] != "ml-team" {
			t.Errorf("%s not reserved: labels=%v taints=%v", name, n.Labels, n.Spec.Taints)
		}
	}
	for _, name := range []string{"n3", "down", "other"} {
		if n := getNode(t, c, name); len(n.Spec.Taints) != 0 || n.Labels[LabelReservedFor] != "" {
			t.Errorf("%s must be untouched: %v %v", name, n.Labels, n.Spec.Taints)
		}
	}
	// Idempotent: a second reconcile changes nothing.
	reconcileRes(t, r, "r1")
	if st2 := getRes(t, c, "r1").Status; len(st2.AllocatedNodes) != 2 {
		t.Errorf("second reconcile: %+v", st2)
	}
	if n := getNode(t, c, "n1"); len(n.Spec.Taints) != 1 {
		t.Errorf("taint duplicated: %v", n.Spec.Taints)
	}
}

func TestReservation_GPUCountFromAllocatableThenLabel(t *testing.T) {
	labelOnly := gpuNode("lbl", "H100", 0, true)
	labelOnly.Status.Allocatable = nil
	labelOnly.Labels[LabelGPUCount] = "4"
	r, c := resReconciler(mkRes("r1", "t", "H100", 12), gpuNode("a", "H100", 8, true), labelOnly)
	reconcileRes(t, r, "r1")
	st := getRes(t, c, "r1").Status
	if st.AllocatedGPUs != 12 || len(st.AllocatedNodes) != 2 {
		t.Errorf("8 (allocatable) + 4 (label) = 12: %+v", st)
	}
	// A node with no GPU information does not count as 8.
	bare := gpuNode("bare", "H100", 0, true)
	bare.Status.Allocatable = nil
	r, c = resReconciler(mkRes("r2", "t", "H100", 1), bare)
	reconcileRes(t, r, "r2")
	if st := getRes(t, c, "r2").Status; st.State != "pending" || len(st.AllocatedNodes) != 0 {
		t.Errorf("no GPU info must not be assumed: %+v", st)
	}
}

func TestReservation_InsufficientHoldsNothingAndExplains(t *testing.T) {
	r, c := resReconciler(mkRes("big", "t", "H100", 32), gpuNode("a", "H100", 8, true), gpuNode("b", "H100", 8, true))
	res := reconcileRes(t, r, "big")
	st := getRes(t, c, "big").Status
	if st.State != "pending" || len(st.AllocatedNodes) != 0 || st.Message == "" || res.RequeueAfter == 0 {
		t.Errorf("status = %+v requeue %v", st, res)
	}
	if n := getNode(t, c, "a"); len(n.Spec.Taints) != 0 {
		t.Error("an unsatisfiable reservation must not hoard nodes")
	}
}

func TestReservation_NoDoubleAllocationAcrossReservations(t *testing.T) {
	r, c := resReconciler(mkRes("first", "a", "H100", 8), mkRes("second", "b", "H100", 8),
		gpuNode("n1", "H100", 8, true), gpuNode("n2", "H100", 8, true))
	reconcileRes(t, r, "first")
	reconcileRes(t, r, "second")
	f, s := getRes(t, c, "first").Status, getRes(t, c, "second").Status
	if len(f.AllocatedNodes) != 1 || len(s.AllocatedNodes) != 1 || f.AllocatedNodes[0] == s.AllocatedNodes[0] {
		t.Fatalf("double allocation: %v vs %v", f.AllocatedNodes, s.AllocatedNodes)
	}
	// Third wants a node but none is free.
	r3 := mkRes("third", "c", "H100", 8)
	if err := c.Create(context.Background(), r3); err != nil {
		t.Fatal(err)
	}
	reconcileRes(t, r, "third")
	if st := getRes(t, c, "third").Status; len(st.AllocatedNodes) != 0 {
		t.Errorf("third must wait: %+v", st)
	}
}

func TestReservation_LostRaceRepicksAnotherNode(t *testing.T) {
	s := newQuotaTestScheme()
	rival := false
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(mkRes("mine", "a", "H100", 8), mkRes("rival", "b", "H100", 8), gpuNode("n1", "H100", 8, true), gpuNode("n2", "H100", 8, true)).
		WithStatusSubresource(&gryviav1.GryviaReservation{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				// A rival reservation claims n1 between our read and our write.
				if n, ok := obj.(*corev1.Node); ok && n.Name == "n1" && !rival {
					rival = true
					cur := &corev1.Node{}
					_ = cl.Get(ctx, types.NamespacedName{Name: "n1"}, cur)
					cur.Labels[LabelReservedFor] = "rival"
					if err := cl.Update(ctx, cur); err != nil {
						return err
					}
					return apierrors.NewConflict(corev1.Resource("nodes"), "n1", nil)
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &GryviaReservationReconciler{Client: c, Scheme: s, Now: func() time.Time { return resNow }}
	reconcileRes(t, r, "mine")
	st := getRes(t, c, "mine").Status
	if st.State != "active" || len(st.AllocatedNodes) != 1 || st.AllocatedNodes[0] != "n2" {
		t.Errorf("must fall back to n2: %+v", st)
	}
	if n := getNode(t, c, "n1"); n.Labels[LabelReservedFor] != "rival" || hasReservedTaint(n, "a") {
		t.Errorf("rival's node was stolen: %v %v", n.Labels, n.Spec.Taints)
	}
}

func TestReservation_ConflictOnPatchRetries(t *testing.T) {
	s := newQuotaTestScheme()
	fails := 2
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(mkRes("r", "a", "H100", 8), gpuNode("n1", "H100", 8, true)).
		WithStatusSubresource(&gryviav1.GryviaReservation{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*corev1.Node); ok && fails > 0 {
					fails--
					return apierrors.NewConflict(corev1.Resource("nodes"), "n1", nil)
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &GryviaReservationReconciler{Client: c, Scheme: s, Now: func() time.Time { return resNow }}
	reconcileRes(t, r, "r")
	if n := getNode(t, c, "n1"); !hasReservedTaint(n, "a") {
		t.Errorf("claim did not survive conflicts: %v", n.Spec.Taints)
	}
}

func TestReservation_ExplicitNodes(t *testing.T) {
	res := mkRes("r", "a", "", 0)
	res.Spec.Resources = gryviav1.ReservationResources{Nodes: []string{"w1", "w2"}}
	r, c := resReconciler(res, gpuNode("w1", "H100", 8, true), gpuNode("w2", "H100", 8, true), gpuNode("w3", "H100", 8, true))
	reconcileRes(t, r, "r")
	if st := getRes(t, c, "r").Status; len(st.AllocatedNodes) != 2 || st.AllocatedGPUs != 16 {
		t.Errorf("status = %+v", st)
	}
	// One listed node NotReady: nothing new is claimed.
	res2 := mkRes("r2", "a", "", 0)
	res2.Spec.Resources = gryviav1.ReservationResources{Nodes: []string{"x1", "x2"}}
	r, c = resReconciler(res2, gpuNode("x1", "H100", 8, true), gpuNode("x2", "H100", 8, false))
	reconcileRes(t, r, "r2")
	if st := getRes(t, c, "r2").Status; st.State != "pending" || st.Message == "" {
		t.Errorf("status = %+v", st)
	}
}

func TestReservation_ExpiryReleasesTaintAndLabels(t *testing.T) {
	res := mkRes("r", "a", "H100", 8)
	end := metav1.NewTime(resNow.Add(time.Hour))
	res.Spec.Schedule = gryviav1.ReservationSchedule{Type: "immediate", EndTime: &end}
	r, c := resReconciler(res, gpuNode("n1", "H100", 8, true))
	other := gpuNode("zkeep", "H100", 8, true)
	other.Spec.Taints = []corev1.Taint{{Key: "example.com/other", Effect: corev1.TaintEffectNoSchedule}}
	if err := c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	reconcileRes(t, r, "r")
	if !hasReservedTaint(getNode(t, c, "n1"), "a") {
		t.Fatal("not reserved")
	}
	// Time passes the end.
	r.Now = func() time.Time { return resNow.Add(2 * time.Hour) }
	reconcileRes(t, r, "r")
	n := getNode(t, c, "n1")
	if len(n.Spec.Taints) != 0 || n.Labels[LabelReservedFor] != "" || n.Labels[LabelReservedBy] != "" {
		t.Errorf("not released: %v %v", n.Labels, n.Spec.Taints)
	}
	st := getRes(t, c, "r").Status
	if st.State != "expired" || st.ActualEndTime == nil || len(st.AllocatedNodes) != 0 {
		t.Errorf("status = %+v", st)
	}
	// Terminal: a later reconcile does not re-claim.
	reconcileRes(t, r, "r")
	if len(getNode(t, c, "n1").Spec.Taints) != 0 {
		t.Error("expired reservation re-claimed a node")
	}
	if len(getNode(t, c, "zkeep").Spec.Taints) != 1 {
		t.Error("foreign taints must be preserved")
	}
}

func TestReservation_AutoExtendKeepsNodesWhileJobsRun(t *testing.T) {
	res := mkRes("r", "a", "H100", 8)
	end := metav1.NewTime(resNow.Add(-time.Minute))
	res.Spec.Schedule = gryviav1.ReservationSchedule{Type: "immediate", EndTime: &end, AutoExtend: true}
	n := gpuNode("n1", "H100", 8, true)
	n.Labels[LabelReservedFor] = "r"
	job := typedJob("j", "ml", "Running")
	job.Annotations = map[string]string{annotationReservation: "r"}
	r, c := resReconciler(res, n, job)
	reconcileRes(t, r, "r")
	if getRes(t, c, "r").Status.State == "expired" {
		t.Error("must not expire while a job runs")
	}
	cur := &gryviav1.GryviaAIJob{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ml", Name: "j"}, cur); err != nil {
		t.Fatal(err)
	}
	cur.Status.Phase = "Succeeded"
	if err := c.Status().Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	reconcileRes(t, r, "r")
	if getRes(t, c, "r").Status.State != "expired" {
		t.Error("must expire once the job is done")
	}
}

func TestReservation_ScheduledWindowAndRecurring(t *testing.T) {
	start := metav1.NewTime(resNow.Add(time.Hour))
	res := mkRes("s", "a", "H100", 8)
	res.Spec.Schedule = gryviav1.ReservationSchedule{Type: "scheduled", StartTime: &start}
	r, c := resReconciler(res, gpuNode("n1", "H100", 8, true))
	out := reconcileRes(t, r, "s")
	if st := getRes(t, c, "s").Status; st.State != "pending" || len(getNode(t, c, "n1").Spec.Taints) != 0 {
		t.Errorf("before start: %+v", st)
	}
	if out.RequeueAfter < 59*time.Minute || out.RequeueAfter > time.Minute+time.Hour {
		// heartbeat caps at one minute
		if out.RequeueAfter != reservationHeartbeat {
			t.Errorf("requeue = %v", out.RequeueAfter)
		}
	}
	r.Now = func() time.Time { return resNow.Add(2 * time.Hour) }
	reconcileRes(t, r, "s")
	if st := getRes(t, c, "s").Status; st.State != "active" || !hasReservedTaint(getNode(t, c, "n1"), "a") {
		t.Errorf("after start: %+v", st)
	}

	// Recurring: 22:00 UTC daily for 8h.
	rec := mkRes("nightly", "a", "H100", 8)
	rec.Spec.Schedule = gryviav1.ReservationSchedule{Type: "recurring", Recurrence: &gryviav1.ReservationRecurrence{Cron: "0 22 * * *", Duration: "8h"}}
	r, c = resReconciler(rec, gpuNode("n1", "H100", 8, true))
	r.Now = func() time.Time { return time.Date(2026, 9, 15, 23, 30, 0, 0, time.UTC) } // inside the window
	reconcileRes(t, r, "nightly")
	if st := getRes(t, c, "nightly").Status; st.State != "active" || !hasReservedTaint(getNode(t, c, "n1"), "a") {
		t.Errorf("inside window: %+v", st)
	}
	r.Now = func() time.Time { return time.Date(2026, 9, 16, 6, 0, 0, 0, time.UTC) } // window closed at 06:00
	reconcileRes(t, r, "nightly")
	if st := getRes(t, c, "nightly").Status; st.State != "pending" || len(getNode(t, c, "n1").Spec.Taints) != 0 {
		t.Errorf("outside window must release: %+v %v", st, getNode(t, c, "n1").Spec.Taints)
	}
	r.Now = func() time.Time { return time.Date(2026, 9, 16, 22, 5, 0, 0, time.UTC) }
	reconcileRes(t, r, "nightly")
	if st := getRes(t, c, "nightly").Status; st.State != "active" {
		t.Errorf("next window: %+v", st)
	}
	// Recurrence ends at endTime.
	end := metav1.NewTime(time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC))
	cur := getRes(t, c, "nightly")
	cur.Spec.Schedule.EndTime = &end
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	r.Now = func() time.Time { return time.Date(2026, 9, 17, 0, 1, 0, 0, time.UTC) }
	reconcileRes(t, r, "nightly")
	if st := getRes(t, c, "nightly").Status; st.State != "expired" || len(getNode(t, c, "n1").Spec.Taints) != 0 {
		t.Errorf("after endTime: %+v", st)
	}
}

func TestReservation_InvalidSchedules(t *testing.T) {
	for name, sch := range map[string]gryviav1.ReservationSchedule{
		"bad cron":    {Type: "recurring", Recurrence: &gryviav1.ReservationRecurrence{Cron: "nope", Duration: "1h"}},
		"bad dur":     {Type: "recurring", Recurrence: &gryviav1.ReservationRecurrence{Cron: "* * * * *", Duration: "soon"}},
		"no start":    {Type: "scheduled"},
		"no recur":    {Type: "recurring"},
		"unknown typ": {Type: "whenever"},
	} {
		res := mkRes("r", "a", "H100", 8)
		res.Spec.Schedule = sch
		r, c := resReconciler(res, gpuNode("n1", "H100", 8, true))
		reconcileRes(t, r, "r")
		st := getRes(t, c, "r").Status
		if st.State != "pending" || st.Message == "" || len(getNode(t, c, "n1").Spec.Taints) != 0 {
			t.Errorf("%s: %+v", name, st)
		}
	}
}

func TestReservation_DeletionRemovesTaintAndLabelsAndFinalizer(t *testing.T) {
	r, c := resReconciler(mkRes("r", "a", "H100", 8), gpuNode("n1", "H100", 8, true))
	reconcileRes(t, r, "r")
	if !hasReservedTaint(getNode(t, c, "n1"), "a") {
		t.Fatal("not reserved")
	}
	if err := c.Delete(context.Background(), getRes(t, c, "r")); err != nil {
		t.Fatal(err)
	}
	reconcileRes(t, r, "r")
	n := getNode(t, c, "n1")
	if len(n.Spec.Taints) != 0 || n.Labels[LabelReservedFor] != "" {
		t.Errorf("not cleaned: %v %v", n.Labels, n.Spec.Taints)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "r"}, &gryviav1.GryviaReservation{}); !apierrors.IsNotFound(err) {
		t.Errorf("reservation should be gone: %v", err)
	}
}

func TestReservation_OrphanedNodeIsSwept(t *testing.T) {
	stale := gpuNode("stale", "H100", 8, true)
	stale.Labels[LabelReservedFor] = "ghost"
	stale.Spec.Taints = []corev1.Taint{{Key: ReservedTaintKey, Value: "x", Effect: corev1.TaintEffectNoSchedule}}
	r, c := resReconciler(mkRes("live", "a", "H100", 8), stale, gpuNode("n1", "H100", 8, true))
	reconcileRes(t, r, "live")
	if n := getNode(t, c, "stale"); n.Labels[LabelReservedFor] != "" || len(n.Spec.Taints) != 0 {
		t.Errorf("orphan not swept: %v %v", n.Labels, n.Spec.Taints)
	}
}

func TestReservation_NotReadyNodeIsReplaced(t *testing.T) {
	r, c := resReconciler(mkRes("r", "a", "H100", 8), gpuNode("n1", "H100", 8, true), gpuNode("n2", "H100", 8, true))
	reconcileRes(t, r, "r")
	if got := getRes(t, c, "r").Status.AllocatedNodes; len(got) != 1 || got[0] != "n1" {
		t.Fatalf("initial = %v", got)
	}
	n := getNode(t, c, "n1")
	n.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := c.Status().Update(context.Background(), n); err != nil {
		t.Skipf("fake client cannot update node status here: %v", err)
	}
	reconcileRes(t, r, "r")
	if got := getRes(t, c, "r").Status.AllocatedNodes; len(got) != 1 || got[0] != "n2" {
		t.Errorf("after n1 went NotReady = %v", got)
	}
	if len(getNode(t, c, "n1").Spec.Taints) != 0 {
		t.Error("the failed node must be released")
	}
}

func TestReservationOwnerValue(t *testing.T) {
	cases := map[string]string{
		"ml-team":           "ml-team",
		"team/a b":          "team-a-b",
		"--x--":             "x",
		"Alice@example.com": "Alice-example.com",
		"":                  "",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	for in, want := range cases {
		if got := ReservationOwnerValue(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestParseWindowDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"8h": 8 * time.Hour, "90m": 90 * time.Minute, "2d": 48 * time.Hour, "1d12h": 36 * time.Hour} {
		if got, err := parseWindowDuration(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0h", "-1h", "xd", "soon"} {
		if _, err := parseWindowDuration(in); err == nil {
			t.Errorf("%q should fail", in)
		}
	}
}
