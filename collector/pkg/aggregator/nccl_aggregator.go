// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package aggregator provides NCCL collective operation metric aggregation.
package aggregator

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/decoder"
)

// NCCLOpStats holds aggregated statistics for a single NCCL operation type.
type NCCLOpStats struct {
	OpType       string
	Count        int64
	TotalBytes   int64
	AvgLatencyNs float64
	P50LatencyNs float64
	P95LatencyNs float64
	P99LatencyNs float64
	Samples      []float64
}

// StragglerEvent records a rank that is significantly slower than the median.
type StragglerEvent struct {
	Rank      uint32
	OpType    string
	LatencyNs uint64
	MedianNs  uint64
	Ratio     float64 // latency / median
	Timestamp time.Time
}

// maxSamples bounds the latency samples kept per operation and per rank.
const maxSamples = 4096

// NCCLAggregator aggregates NCCL collective operation metrics.
type NCCLAggregator struct {
	mu          sync.RWMutex
	window      time.Duration
	opStats     map[string]*NCCLOpStats // keyed by op type name
	rankLatency map[uint32][]float64    // per-rank latency samples
	stragglers  []StragglerEvent
}

// NewNCCLAggregator creates an NCCLAggregator with the given window duration.
func NewNCCLAggregator(window time.Duration) *NCCLAggregator {
	return &NCCLAggregator{
		window:      window,
		opStats:     make(map[string]*NCCLOpStats),
		rankLatency: make(map[uint32][]float64),
	}
}

// Process ingests a GPU event and updates NCCL statistics if applicable.
func (a *NCCLAggregator) Process(event decoder.GPUEvent) {
	if event.EventType != decoder.GPUEvtNCCLOp {
		return
	}

	opName := decoder.NCCLOpName(event.NCCLOp)
	latency := float64(event.LatencyNs)

	a.mu.Lock()
	defer a.mu.Unlock()

	stats, ok := a.opStats[opName]
	if !ok {
		stats = &NCCLOpStats{OpType: opName}
		a.opStats[opName] = stats
	}

	stats.Count++
	stats.TotalBytes += int64(event.Bytes)
	stats.Samples = append(stats.Samples, latency)
	if len(stats.Samples) > maxSamples {
		stats.Samples = append(stats.Samples[:0], stats.Samples[len(stats.Samples)-maxSamples:]...)
	}

	// Update running average.
	stats.AvgLatencyNs = stats.AvgLatencyNs + (latency-stats.AvgLatencyNs)/float64(stats.Count)

	// Track per-rank latency for straggler detection.
	if event.WorldSize > 0 {
		rl := append(a.rankLatency[event.SrcRank], latency)
		if len(rl) > maxSamples {
			rl = append(rl[:0], rl[len(rl)-maxSamples:]...)
		}
		a.rankLatency[event.SrcRank] = rl
	}

	// Recompute percentiles periodically (every 100 samples).
	if stats.Count%100 == 0 {
		a.computePercentiles(stats)
	}
}

func (a *NCCLAggregator) computePercentiles(stats *NCCLOpStats) {
	if len(stats.Samples) == 0 {
		return
	}
	sorted := make([]float64, len(stats.Samples))
	copy(sorted, stats.Samples)
	sort.Float64s(sorted)

	stats.P50LatencyNs = percentileFloat(sorted, 50)
	stats.P95LatencyNs = percentileFloat(sorted, 95)
	stats.P99LatencyNs = percentileFloat(sorted, 99)
}

// GetOpStats returns a snapshot of per-operation-type statistics.
func (a *NCCLAggregator) GetOpStats() map[string]*NCCLOpStats {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make(map[string]*NCCLOpStats, len(a.opStats))
	for k, v := range a.opStats {
		cp := *v
		a.computePercentiles(&cp)
		cp.Samples = nil // don't expose raw samples
		result[k] = &cp
	}
	return result
}

// GetStragglers returns the recorded straggler events.
func (a *NCCLAggregator) GetStragglers() []StragglerEvent {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make([]StragglerEvent, len(a.stragglers))
	copy(result, a.stragglers)
	return result
}

// DetectStragglers flags ranks with latency > 2x the median across all ranks.
func (a *NCCLAggregator) DetectStragglers() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.rankLatency) < 2 {
		return
	}

	// Compute median latency per rank, then overall median of medians.
	rankMedians := make(map[uint32]float64)
	var allMedians []float64

	for rank, samples := range a.rankLatency {
		if len(samples) == 0 {
			continue
		}
		sorted := make([]float64, len(samples))
		copy(sorted, samples)
		sort.Float64s(sorted)
		median := sorted[len(sorted)/2]
		rankMedians[rank] = median
		allMedians = append(allMedians, median)
	}

	if len(allMedians) == 0 {
		return
	}

	sort.Float64s(allMedians)
	overallMedian := allMedians[len(allMedians)/2]

	if overallMedian == 0 {
		return
	}

	now := time.Now()
	for rank, median := range rankMedians {
		ratio := median / overallMedian
		if ratio > 2.0 {
			a.stragglers = append(a.stragglers, StragglerEvent{
				Rank:      rank,
				OpType:    "aggregate",
				LatencyNs: uint64(median),
				MedianNs:  uint64(overallMedian),
				Ratio:     ratio,
				Timestamp: now,
			})
		}
	}

	// Keep stragglers list bounded.
	const maxStragglers = 1000
	if len(a.stragglers) > maxStragglers {
		a.stragglers = a.stragglers[len(a.stragglers)-maxStragglers:]
	}
}

// percentileFloat computes the p-th percentile from a sorted slice.
func percentileFloat(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(p)/100.0*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
