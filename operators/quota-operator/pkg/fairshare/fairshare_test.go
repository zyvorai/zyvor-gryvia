// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fairshare

import (
	"math"
	"reflect"
	"strconv"
	"testing"
)

func TestShare(t *testing.T) {
	cases := []struct {
		name  string
		q     Queue
		usage float64
		want  float64
	}{
		{"under quota", Queue{Nominal: 8}, 6, 0},
		{"at quota", Queue{Nominal: 8}, 8, 0},
		{"borrowing", Queue{Nominal: 8}, 12, 4},
		{"weight divides", Queue{Nominal: 8, Weight: 2}, 12, 2},
		{"zero weight is one", Queue{Nominal: 0, Weight: 0}, 3, 3},
	}
	for _, c := range cases {
		if got := Share(c.q, c.usage); got != c.want {
			t.Errorf("%s: %v want %v", c.name, got, c.want)
		}
	}
}

func fixture() ([]Queue, []Workload) {
	return []Queue{
		{Name: "a", Nominal: 8, Usage: 4},
		{Name: "b", Nominal: 4, Usage: 10},
		{Name: "c", Nominal: 4, Usage: 6},
	}, []Workload{
		{ID: "b1", Queue: "b", GPUs: 4, Priority: 5, Preemptible: true},
		{ID: "b2", Queue: "b", GPUs: 2, Priority: 1, Preemptible: true},
		{ID: "b3", Queue: "b", GPUs: 4, Priority: 9, Preemptible: false},
		{ID: "c1", Queue: "c", GPUs: 2, Priority: 0, Preemptible: true},
		{ID: "c2", Queue: "c", GPUs: 4, Priority: 3, Preemptible: true},
		{ID: "a1", Queue: "a", GPUs: 4, Priority: 0, Preemptible: true},
	}
}

func TestSelectVictims(t *testing.T) {
	cases := []struct {
		name     string
		free     float64
		req      Request
		strategy Strategy
		fits     bool
		victims  []string
	}{
		{"already fits", 4, Request{"a", 4}, LessThanOrEqualToFinalShare, true, nil},
		{"reclaims from highest share, lowest priority first", 0, Request{"a", 2}, LessThanOrEqualToFinalShare, true, []string{"b2"}},
		{"final-share rule refuses to drop a target below the preemptor", 0, Request{"a", 6}, LessThanOrEqualToFinalShare, false, nil},
		{"initial-share rule takes two victims; b stops at nominal", 0, Request{"a", 6}, LessThanInitialShare, true, []string{"b2", "b1"}},
		{"cannot be met without breaching quotas", 0, Request{"a", 20}, LessThanOrEqualToFinalShare, false, nil},
		{"initial share strategy", 0, Request{"a", 2}, LessThanInitialShare, true, []string{"b2"}},
		{"requester never preempts itself", 0, Request{"b", 2}, LessThanOrEqualToFinalShare, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, w := fixture()
			p, err := SelectVictims(q, w, c.free, c.req, c.strategy)
			if err != nil {
				t.Fatal(err)
			}
			if p.Fits != c.fits || !reflect.DeepEqual(p.Victims, c.victims) {
				t.Fatalf("plan = %+v", p)
			}
		})
	}
}

func TestSelectVictimsNeverBreachesNominal(t *testing.T) {
	q, w := fixture()
	p, err := SelectVictims(q, w, 0, Request{"a", 8}, LessThanInitialShare)
	if err != nil {
		t.Fatal(err)
	}
	// b borrows 6 above 4 and c borrows 2 above 4: at most 8 can be reclaimed, minus the non-preemptible b3.
	used := map[string]float64{"b": 10, "c": 6}
	byID := map[string]Workload{}
	for _, x := range w {
		byID[x.ID] = x
	}
	for _, id := range p.Victims {
		used[byID[id].Queue] -= byID[id].GPUs
	}
	if used["b"] < 4 || used["c"] < 4 {
		t.Fatalf("a queue was pushed below its nominal quota: %v (%+v)", used, p)
	}
}

func TestSelectVictimsDoesNotMutateInput(t *testing.T) {
	q, w := fixture()
	_, _ = SelectVictims(q, w, 0, Request{"a", 6}, LessThanOrEqualToFinalShare)
	q2, w2 := fixture()
	if !reflect.DeepEqual(q, q2) || !reflect.DeepEqual(w, w2) {
		t.Fatal("input modified")
	}
}

func TestSelectVictimsHostile(t *testing.T) {
	big := make([]Queue, MaxQueues+1)
	for i := range big {
		big[i] = Queue{Name: strconv.Itoa(i)}
	}
	ok := []Queue{{Name: "a"}, {Name: "b"}}
	cases := []struct {
		name string
		q    []Queue
		w    []Workload
		free float64
		req  Request
		s    Strategy
	}{
		{"unknown strategy", ok, nil, 0, Request{"a", 1}, "Nope"},
		{"too many queues", big, nil, 0, Request{"0", 1}, LessThanInitialShare},
		{"duplicate queue", []Queue{{Name: "a"}, {Name: "a"}}, nil, 0, Request{"a", 1}, LessThanInitialShare},
		{"empty queue name", []Queue{{Name: ""}}, nil, 0, Request{"", 1}, LessThanInitialShare},
		{"NaN nominal", []Queue{{Name: "a", Nominal: math.NaN()}}, nil, 0, Request{"a", 1}, LessThanInitialShare},
		{"negative usage", []Queue{{Name: "a", Usage: -1}}, nil, 0, Request{"a", 1}, LessThanInitialShare},
		{"Inf weight", []Queue{{Name: "a", Weight: math.Inf(1)}}, nil, 0, Request{"a", 1}, LessThanInitialShare},
		{"unknown workload queue", ok, []Workload{{ID: "x", Queue: "zzz", GPUs: 1}}, 0, Request{"a", 1}, LessThanInitialShare},
		{"duplicate workload", ok, []Workload{{ID: "x", Queue: "b", GPUs: 1}, {ID: "x", Queue: "b", GPUs: 1}}, 0, Request{"a", 1}, LessThanInitialShare},
		{"zero GPU workload", ok, []Workload{{ID: "x", Queue: "b"}}, 0, Request{"a", 1}, LessThanInitialShare},
		{"NaN free", ok, nil, math.NaN(), Request{"a", 1}, LessThanInitialShare},
		{"zero request", ok, nil, 0, Request{"a", 0}, LessThanInitialShare},
		{"huge request", ok, nil, 0, Request{"a", 1e30}, LessThanInitialShare},
		{"unknown request queue", ok, nil, 0, Request{"nope", 1}, LessThanInitialShare},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := SelectVictims(c.q, c.w, c.free, c.req, c.s); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
