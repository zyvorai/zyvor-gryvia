package scheduler

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestFairShareOrdersUnderGuaranteeFirst(t *testing.T) {
	e, _ := NewNativeQueueEngine([]NativeQueuePolicy{
		{Name: "a", GuaranteeGPU: 2000, MaxGPU: 4000, Weight: 1, Borrow: true},
		{Name: "b", GuaranteeGPU: 2000, MaxGPU: 4000, Weight: 1, Borrow: true},
	})
	alloc := []NativeAllocation{{WorkloadID: "a0", Queue: "a", GPU: 2000}}
	now := time.Now()
	q, err := e.Order([]NativeWorkload{
		{ID: "a1", Queue: "a", Priority: 10, Replicas: 1, GPUPerPod: 1000, CreatedAt: now},
		{ID: "b1", Queue: "b", Priority: 10, Replicas: 1, GPUPerPod: 1000, CreatedAt: now.Add(time.Second)},
	}, alloc)
	if err != nil {
		t.Fatal(err)
	}
	if q[0].Queue != "b" {
		t.Fatalf("first=%s want b", q[0].Queue)
	}
}

func TestGangAdmission(t *testing.T) {
	e, _ := NewNativeQueueEngine([]NativeQueuePolicy{{Name: "q", MaxGPU: 8000, Borrow: true}})
	_, err := e.PlanAdmission(NativeWorkload{ID: "x", Queue: "q", Replicas: 4, MinAvailable: 4, GPUPerPod: 1000}, nil, 3000)
	if !errors.Is(err, ErrNoPreemptionPlan) {
		t.Fatalf("err=%v", err)
	}
}

func TestSafePreemption(t *testing.T) {
	e, _ := NewNativeQueueEngine([]NativeQueuePolicy{
		{Name: "low", GuaranteeGPU: 0, MaxGPU: 4000, Borrow: true},
		{Name: "high", GuaranteeGPU: 1000, MaxGPU: 4000, Borrow: true},
	})
	p, err := e.PlanAdmission(
		NativeWorkload{ID: "h", Queue: "high", Priority: 100, Replicas: 1, GPUPerPod: 1000},
		[]NativeAllocation{{WorkloadID: "l", Queue: "low", Priority: 1, GPU: 1000, Preemptible: true}},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Allow || len(p.Victims) != 1 || p.Victims[0].WorkloadID != "l" {
		t.Fatalf("%+v", p)
	}
}

func TestNoEqualPriorityPreemption(t *testing.T) {
	e, _ := NewNativeQueueEngine([]NativeQueuePolicy{{Name: "a", Borrow: true}, {Name: "b", Borrow: true}})
	_, err := e.PlanAdmission(
		NativeWorkload{ID: "b", Queue: "b", Priority: 10, Replicas: 1, GPUPerPod: 1000},
		[]NativeAllocation{{WorkloadID: "a", Queue: "a", Priority: 10, GPU: 1000, Preemptible: true}},
		0,
	)
	if !errors.Is(err, ErrNoPreemptionPlan) {
		t.Fatalf("err=%v", err)
	}
}

func TestFractionalMPS(t *testing.T) {
	req, err := ParseGPUShare(map[string]string{AnnotationGPUShare: "0.25", AnnotationGPUIsolation: "mps"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if req.MilliGPU != 250 {
		t.Fatalf("%d", req.MilliGPU)
	}
	p, err := PlanGPURuntime(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Env["CUDA_MPS_ACTIVE_THREAD_PERCENTAGE"] != "25" {
		t.Fatalf("%+v", p)
	}
}

func TestAutoMIG(t *testing.T) {
	req, _ := ParseGPUShare(map[string]string{AnnotationGPUShare: "0.5"}, 1)
	p, err := PlanGPURuntime(req, map[string]string{"nvidia.com/mig.capable": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != GPUIsolationMIG {
		t.Fatalf("%+v", p)
	}
}

func TestWholeGPU(t *testing.T) {
	req, _ := ParseGPUShare(nil, 2)
	p, err := PlanGPURuntime(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.ResourceName != "nvidia.com/gpu" || p.ResourceQty != 2 {
		t.Fatalf("%+v", p)
	}
}

func TestTelemetry(t *testing.T) {
	x := TelemetryFromLabels(map[string]string{
		LabelGPUUtilization: "20", LabelGPUMemoryUsed: "25", LabelNVLinkHealth: "100",
		LabelRDMAHealth: "90", LabelStorageHealth: "80",
	})
	if got := LiveTelemetryScore(x, true, true); got < 70 {
		t.Fatalf("score=%d", got)
	}
}

func TestElasticAdmissionRespectsCeilingAndWholePods(t *testing.T) {
	e, _ := NewNativeQueueEngine([]NativeQueuePolicy{{Name: "q", MaxGPU: 2500, Borrow: true}})
	p, err := e.PlanAdmission(NativeWorkload{ID: "j", Queue: "q", Replicas: 4, MinAvailable: 1, GPUPerPod: 1000}, nil, 8000)
	if err != nil || p.NeedGPU != 2000 {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
}
func TestPreemptionCannotBypassBorrowDisabled(t *testing.T) {
	e, _ := NewNativeQueueEngine([]NativeQueuePolicy{{Name: "q", GuaranteeGPU: 1000, MaxGPU: 4000, Borrow: false}, {Name: "other", Borrow: true}})
	_, err := e.PlanAdmission(NativeWorkload{ID: "j", Queue: "q", Replicas: 1, GPUPerPod: 1000, Priority: 100}, []NativeAllocation{{WorkloadID: "existing", Queue: "q", GPU: 1000}, {WorkloadID: "victim", Queue: "other", GPU: 1000, Priority: 1, Preemptible: true}}, 0)
	if !errors.Is(err, ErrGangUnsatisfied) {
		t.Fatalf("err=%v", err)
	}
}
func TestOrderDeterministicAtIdenticalTimestamp(t *testing.T) {
	e, _ := NewNativeQueueEngine([]NativeQueuePolicy{{Name: "q", Borrow: true}})
	now := time.Now()
	order, err := e.Order([]NativeWorkload{{ID: "z", Queue: "q", Replicas: 1, GPUPerPod: 1000, CreatedAt: now}, {ID: "a", Queue: "q", Replicas: 1, GPUPerPod: 1000, CreatedAt: now}}, nil)
	if err != nil || order[0].ID != "a" {
		t.Fatalf("order=%v err=%v", order, err)
	}
}
func TestRejectEmptyDuplicateAndNonFinitePolicies(t *testing.T) {
	for _, policies := range [][]NativeQueuePolicy{nil, {{Name: "q"}, {Name: "q"}}, {{Name: "q", Weight: math.NaN()}}} {
		if _, err := NewNativeQueueEngine(policies); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}
