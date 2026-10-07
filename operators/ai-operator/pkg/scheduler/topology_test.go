package scheduler

import (
	"context"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func topoNode(name, block, rack string) *corev1.Node {
	n := fabNode(name, 8)
	if block != "" {
		n.Labels[LabelIBBlock] = block
	}
	if rack != "" {
		n.Labels[LabelRack] = rack
	}
	return n
}

func TestGroupPlacement(t *testing.T) {
	nodes := []corev1.Node{
		*topoNode("a1", "b1", "r1"), *topoNode("a2", "b1", "r1"), *topoNode("a3", "b1", "r2"), *topoNode("a4", "b1", "r2"),
		*topoNode("c1", "b2", "r3"), *topoNode("c2", "b2", "r3"),
		*topoNode("d1", "", "r4"), *topoNode("d2", "", "r4"), *topoNode("d3", "", "r4"),
	}
	scored := []NodeScore{{"a1", 90}, {"c1", 80}, {"d1", 70}, {"a2", 60}, {"c2", 50}, {"a3", 40}, {"d2", 30}, {"a4", 20}, {"d3", 10}}

	names := func(p []NodeScore) string {
		var out []string
		for _, s := range p {
			out = append(out, s.NodeName)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		n         int
		want, key string
		value     string
		ok        bool
	}{
		{2, "c1,c2", LabelIBBlock, "b2", true},    // smallest block that fits, not the best-scored node's block
		{3, "a1,a2,a3", LabelIBBlock, "b1", true}, // only b1 holds 3
		{5, "", "", "", false},                    // no block or rack holds 5
		{1, "", "", "", false},                    // single node: nothing to group
	} {
		picked, key, value, ok := groupPlacement(scored, nodes, tc.n)
		if ok != tc.ok || names(picked) != tc.want || key != tc.key || value != tc.value {
			t.Errorf("n=%d: got %s %s=%s ok=%v", tc.n, names(picked), key, value, ok)
		}
	}

	// No block fits 3 without b1; racks are tried next.
	noB1 := []corev1.Node{*topoNode("c1", "b2", "r3"), *topoNode("c2", "b2", "r3"), *topoNode("d1", "", "r4"), *topoNode("d2", "", "r4"), *topoNode("d3", "", "r4")}
	picked, key, value, ok := groupPlacement([]NodeScore{{"c1", 9}, {"d1", 8}, {"c2", 7}, {"d2", 6}, {"d3", 5}}, noB1, 3)
	if !ok || key != LabelRack || value != "r4" || names(picked) != "d1,d2,d3" {
		t.Errorf("rack fallback: %s %s=%s ok=%v", names(picked), key, value, ok)
	}
}

func topoClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = gryviav1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func TestFindOptimalNodesTopology(t *testing.T) {
	c := topoClient(topoNode("a1", "b1", ""), topoNode("a2", "b1", ""), topoNode("a3", "b1", ""), topoNode("c1", "b2", ""), topoNode("c2", "b2", ""))
	job := &gryviav1.GryviaAIJob{Spec: gryviav1.GryviaAIJobSpec{GPUs: 8, Distributed: &gryviav1.DistributedConfig{Enabled: true, Nodes: 2, GpusPerNode: 8}}}

	nodes, err := FindOptimalNodes(WithTopologyPlacement(context.Background()), c, job)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(nodes)
	if strings.Join(nodes, ",") != "c1,c2" {
		t.Fatalf("2 nodes must come from the smallest fitting block b2, got %v", nodes)
	}
	if got := SharedTopology(context.Background(), c, nodes); got != "gryvia.io/ib-block=b2" {
		t.Fatalf("shared topology %q", got)
	}
	if got := SharedTopology(context.Background(), c, []string{"a1", "c1"}); got != "" {
		t.Fatalf("nodes in different blocks share nothing, got %q", got)
	}

	job.Spec.Distributed.Nodes = 4
	if nodes, err = FindOptimalNodes(WithTopologyPlacement(context.Background()), c, job); err != nil || len(nodes) != 4 {
		t.Fatalf("no group fits 4: the plain ranking must still place the job, got %v %v", nodes, err)
	}

	job.Spec.Distributed.Nodes = 2
	job.Annotations = map[string]string{AnnotationTopologyPlacement: "false"}
	if _, err := FindOptimalNodes(WithTopologyPlacement(context.Background()), c, job); err != nil {
		t.Fatal(err)
	}
}
