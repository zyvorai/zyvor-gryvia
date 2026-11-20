// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

var holdT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestGPUHoldsHeldSumsOtherJobsAndSkipsOwn(t *testing.T) {
	r := NewGPUHolds()
	r.Hold("ns/a", []string{"n1", "n2"}, 8, time.Minute, holdT0)
	r.Hold("ns/b", []string{"n2"}, 4, time.Minute, holdT0)

	got := r.Held("ns/c", holdT0)
	if got["n1"] != 8 || got["n2"] != 12 {
		t.Errorf("Held for another job = %v, want n1=8 n2=12", got)
	}
	own := r.Held("ns/a", holdT0)
	if own["n1"] != 0 || own["n2"] != 4 {
		t.Errorf("a job must not see its own hold: %v, want only b's 4 on n2", own)
	}
}

func TestGPUHoldsExpireAndRelease(t *testing.T) {
	r := NewGPUHolds()
	r.Hold("ns/a", []string{"n1"}, 8, time.Minute, holdT0)
	if got := r.Held("ns/b", holdT0.Add(59*time.Second))["n1"]; got != 8 {
		t.Errorf("before expiry Held = %d, want 8", got)
	}
	if got := r.Held("ns/b", holdT0.Add(time.Minute))["n1"]; got != 0 {
		t.Errorf("a hold must not outlive its ttl, Held = %d", got)
	}
	if r.Len(holdT0.Add(time.Minute)) != 0 {
		t.Error("an expired hold must be dropped")
	}

	r.Hold("ns/a", []string{"n1"}, 8, time.Hour, holdT0)
	r.Release("ns/a")
	if got := r.Held("ns/b", holdT0)["n1"]; got != 0 {
		t.Errorf("after Release Held = %d, want 0", got)
	}
}

func TestGPUHoldsHoldReplacesAndIgnoresEmpty(t *testing.T) {
	r := NewGPUHolds()
	r.Hold("ns/a", []string{"n1"}, 8, time.Hour, holdT0)
	r.Hold("ns/a", []string{"n2"}, 2, time.Hour, holdT0) // re-placement: replaces, never adds
	got := r.Held("ns/b", holdT0)
	if got["n1"] != 0 || got["n2"] != 2 {
		t.Errorf("a re-placement must replace the old hold: %v", got)
	}
	r.Hold("ns/a", nil, 8, time.Hour, holdT0)
	r.Hold("ns/c", []string{"n1"}, 0, time.Hour, holdT0)
	if r.Len(holdT0) != 0 {
		t.Errorf("empty or zero-GPU holds must not be stored, Len = %d", r.Len(holdT0))
	}
}

func heldNode(name string, gpus string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"gryvia.io/gpu-count": gpus}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func distributedJob(name string, nodes, gpusPerNode int32) *gryviav1.GryviaAIJob {
	return &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: gryviav1.GryviaAIJobSpec{
			GPUs:        nodes * gpusPerNode,
			Distributed: &gryviav1.DistributedConfig{Enabled: true, Nodes: nodes, GpusPerNode: gpusPerNode},
		},
	}
}

func heldClient(t *testing.T, nodes ...*corev1.Node) *fake.ClientBuilder {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = gryviav1.AddToScheme(s)
	b := fake.NewClientBuilder().WithScheme(s)
	for _, n := range nodes {
		b = b.WithObjects(n)
	}
	return b
}

// Held GPUs count as used by the placement, so a job needing 8 GPUs on each of two nodes no longer fits
// when another job holds a node, and fits again once the hold is gone.
func TestFindOptimalNodesHeldCountsHeldGPUsAsUsed(t *testing.T) {
	c := heldClient(t, heldNode("n1", "8"), heldNode("n2", "8")).Build()
	ctx := context.Background()
	job := distributedJob("b", 2, 8)

	if nodes, err := FindOptimalNodesHeld(ctx, c, job, nil); err != nil || len(nodes) != 2 {
		t.Fatalf("no holds: nodes=%v err=%v, want both nodes", nodes, err)
	}
	if _, err := FindOptimalNodesHeld(ctx, c, job, map[string]int64{"n1": 8}); err == nil || !strings.Contains(err.Error(), "not enough nodes") {
		t.Errorf("a fully held node must not count as free: err=%v", err)
	}
	if _, err := FindOptimalNodesHeld(ctx, c, job, map[string]int64{"n1": 4}); err == nil {
		t.Error("a node with only 4 of 8 GPUs free cannot host an 8-GPU member")
	}
	small := distributedJob("s", 2, 4)
	if nodes, err := FindOptimalNodesHeld(ctx, c, small, map[string]int64{"n1": 4}); err != nil || len(nodes) != 2 {
		t.Errorf("4 GPUs per member still fit beside a 4-GPU hold: nodes=%v err=%v", nodes, err)
	}
}

// The race the hold closes: two jobs placed in the same window must not get the same GPUs.
func TestTwoJobsCannotBothTakeTheSameFreeGPUs(t *testing.T) {
	c := heldClient(t, heldNode("n1", "8"), heldNode("n2", "8")).Build()
	ctx := context.Background()
	res := NewGPUHolds()
	a, b := distributedJob("a", 2, 8), distributedJob("b", 2, 8)
	keyA, keyB := "ns/a", "ns/b"

	// Without holds both are told the same two nodes are free.
	na, _ := FindOptimalNodes(ctx, c, a)
	nb, _ := FindOptimalNodes(ctx, c, b)
	if len(na) != 2 || len(nb) != 2 {
		t.Fatalf("baseline: both jobs see free nodes (the race), got %v and %v", na, nb)
	}

	// With them, the second job is refused until the first is settled.
	nodesA, err := FindOptimalNodesHeld(ctx, c, a, res.Held(keyA, holdT0))
	if err != nil {
		t.Fatal(err)
	}
	res.Hold(keyA, nodesA, int64(GPUsPerPlacedNode(a)), time.Minute, holdT0)
	if _, err := FindOptimalNodesHeld(ctx, c, b, res.Held(keyB, holdT0)); err == nil {
		t.Fatal("job b was placed on GPUs job a reserved")
	}
	res.Release(keyA)
	if nodes, err := FindOptimalNodesHeld(ctx, c, b, res.Held(keyB, holdT0)); err != nil || len(nodes) != 2 {
		t.Errorf("after a's release b must fit: nodes=%v err=%v", nodes, err)
	}
}

func TestGPUsPerPlacedNodeMatchesThePlacement(t *testing.T) {
	if got := GPUsPerPlacedNode(distributedJob("d", 2, 8)); got != 8 {
		t.Errorf("distributed with GpusPerNode: %d, want 8", got)
	}
	single := &gryviav1.GryviaAIJob{Spec: gryviav1.GryviaAIJobSpec{GPUs: 4}}
	if got := GPUsPerPlacedNode(single); got != 4 {
		t.Errorf("single-node job: %d, want spec.gpus (4)", got)
	}
	noPer := &gryviav1.GryviaAIJob{Spec: gryviav1.GryviaAIJobSpec{GPUs: 2, Distributed: &gryviav1.DistributedConfig{Enabled: true, Nodes: 2}}}
	if got := GPUsPerPlacedNode(noPer); got != 2 {
		t.Errorf("distributed without GpusPerNode: %d, want spec.gpus (2), as findOptimalNodes always did", got)
	}
}

// Elastic placement starts as soon as minNodes qualify and takes as many as are free up to maxNodes.
func TestElasticPlacementTakesBetweenMinAndMax(t *testing.T) {
	ctx := context.Background()
	job := distributedJob("el", 4, 8)
	job.Spec.Distributed.Elastic = &gryviav1.ElasticConfig{MinNodes: 2}

	three := heldClient(t, heldNode("n1", "8"), heldNode("n2", "8"), heldNode("n3", "8")).Build()
	if nodes, err := FindOptimalNodes(ctx, three, job); err != nil || len(nodes) != 3 {
		t.Errorf("3 free nodes: got %v err=%v, want 3 (between min 2 and max 4)", nodes, err)
	}
	two := heldClient(t, heldNode("n1", "8"), heldNode("n2", "8")).Build()
	if nodes, err := FindOptimalNodes(ctx, two, job); err != nil || len(nodes) != 2 {
		t.Errorf("2 free nodes: got %v err=%v, want 2 (the minimum)", nodes, err)
	}
	one := heldClient(t, heldNode("n1", "8")).Build()
	if _, err := FindOptimalNodes(ctx, one, job); err == nil {
		t.Error("1 node is below minNodes and must not place")
	}
	five := heldClient(t, heldNode("n1", "8"), heldNode("n2", "8"), heldNode("n3", "8"), heldNode("n4", "8"), heldNode("n5", "8")).Build()
	if nodes, _ := FindOptimalNodes(ctx, five, job); len(nodes) != 4 {
		t.Errorf("5 free nodes: got %d, want the maximum 4", len(nodes))
	}
	fixed := distributedJob("fx", 4, 8)
	if _, err := FindOptimalNodes(ctx, three, fixed); err == nil {
		t.Error("a non-elastic job needing 4 nodes must still fail with 3")
	}
}
