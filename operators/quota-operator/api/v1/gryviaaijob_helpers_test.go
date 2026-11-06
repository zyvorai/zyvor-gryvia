package v1

import "testing"

func TestEffectiveGPUsWhole(t *testing.T) {
	s := GryviaAIJobSpec{GPUs: 4}
	if got := s.EffectiveGPUs(nil); got != 4 {
		t.Fatalf("got %v want 4", got)
	}
}

func TestEffectiveGPUsShareFraction(t *testing.T) {
	s := GryviaAIJobSpec{GPUs: 2}
	got := s.EffectiveGPUs(map[string]string{"gryvia.io/gpu-share": "0.5"})
	if got != 1 {
		t.Fatalf("got %v want 1", got)
	}
}

func TestEffectiveGPUsShareAbsolute(t *testing.T) {
	s := GryviaAIJobSpec{GPUs: 8}
	got := s.EffectiveGPUs(map[string]string{"gryvia.io/gpu-share": "2.5"})
	if got != 2.5 {
		t.Fatalf("got %v want 2.5", got)
	}
}

func TestEffectiveGPUsMIG(t *testing.T) {
	s := GryviaAIJobSpec{GPUs: 1, MigProfile: "1g.10gb", GpuType: "A100-80G"}
	got := s.EffectiveGPUs(nil)
	if got != 0.125 {
		t.Fatalf("got %v want 0.125", got)
	}
}
