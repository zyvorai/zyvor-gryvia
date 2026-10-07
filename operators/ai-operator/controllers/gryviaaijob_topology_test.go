package controllers

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/scheduler"
)

func blockNode(name, block string) *corev1.Node {
	n := gpuNode(name)
	n.Labels[scheduler.LabelIBBlock] = block
	return n
}

func topologyJob(name string) *gryviav1.GryviaAIJob {
	j := newTestAIJob(name, ns)
	j.Spec.GPUs = 8
	j.Spec.WorkloadKind = gryviav1.WorkloadKindJob
	j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2, GpusPerNode: 8}
	return j
}

func TestAIJobTopologyPlacement(t *testing.T) {
	r, c := newAIJobReconciler(topologyJob("train"), blockNode("a1", "b1"), blockNode("a2", "b1"), blockNode("a3", "b1"),
		blockNode("c1", "b2"), blockNode("c2", "b2"))
	r.TopologyPlacement = true
	reconcileN(t, r, "train", 3)

	job := getAIJob(t, c, "train")
	if job.Status.PlacementTopology != "gryvia.io/ib-block=b2" {
		t.Fatalf("placement topology %q (nodes %v)", job.Status.PlacementTopology, job.Status.NodesAllocated)
	}
	for _, cond := range job.Status.Conditions {
		if cond.Type == ConditionScheduled && !strings.Contains(cond.Message, "within gryvia.io/ib-block=b2") {
			t.Errorf("scheduled condition %q", cond.Message)
		}
	}
	bj := &batchv1.Job{}
	if !exists(t, c, bj, "train") {
		t.Fatal("workload not created")
	}
	var found bool
	for _, term := range bj.Spec.Template.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		e := term.Preference.MatchExpressions
		if len(e) == 1 && e[0].Key == scheduler.LabelIBBlock && e[0].Values[0] == "b2" && term.Weight == 80 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no preferred affinity toward b2: %+v", bj.Spec.Template.Spec.Affinity)
	}
}

func TestAIJobTopologyPlacementOff(t *testing.T) {
	r, c := newAIJobReconciler(topologyJob("train"), blockNode("a1", "b1"), blockNode("c1", "b2"), blockNode("c2", "b2"))
	reconcileN(t, r, "train", 3)
	job := getAIJob(t, c, "train")
	if job.Status.PlacementTopology != "" {
		t.Fatalf("topology placement is opt-in, got %q", job.Status.PlacementTopology)
	}
	bj := &batchv1.Job{}
	if !exists(t, c, bj, "train") {
		t.Fatal("workload not created")
	}
	if a := bj.Spec.Template.Spec.Affinity; a != nil && a.NodeAffinity != nil {
		for _, term := range a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			if term.Preference.MatchExpressions[0].Key == scheduler.LabelIBBlock {
				t.Fatal("no topology affinity without the flag")
			}
		}
	}
}
