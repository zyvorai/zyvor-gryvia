// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package aggregator computes per-service-pair metrics over a sliding
// time window.  It receives decoded FlowEvents and maintains running
// statistics (p50/p95/p99 latency, throughput, connection rate) that
// the Prometheus exporter and graph builder consume.
package aggregator

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/decoder"
)

// ServicePair identifies directional traffic between two services.
type ServicePair struct {
	SrcService string
	DstService string
	Protocol   uint8
}

// Stats holds aggregated metrics for a single service pair.
type Stats struct {
	BytesTotal      uint64
	PacketsTotal    uint64
	ConnectionCount uint64
	Latencies       []float64 // in seconds
	LastSeen        time.Time
}

// Snapshot is a point-in-time copy of Stats with computed percentiles.
type Snapshot struct {
	BytesTotal      uint64
	PacketsTotal    uint64
	ConnectionCount uint64
	P50Latency      float64
	P95Latency      float64
	P99Latency      float64
	BytesPerSec     float64
	LastSeen        time.Time
}

// Aggregator maintains a sliding window of per-service metrics.
type Aggregator struct {
	mu       sync.RWMutex
	window   time.Duration
	pairs    map[ServicePair]*Stats
	expireAt map[ServicePair]time.Time
}

// New creates an Aggregator with the given window duration in seconds.
func New(windowSec int) *Aggregator {
	a := &Aggregator{
		window:   time.Duration(windowSec) * time.Second,
		pairs:    make(map[ServicePair]*Stats),
		expireAt: make(map[ServicePair]time.Time),
	}
	go a.pruneLoop()
	return a
}

// Record ingests a single flow event.
func (a *Aggregator) Record(ev decoder.FlowEvent) {
	pair := ServicePair{
		SrcService: ev.SrcService,
		DstService: ev.DstService,
		Protocol:   ev.Protocol,
	}
	if pair.SrcService == "" {
		pair.SrcService = decoder.IPToString(ev.SrcIP)
	}
	if pair.DstService == "" {
		pair.DstService = decoder.IPToString(ev.DstIP)
	}

	now := time.Now()
	latencySec := float64(ev.LatencyNs) / 1e9

	a.mu.Lock()
	defer a.mu.Unlock()

	s, ok := a.pairs[pair]
	if !ok {
		s = &Stats{}
		a.pairs[pair] = s
	}

	s.BytesTotal += uint64(ev.Bytes)
	s.PacketsTotal++
	s.ConnectionCount++
	s.LastSeen = now
	if ev.LatencyNs > 0 {
		s.Latencies = append(s.Latencies, latencySec)
	}

	a.expireAt[pair] = now.Add(a.window)
}

// Snapshot returns computed metrics for all active service pairs.
func (a *Aggregator) Snapshot() map[ServicePair]Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make(map[ServicePair]Snapshot, len(a.pairs))
	for pair, s := range a.pairs {
		snap := Snapshot{
			BytesTotal:      s.BytesTotal,
			PacketsTotal:    s.PacketsTotal,
			ConnectionCount: s.ConnectionCount,
			LastSeen:        s.LastSeen,
		}

		elapsed := time.Since(s.LastSeen).Seconds()
		if elapsed > 0 && elapsed < a.window.Seconds() {
			snap.BytesPerSec = float64(s.BytesTotal) / elapsed
		}

		if len(s.Latencies) > 0 {
			sorted := make([]float64, len(s.Latencies))
			copy(sorted, s.Latencies)
			sort.Float64s(sorted)
			snap.P50Latency = percentile(sorted, 50)
			snap.P95Latency = percentile(sorted, 95)
			snap.P99Latency = percentile(sorted, 99)
		}

		result[pair] = snap
	}
	return result
}

func percentile(sorted []float64, p int) float64 {
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

func (a *Aggregator) pruneLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		a.prune()
	}
}

func (a *Aggregator) prune() {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()

	for pair, expiry := range a.expireAt {
		if now.After(expiry) {
			delete(a.pairs, pair)
			delete(a.expireAt, pair)
		}
	}
}
