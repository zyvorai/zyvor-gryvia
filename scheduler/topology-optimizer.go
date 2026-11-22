// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package main

import (
	"context"
	"fmt"
	"log"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// GPU Topology Optimizer
// Optimizes GPU placement for multi-GPU jobs based on NVLink topology

type TopologyOptimizer struct {
	clientset *kubernetes.Clientset
}

type GPUNode struct {
	Name         string
	GPUs         []GPUDevice
	NVLinks      map[int][]int // GPU index -> connected GPU indices
	CPUNUMANodes []int
}

type GPUDevice struct {
	Index       int
	UUID        string
	Type        string
	Memory      int64
	NUMANode    int
	PCIBusID    string
	NVLinkPeers []int
}

type PlacementScore struct {
	NodeName    string
	Score       float64
	Reasoning   string
	Topology    string
	NVLinkCount int
}

// Scoring weights for GPU placement (must sum to 100 with bonuses/penalties)
const (
	nvlinkWeight      = 40.0 // NVLink connectivity importance
	numaWeight        = 30.0 // NUMA locality importance
	utilizationWeight = 20.0 // Current utilization importance
	fullNodeBonus     = 10.0 // Bonus for full-node allocation
	fragmentPenalty   = 5.0  // Penalty for fragmenting a node
)

func NewTopologyOptimizer() (*TopologyOptimizer, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &TopologyOptimizer{
		clientset: clientset,
	}, nil
}

// GetGPUTopology discovers GPU topology on a node
func (to *TopologyOptimizer) GetGPUTopology(nodeName string) (*GPUNode, error) {
	// In production, query NVML/nvidia-smi for actual topology
	// For now, return example topology

	// Example: DGX A100 with 8 GPUs
	if nodeName == "dgx-a100-1" {
		return &GPUNode{
			Name: nodeName,
			GPUs: []GPUDevice{
				{Index: 0, Type: "A100-80G", Memory: 80 * 1024 * 1024 * 1024, NUMANode: 0, NVLinkPeers: []int{1, 2, 3, 4, 5, 6, 7}},
				{Index: 1, Type: "A100-80G", Memory: 80 * 1024 * 1024 * 1024, NUMANode: 0, NVLinkPeers: []int{0, 2, 3, 4, 5, 6, 7}},
				{Index: 2, Type: "A100-80G", Memory: 80 * 1024 * 1024 * 1024, NUMANode: 0, NVLinkPeers: []int{0, 1, 3, 4, 5, 6, 7}},
				{Index: 3, Type: "A100-80G", Memory: 80 * 1024 * 1024 * 1024, NUMANode: 0, NVLinkPeers: []int{0, 1, 2, 4, 5, 6, 7}},
				{Index: 4, Type: "A100-80G", Memory: 80 * 1024 * 1024 * 1024, NUMANode: 1, NVLinkPeers: []int{0, 1, 2, 3, 5, 6, 7}},
				{Index: 5, Type: "A100-80G", Memory: 80 * 1024 * 1024 * 1024, NUMANode: 1, NVLinkPeers: []int{0, 1, 2, 3, 4, 6, 7}},
				{Index: 6, Type: "A100-80G", Memory: 80 * 1024 * 1024 * 1024, NUMANode: 1, NVLinkPeers: []int{0, 1, 2, 3, 4, 5, 7}},
				{Index: 7, Type: "A100-80G", Memory: 80 * 1024 * 1024 * 1024, NUMANode: 1, NVLinkPeers: []int{0, 1, 2, 3, 4, 5, 6}},
			},
			NVLinks: map[int][]int{
				0: {1, 2, 3, 4, 5, 6, 7}, // Full mesh NVLink
				1: {0, 2, 3, 4, 5, 6, 7},
				2: {0, 1, 3, 4, 5, 6, 7},
				3: {0, 1, 2, 4, 5, 6, 7},
				4: {0, 1, 2, 3, 5, 6, 7},
				5: {0, 1, 2, 3, 4, 6, 7},
				6: {0, 1, 2, 3, 4, 5, 7},
				7: {0, 1, 2, 3, 4, 5, 6},
			},
			CPUNUMANodes: []int{0, 1},
		}, nil
	}

	// Example: Standard 4xV100 node
	return &GPUNode{
		Name: nodeName,
		GPUs: []GPUDevice{
			{Index: 0, Type: "V100", Memory: 32 * 1024 * 1024 * 1024, NUMANode: 0, NVLinkPeers: []int{1}},
			{Index: 1, Type: "V100", Memory: 32 * 1024 * 1024 * 1024, NUMANode: 0, NVLinkPeers: []int{0}},
			{Index: 2, Type: "V100", Memory: 32 * 1024 * 1024 * 1024, NUMANode: 1, NVLinkPeers: []int{3}},
			{Index: 3, Type: "V100", Memory: 32 * 1024 * 1024 * 1024, NUMANode: 1, NVLinkPeers: []int{2}},
		},
		NVLinks: map[int][]int{
			0: {1},
			1: {0},
			2: {3},
			3: {2},
		},
		CPUNUMANodes: []int{0, 1},
	}, nil
}

// OptimizePlacement finds best GPU placement for a job
func (to *TopologyOptimizer) OptimizePlacement(gpuCount int, gpuType string) ([]PlacementScore, error) {
	ctx := context.Background()

	// Get all GPU nodes
	nodes, err := to.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "gryvia.io/gpu",
	})
	if err != nil {
		return nil, err
	}

	var scores []PlacementScore

	for _, node := range nodes.Items {
		score := to.scoreNode(node.Name, gpuCount, gpuType)
		scores = append(scores, score)
	}

	// Sort by score (descending)
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].Score > scores[j].Score
	})

	return scores, nil
}

// scoreNode scores a node for GPU placement
func (to *TopologyOptimizer) scoreNode(nodeName string, gpuCount int, gpuType string) PlacementScore {
	topology, err := to.GetGPUTopology(nodeName)
	if err != nil {
		return PlacementScore{
			NodeName:  nodeName,
			Score:     0,
			Reasoning: fmt.Sprintf("Error getting topology: %v", err),
		}
	}

	score := 0.0
	reasoning := []string{}

	// 1. Check if node has enough GPUs of correct type
	availableGPUs := 0
	for _, gpu := range topology.GPUs {
		if gpu.Type == gpuType {
			availableGPUs++
		}
	}

	if availableGPUs < gpuCount {
		return PlacementScore{
			NodeName:  nodeName,
			Score:     0,
			Reasoning: fmt.Sprintf("Insufficient GPUs: has %d, needs %d", availableGPUs, gpuCount),
		}
	}

	// 2. Score based on NVLink connectivity
	nvlinkScore := to.scoreNVLinkTopology(topology, gpuCount)
	score += nvlinkScore * nvlinkWeight
	reasoning = append(reasoning, fmt.Sprintf("NVLink: %.1f", nvlinkScore))

	// 3. Score based on NUMA locality
	numaScore := to.scoreNUMALocality(topology, gpuCount)
	score += numaScore * numaWeight
	reasoning = append(reasoning, fmt.Sprintf("NUMA: %.1f", numaScore))

	// 4. Score based on current utilization
	utilizationScore := to.scoreUtilization(nodeName)
	score += utilizationScore * utilizationWeight
	reasoning = append(reasoning, fmt.Sprintf("Util: %.1f", utilizationScore))

	// 5. Bonus for full-node allocation
	if gpuCount == len(topology.GPUs) {
		score += fullNodeBonus
		reasoning = append(reasoning, "Full node")
	}

	// 6. Penalty for fragmentation
	if gpuCount < len(topology.GPUs) && gpuCount > len(topology.GPUs)/2 {
		score -= fragmentPenalty
		reasoning = append(reasoning, "Fragments node")
	}

	topologyType := to.describeTopology(topology, gpuCount)
	nvlinkCount := to.countNVLinks(topology, gpuCount)

	return PlacementScore{
		NodeName:    nodeName,
		Score:       score,
		Reasoning:   fmt.Sprintf("%v", reasoning),
		Topology:    topologyType,
		NVLinkCount: nvlinkCount,
	}
}

// scoreNVLinkTopology scores NVLink connectivity
func (to *TopologyOptimizer) scoreNVLinkTopology(node *GPUNode, gpuCount int) float64 {
	if gpuCount == 1 {
		return 100 // NVLink doesn't matter for single GPU
	}

	// Count total possible NVLinks between requested GPUs
	maxPossibleLinks := gpuCount * (gpuCount - 1) / 2

	// Count actual NVLinks (assume we take first N GPUs)
	actualLinks := 0
	for i := 0; i < gpuCount && i < len(node.GPUs); i++ {
		for j := i + 1; j < gpuCount && j < len(node.GPUs); j++ {
			// Check if GPU i and GPU j are NVLink connected
			if contains(node.GPUs[i].NVLinkPeers, j) {
				actualLinks++
			}
		}
	}

	if maxPossibleLinks == 0 {
		return 100
	}

	// Full mesh NVLink = 100, partial = proportional, none = 0
	return float64(actualLinks) / float64(maxPossibleLinks) * 100
}

// scoreNUMALocality scores NUMA node locality
func (to *TopologyOptimizer) scoreNUMALocality(node *GPUNode, gpuCount int) float64 {
	if gpuCount == 1 {
		return 100 // NUMA doesn't matter for single GPU
	}

	// Best: all GPUs on same NUMA node
	// Worst: GPUs spread across all NUMA nodes

	numaNodes := make(map[int]int)
	for i := 0; i < gpuCount && i < len(node.GPUs); i++ {
		numaNodes[node.GPUs[i].NUMANode]++
	}

	if len(numaNodes) == 1 {
		return 100 // All on same NUMA node
	}

	// Score based on concentration
	maxConcentration := 0
	for _, count := range numaNodes {
		if count > maxConcentration {
			maxConcentration = count
		}
	}

	return float64(maxConcentration) / float64(gpuCount) * 100
}

// scoreUtilization scores based on current node utilization
func (to *TopologyOptimizer) scoreUtilization(nodeName string) float64 {
	// In production, query actual GPU utilization
	// For now, return example score

	// Prefer nodes with lower utilization
	// 0% util = 100 score, 100% util = 0 score
	return 75 // Example: 25% utilized -> 75 score
}

// describeTopology describes the GPU topology
func (to *TopologyOptimizer) describeTopology(node *GPUNode, gpuCount int) string {
	if gpuCount == 1 {
		return "single-gpu"
	}

	// Check if full mesh NVLink
	fullMesh := true
	for i := 0; i < gpuCount && i < len(node.GPUs); i++ {
		if len(node.GPUs[i].NVLinkPeers) < gpuCount-1 {
			fullMesh = false
			break
		}
	}

	if fullMesh {
		return fmt.Sprintf("nvlink-mesh-%d", gpuCount)
	}

	// Check if paired
	if gpuCount == 2 && len(node.GPUs) > 0 && len(node.GPUs[0].NVLinkPeers) >= 1 {
		return "nvlink-pair"
	}

	return "pcie-only"
}

// countNVLinks counts NVLinks between requested GPUs
func (to *TopologyOptimizer) countNVLinks(node *GPUNode, gpuCount int) int {
	count := 0
	for i := 0; i < gpuCount && i < len(node.GPUs); i++ {
		for j := i + 1; j < gpuCount && j < len(node.GPUs); j++ {
			if contains(node.GPUs[i].NVLinkPeers, j) {
				count++
			}
		}
	}
	return count
}

// GetRecommendation provides placement recommendation
func (to *TopologyOptimizer) GetRecommendation(gpuCount int, gpuType string) string {
	scores, err := to.OptimizePlacement(gpuCount, gpuType)
	if err != nil || len(scores) == 0 {
		return "No suitable nodes found"
	}

	best := scores[0]

	recommendation := fmt.Sprintf(`
GPU Placement Recommendation
============================

Recommended Node: %s
Score: %.1f/100
Topology: %s
NVLink Connections: %d

Reasoning: %s

Performance Impact:
`, best.NodeName, best.Score, best.Topology, best.NVLinkCount, best.Reasoning)

	// Add performance estimates
	if best.Topology == fmt.Sprintf("nvlink-mesh-%d", gpuCount) {
		recommendation += `  • All-reduce bandwidth: ~600 GB/s (NVLink)
  • Scaling efficiency: 95%+
  • Recommended for: Large-scale training`
	} else if best.Topology == "nvlink-pair" {
		recommendation += `  • All-reduce bandwidth: ~300 GB/s (NVLink)
  • Scaling efficiency: 90%
  • Recommended for: Small-scale training`
	} else {
		recommendation += `  • All-reduce bandwidth: ~50 GB/s (PCIe)
  • Scaling efficiency: 70-80%
  • Recommended for: Inference, small models`
	}

	recommendation += "\n\nAlternatives:\n"
	for i := 1; i < len(scores) && i < 3; i++ {
		alt := scores[i]
		recommendation += fmt.Sprintf("  %d. %s (score: %.1f, topology: %s)\n",
			i+1, alt.NodeName, alt.Score, alt.Topology)
	}

	return recommendation
}

func contains(slice []int, item int) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func main() {
	optimizer, err := NewTopologyOptimizer()
	if err != nil {
		log.Fatal(err)
	}

	// Example: Find best placement for 8xA100 job
	recommendation := optimizer.GetRecommendation(8, "A100-80G")
	fmt.Println(recommendation)
}
