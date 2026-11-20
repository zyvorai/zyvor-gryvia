// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package aggregator provides GPU memory transfer metric aggregation.
package aggregator

import (
	"sync"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/decoder"
)

// DirectionStats holds aggregated metrics for a single memory transfer direction.
type DirectionStats struct {
	Direction     string
	TotalBytes    int64
	TransferCount int64
	AvgLatencyNs  float64
	ThroughputBps float64
	Samples       []float64
}

// GPUMemAggregator aggregates GPU memory transfer metrics by direction.
type GPUMemAggregator struct {
	mu             sync.RWMutex
	window         time.Duration
	directionStats map[uint8]*DirectionStats // per-direction (H2D, D2H, D2D, PEER)
	allocations    map[uint32]int64          // pid -> total allocated bytes
	startTime      time.Time
}

// NewGPUMemAggregator creates a GPUMemAggregator with the given window duration.
func NewGPUMemAggregator(window time.Duration) *GPUMemAggregator {
	return &GPUMemAggregator{
		window:         window,
		directionStats: make(map[uint8]*DirectionStats),
		allocations:    make(map[uint32]int64),
		startTime:      time.Now(),
	}
}

// Process ingests a GPU event and updates memory transfer statistics.
func (a *GPUMemAggregator) Process(event decoder.GPUEvent) {
	if event.EventType != decoder.GPUEvtMemTransfer {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	stats, ok := a.directionStats[event.Direction]
	if !ok {
		stats = &DirectionStats{
			Direction: decoder.MemDirectionName(event.Direction),
		}
		a.directionStats[event.Direction] = stats
	}

	stats.TransferCount++
	stats.TotalBytes += int64(event.Bytes)
	stats.Samples = append(stats.Samples, float64(event.LatencyNs))

	// Update running average latency.
	stats.AvgLatencyNs = stats.AvgLatencyNs + (float64(event.LatencyNs)-stats.AvgLatencyNs)/float64(stats.TransferCount)

	// Update throughput (bytes per second over the window).
	elapsed := time.Since(a.startTime).Seconds()
	if elapsed > 0 {
		stats.ThroughputBps = float64(stats.TotalBytes) / elapsed
	}

	// Track per-PID allocations.
	a.allocations[event.PID] += int64(event.Bytes)
}

// GetDirectionStats returns a snapshot of per-direction statistics.
func (a *GPUMemAggregator) GetDirectionStats() map[string]*DirectionStats {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make(map[string]*DirectionStats, len(a.directionStats))
	for _, v := range a.directionStats {
		cp := *v
		cp.Samples = nil // don't expose raw samples
		result[v.Direction] = &cp
	}
	return result
}

// GetAllocations returns per-PID total allocated bytes.
func (a *GPUMemAggregator) GetAllocations() map[uint32]int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make(map[uint32]int64, len(a.allocations))
	for k, v := range a.allocations {
		result[k] = v
	}
	return result
}
