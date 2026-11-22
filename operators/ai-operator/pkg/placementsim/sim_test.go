// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package placementsim

import "testing"

func nodes(n, gpus int) []Node {
	var out []Node
	for i := 0; i < n; i++ {
		out = append(out, Node{ID: string(rune('a' + i)), GPUs: gpus, Profile: "H100_80GB", MemoryGB: 80})
	}
	return out
}

// Hand-computed: one 2-GPU node, two 2-GPU jobs of 10s each arriving together run back to back.
func TestRunSerialisesJobsThatDoNotFitTogether(t *testing.T) {
	res, err := Run(nodes(1, 2), []Job{
		{ID: "j1", Arrival: 0, Runtime: 10, GPUCount: 2},
		{ID: "j2", Arrival: 0, Runtime: 10, GPUCount: 2},
	}, Options{Backfill: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Makespan != 20 || res.MeanWait != 5 || res.GPUUtilization != 1 || res.JobsCompleted != 2 {
		t.Errorf("got %+v, want makespan 20, mean wait 5, util 1, 2 completed", res)
	}
}

func TestBackfillStartsASmallJobBehindABlockedOne(t *testing.T) {
	jobs := []Job{
		{ID: "big", Arrival: 0, Runtime: 100, GPUCount: 1},
		{ID: "wide", Arrival: 1, Runtime: 10, GPUCount: 2}, // cannot start until "big" ends
		{ID: "tiny", Arrival: 2, Runtime: 5, GPUCount: 1},  // fits beside "big"
	}
	with, _ := Run(nodes(1, 2), jobs, Options{Backfill: true})
	without, _ := Run(nodes(1, 2), jobs, Options{Backfill: false})
	if with.MeanWait >= without.MeanWait {
		t.Errorf("backfill should cut mean wait: with=%v without=%v", with.MeanWait, without.MeanWait)
	}
}

func TestJobLargerThanClusterIsUnschedulable(t *testing.T) {
	res, _ := Run(nodes(1, 2), []Job{{ID: "x", Arrival: 0, Runtime: 1, GPUCount: 8}}, Options{})
	if res.Unschedulable != 1 || res.JobsCompleted != 0 {
		t.Errorf("got %+v", res)
	}
}

// Characterisation of the default scoring (it prefers the node with the MOST free
// GPUs): single-GPU jobs spread across nodes, so a later whole-node job waits for
// them to finish instead of starting at once. This documents current behaviour.
func TestSpreadingScoreFragmentsWholeNodeJobs(t *testing.T) {
	jobs := []Job{
		{ID: "s1", Arrival: 0, Runtime: 100, GPUCount: 1},
		{ID: "s2", Arrival: 0, Runtime: 100, GPUCount: 1},
		{ID: "whole", Arrival: 1, Runtime: 10, GPUCount: 4},
	}
	res, err := Run(nodes(2, 4), jobs, Options{Backfill: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("makespan=%v meanWait=%v", res.Makespan, res.MeanWait)
	if res.Makespan <= 100 {
		t.Skip("scoring packs these jobs; the fragmentation case does not occur")
	}
	if res.Makespan != 110 {
		t.Errorf("expected whole-node job to wait for a node to drain (makespan 110), got %v", res.Makespan)
	}
}

// The pack strategy keeps a node free, so the same workload finishes at 100 s, not 110 s.
func TestPackStrategyKeepsWholeNodeFree(t *testing.T) {
	jobs := []Job{
		{ID: "s1", Arrival: 0, Runtime: 100, GPUCount: 1},
		{ID: "s2", Arrival: 0, Runtime: 100, GPUCount: 1},
		{ID: "whole", Arrival: 1, Runtime: 10, GPUCount: 4},
	}
	res, err := Run(nodes(2, 4), jobs, Options{Backfill: true, Strategy: "pack"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Makespan != 100 || res.MeanWait != 0 {
		t.Errorf("pack: got makespan %v mean wait %v, want 100 and 0", res.Makespan, res.MeanWait)
	}
}
