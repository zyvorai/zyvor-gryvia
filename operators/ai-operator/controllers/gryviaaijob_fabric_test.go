package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/scheduler"
)

func fabTestNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"gryvia.io/gpu-count": "8"}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func fabTestSignal(node string, delta float64, age time.Duration) *gryviav1.GryviaNodeFabric {
	return &gryviav1.GryviaNodeFabric{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Spec: gryviav1.GryviaNodeFabricSpec{
			NodeName: node, ScoreDelta: delta, Reasons: []string{"rdma retry rate 9%"},
			MeasuredAt: metav1.NewTime(time.Now().Add(-age)),
		},
	}
}

// runFabricJob reconciles a fresh job on 3 equal nodes where n1 has the given signal.
func runFabricJob(t *testing.T, operatorOn bool, ann string, sig *gryviav1.GryviaNodeFabric) (*gryviav1.GryviaAIJob, *batchv1.Job, *record.FakeRecorder) {
	t.Helper()
	job := newTestAIJob("fj", "default")
	job.Spec.GPUs = 1
	job.Spec.GpuType = ""
	if ann != "" {
		job.Annotations = map[string]string{scheduler.AnnotationFabricAware: ann}
	}
	objs := []client.Object{job, fabTestNode("n1"), fabTestNode("n2"), fabTestNode("n3")}
	if sig != nil {
		objs = append(objs, sig)
	}
	r, c := newAIJobReconciler(objs...)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	r.FabricAware = operatorOn
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "fj", Namespace: "default"}}
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	got := &gryviav1.GryviaAIJob{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	bj := &batchv1.Job{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "fj", Namespace: "default"}, bj); err != nil {
		t.Fatalf("batch job: %v", err)
	}
	return got, bj, rec
}

func TestAIJob_FabricAware_OffByDefault(t *testing.T) {
	job, bj, rec := runFabricJob(t, false, "", fabTestSignal("n1", 1, time.Minute))
	if len(job.Status.NodesAllocated) != 1 {
		t.Fatalf("nodes = %v", job.Status.NodesAllocated)
	}
	if job.Status.PlacementExplanation != nil || bj.Spec.Template.Spec.Affinity != nil || len(rec.Events) != 0 {
		t.Errorf("feature off must change nothing: expl=%v aff=%v events=%d",
			job.Status.PlacementExplanation, bj.Spec.Template.Spec.Affinity, len(rec.Events))
	}
}

func TestAIJob_FabricAware_ExplainsAndPrefers(t *testing.T) {
	cases := []struct {
		name       string
		operatorOn bool
		ann        string
		sig        *gryviav1.GryviaNodeFabric
		wantApply  bool
	}{
		{"operator flag on", true, "", fabTestSignal("n1", 1, time.Minute), true},
		{"job opts in with operator off", false, "true", fabTestSignal("n1", 1, time.Minute), true},
		{"job opts out with operator on", true, "false", fabTestSignal("n1", 1, time.Minute), false},
		{"stale signal ignored", true, "", fabTestSignal("n1", 1, 10*time.Minute), false},
		{"no signal", true, "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			job, bj, rec := runFabricJob(t, c.operatorOn, c.ann, c.sig)
			if len(job.Status.NodesAllocated) != 1 {
				t.Fatalf("nodes = %v", job.Status.NodesAllocated)
			}
			aff := bj.Spec.Template.Spec.Affinity
			if !c.wantApply {
				if job.Status.PlacementExplanation != nil || aff != nil || len(rec.Events) != 0 {
					t.Fatalf("expected no effect: expl=%v aff=%v", job.Status.PlacementExplanation, aff)
				}
				return
			}
			if job.Status.NodesAllocated[0] == "n1" {
				t.Errorf("penalised n1 was selected")
			}
			var n1 *gryviav1.PlacementExplanation
			for i := range job.Status.PlacementExplanation {
				if job.Status.PlacementExplanation[i].Node == "n1" {
					n1 = &job.Status.PlacementExplanation[i]
				}
			}
			if n1 == nil || n1.FabricPenalty != 25 || n1.FinalScore != n1.BaseScore-25 || len(n1.Reasons) == 0 {
				t.Fatalf("n1 explanation = %+v", n1)
			}
			select {
			case ev := <-rec.Events:
				if !strings.Contains(ev, "FabricAwarePlacement") || !strings.Contains(ev, "n1 -25") {
					t.Errorf("event = %q", ev)
				}
			default:
				t.Error("no event")
			}
			if aff == nil || aff.NodeAffinity == nil ||
				len(aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 ||
				aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
				t.Fatalf("expected exactly one soft preferred term, got %+v", aff)
			}
			term := aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0]
			if term.Preference.MatchFields[0].Values[0] != job.Status.NodesAllocated[0] {
				t.Errorf("preference %v does not point at the selected node %v", term, job.Status.NodesAllocated)
			}
		})
	}
}

func TestAIJob_BuildAffinity_KeepsUserAffinity(t *testing.T) {
	r, _ := newAIJobReconciler()
	user := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}}}}},
	}}
	job := newTestAIJob("j", "default")
	job.Spec.Affinity = user
	job.Status.NodesAllocated = []string{"n2"}
	job.Status.PlacementExplanation = []gryviav1.PlacementExplanation{{Node: "n1", BaseScore: 50, FabricPenalty: 10, FinalScore: 40}}

	got := r.buildAffinity(job)
	if got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil || len(got.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("merge lost data: %+v", got)
	}
	if user.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution != nil {
		t.Error("job spec was mutated")
	}
	job.Annotations = map[string]string{scheduler.AnnotationFabricAware: "false"}
	if r.buildAffinity(job) != user {
		t.Error("opt-out must return the user affinity untouched")
	}
}

func TestPlacementAffinityUsesPreferredNodesForKueueJobs(t *testing.T) {
	r := &GryviaAIJobReconciler{}
	job := &gryviav1.GryviaAIJob{Status: gryviav1.GryviaAIJobStatus{
		PreferredNodes:       []string{"n2"},
		PlacementExplanation: []gryviav1.PlacementExplanation{{Node: "n1", FabricPenalty: 20}},
	}}
	aff := r.buildPlacementAffinity(job)
	if aff == nil || aff.NodeAffinity == nil || len(aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("want one soft term toward n2, got %+v", aff)
	}
	if aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Fatal("preferredNodes must never become required affinity")
	}

	job.Annotations = map[string]string{"gryvia.io/pin-placement": "true"}
	if aff := r.buildPlacementAffinity(job); aff != nil && aff.NodeAffinity != nil && aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Fatal("pin-placement must not pin to advisory preferredNodes")
	}
}
