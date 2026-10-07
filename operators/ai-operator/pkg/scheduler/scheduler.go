package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// NodeScore represents a node with its scheduling score
type NodeScore struct {
	NodeName string
	Score    int
}

// FindOptimalNodes finds the best nodes for running an AI job
func FindOptimalNodes(ctx context.Context, k8sClient client.Client, job *gryviav1.GryviaAIJob) ([]string, error) {
	p, err := findOptimalNodes(ctx, k8sClient, job, nil, nil)
	return p.Nodes, err
}

// FindOptimalNodesHeld is FindOptimalNodes that also counts held GPUs per node (see Reservations) as used.
func FindOptimalNodesHeld(ctx context.Context, k8sClient client.Client, job *gryviav1.GryviaAIJob, held map[string]int64) ([]string, error) {
	p, err := findOptimalNodes(ctx, k8sClient, job, nil, held)
	return p.Nodes, err
}

// GPUsPerPlacedNode is how many GPUs the placement looks for on each node it picks: GpusPerNode for a
// distributed job that sets it, otherwise spec.gpus.
func GPUsPerPlacedNode(job *gryviav1.GryviaAIJob) int32 {
	if job.Spec.Distributed != nil && job.Spec.Distributed.Enabled && job.Spec.Distributed.GpusPerNode > 0 {
		return job.Spec.Distributed.GpusPerNode
	}
	return job.Spec.GPUs
}

// FindOptimalNodesFabric is FindOptimalNodes with fabric-aware ranking: fresh
// per-node fabric penalties (capped, see FabricOptions) are subtracted from the
// scores before the node choice, and the changes are returned as an explanation.
// The caller decides the opt-in (see FabricEnabled).
func FindOptimalNodesFabric(ctx context.Context, k8sClient client.Client, job *gryviav1.GryviaAIJob, opt FabricOptions) (Placement, error) {
	return findOptimalNodes(ctx, k8sClient, job, &opt, nil)
}

// FindOptimalNodesFabricHeld is FindOptimalNodesFabric that also counts held GPUs per node as used.
func FindOptimalNodesFabricHeld(ctx context.Context, k8sClient client.Client, job *gryviav1.GryviaAIJob, opt FabricOptions, held map[string]int64) (Placement, error) {
	return findOptimalNodes(ctx, k8sClient, job, &opt, held)
}

func findOptimalNodes(ctx context.Context, k8sClient client.Client, job *gryviav1.GryviaAIJob, fab *FabricOptions, held map[string]int64) (Placement, error) {
	// Get all nodes
	nodes := &corev1.NodeList{}
	if err := k8sClient.List(ctx, nodes); err != nil {
		return Placement{}, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Get all non-terminal pods requesting GPUs to calculate usage per node.
	// We filter by the nvidia.com/gpu resource in the field selector to avoid
	// loading every pod in the cluster. Since field selectors don't support
	// resource requests, we use a label selector for running pods instead.
	pods := &corev1.PodList{}
	if err := k8sClient.List(ctx, pods, client.MatchingFields{"status.phase": "Running"}); err != nil {
		// Fall back to listing all pods if field selector is not indexed
		pods = &corev1.PodList{}
		if err := k8sClient.List(ctx, pods); err != nil {
			return Placement{}, fmt.Errorf("failed to list pods: %w", err)
		}
	}

	gpuUsagePerNode := calculateGPUUsagePerNode(pods.Items)
	// GPUs reserved for jobs placed but not yet bound count as used.
	for node, h := range held {
		gpuUsagePerNode[node] += h
	}

	// Determine GPUs needed per node
	gpusNeeded := GPUsPerPlacedNode(job)

	// Filter nodes based on job requirements
	eligibleNodes := filterNodes(nodes.Items, job, gpuUsagePerNode, gpusNeeded)
	if len(eligibleNodes) == 0 {
		return Placement{}, fmt.Errorf("no nodes meet the job requirements (gpuType=%q, gpus=%d, network=%q, %d nodes evaluated)",
			job.Spec.GpuType, gpusNeeded, job.Spec.Network, len(nodes.Items))
	}

	// Score nodes
	scoredNodes := scoreNodes(eligibleNodes, job, gpuUsagePerNode)

	var explanation []gryviav1.PlacementExplanation
	if fab == nil {
		// Sort by score (highest first)
		sort.Slice(scoredNodes, func(i, j int) bool {
			return scoredNodes[i].Score > scoredNodes[j].Score
		})
	} else {
		scoredNodes, explanation = rankFabric(ctx, k8sClient, scoredNodes, *fab)
	}
	scoredNodes = preferLocal(scoredNodes, eligibleNodes, preferredPools(ctx))

	// Determine how many nodes we need
	requiredNodes := 1
	if job.Spec.Distributed != nil && job.Spec.Distributed.Enabled {
		requiredNodes = int(job.Spec.Distributed.Nodes)
	}
	if min, max, ok := job.Spec.Distributed.ElasticBounds(); ok {
		// Elastic: start as soon as minNodes qualify, and take as many as are available up to maxNodes.
		requiredNodes = int(min)
		if want := int(max); len(scoredNodes) > requiredNodes {
			requiredNodes = len(scoredNodes)
			if requiredNodes > want {
				requiredNodes = want
			}
		}
	}

	if len(scoredNodes) < requiredNodes {
		return Placement{}, fmt.Errorf("not enough nodes: need %d, found %d", requiredNodes, len(scoredNodes))
	}

	// Select top N nodes
	selectedNodes := make([]string, requiredNodes)
	for i := 0; i < requiredNodes; i++ {
		selectedNodes[i] = scoredNodes[i].NodeName
	}

	return Placement{Nodes: selectedNodes, Explanation: explanation}, nil
}

// rankFabric loads the node signals (fail open) and ranks with them.
func rankFabric(ctx context.Context, k8sClient client.Client, scored []NodeScore, opt FabricOptions) ([]NodeScore, []gryviav1.PlacementExplanation) {
	now := time.Now
	if opt.Now != nil {
		now = opt.Now
	}
	load := opt.Signals
	if load == nil {
		load = func(ctx context.Context) ([]NodeSignal, error) { return LoadNodeSignals(ctx, k8sClient) }
	}
	sigs, err := load(ctx)
	if err != nil {
		if opt.OnError != nil {
			opt.OnError(err)
		}
		sigs = nil // never penalise on missing data
	}
	t := now()
	return rankWithFabric(scored, FreshSignals(sigs, t), opt, t)
}

// calculateGPUUsagePerNode sums the nvidia.com/gpu requests across all
// non-terminal pods scheduled on each node.
func calculateGPUUsagePerNode(pods []corev1.Pod) map[string]int64 {
	usage := make(map[string]int64)
	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if gpuReq, ok := c.Resources.Requests["nvidia.com/gpu"]; ok {
				usage[pod.Spec.NodeName] += gpuReq.Value()
			} else if gpuLim, ok := c.Resources.Limits["nvidia.com/gpu"]; ok {
				usage[pod.Spec.NodeName] += gpuLim.Value()
			}
		}
	}
	return usage
}

// filterNodes filters nodes based on job requirements and GPU availability
func filterNodes(nodes []corev1.Node, job *gryviav1.GryviaAIJob, gpuUsage map[string]int64, gpusNeeded int32) []corev1.Node {
	var eligible []corev1.Node

	for _, node := range nodes {
		// Check if node is ready
		if !isNodeReady(node) {
			continue
		}

		// Check GPU type if specified
		if job.Spec.GpuType != "" && job.Spec.GpuType != "any" {
			if gpuType, exists := node.Labels["gryvia.io/gpu"]; !exists || gpuType != job.Spec.GpuType {
				continue
			}
		}

		// Check RDMA requirement
		if job.Spec.Network == "rdma" {
			if rdma, exists := node.Labels["gryvia.io/rdma"]; !exists || rdma != "true" {
				continue
			}
		}

		// Check SR-IOV requirement
		if job.Spec.Network == "sriov" {
			if sriov, exists := node.Labels["gryvia.io/sriov"]; !exists || sriov != "true" {
				continue
			}
		}

		// Check custom node selector
		if !matchesNodeSelector(node, job.Spec.NodeSelector) {
			continue
		}

		// Check GPU availability: node must have enough free GPUs
		availableGPUs := getAvailableGPUs(node, gpuUsage)
		if availableGPUs < int64(gpusNeeded) {
			continue
		}

		eligible = append(eligible, node)
	}

	return eligible
}

// AnnotationPlacementStrategy selects the node-scoring strategy for one job: "pack" (best-fit,
// keeps whole nodes free) or anything else for the default spread.
const (
	AnnotationPlacementStrategy = "gryvia.io/placement-strategy"
	StrategyPack                = "pack"
)

// totalGPUsOf is the node's GPU count (label first, then nvidia.com/gpu allocatable).
func totalGPUsOf(node corev1.Node) int64 {
	return getAvailableGPUs(node, nil)
}

// getAvailableGPUs returns the number of GPUs available on a node.
// It checks both the gryvia.io/gpu-count label and the
// nvidia.com/gpu allocatable resource, then subtracts current usage.
func getAvailableGPUs(node corev1.Node, gpuUsage map[string]int64) int64 {
	var totalGPUs int64

	// First try the gryvia label (set by the GPU operator)
	if countStr, exists := node.Labels["gryvia.io/gpu-count"]; exists {
		if count, err := strconv.ParseInt(countStr, 10, 64); err == nil {
			totalGPUs = count
		}
	}

	// Fall back to the nvidia.com/gpu allocatable resource
	if totalGPUs == 0 {
		if gpuResource, ok := node.Status.Allocatable["nvidia.com/gpu"]; ok {
			totalGPUs = gpuResource.Value()
		}
	}

	available := totalGPUs - gpuUsage[node.Name]
	if available < 0 {
		return 0
	}
	return available
}

// scoreNodes assigns a score to each node based on various factors
func scoreNodes(nodes []corev1.Node, job *gryviav1.GryviaAIJob, gpuUsage map[string]int64) []NodeScore {
	scored := make([]NodeScore, len(nodes))

	for i, node := range nodes {
		score := 0

		// Prefer nodes with matching GPU type
		if job.Spec.GpuType != "" {
			if gpuType, exists := node.Labels["gryvia.io/gpu"]; exists && gpuType == job.Spec.GpuType {
				score += 50
			}
		}

		// Prefer nodes with RDMA if requested
		if job.Spec.Network == "rdma" {
			if rdma, exists := node.Labels["gryvia.io/rdma"]; exists && rdma == "true" {
				score += 30
			}
		}

		// Prefer nodes with NVLink/NVSwitch for multi-GPU jobs
		if job.Spec.GPUs > 1 {
			if interconnect, exists := node.Labels["gryvia.io/interconnect"]; exists {
				if interconnect == "NVSwitch" {
					score += 40
				} else if interconnect == "NVLink" {
					score += 30
				}
			}
		}

		// Score based on available GPU count. Default spreads (more free GPUs wins); the
		// "pack" strategy is best-fit (fewer free GPUs that still fit wins), which keeps
		// whole nodes free for large jobs. See AnnotationPlacementStrategy.
		availableGPUs := getAvailableGPUs(node, gpuUsage)
		if job.Annotations[AnnotationPlacementStrategy] == StrategyPack {
			score += int(totalGPUsOf(node)-availableGPUs) * 5
		} else {
			score += int(availableGPUs) * 5
		}

		// Score based on GPU memory (from gryvia label)
		if memStr, exists := node.Labels["gryvia.io/gpu-memory"]; exists {
			if memGB, err := strconv.ParseInt(memStr, 10, 64); err == nil {
				score += int(memGB / 10) // 1 point per 10GB GPU memory
			}
		}

		// Score based on general node resources
		score += calculateResourceScore(node)

		scored[i] = NodeScore{
			NodeName: node.Name,
			Score:    score,
		}
	}

	return scored
}

// isNodeReady checks if a node is ready
func isNodeReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// matchesNodeSelector checks if node matches the selector
func matchesNodeSelector(node corev1.Node, selector map[string]string) bool {
	if selector == nil {
		return true
	}

	for key, value := range selector {
		if nodeValue, exists := node.Labels[key]; !exists || nodeValue != value {
			return false
		}
	}
	return true
}

// calculateResourceScore calculates a score based on available resources
func calculateResourceScore(node corev1.Node) int {
	score := 0

	// Prefer nodes with more allocatable memory
	if memory, ok := node.Status.Allocatable[corev1.ResourceMemory]; ok {
		memoryGB := memory.Value() / (1024 * 1024 * 1024)
		score += int(memoryGB / 100) // 1 point per 100GB
	}

	// Prefer nodes with more allocatable CPUs
	if cpu, ok := node.Status.Allocatable[corev1.ResourceCPU]; ok {
		score += int(cpu.Value() / 10) // 1 point per 10 CPUs
	}

	// Prefer nodes with more allocatable GPU capacity
	gpuResourceName := corev1.ResourceName("nvidia.com/gpu")
	if gpu, ok := node.Status.Allocatable[gpuResourceName]; ok {
		score += int(gpu.Value()) * 3 // 3 points per GPU capacity
	}

	return score
}
