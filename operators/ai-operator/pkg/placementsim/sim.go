// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package placementsim replays a Janus-style cluster and workload through the
// operator's real placement code (scheduler.FindOptimalNodesHeld) with a fake
// Kubernetes client, and reports makespan, wait and GPU utilisation. It needs no
// GPUs; it checks placement quality, not NCCL, RDMA or training performance.
//
// Modelled: whole-GPU jobs, FIFO queue (with Options.Backfill, like Janus' fifo,
// a job that does not fit is skipped; without it the head blocks), nodes as bins of GPUs. Not modelled: NVLink/PCIe topology penalties,
// MIG, preemption, GPU memory as a placement constraint (violations are counted).
package placementsim

import (
	"context"
	"fmt"
	"math"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/scheduler"
)

// Node is one simulated node.
type Node struct {
	ID       string
	GPUs     int
	Profile  string
	MemoryGB int
}

// Job is one simulated job.
type Job struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Arrival     float64 `json:"arrival_time"`
	Runtime     float64 `json:"runtime"`
	GPUCount    int     `json:"gpu_count"`
	GPUMemoryGB int     `json:"gpu_memory_gb"`
}

// Result are the headline numbers; the names follow Janus' metrics.json.
type Result struct {
	Makespan        float64 `json:"makespan"`
	MeanWait        float64 `json:"mean_wait_time"`
	GPUUtilization  float64 `json:"gpu_utilization"`
	JobsCompleted   int     `json:"jobs_completed"`
	JobsTotal       int     `json:"jobs_total"`
	Unschedulable   int     `json:"jobs_unschedulable"`
	MemoryViolation int     `json:"memory_violations"`
}

type clusterFile struct {
	Cluster struct {
		Nodes []struct {
			ID   string `json:"id"`
			GPUs []struct {
				Profile string `json:"profile"`
			} `json:"gpus"`
		} `json:"nodes"`
	} `json:"cluster"`
	Workload struct {
		Path string `json:"path"`
	} `json:"workload"`
}

type workloadFile struct {
	Jobs []Job `json:"jobs"`
}

// ParseCluster reads a Janus cluster YAML. memoryGB maps profile name to GPU memory.
func ParseCluster(b []byte, memoryGB map[string]int) ([]Node, string, error) {
	var f clusterFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, "", err
	}
	var nodes []Node
	for _, n := range f.Cluster.Nodes {
		if len(n.GPUs) == 0 {
			continue
		}
		p := n.GPUs[0].Profile
		nodes = append(nodes, Node{ID: n.ID, GPUs: len(n.GPUs), Profile: p, MemoryGB: memoryGB[p]})
	}
	if len(nodes) == 0 {
		return nil, "", fmt.Errorf("cluster has no GPU nodes")
	}
	return nodes, f.Workload.Path, nil
}

// ParseWorkload reads a Janus workload YAML.
func ParseWorkload(b []byte) ([]Job, error) {
	var f workloadFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return f.Jobs, nil
}

type running struct {
	job   Job
	end   float64
	nodes map[string]int // node -> GPUs held
}

// Options tune the queue discipline.
type Options struct {
	// Backfill lets a later job start when an earlier one does not fit (Janus' fifo does).
	Backfill bool
	// Strategy is the gryvia.io/placement-strategy annotation put on every job ("" = default spread).
	Strategy string
}

// Run simulates the workload on the cluster.
func Run(nodes []Node, jobs []Job, opt Options) (Result, error) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return Result{}, err
	}
	if err := gryviav1.AddToScheme(scheme); err != nil {
		return Result{}, err
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	maxPer, totalGPUs := 0, 0
	byName := map[string]Node{}
	for _, n := range nodes {
		if n.GPUs > maxPer {
			maxPer = n.GPUs
		}
		totalGPUs += n.GPUs
		byName[n.ID] = n
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: n.ID, Labels: map[string]string{
				"gryvia.io/gpu": n.Profile, "gryvia.io/gpu-count": fmt.Sprint(n.GPUs),
				"gryvia.io/gpu-memory": fmt.Sprint(n.MemoryGB)}},
			Status: corev1.NodeStatus{
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				Allocatable: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(int64(n.GPUs), resource.DecimalSI)},
			},
		}
		if err := c.Create(ctx, node); err != nil {
			return Result{}, err
		}
	}

	pending := append([]Job(nil), jobs...)
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].Arrival < pending[j].Arrival })
	res := Result{JobsTotal: len(jobs)}
	var runs []*running
	var now, waitSum, gpuSeconds float64
	var queue []Job

	place := func(j Job) (*running, error) {
		spec := gryviav1.GryviaAIJobSpec{GPUs: int32(j.GPUCount)}
		per := j.GPUCount
		if j.GPUCount > maxPer {
			n := int(math.Ceil(float64(j.GPUCount) / float64(maxPer)))
			spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: int32(n), GpusPerNode: int32(maxPer)}
			per = maxPer
		}
		job := &gryviav1.GryviaAIJob{ObjectMeta: metav1.ObjectMeta{Name: j.ID, Namespace: "sim"}, Spec: spec}
		if opt.Strategy != "" {
			job.Annotations = map[string]string{scheduler.AnnotationPlacementStrategy: opt.Strategy}
		}
		chosen, err := scheduler.FindOptimalNodesHeld(ctx, c, job, nil)
		if err != nil {
			return nil, err
		}
		r := &running{job: j, end: now + j.Runtime, nodes: map[string]int{}}
		left := j.GPUCount
		for _, name := range chosen {
			g := per
			if left < g {
				g = left
			}
			left -= g
			r.nodes[name] = g
			if j.GPUMemoryGB > 0 && byName[name].MemoryGB > 0 && byName[name].MemoryGB < j.GPUMemoryGB {
				res.MemoryViolation++
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%s", j.ID, name), Namespace: "sim"},
				Spec: corev1.PodSpec{NodeName: name, Containers: []corev1.Container{{Name: "t", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(int64(g), resource.DecimalSI)}}}}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}
			if err := c.Create(ctx, pod); err != nil {
				return nil, err
			}
		}
		return r, nil
	}
	finish := func(r *running) error {
		for name := range r.nodes {
			p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%s", r.job.ID, name), Namespace: "sim"}}
			if err := c.Delete(ctx, p); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		gpuSeconds += float64(r.job.GPUCount) * r.job.Runtime
		res.JobsCompleted++
		return nil
	}

	for len(pending) > 0 || len(queue) > 0 || len(runs) > 0 {
		// Admit arrivals up to now.
		for len(pending) > 0 && pending[0].Arrival <= now {
			queue = append(queue, pending[0])
			pending = pending[1:]
		}
		// Start what fits, in arrival order. Without backfill the first job that does not fit stops the pass.
		for i := 0; i < len(queue); {
			if queue[i].GPUCount > totalGPUs {
				res.Unschedulable++
				queue = append(queue[:i], queue[i+1:]...)
				continue
			}
			r, err := place(queue[i])
			if err != nil {
				if !opt.Backfill {
					break
				}
				i++
				continue
			}
			waitSum += now - queue[i].Arrival
			runs = append(runs, r)
			queue = append(queue[:i], queue[i+1:]...)
		}
		// Advance to the next event: a completion or an arrival.
		next := math.Inf(1)
		for _, r := range runs {
			next = math.Min(next, r.end)
		}
		if len(pending) > 0 {
			next = math.Min(next, pending[0].Arrival)
		}
		if math.IsInf(next, 1) {
			if len(queue) > 0 {
				res.Unschedulable += len(queue) // nothing running and the head still cannot start
			}
			break
		}
		now = next
		kept := runs[:0]
		for _, r := range runs {
			if r.end <= now {
				if err := finish(r); err != nil {
					return res, err
				}
				continue
			}
			kept = append(kept, r)
		}
		runs = kept
	}
	res.Makespan = now
	if started := res.JobsCompleted; started > 0 {
		res.MeanWait = waitSum / float64(started)
	}
	if now > 0 && totalGPUs > 0 {
		res.GPUUtilization = gpuSeconds / (now * float64(totalGPUs))
	}
	return res, nil
}
