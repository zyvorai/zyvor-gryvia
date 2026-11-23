// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package idlecost

import (
	"math"
	"strings"
	"testing"
)

func sum(r Result) float64 {
	var s int64
	for _, x := range r.Shares {
		s += cents(x.Idle)
	}
	return float64(s) / 100
}

func TestAllocate(t *testing.T) {
	cases := []struct {
		name        string
		idle        float64
		in          []Input
		want        []float64
		unallocated float64
	}{
		{"proportional", 100, []Input{{"a", 75}, {"b", 25}}, []float64{75, 25}, 0},
		{"remainder goes to largest fraction", 0.01, []Input{{"a", 1}, {"b", 1}, {"c", 1}}, []float64{0.01, 0, 0}, 0},
		{"thirds sum exactly", 100, []Input{{"a", 1}, {"b", 1}, {"c", 1}}, []float64{33.34, 33.33, 33.33}, 0},
		{"zero cost center gets nothing", 10, []Input{{"a", 0}, {"b", 5}}, []float64{0, 10}, 0},
		{"no usage leaves idle unallocated", 40, []Input{{"a", 0}}, []float64{0}, 40},
		{"no centers", 40, nil, nil, 40},
		{"no idle", 0, []Input{{"a", 3}}, []float64{0}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Allocate(c.idle, c.in)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Shares) != len(c.want) {
				t.Fatalf("shares %v", r.Shares)
			}
			for i, w := range c.want {
				if math.Abs(r.Shares[i].Idle-w) > 1e-9 {
					t.Errorf("share %d = %v want %v", i, r.Shares[i].Idle, w)
				}
			}
			if math.Abs(r.Unallocated-c.unallocated) > 1e-9 {
				t.Errorf("unallocated %v want %v", r.Unallocated, c.unallocated)
			}
			if math.Abs(sum(r)+r.Unallocated-math.Round(c.idle*100)/100) > 1e-9 {
				t.Errorf("shares %v + unallocated %v != idle %v", sum(r), r.Unallocated, c.idle)
			}
		})
	}
}

func TestAllocateHostile(t *testing.T) {
	many := make([]Input, MaxCenters+1)
	for i := range many {
		many[i] = Input{ID: strings.Repeat("x", 3) + string(rune(i+1000)), Cost: 1}
	}
	cases := []struct {
		name string
		idle float64
		in   []Input
	}{
		{"NaN idle", math.NaN(), nil},
		{"Inf idle", math.Inf(1), nil},
		{"negative idle", -1, nil},
		{"huge idle", 1e13, nil},
		{"NaN cost", 1, []Input{{"a", math.NaN()}}},
		{"negative cost", 1, []Input{{"a", -1}}},
		{"Inf cost", 1, []Input{{"a", math.Inf(1)}}},
		{"empty id", 1, []Input{{"", 1}}},
		{"duplicate id", 1, []Input{{"a", 1}, {"a", 1}}},
		{"too many", 1, many},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Allocate(c.idle, c.in); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestAllocateLargeStaysExact(t *testing.T) {
	in := []Input{{"a", 123456789.01}, {"b", 987654321.99}, {"c", 0.01}}
	r, err := Allocate(999999999.99, in)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(sum(r)-999999999.99) > 1e-6 {
		t.Fatalf("sum %v", sum(r))
	}
}

func TestIdleCost(t *testing.T) {
	if v, err := IdleCost(1000, 600, 2.5); err != nil || v != 1000 {
		t.Fatalf("%v %v", v, err)
	}
	if v, _ := IdleCost(100, 150, 2); v != 0 {
		t.Fatalf("over-allocation must not give negative idle, got %v", v)
	}
	for _, a := range [][3]float64{{math.NaN(), 1, 1}, {1, -1, 1}, {1, 1, math.Inf(1)}, {1e13, 0, 1}} {
		if _, err := IdleCost(a[0], a[1], a[2]); err == nil {
			t.Errorf("expected error for %v", a)
		}
	}
}
