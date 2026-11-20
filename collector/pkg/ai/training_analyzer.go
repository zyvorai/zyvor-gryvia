// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package ai provides AI/ML training workload analysis including
// communication pattern detection, rank statistics, and straggler identification.
package ai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/zyvorai/gryvia/collector/pkg/decoder"
)

// RankPair identifies a directional communication pair between two ranks.
type RankPair struct {
	Src uint32 `json:"src"`
	Dst uint32 `json:"dst"`
}

// RankStats holds per-rank statistics for training workloads.
type RankStats struct {
	Rank             uint32  `json:"rank"`
	TotalCollectives int64   `json:"total_collectives"`
	AvgLatencyNs     float64 `json:"avg_latency_ns"`
	TotalBytes       int64   `json:"total_bytes"`
	IsStraggler      bool    `json:"is_straggler"`
}

// TrainingAnalyzer analyses distributed training communication patterns.
type TrainingAnalyzer struct {
	mu               sync.RWMutex
	rankStats        map[uint32]*RankStats
	commMatrix       map[RankPair]int64 // (src, dst) -> bytes transferred
	commComputeRatio float64
	pattern          string // "ring_allreduce", "tree_allreduce", "pipelined", "unknown"

	totalCommNs    float64
	totalComputeNs float64
}

// NewTrainingAnalyzer creates a TrainingAnalyzer.
func NewTrainingAnalyzer() *TrainingAnalyzer {
	return &TrainingAnalyzer{
		rankStats:  make(map[uint32]*RankStats),
		commMatrix: make(map[RankPair]int64),
		pattern:    "unknown",
	}
}

// Process ingests a GPU event and updates training analysis.
func (a *TrainingAnalyzer) Process(event decoder.GPUEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch event.EventType {
	case decoder.GPUEvtNCCLOp:
		a.processCollective(event)
	case decoder.GPUEvtCUDALaunch:
		a.totalComputeNs += float64(event.LatencyNs)
	case decoder.GPUEvtCUDASync:
		a.totalComputeNs += float64(event.LatencyNs)
	case decoder.GPUEvtRDMASend, decoder.GPUEvtRDMARecv:
		a.processRDMA(event)
	}

	// Update comm/compute ratio.
	if a.totalComputeNs > 0 {
		a.commComputeRatio = a.totalCommNs / a.totalComputeNs
	}
}

func (a *TrainingAnalyzer) processCollective(event decoder.GPUEvent) {
	// The current NCCL uprobe emits zero for rank and world size. Do not
	// represent all ranks as rank 0 or claim a communication topology.
	if event.WorldSize == 0 {
		a.totalCommNs += float64(event.LatencyNs)
		return
	}
	// Update rank stats.
	rs, ok := a.rankStats[event.SrcRank]
	if !ok {
		rs = &RankStats{Rank: event.SrcRank}
		a.rankStats[event.SrcRank] = rs
	}
	rs.TotalCollectives++
	rs.TotalBytes += int64(event.Bytes)
	rs.AvgLatencyNs = rs.AvgLatencyNs + (float64(event.LatencyNs)-rs.AvgLatencyNs)/float64(rs.TotalCollectives)

	// Update communication matrix.
	if event.DstRank != event.SrcRank {
		pair := RankPair{Src: event.SrcRank, Dst: event.DstRank}
		a.commMatrix[pair] += int64(event.Bytes)
	}

	a.totalCommNs += float64(event.LatencyNs)
}

func (a *TrainingAnalyzer) processRDMA(event decoder.GPUEvent) {
	if event.WorldSize == 0 { // rank unknown: do not fabricate a (0,0) pair
		a.totalCommNs += float64(event.LatencyNs)
		return
	}
	pair := RankPair{Src: event.SrcRank, Dst: event.DstRank}
	a.commMatrix[pair] += int64(event.Bytes)
	a.totalCommNs += float64(event.LatencyNs)
}

// GetRankStats returns a snapshot of per-rank statistics.
func (a *TrainingAnalyzer) GetRankStats() map[uint32]*RankStats {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make(map[uint32]*RankStats, len(a.rankStats))
	for k, v := range a.rankStats {
		cp := *v
		result[k] = &cp
	}
	return result
}

// GetCommMatrix returns the communication matrix as a map of rank pairs to bytes.
func (a *TrainingAnalyzer) GetCommMatrix() map[RankPair]int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make(map[RankPair]int64, len(a.commMatrix))
	for k, v := range a.commMatrix {
		result[k] = v
	}
	return result
}

// DetectPattern analyses the communication matrix to infer the collective
// communication pattern in use.
func (a *TrainingAnalyzer) DetectPattern() string {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.commMatrix) == 0 || len(a.rankStats) < 2 {
		a.pattern = "unknown"
		return a.pattern
	}

	numRanks := uint32(len(a.rankStats))

	// Count how many unique rank pairs have communication.
	activePairs := len(a.commMatrix)

	// Ring AllReduce: each rank communicates with exactly 2 neighbours.
	// Expected pairs ~ 2 * numRanks (bidirectional ring).
	expectedRing := int(2 * numRanks)

	// Tree AllReduce: communication follows a tree pattern.
	// Expected pairs ~ 2 * (numRanks - 1) for a binary tree.
	expectedTree := int(2 * (numRanks - 1))

	// Full mesh (pipelined / all-to-all): each rank talks to every other.
	expectedMesh := int(numRanks * (numRanks - 1))

	// Find closest match.
	ringDelta := abs(activePairs - expectedRing)
	treeDelta := abs(activePairs - expectedTree)
	meshDelta := abs(activePairs - expectedMesh)

	switch {
	case ringDelta <= treeDelta && ringDelta <= meshDelta:
		a.pattern = "ring_allreduce"
	case treeDelta <= ringDelta && treeDelta <= meshDelta:
		a.pattern = "tree_allreduce"
	default:
		a.pattern = "pipelined"
	}

	return a.pattern
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// GetCommComputeRatio returns the ratio of communication time to compute time.
func (a *TrainingAnalyzer) GetCommComputeRatio() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.commComputeRatio
}

// ServeHTTP handles GET /api/v1/ai/training.
func (a *TrainingAnalyzer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pattern := a.DetectPattern()

	w.Header().Set("Content-Type", "application/json")
	resp := struct {
		RankStats        map[uint32]*RankStats `json:"rank_stats"`
		Pattern          string                `json:"pattern"`
		CommComputeRatio float64               `json:"comm_compute_ratio"`
		CommMatrix       map[string]int64      `json:"comm_matrix"`
	}{
		RankStats:        a.GetRankStats(),
		Pattern:          pattern,
		CommComputeRatio: a.GetCommComputeRatio(),
		CommMatrix:       a.commMatrixJSON(),
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *TrainingAnalyzer) commMatrixJSON() map[string]int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make(map[string]int64, len(a.commMatrix))
	for pair, bytes := range a.commMatrix {
		key := rankPairKey(pair)
		result[key] = bytes
	}
	return result
}

func rankPairKey(p RankPair) string {
	return fmt.Sprintf("%d->%d", p.Src, p.Dst)
}
