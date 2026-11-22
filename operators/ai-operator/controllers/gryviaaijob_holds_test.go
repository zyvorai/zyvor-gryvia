// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/scheduler"
)

func holdNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"gryvia.io/gpu-count": "8"}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

// a distributed job that needs two whole 8-GPU nodes
func holdJob(name string) *gryviav1.GryviaAIJob {
	j := newTestAIJob(name, "default")
	j.Spec.GPUs = 16
	j.Spec.GpuType = ""
	j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Framework: "pytorch", Nodes: 2, GpusPerNode: 8}
	return j
}

// reconcileTwice runs the init pass and the placement pass; a failed placement returns an error by design.
func reconcileTwice(t *testing.T, r *GryviaAIJobReconciler, name string) {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}}
	for i := 0; i < 2; i++ {
		_, _ = r.Reconcile(context.Background(), req)
	}
}

func scheduled(t *testing.T, r *GryviaAIJobReconciler, name string) (ok bool, reason string, nodes int) {
	t.Helper()
	job := &gryviav1.GryviaAIJob{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, job); err != nil {
		t.Fatal(err)
	}
	for _, c := range job.Status.Conditions {
		if c.Type == ConditionScheduled {
			return c.Status == metav1.ConditionTrue, c.Reason, len(job.Status.NodesAllocated)
		}
	}
	return false, "", len(job.Status.NodesAllocated)
}

// Off (the default), two jobs placed back to back are both told the same two nodes are free: the race.
func TestPlacementWithoutHoldsLetsTwoJobsTakeTheSameNodes(t *testing.T) {
	r, _ := newAIJobReconciler(holdNode("n1"), holdNode("n2"), holdJob("a"), holdJob("b"))
	reconcileTwice(t, r, "a")
	reconcileTwice(t, r, "b")
	for _, n := range []string{"a", "b"} {
		if ok, reason, nodes := scheduled(t, r, n); !ok || nodes != 2 {
			t.Errorf("job %s: scheduled=%v reason=%q nodes=%d, want both jobs placed on the same two nodes (the unreserved race)", n, ok, reason, nodes)
		}
	}
}

// On, the second job waits (Pending, SchedulingFailed) while the first holds the nodes, and is placed once the
// first is deleted.
func TestPlacementHoldsHoldNodesUntilTheJobIsGone(t *testing.T) {
	r, c := newAIJobReconciler(holdNode("n1"), holdNode("n2"), holdJob("a"), holdJob("b"))
	r.PlacementHolds = true

	reconcileTwice(t, r, "a")
	if ok, reason, nodes := scheduled(t, r, "a"); !ok || nodes != 2 {
		t.Fatalf("job a: scheduled=%v reason=%q nodes=%d, want placed on 2 nodes", ok, reason, nodes)
	}
	reconcileTwice(t, r, "b")
	if ok, reason, _ := scheduled(t, r, "b"); ok || reason != "SchedulingFailed" {
		t.Fatalf("job b: scheduled=%v reason=%q, want held back with SchedulingFailed while a holds the nodes", ok, reason)
	}

	// a is deleted: its hold is released on the NotFound reconcile and b can be placed.
	a := &gryviav1.GryviaAIJob{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "a", Namespace: "default"}, a); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a", Namespace: "default"}})
	reconcileTwice(t, r, "b")
	if ok, reason, nodes := scheduled(t, r, "b"); !ok || nodes != 2 {
		t.Errorf("job b after a's release: scheduled=%v reason=%q nodes=%d, want placed on 2 nodes", ok, reason, nodes)
	}
}

// A job that ends releases its hold, so a finished or failed job never keeps GPUs reserved.
func TestPlacementHoldsReleaseWhenTheJobEnds(t *testing.T) {
	r, c := newAIJobReconciler(holdNode("n1"), holdNode("n2"), holdJob("a"), holdJob("b"))
	r.PlacementHolds = true
	reconcileTwice(t, r, "a")

	a := &gryviav1.GryviaAIJob{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "a", Namespace: "default"}, a); err != nil {
		t.Fatal(err)
	}
	a.Status.Phase = PhaseSucceeded
	if err := c.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a", Namespace: "default"}})

	reconcileTwice(t, r, "b")
	if ok, reason, nodes := scheduled(t, r, "b"); !ok || nodes != 2 {
		t.Errorf("job b after a succeeded: scheduled=%v reason=%q nodes=%d, want placed", ok, reason, nodes)
	}
}

// Releasing when all replicas are ready: the pods now count as used GPUs themselves.
func TestReleaseIfSettledOnAllReplicasReady(t *testing.T) {
	r, _ := newAIJobReconciler()
	r.PlacementHolds = true
	job := holdJob("a")
	r.holdPlacement(job, []string{"n1", "n2"})
	if got := r.placementHeld(holdJob("other")); got["n1"] != 8 {
		t.Fatalf("setup: held = %v, want 8 on n1", got)
	}

	job.Status.ReplicasReady = 1 // one of two pods up: still held
	r.releaseIfSettled(job)
	if got := r.placementHeld(holdJob("other")); got["n1"] != 8 {
		t.Errorf("released with 1 of 2 replicas ready: held = %v", got)
	}
	job.Status.ReplicasReady = 2
	r.releaseIfSettled(job)
	if got := r.placementHeld(holdJob("other")); len(got) != 0 {
		t.Errorf("not released with all replicas ready: held = %v", got)
	}
}

// Off, none of the helpers hold or release anything.
func TestHoldHelpersAreInertWhenOff(t *testing.T) {
	r, _ := newAIJobReconciler()
	job := holdJob("a")
	r.holdPlacement(job, []string{"n1"})
	if got := r.placementHeld(holdJob("other")); got != nil {
		t.Errorf("held = %v with holds off, want nil", got)
	}
}

func TestBuildAffinityPinsOnlyWhenAnnotated(t *testing.T) {
	r := &GryviaAIJobReconciler{}
	job := &gryviav1.GryviaAIJob{Status: gryviav1.GryviaAIJobStatus{NodesAllocated: []string{"n1"}}}
	if aff := r.buildAffinity(job); aff != nil {
		t.Fatalf("unannotated job got affinity: %+v", aff)
	}
	job.Annotations = map[string]string{scheduler.AnnotationPinPlacement: "true"}
	aff := r.buildAffinity(job)
	if aff == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields[0].Values[0] != "n1" {
		t.Fatalf("annotated job not pinned: %+v", aff)
	}
}
