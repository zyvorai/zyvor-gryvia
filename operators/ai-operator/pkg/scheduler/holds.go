// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package scheduler

import (
	"sync"
	"time"
)

// GPUHolds hold the GPUs of jobs that have been placed but whose pods are not bound to a node yet.
//
// Placement counts the GPUs of pods that are bound to a node, so between the moment a job is placed and the
// moment kube-scheduler binds its pods, two jobs reconciled in that window both see the same GPUs as free and
// pick the same nodes. A hold closes that window: while a job's pods are coming up, the GPUs of its
// chosen nodes count as used for every other job's placement. It never changes where a job is placed, only
// what the others see as free, so fabric-aware ranking and every filter stay as they are.
//
// State is in memory. After an operator restart holds are gone, which only reopens the window for
// jobs placed just before the restart; every hold also expires on its own, so a missed release cannot
// hold GPUs for long.
type GPUHolds struct {
	mu   sync.Mutex
	held map[string]hold
}

type hold struct {
	nodes       []string
	gpusPerNode int64
	expires     time.Time
}

// NewGPUHolds returns an empty set.
func NewGPUHolds() *GPUHolds {
	return &GPUHolds{held: map[string]hold{}}
}

// Hold holds gpusPerNode GPUs on each of nodes for the job key until now+ttl, replacing any earlier
// hold of the same job. A hold with no nodes or no GPUs is not stored.
func (r *GPUHolds) Hold(key string, nodes []string, gpusPerNode int64, ttl time.Duration, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(nodes) == 0 || gpusPerNode <= 0 || ttl <= 0 {
		delete(r.held, key)
		return
	}
	r.held[key] = hold{nodes: append([]string(nil), nodes...), gpusPerNode: gpusPerNode, expires: now.Add(ttl)}
}

// Release drops the job's hold, if any.
func (r *GPUHolds) Release(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.held, key)
}

// Held returns the GPUs held per node by every job except excludeKey (a job never competes with its own
// hold, so re-placing it does not count its own nodes twice). Expired holds are dropped.
func (r *GPUHolds) Held(excludeKey string, now time.Time) map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int64{}
	for key, res := range r.held {
		if !res.expires.After(now) {
			delete(r.held, key)
			continue
		}
		if key == excludeKey {
			continue
		}
		for _, n := range res.nodes {
			out[n] += res.gpusPerNode
		}
	}
	return out
}

// Len returns the number of live holds.
func (r *GPUHolds) Len(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for key, res := range r.held {
		if !res.expires.After(now) {
			delete(r.held, key)
			continue
		}
		n++
	}
	return n
}
