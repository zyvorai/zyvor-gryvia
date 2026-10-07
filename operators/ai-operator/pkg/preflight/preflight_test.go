package preflight

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

type golden struct {
	Name  string `json:"name"`
	Input struct {
		GPUType                 string  `json:"gpuType"`
		Nodes                   int32   `json:"nodes"`
		GPUsPerNode             int32   `json:"gpusPerNode"`
		ParamsBillions          float64 `json:"parametersBillions"`
		WeightBits              int     `json:"weightBits"`
		TensorParallel          int32   `json:"tensorParallel"`
		ExtraGiB                float64 `json:"extraMemoryGiBPerGPU"`
		RequireRDMA             bool    `json:"requireRDMA"`
		RequireFastInterconnect bool    `json:"requireFastInterconnect"`
	} `json:"input"`
	Pools  []Pool `json:"pools"`
	Expect struct {
		State    string   `json:"state"`
		PerGPU   float64  `json:"perGPU"`
		Eligible []string `json:"eligible"`
	} `json:"expect"`
}

// The same file is evaluated by the gateway's Python engine (tests/test_preflight_golden.py).
func TestGoldenMatchesGateway(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []golden
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			for i := range c.Pools {
				if c.Pools[i].Interconnect == "" {
					c.Pools[i].Interconnect = "none"
				}
			}
			in := Input{GPUType: c.Input.GPUType, Nodes: c.Input.Nodes, GPUsPerNode: c.Input.GPUsPerNode,
				ParamsBillions: c.Input.ParamsBillions, WeightBits: c.Input.WeightBits, TensorParallel: c.Input.TensorParallel,
				ExtraGiBPerGPU: c.Input.ExtraGiB, RequireRDMA: c.Input.RequireRDMA, RequireFastInterconnect: c.Input.RequireFastInterconnect}
			r := Evaluate(in, c.Pools)
			if r.State != c.Expect.State {
				t.Fatalf("state = %s, want %s (%s)", r.State, c.Expect.State, r.Summary())
			}
			if math.Abs(r.PerGPUGiB-c.Expect.PerGPU) > 0.001 {
				t.Fatalf("perGPU = %v, want %v", r.PerGPUGiB, c.Expect.PerGPU)
			}
			got := []string{}
			for _, cand := range r.Candidates {
				if cand.Eligible {
					got = append(got, cand.Pool)
				}
			}
			if !reflect.DeepEqual(got, c.Expect.Eligible) {
				t.Fatalf("eligible = %v, want %v", got, c.Expect.Eligible)
			}
		})
	}
}

func job(ann map[string]string, gpus int32) *gryviav1.GryviaAIJob {
	return &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns", Annotations: ann},
		Spec:       gryviav1.GryviaAIJobSpec{GPUs: gpus, GpuType: "A100", Network: "rdma"},
	}
}

func TestFromJob(t *testing.T) {
	if _, req, _ := FromJob(job(nil, 1)); req {
		t.Fatal("a job without the params annotation does not opt in")
	}
	in, req, errs := FromJob(job(map[string]string{AnnotationParams: "70", AnnotationWeightBits: "8",
		AnnotationTensorPar: "4", AnnotationExtraGiB: "6.5", AnnotationFastInterconn: "true"}, 4))
	if !req || len(errs) > 0 {
		t.Fatalf("req=%v errs=%v", req, errs)
	}
	want := Input{GPUType: "A100", Nodes: 1, GPUsPerNode: 4, ParamsBillions: 70, WeightBits: 8, TensorParallel: 4,
		ExtraGiBPerGPU: 6.5, RequireRDMA: true, RequireFastInterconnect: true}
	if in != want {
		t.Fatalf("in = %+v", in)
	}
	_, _, errs = FromJob(job(map[string]string{AnnotationParams: "-1", AnnotationWeightBits: "12",
		AnnotationTensorPar: "8", AnnotationExtraGiB: "x", AnnotationFastInterconn: "maybe"}, 4))
	if len(errs) != 5 {
		t.Fatalf("errs = %v", errs)
	}
	_, _, errs = FromJob(job(map[string]string{AnnotationParams: "7"}, 0))
	if len(errs) != 1 || !strings.Contains(errs[0], "GPU job") {
		t.Fatalf("errs = %v", errs)
	}
}

func node(name string, labels map[string]string, gpus int64, cordoned bool) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			"nvidia.com/gpu": *resource.NewQuantity(gpus, resource.DecimalSI)}},
	}
}

func TestPoolsFromNodes(t *testing.T) {
	a := map[string]string{LabelGPU: "A100", LabelGPUMemory: "80", LabelRDMA: "true", LabelInterconnect: "NVSwitch"}
	pools := PoolsFromNodes([]corev1.Node{
		node("a1", a, 8, false),
		node("a2", a, 8, false),
		node("a3", a, 8, true), // cordoned: not capacity
		node("l1", map[string]string{LabelGPU: "L4", LabelNvidiaMemory: "24576"}, 1, false),
		node("cpu", nil, 0, false),
	})
	if len(pools) != 2 {
		t.Fatalf("pools = %+v", pools)
	}
	if p := pools[0]; p.Name != "a100-x8" || p.Nodes != 2 || p.MemoryGiB != 80 || !p.RDMA || p.Interconnect != "nvswitch" {
		t.Fatalf("pool 0 = %+v", p)
	}
	if p := pools[1]; p.Name != "l4-x1" || p.MemoryGiB != 24 || p.Interconnect != "none" {
		t.Fatalf("pool 1 = %+v", p)
	}
}

func TestEmptyAndUnknownMemoryAreIncomplete(t *testing.T) {
	in := Input{GPUType: "A100", Nodes: 1, GPUsPerNode: 1, ParamsBillions: 7, WeightBits: 16, TensorParallel: 1}
	if r := Evaluate(in, nil); r.State != StateIncomplete {
		t.Fatalf("no pools: %s", r.State)
	}
	r := Evaluate(in, []Pool{{Name: "a", GPUType: "A100", Nodes: 1, GPUsPerNode: 8, Interconnect: "none"}})
	if r.State != StateIncomplete {
		t.Fatalf("unknown memory: %s", r.State)
	}
}
