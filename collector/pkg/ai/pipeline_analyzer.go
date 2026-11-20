// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package ai provides pipeline bottleneck analysis for AI/ML training workloads.
package ai

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/decoder"
)

// PipelineAnalyzer identifies bottlenecks in the data-to-GPU pipeline.
type PipelineAnalyzer struct {
	mu             sync.RWMutex
	ioThroughput   float64 // bytes/sec from storage
	netIngestion   float64 // bytes/sec from network
	gpuUtilization float64 // fraction of time GPU is busy (0.0..1.0)
	bottleneck     string  // "data_loading", "preprocessing", "compute", "communication"
	recommendation string

	// Internal tracking.
	ioBytes     uint64
	ioSamples   int64
	netBytes    uint64
	netSamples  int64
	gpuBusyNs   uint64
	gpuTotalNs  uint64
	commTotalNs uint64
	startTime   time.Time
}

// NewPipelineAnalyzer creates a PipelineAnalyzer.
func NewPipelineAnalyzer() *PipelineAnalyzer {
	return &PipelineAnalyzer{
		bottleneck:     "unknown",
		recommendation: "insufficient data to make a recommendation",
		startTime:      time.Now(),
	}
}

// ProcessIOEvent records an I/O (storage) event with bytes transferred and latency.
func (a *PipelineAnalyzer) ProcessIOEvent(bytes uint64, latencyNs uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.ioBytes += bytes
	a.ioSamples++

	elapsed := time.Since(a.startTime).Seconds()
	if elapsed > 0 {
		a.ioThroughput = float64(a.ioBytes) / elapsed
	}

	a.analyze()
}

// ProcessNetEvent records a network ingestion event.
func (a *PipelineAnalyzer) ProcessNetEvent(bytes uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.netBytes += bytes
	a.netSamples++

	elapsed := time.Since(a.startTime).Seconds()
	if elapsed > 0 {
		a.netIngestion = float64(a.netBytes) / elapsed
	}

	a.analyze()
}

// ProcessGPUEvent records a GPU event for utilization tracking.
func (a *PipelineAnalyzer) ProcessGPUEvent(event decoder.GPUEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch event.EventType {
	case decoder.GPUEvtCUDALaunch, decoder.GPUEvtCUDASync:
		a.gpuBusyNs += event.LatencyNs
	case decoder.GPUEvtNCCLOp, decoder.GPUEvtRDMASend, decoder.GPUEvtRDMARecv:
		a.commTotalNs += event.LatencyNs
	}

	// Estimate total wall-clock time in nanoseconds.
	a.gpuTotalNs = uint64(time.Since(a.startTime).Nanoseconds())
	if a.gpuTotalNs > 0 {
		a.gpuUtilization = float64(a.gpuBusyNs) / float64(a.gpuTotalNs)
		if a.gpuUtilization > 1.0 {
			a.gpuUtilization = 1.0 // clamp for multi-GPU overlap
		}
	}

	a.analyze()
}

// analyze determines the current bottleneck and generates a recommendation.
// Must be called with mu held.
func (a *PipelineAnalyzer) analyze() {
	elapsed := time.Since(a.startTime).Seconds()
	if elapsed < 1.0 || (a.ioSamples == 0 && a.netSamples == 0) {
		return
	}

	commFraction := float64(0)
	if a.gpuTotalNs > 0 {
		commFraction = float64(a.commTotalNs) / float64(a.gpuTotalNs)
	}

	// Determine bottleneck by checking which stage is the limiting factor.
	switch {
	case a.gpuUtilization < 0.3 && a.ioThroughput < 100*1024*1024:
		// GPU mostly idle and low I/O throughput -> data loading bottleneck.
		a.bottleneck = "data_loading"
		a.recommendation = "increase data loading parallelism (num_workers) or use faster storage (NVMe, distributed filesystem)"

	case a.gpuUtilization < 0.5 && a.netIngestion < 50*1024*1024:
		// GPU under-utilized and low network throughput -> preprocessing bottleneck.
		a.bottleneck = "preprocessing"
		a.recommendation = "move preprocessing to GPU (DALI/torchdata) or increase CPU preprocessing workers"

	case commFraction > 0.5:
		// Communication dominates -> communication bottleneck.
		a.bottleneck = "communication"
		a.recommendation = "consider gradient compression, increase batch size, or switch to pipelined parallelism"

	case a.gpuUtilization > 0.8:
		// GPU is well-utilized -> compute is the bottleneck (expected).
		a.bottleneck = "compute"
		a.recommendation = "pipeline is balanced; consider mixed-precision training or model optimizations to speed up compute"

	default:
		a.bottleneck = "unknown"
		a.recommendation = "collect more samples for accurate analysis"
	}
}

// GetBottleneck returns the identified bottleneck and a recommendation.
func (a *PipelineAnalyzer) GetBottleneck() (string, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.bottleneck, a.recommendation
}

// ServeHTTP handles GET /api/v1/ai/pipeline.
func (a *PipelineAnalyzer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	a.mu.RLock()
	resp := struct {
		Bottleneck     string  `json:"bottleneck"`
		Recommendation string  `json:"recommendation"`
		IOThroughput   float64 `json:"io_throughput_bps"`
		NetIngestion   float64 `json:"net_ingestion_bps"`
		GPUUtilization float64 `json:"gpu_utilization"`
	}{
		Bottleneck:     a.bottleneck,
		Recommendation: a.recommendation,
		IOThroughput:   a.ioThroughput,
		NetIngestion:   a.netIngestion,
		GPUUtilization: a.gpuUtilization,
	}
	a.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
