package controllers

import (
	"testing"

	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
)

// calculateEffectiveGPUs is the capacity estimate reported in the policy status. It is arithmetic on the
// policy, not a measurement of what the NVIDIA device plugin actually exposes.
func TestCalculateEffectiveGPUs(t *testing.T) {
	r := &GryviaGPUSharingPolicyReconciler{}
	policy := func(strategy string, mut func(*gryviav1.GryviaGPUSharingPolicySpec)) *gryviav1.GryviaGPUSharingPolicy {
		p := &gryviav1.GryviaGPUSharingPolicy{}
		p.Spec.Strategy = strategy
		if mut != nil {
			mut(&p.Spec)
		}
		return p
	}

	for _, tc := range []struct {
		name string
		p    *gryviav1.GryviaGPUSharingPolicy
		want int
	}{
		{"unknown strategy keeps physical", policy("exclusive", nil), 8},
		{"time-slicing default 4x", policy("time-slicing", nil), 32},
		{"time-slicing replicas", policy("time-slicing", func(s *gryviav1.GryviaGPUSharingPolicySpec) {
			s.TimeSlicing = &gryviav1.GPUTimeSlicingConfig{MaxPodsPerGPU: 10}
		}), 80},
		{"fractional default 1/4", policy("fractional", nil), 32},
		{"fractional 1/2", policy("fractional", func(s *gryviav1.GryviaGPUSharingPolicySpec) {
			s.FractionalGPU = &gryviav1.GPUFractionalConfig{Granularity: "1/2"}
		}), 16},
		{"fractional 1/8", policy("fractional", func(s *gryviav1.GryviaGPUSharingPolicySpec) {
			s.FractionalGPU = &gryviav1.GPUFractionalConfig{Granularity: "1/8"}
		}), 64},
		{"fractional 1/16", policy("fractional", func(s *gryviav1.GryviaGPUSharingPolicySpec) {
			s.FractionalGPU = &gryviav1.GPUFractionalConfig{Granularity: "1/16"}
		}), 128},
		{"fractional unknown granularity falls back to 1/4", policy("fractional", func(s *gryviav1.GryviaGPUSharingPolicySpec) {
			s.FractionalGPU = &gryviav1.GPUFractionalConfig{Granularity: "1/3"}
		}), 32},
		{"mig default 7 per GPU", policy("mig", nil), 56},
		{"mig profile counts", policy("mig", func(s *gryviav1.GryviaGPUSharingPolicySpec) {
			s.MIG = &gryviav1.GPUMIGConfig{Profiles: []gryviav1.GPUMIGProfile{{Name: "3g.40gb", Count: 1}, {Name: "2g.20gb", Count: 2}}}
		}), 24},
	} {
		if got := r.calculateEffectiveGPUs(tc.p, 8); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
