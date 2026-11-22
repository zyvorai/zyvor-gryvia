// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Command placement-sim runs a Janus cluster config and workload through the
// operator's real node-selection code. See pkg/placementsim for what is modelled.
//
//	placement-sim -config ../janus/configs/clusters/small_h100.yaml [-hardware ../janus/configs/hardware]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/placementsim"
)

func main() {
	cfg := flag.String("config", "", "Janus cluster YAML (required)")
	hw := flag.String("hardware", "", "Janus hardware profile dir (default: hardware_profiles_dir from the config)")
	workload := flag.String("workload", "", "workload YAML (default: workload.path from the config)")
	backfill := flag.Bool("backfill", true, "skip a job that does not fit and start later ones (Janus fifo does)")
	strategy := flag.String("strategy", "", "placement strategy annotation for every job: pack | (empty = default spread)")
	asJSON := flag.Bool("json", false, "print JSON")
	flag.Parse()
	if *cfg == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*cfg, *hw, *workload, *asJSON, *backfill, *strategy); err != nil {
		fmt.Fprintln(os.Stderr, "placement-sim:", err)
		os.Exit(1)
	}
}

func run(cfgPath, hwDir, wlPath string, asJSON, backfill bool, strategy string) error {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var extra struct {
		Dir string `json:"hardware_profiles_dir"`
	}
	_ = yaml.Unmarshal(raw, &extra)
	base := filepath.Dir(cfgPath)
	if hwDir == "" && extra.Dir != "" {
		hwDir = filepath.Join(base, extra.Dir)
	}
	mem := map[string]int{}
	if hwDir != "" {
		files, _ := filepath.Glob(filepath.Join(hwDir, "*.yaml"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var p struct {
				Name     string `json:"name"`
				MemoryGB int    `json:"memory_gb"`
			}
			if yaml.Unmarshal(b, &p) == nil && p.Name != "" {
				mem[p.Name] = p.MemoryGB
			}
		}
	}
	nodes, path, err := placementsim.ParseCluster(raw, mem)
	if err != nil {
		return err
	}
	if wlPath == "" {
		wlPath = filepath.Join(base, path)
	}
	wb, err := os.ReadFile(wlPath)
	if err != nil {
		return err
	}
	jobs, err := placementsim.ParseWorkload(wb)
	if err != nil {
		return err
	}
	res, err := placementsim.Run(nodes, jobs, placementsim.Options{Backfill: backfill, Strategy: strategy})
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	fmt.Printf("Gryvia placement-sim (real FindOptimalNodesHeld, fake client)\n  makespan:          %.2f\n  mean wait time:    %.2f\n  gpu utilization:   %.2f%%\n  jobs completed:    %d/%d\n  unschedulable:     %d\n  memory violations: %d\n",
		res.Makespan, res.MeanWait, res.GPUUtilization*100, res.JobsCompleted, res.JobsTotal, res.Unschedulable, res.MemoryViolation)
	return nil
}
