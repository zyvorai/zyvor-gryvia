// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package scheduler

import (
	"math"
	"testing"
)

func TestFabricPenalty(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"zero (empty status)", 0, 0},
		{"negative", -0.5, 0},
		{"NaN", math.NaN(), 0},
		{"quarter", 0.25, 6.25},
		{"one", 1, MaxFabricPenalty},
		{"above one is capped", 7, MaxFabricPenalty},
		{"+Inf is capped", math.Inf(1), MaxFabricPenalty},
		{"-Inf", math.Inf(-1), 0},
	}
	for _, c := range cases {
		if got := FabricPenalty(c.in); got != c.want {
			t.Errorf("%s: FabricPenalty(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
	if MaxFabricPenalty != 25 {
		t.Errorf("cap = %v, want 25", MaxFabricPenalty)
	}
}

func TestApplyFabricPenalty(t *testing.T) {
	if got := ApplyFabricPenalty(80, 0); got != 80 {
		t.Errorf("healthy fabric changed the score: %v", got)
	}
	if got := ApplyFabricPenalty(80, 0.4); got != 70 {
		t.Errorf("got %v, want 70", got)
	}
	if got := ApplyFabricPenalty(10, 1); got != 0 {
		t.Errorf("score must not go negative, got %v", got)
	}
	if got := ApplyFabricPenalty(0, 0.5); got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestRescoreNodes(t *testing.T) {
	in := []NodeScore{{NodeName: "a", Score: 80}, {NodeName: "b", Score: 60}, {NodeName: "c", Score: 10}}
	// No feed: a copy with identical scores.
	for _, m := range []map[string]float64{nil, {}} {
		out := RescoreNodes(in, m)
		if len(out) != 3 || out[0] != in[0] || out[1] != in[1] || out[2] != in[2] {
			t.Fatalf("empty feed changed scores: %v", out)
		}
		out[0].Score = 1
		if in[0].Score != 80 {
			t.Fatal("output aliases the input")
		}
	}
	out := RescoreNodes(in, map[string]float64{"a": 0.4, "c": 1, "unknown": 1, "b": math.NaN()})
	if out[0].Score != 70 || out[1].Score != 60 || out[2].Score != 0 {
		t.Fatalf("got %v", out)
	}
	if out[0].NodeName != "a" || out[2].NodeName != "c" {
		t.Fatalf("order changed: %v", out)
	}
	if in[0].Score != 80 || in[2].Score != 10 {
		t.Fatal("input modified")
	}
	if got := RescoreNodes(nil, map[string]float64{"a": 1}); len(got) != 0 {
		t.Fatal(got)
	}
}
