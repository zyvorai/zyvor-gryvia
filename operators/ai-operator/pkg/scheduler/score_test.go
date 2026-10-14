package scheduler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func scoreNode(name string, labels map[string]string) corev1.Node {
	l := map[string]string{"gryvia.io/gpu-count": "8"}
	for k, v := range labels {
		l[k] = v
	}
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
}

func scoreJob(gpus int32, gpuType, network string) *gryviav1.GryviaAIJob {
	return &gryviav1.GryviaAIJob{Spec: gryviav1.GryviaAIJobSpec{GPUs: gpus, GpuType: gpuType, Network: network}}
}

// delta returns score(with) - score(base) for the same job, so the other terms cancel.
func delta(t *testing.T, job *gryviav1.GryviaAIJob, base, with corev1.Node, usage map[string]int64) int {
	t.Helper()
	s := scoreNodes([]corev1.Node{base, with}, job, usage)
	return s[1].Score - s[0].Score
}

func TestScoreGPUTypeMatch(t *testing.T) {
	base := scoreNode("a", map[string]string{"gryvia.io/gpu": "H100"})
	with := scoreNode("b", map[string]string{"gryvia.io/gpu": "A100"})
	if got := delta(t, scoreJob(1, "A100", ""), base, with, nil); got != 50 {
		t.Fatalf("GPU type match delta = %d, want 50", got)
	}
	if got := delta(t, scoreJob(1, "", ""), base, with, nil); got != 0 {
		t.Fatalf("no requested type delta = %d, want 0", got)
	}
}

func TestScoreRDMAOnlyWhenRequested(t *testing.T) {
	base := scoreNode("a", nil)
	with := scoreNode("b", map[string]string{"gryvia.io/rdma": "true"})
	if got := delta(t, scoreJob(1, "", "rdma"), base, with, nil); got != 30 {
		t.Fatalf("rdma delta = %d, want 30", got)
	}
	if got := delta(t, scoreJob(1, "", "standard"), base, with, nil); got != 0 {
		t.Fatalf("rdma delta for non-rdma job = %d, want 0", got)
	}
}

func TestScoreInterconnectOnlyForMultiGPU(t *testing.T) {
	base := scoreNode("a", nil)
	for _, tc := range []struct {
		interconnect string
		want         int
	}{{"NVSwitch", 40}, {"NVLink", 30}, {"PCIe", 0}} {
		with := scoreNode("b", map[string]string{"gryvia.io/interconnect": tc.interconnect})
		if got := delta(t, scoreJob(4, "", ""), base, with, nil); got != tc.want {
			t.Errorf("%s multi-GPU delta = %d, want %d", tc.interconnect, got, tc.want)
		}
		if got := delta(t, scoreJob(1, "", ""), base, with, nil); got != 0 {
			t.Errorf("%s single-GPU delta = %d, want 0", tc.interconnect, got)
		}
	}
}

func TestScoreGPUMemoryPerTenGB(t *testing.T) {
	base := scoreNode("a", nil)
	with := scoreNode("b", map[string]string{"gryvia.io/gpu-memory": "80"})
	if got := delta(t, scoreJob(1, "", ""), base, with, nil); got != 8 {
		t.Fatalf("memory delta = %d, want 8", got)
	}
}

func TestScoreSpreadVersusPack(t *testing.T) {
	busy, idle := scoreNode("busy", nil), scoreNode("idle", nil)
	usage := map[string]int64{"busy": 6} // busy has 2 free GPUs, idle has 8

	spread := scoreJob(1, "", "")
	s := scoreNodes([]corev1.Node{busy, idle}, spread, usage)
	if s[1].Score-s[0].Score != 6*5 {
		t.Fatalf("spread: idle should lead by 30, got %d", s[1].Score-s[0].Score)
	}

	pack := scoreJob(1, "", "")
	pack.Annotations = map[string]string{AnnotationPlacementStrategy: StrategyPack}
	s = scoreNodes([]corev1.Node{busy, idle}, pack, usage)
	if s[0].Score-s[1].Score != 6*5 {
		t.Fatalf("pack: busy should lead by 30, got %d", s[0].Score-s[1].Score)
	}
}

func TestMigProfileFiltersOnAdvertisedSlices(t *testing.T) {
	ready := []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	withSlices := scoreNode("mig", nil)
	withSlices.Status.Conditions = ready
	withSlices.Status.Allocatable = corev1.ResourceList{MigResourceName("1g.10gb"): resource.MustParse("14")}
	whole := scoreNode("whole", nil) // 8 whole GPUs by label, no MIG resource
	whole.Status.Conditions = ready

	job := scoreJob(4, "", "")
	job.Spec.MigProfile = "1g.10gb"
	got := filterNodes([]corev1.Node{withSlices, whole}, job, nil, 4)
	if len(got) != 1 || got[0].Name != "mig" {
		t.Fatalf("MIG job must only match nodes advertising the slice resource, got %v", got)
	}

	job.Spec.MigProfile = ""
	got = filterNodes([]corev1.Node{withSlices, whole}, job, nil, 4)
	if len(got) != 2 {
		t.Fatalf("whole-GPU job must keep matching by GPU count (both nodes have 8), got %d nodes", len(got))
	}
}
