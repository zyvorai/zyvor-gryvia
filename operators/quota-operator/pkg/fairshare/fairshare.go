// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package fairshare computes weighted borrowing shares for tenant GPU queues and chooses which
// running workloads to preempt so a queue that is under its quota can reclaim capacity others
// borrowed. It follows the model of Kueue fair sharing (a share is borrowed capacity divided by
// the queue weight; admission favours the lowest share, preemption targets the highest) and of
// Run:ai reclaim (borrowed, preemptible work is taken back; a queue is never pushed below its
// own quota). See docs/RESEARCH.md. The strategy rules are our reading of the Kueue strategy names
// LessThanOrEqualToFinalShare and LessThanInitialShare, not a copy of Kueue code.
//
// It is pure: no I/O, no clock. Callers pass a snapshot. Nothing in the operators calls it yet;
// it is the tested decision core for a future admission/reclaim controller.
package fairshare

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// Caps that keep a hostile snapshot from making the planner do unbounded work.
const (
	MaxQueues    = 1000
	MaxWorkloads = 100000
	MaxGPUs      = 1e9
)

// Strategy selects when a candidate queue may be preempted.
type Strategy string

const (
	// LessThanOrEqualToFinalShare: the preemptor share after admission must be <= the target share after the preemption.
	LessThanOrEqualToFinalShare Strategy = "LessThanOrEqualToFinalShare"
	// LessThanInitialShare: the preemptor share after admission must be < the target share before the preemption.
	LessThanInitialShare Strategy = "LessThanInitialShare"
)

// Queue is a tenant queue. Nominal is its guaranteed GPUs, Usage what it runs now.
type Queue struct {
	Name    string
	Nominal float64
	Usage   float64
	Weight  float64 // 0 is treated as 1
}

// Workload is a running workload that may be a preemption victim.
type Workload struct {
	ID          string
	Queue       string
	GPUs        float64
	Priority    int32
	Preemptible bool
}

// Request is the workload that wants to start.
type Request struct {
	Queue string
	GPUs  float64
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= MaxGPUs }

func weight(q Queue) float64 {
	if q.Weight == 0 {
		return 1
	}
	return q.Weight
}

// Share is the weighted borrowing share of a queue at a given usage: max(0, usage-nominal)/weight.
func Share(q Queue, usage float64) float64 {
	b := usage - q.Nominal
	if b <= 0 {
		return 0
	}
	return b / weight(q)
}

// Validate checks a snapshot.
func Validate(queues []Queue, running []Workload) error {
	if len(queues) > MaxQueues || len(running) > MaxWorkloads {
		return fmt.Errorf("snapshot too large (max %d queues, %d workloads)", MaxQueues, MaxWorkloads)
	}
	names := map[string]bool{}
	for _, q := range queues {
		if q.Name == "" || names[q.Name] {
			return errors.New("queue names must be unique and non-empty")
		}
		names[q.Name] = true
		if !finite(q.Nominal) || !finite(q.Usage) || !finite(q.Weight) {
			return fmt.Errorf("queue %q has a non-finite or negative number", q.Name)
		}
	}
	ids := map[string]bool{}
	for _, w := range running {
		if w.ID == "" || ids[w.ID] {
			return errors.New("workload IDs must be unique and non-empty")
		}
		ids[w.ID] = true
		if !names[w.Queue] {
			return fmt.Errorf("workload %q is in unknown queue %q", w.ID, w.Queue)
		}
		if !finite(w.GPUs) || w.GPUs == 0 {
			return fmt.Errorf("workload %q has an invalid GPU count", w.ID)
		}
	}
	return nil
}

// Plan is the outcome of SelectVictims.
type Plan struct {
	Victims []string // workload IDs, in preemption order
	Freed   float64
	Fits    bool // true when free + Freed covers the request
}

// SelectVictims picks workloads to preempt so req fits in free GPUs. Only preemptible workloads in
// other queues that are borrowing (usage above nominal) are candidates, and a victim is never taken
// if that would push its queue below its nominal quota. Candidates are taken from the queue with
// the highest share first, then lowest priority, then largest workload (fewest victims), then ID.
// When the request cannot be satisfied it returns an empty plan with Fits=false (no partial plan).
func SelectVictims(queues []Queue, running []Workload, free float64, req Request, s Strategy) (Plan, error) {
	if s != LessThanOrEqualToFinalShare && s != LessThanInitialShare {
		return Plan{}, fmt.Errorf("unknown strategy %q", s)
	}
	if err := Validate(queues, running); err != nil {
		return Plan{}, err
	}
	if !finite(free) || !finite(req.GPUs) || req.GPUs == 0 {
		return Plan{}, errors.New("free and request GPUs must be finite, non-negative and the request non-zero")
	}
	byName := map[string]*Queue{}
	for i := range queues {
		q := queues[i]
		byName[q.Name] = &q
	}
	pre, ok := byName[req.Queue]
	if !ok {
		return Plan{}, fmt.Errorf("unknown request queue %q", req.Queue)
	}
	if free >= req.GPUs {
		return Plan{Fits: true}, nil
	}
	preAfter := Share(*pre, pre.Usage+req.GPUs)
	cands := map[string][]Workload{}
	for _, w := range running {
		if w.Queue != req.Queue && w.Preemptible {
			cands[w.Queue] = append(cands[w.Queue], w)
		}
	}
	for _, l := range cands {
		sort.Slice(l, func(i, j int) bool {
			a, b := l[i], l[j]
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			if a.GPUs != b.GPUs {
				return a.GPUs > b.GPUs
			}
			return a.ID < b.ID
		})
	}
	var plan Plan
	need := req.GPUs - free
	for plan.Freed < need {
		names := make([]string, 0, len(cands))
		for n := range cands {
			names = append(names, n)
		}
		sort.Strings(names)
		var best *Queue
		bestShare := -1.0
		for _, n := range names {
			if sh := Share(*byName[n], byName[n].Usage); sh > bestShare {
				best, bestShare = byName[n], sh
			}
		}
		if best == nil || bestShare <= 0 {
			return Plan{}, nil
		}
		l := cands[best.Name]
		picked := -1
		for i, w := range l {
			after := best.Usage - w.GPUs
			if after < best.Nominal {
				continue // never push a queue below its own quota
			}
			finalShare := Share(*best, after)
			if (s == LessThanOrEqualToFinalShare && preAfter <= finalShare) || (s == LessThanInitialShare && preAfter < bestShare) {
				picked = i
				break
			}
		}
		if picked < 0 {
			delete(cands, best.Name)
			continue
		}
		w := l[picked]
		cands[best.Name] = append(l[:picked:picked], l[picked+1:]...)
		best.Usage -= w.GPUs
		plan.Victims = append(plan.Victims, w.ID)
		plan.Freed += w.GPUs
	}
	plan.Fits = true
	return plan, nil
}
