// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package idlecost attributes the cost of idle (provisioned but unallocated) GPU capacity to
// the cost centers that used the cluster, in proportion to what they spent. This mirrors the
// idle-cost sharing of OpenCost (idle = asset cost minus workload cost) so a chargeback adds
// up to the real GPU bill, not only to the metered job cost.
//
// It is pure: no clock, no I/O. Amounts are rounded to cents with the largest-remainder rule,
// so the shares always sum exactly to the allocated idle cost.
package idlecost

import (
	"fmt"
	"math"
	"sort"
)

// MaxCenters bounds the input so a hostile object cannot make the controller do unbounded work.
const MaxCenters = 10000

// MaxAmount bounds every amount; larger values are refused instead of losing cent precision.
const MaxAmount = 1e12

// Input is the metered cost of one cost center.
type Input struct {
	ID   string
	Cost float64
}

// Share is one cost center after idle attribution.
type Share struct {
	ID   string
	Cost float64
	Idle float64
}

// Result is the attribution. Unallocated is the idle cost nobody could be charged for (no center
// had any cost); Allocated + Unallocated always equals the idle cost passed in (rounded to cents).
type Result struct {
	Shares      []Share
	Allocated   float64
	Unallocated float64
}

func valid(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= MaxAmount }

func cents(v float64) int64 { return int64(math.Round(v * 100)) }

// IdleCost is the cost of capacity that was provisioned but not allocated:
// (capacityGPUHours - allocatedGPUHours) * rate, never negative.
func IdleCost(capacityGPUHours, allocatedGPUHours, rate float64) (float64, error) {
	if !valid(capacityGPUHours) || !valid(allocatedGPUHours) || !valid(rate) {
		return 0, fmt.Errorf("capacity, allocation and rate must be finite, non-negative and at most %g", MaxAmount)
	}
	idle := capacityGPUHours - allocatedGPUHours
	if idle < 0 {
		idle = 0
	}
	return math.Round(idle*rate*100) / 100, nil
}

// Allocate splits idle across inputs proportionally to Cost. Order of the result follows the
// input order. IDs must be non-empty and unique.
func Allocate(idle float64, in []Input) (Result, error) {
	if !valid(idle) {
		return Result{}, fmt.Errorf("idle cost must be finite, non-negative and at most %g", MaxAmount)
	}
	if len(in) > MaxCenters {
		return Result{}, fmt.Errorf("at most %d cost centers are supported", MaxCenters)
	}
	seen := make(map[string]bool, len(in))
	var total int64
	for _, c := range in {
		if c.ID == "" || seen[c.ID] {
			return Result{}, fmt.Errorf("cost center IDs must be unique and non-empty")
		}
		seen[c.ID] = true
		if !valid(c.Cost) {
			return Result{}, fmt.Errorf("cost center %q has an invalid cost", c.ID)
		}
		total += cents(c.Cost)
	}
	idleC := cents(idle)
	res := Result{Shares: make([]Share, len(in))}
	for i, c := range in {
		res.Shares[i] = Share{ID: c.ID, Cost: c.Cost}
	}
	if total == 0 || idleC == 0 {
		res.Unallocated = float64(idleC) / 100
		return res, nil
	}
	type rem struct {
		i    int
		frac float64
	}
	parts := make([]int64, len(in))
	rems := make([]rem, 0, len(in))
	var assigned int64
	for i, c := range in {
		exact := float64(idleC) * float64(cents(c.Cost)) / float64(total)
		parts[i] = int64(math.Floor(exact))
		assigned += parts[i]
		rems = append(rems, rem{i, exact - math.Floor(exact)})
	}
	sort.SliceStable(rems, func(a, b int) bool {
		if rems[a].frac != rems[b].frac {
			return rems[a].frac > rems[b].frac
		}
		return in[rems[a].i].ID < in[rems[b].i].ID
	})
	for k := int64(0); k < idleC-assigned; k++ {
		parts[rems[k].i]++
	}
	for i := range res.Shares {
		res.Shares[i].Idle = float64(parts[i]) / 100
	}
	res.Allocated = float64(idleC) / 100
	return res, nil
}
