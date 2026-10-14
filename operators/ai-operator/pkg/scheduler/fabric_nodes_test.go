package scheduler

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestNodeSignalFresh(t *testing.T) {
	cases := []struct {
		name string
		sig  NodeSignal
		want bool
	}{
		{"fresh default ttl", NodeSignal{Node: "a", MeasuredAt: t0.Add(-4 * time.Minute)}, true},
		{"exactly at ttl", NodeSignal{Node: "a", MeasuredAt: t0.Add(-5 * time.Minute)}, true},
		{"stale default ttl", NodeSignal{Node: "a", MeasuredAt: t0.Add(-5*time.Minute - time.Second)}, false},
		{"custom ttl expired", NodeSignal{Node: "a", MeasuredAt: t0.Add(-40 * time.Second), TTL: 30 * time.Second}, false},
		{"custom ttl fresh", NodeSignal{Node: "a", MeasuredAt: t0.Add(-20 * time.Second), TTL: 30 * time.Second}, true},
		{"no measuredAt", NodeSignal{Node: "a"}, false},
		{"future beyond skew", NodeSignal{Node: "a", MeasuredAt: t0.Add(10 * time.Minute)}, false},
		{"small future skew ok", NodeSignal{Node: "a", MeasuredAt: t0.Add(30 * time.Second)}, true},
		{"no node", NodeSignal{MeasuredAt: t0}, false},
	}
	for _, c := range cases {
		if got := c.sig.Fresh(t0); got != c.want {
			t.Errorf("%s: Fresh = %v, want %v", c.name, got, c.want)
		}
	}
}

func sig(node string, delta float64, age time.Duration, reasons ...string) NodeSignal {
	return NodeSignal{Node: node, ScoreDelta: delta, MeasuredAt: t0.Add(-age), Reasons: reasons}
}

func TestRankWithFabric(t *testing.T) {
	base := []NodeScore{{"a", 100}, {"b", 100}, {"c", 90}, {"d", 40}}
	cases := []struct {
		name     string
		sigs     []NodeSignal
		max      float64
		order    []string
		penalty  map[string]int32
		wantNone bool
	}{
		{name: "no signals: identical to plain ranking", order: []string{"a", "b", "c", "d"}, wantNone: true},
		{name: "stale signal ignored", sigs: []NodeSignal{sig("a", 1, 10*time.Minute, "rdma")},
			order: []string{"a", "b", "c", "d"}, wantNone: true},
		{name: "zero delta is not a penalty", sigs: []NodeSignal{sig("a", 0, time.Second)},
			order: []string{"a", "b", "c", "d"}, wantNone: true},
		{name: "fresh penalty demotes a", sigs: []NodeSignal{sig("a", 0.4, time.Minute, "rdma retry 5%")},
			order: []string{"b", "a", "c", "d"}, penalty: map[string]int32{"a": 10}},
		{name: "cap at 25 even for huge delta", sigs: []NodeSignal{sig("a", 50, time.Minute, "x")},
			order: []string{"b", "c", "a", "d"}, penalty: map[string]int32{"a": 25}},
		{name: "lower cap", sigs: []NodeSignal{sig("a", 1, time.Minute, "x")}, max: 5,
			order: []string{"b", "a", "c", "d"}, penalty: map[string]int32{"a": 5}},
		{name: "equal final scores break on node name", sigs: []NodeSignal{sig("a", 0.4, time.Minute, "x"), sig("b", 0.4, time.Minute, "y")},
			order: []string{"a", "b", "c", "d"}, penalty: map[string]int32{"a": 10, "b": 10}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := append([]NodeScore(nil), base...)
			got, expl := rankWithFabric(in, FreshSignals(c.sigs, t0), FabricOptions{MaxPenalty: c.max}, t0)
			if !reflect.DeepEqual(in, base) {
				t.Fatalf("input modified: %v", in)
			}
			var order []string
			for _, n := range got {
				order = append(order, n.NodeName)
			}
			if !reflect.DeepEqual(order, c.order) {
				t.Errorf("order = %v, want %v", order, c.order)
			}
			if c.wantNone {
				if expl != nil {
					t.Errorf("explanation = %v, want nil", expl)
				}
				return
			}
			for _, e := range expl {
				if e.FabricPenalty != c.penalty[e.Node] {
					t.Errorf("node %s penalty = %d, want %d", e.Node, e.FabricPenalty, c.penalty[e.Node])
				}
				if e.FinalScore != e.BaseScore-e.FabricPenalty {
					t.Errorf("node %s final %d != base %d - pen %d", e.Node, e.FinalScore, e.BaseScore, e.FabricPenalty)
				}
				if e.FabricPenalty > 0 && (len(e.Reasons) == 0 || e.SignalAgeSeconds != 60) {
					t.Errorf("node %s missing reasons/age: %+v", e.Node, e)
				}
			}
		})
	}
}

func TestRankWithFabricExplanationBounded(t *testing.T) {
	var scored []NodeScore
	var sigs []NodeSignal
	for i := 0; i < 40; i++ {
		n := string(rune('A'+i/26)) + string(rune('a'+i%26))
		scored = append(scored, NodeScore{n, 200 - i})
		if i < 3 {
			sigs = append(sigs, sig(n, 1, time.Second, "x"))
		}
	}
	_, expl := rankWithFabric(scored, FreshSignals(sigs, t0), FabricOptions{}, t0)
	if len(expl) > 2*MaxExplained || len(expl) == 0 {
		t.Fatalf("explanation size %d out of bounds", len(expl))
	}
}

func TestFabricEnabled(t *testing.T) {
	mk := func(v string) *gryviav1.GryviaAIJob {
		j := &gryviav1.GryviaAIJob{}
		if v != "" {
			j.Annotations = map[string]string{AnnotationFabricAware: v}
		}
		return j
	}
	cases := []struct {
		ann  string
		op   bool
		want bool
	}{
		{"", false, false}, {"", true, true}, {"true", false, true},
		{"false", true, false}, {"bogus", false, false}, {"bogus", true, true},
	}
	for _, c := range cases {
		if got := FabricEnabled(mk(c.ann), c.op); got != c.want {
			t.Errorf("ann=%q op=%v: got %v", c.ann, c.op, got)
		}
	}
	if FabricEnabled(nil, false) {
		t.Error("nil job must follow the operator default")
	}
}

func fabNode(name string, gpus int) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"gryvia.io/gpu-count": "8"}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func TestFindOptimalNodesFabric(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = gryviav1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(fabNode("n1", 8), fabNode("n2", 8), fabNode("n3", 8)).Build()
	job := &gryviav1.GryviaAIJob{Spec: gryviav1.GryviaAIJobSpec{GPUs: 1}}

	// Baseline: identical scores; FindOptimalNodes is untouched.
	if _, err := FindOptimalNodes(context.Background(), c, job); err != nil {
		t.Fatal(err)
	}
	p, err := FindOptimalNodesFabric(context.Background(), c, job, FabricOptions{
		Now: func() time.Time { return t0 },
		Signals: func(context.Context) ([]NodeSignal, error) {
			return []NodeSignal{sig("n1", 1, time.Minute, "nccl p99 80ms"), sig("n2", 1, 9*time.Minute, "stale")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Nodes) != 1 || p.Nodes[0] == "n1" {
		t.Errorf("penalised n1 chosen: %v", p.Nodes)
	}
	var sawN1 bool
	for _, e := range p.Explanation {
		if e.Node == "n1" && e.FabricPenalty == 25 {
			sawN1 = true
		}
		if e.Node == "n2" && e.FabricPenalty != 0 {
			t.Errorf("stale n2 was penalised: %+v", e)
		}
	}
	if !sawN1 {
		t.Errorf("no n1 penalty explained: %+v", p.Explanation)
	}

	// Lookup error: fail open, no explanation.
	var got error
	p, err = FindOptimalNodesFabric(context.Background(), c, job, FabricOptions{
		Signals: func(context.Context) ([]NodeSignal, error) { return nil, errors.New("forbidden") },
		OnError: func(e error) { got = e },
	})
	if err != nil || got == nil || p.Explanation != nil || len(p.Nodes) != 1 {
		t.Errorf("fail-open broken: %+v err=%v cb=%v", p, err, got)
	}
}

func TestLoadNodeSignals(t *testing.T) {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	nf := &gryviav1.GryviaNodeFabric{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec: gryviav1.GryviaNodeFabricSpec{NodeName: "n1", ScoreDelta: 0.5, TTLSeconds: 60,
			MeasuredAt: metav1.NewTime(t0), Reasons: []string{"pfc"}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(nf).Build()
	sigs, err := LoadNodeSignals(context.Background(), c)
	if err != nil || len(sigs) != 1 || sigs[0].TTL != time.Minute {
		t.Fatalf("got %+v err=%v", sigs, err)
	}
}

func TestPreferredNodeTerms(t *testing.T) {
	pen := []gryviav1.PlacementExplanation{{Node: "a", FabricPenalty: 5}}
	if got := PreferredNodeTerms([]string{"b"}, nil); got != nil {
		t.Errorf("no explanation must give no terms: %v", got)
	}
	if got := PreferredNodeTerms([]string{"b"}, []gryviav1.PlacementExplanation{{Node: "b"}}); got != nil {
		t.Errorf("no penalty must give no terms: %v", got)
	}
	got := PreferredNodeTerms([]string{"b", "c"}, pen)
	if len(got) != 2 || got[0].Weight != 100 || got[0].Preference.MatchFields[0].Values[0] != "b" {
		t.Errorf("terms = %+v", got)
	}
}

func TestPinToNodesAddsRequirementToEveryTerm(t *testing.T) {
	orig := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{
			{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "a", Operator: corev1.NodeSelectorOpExists}}},
			{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "b", Operator: corev1.NodeSelectorOpExists}}},
		}}}}
	got := PinToNodes(orig, []string{"n1", "n2"})
	for i, term := range got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if len(term.MatchFields) != 1 || term.MatchFields[0].Key != "metadata.name" || len(term.MatchFields[0].Values) != 2 {
			t.Errorf("term %d not pinned: %+v", i, term)
		}
		if len(term.MatchExpressions) != 1 {
			t.Errorf("term %d lost the user's expression", i)
		}
	}
	if len(orig.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields) != 0 {
		t.Error("input was mutated")
	}
}

func TestPinToNodesCreatesTermAndIgnoresEmpty(t *testing.T) {
	got := PinToNodes(nil, []string{"n1"})
	if terms := got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms; len(terms) != 1 || terms[0].MatchFields[0].Values[0] != "n1" {
		t.Errorf("unexpected terms: %+v", terms)
	}
	if PinToNodes(nil, nil) != nil {
		t.Error("empty nodes must leave affinity nil")
	}
}

func TestFabricPreferenceIgnoresFreeCapacity(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = gryviav1.AddToScheme(s)
	// All GPUs of every node are in use: FindOptimalNodes finds nothing, FabricPreference still ranks.
	busy := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p-" + name, Namespace: "d"},
			Spec: corev1.PodSpec{NodeName: name, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(fabNode("n1", 8), fabNode("n2", 8), busy("n1"), busy("n2")).Build()
	job := &gryviav1.GryviaAIJob{Spec: gryviav1.GryviaAIJobSpec{GPUs: 4}}

	if _, err := FindOptimalNodes(context.Background(), c, job); err == nil {
		t.Fatal("precondition: the capacity-aware path should find no node")
	}
	p, err := FabricPreference(context.Background(), c, job, FabricOptions{
		Now: func() time.Time { return t0 },
		Signals: func(context.Context) ([]NodeSignal, error) {
			return []NodeSignal{sig("n1", 1, time.Minute, "nccl p99 80ms")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Nodes) != 1 || p.Nodes[0] != "n2" {
		t.Fatalf("want the healthy node n2 preferred, got %v", p.Nodes)
	}
	if len(PreferredNodeTerms(p.Nodes, p.Explanation)) == 0 {
		t.Fatal("a penalised node should yield soft affinity terms")
	}
}
