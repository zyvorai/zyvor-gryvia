// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package tuning provides TCP tuning recommendations based on observed
// connection statistics from eBPF probes.
package tuning

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// ConnKey uniquely identifies a TCP connection by its 4-tuple.
type ConnKey struct {
	SrcIP   uint32
	DstIP   uint32
	SrcPort uint16
	DstPort uint16
}

// TCPConnStats holds aggregated TCP statistics for a single connection.
type TCPConnStats struct {
	AvgCwnd    float64 `json:"avg_cwnd"`
	AvgRTT     float64 `json:"avg_rtt_us"`
	AvgRcvWnd  float64 `json:"avg_rcv_wnd"`
	BufferUtil float64 `json:"buffer_util"`
	Samples    int     `json:"samples"`
}

// TCPRecommendation is a tuning suggestion for a specific service/connection.
type TCPRecommendation struct {
	Service   string `json:"service"`
	Type      string `json:"type"` // "buffer_size", "congestion_control", "keepalive"
	Current   string `json:"current"`
	Suggested string `json:"suggested"`
	Reason    string `json:"reason"`
}

// TuningEvent is the raw TCP tuning event from the eBPF probe.
type TuningEvent struct {
	SrcIP   uint32
	DstIP   uint32
	SrcPort uint16
	DstPort uint16
	Cwnd    uint32
	RTTUs   uint32 // round-trip time in microseconds
	RcvWnd  uint32
	SndBuf  uint32
	RcvBuf  uint32
}

// TCPAdvisor analyses TCP connection metrics and generates tuning recommendations.
type TCPAdvisor struct {
	mu              sync.RWMutex
	connStats       map[ConnKey]*TCPConnStats
	recommendations []TCPRecommendation
}

// NewTCPAdvisor creates a TCPAdvisor.
func NewTCPAdvisor() *TCPAdvisor {
	return &TCPAdvisor{
		connStats: make(map[ConnKey]*TCPConnStats),
	}
}

// ProcessTuningEvent ingests a raw TCP tuning event.
func (a *TCPAdvisor) ProcessTuningEvent(event TuningEvent) {
	key := ConnKey{
		SrcIP:   event.SrcIP,
		DstIP:   event.DstIP,
		SrcPort: event.SrcPort,
		DstPort: event.DstPort,
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	stats, ok := a.connStats[key]
	if !ok {
		stats = &TCPConnStats{}
		a.connStats[key] = stats
	}

	stats.Samples++
	n := float64(stats.Samples)

	// Running averages.
	stats.AvgCwnd = stats.AvgCwnd + (float64(event.Cwnd)-stats.AvgCwnd)/n
	stats.AvgRTT = stats.AvgRTT + (float64(event.RTTUs)-stats.AvgRTT)/n
	stats.AvgRcvWnd = stats.AvgRcvWnd + (float64(event.RcvWnd)-stats.AvgRcvWnd)/n

	// Buffer utilization: fraction of send buffer actually used.
	if event.SndBuf > 0 {
		util := float64(event.Cwnd) / float64(event.SndBuf)
		if util > 1.0 {
			util = 1.0
		}
		stats.BufferUtil = stats.BufferUtil + (util-stats.BufferUtil)/n
	}
}

// GenerateRecommendations analyses all connection statistics and returns
// tuning recommendations.
func (a *TCPAdvisor) GenerateRecommendations() []TCPRecommendation {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.recommendations = nil

	for key, stats := range a.connStats {
		if stats.Samples < 10 {
			continue // need enough samples
		}

		svc := fmt.Sprintf("%s:%d -> %s:%d",
			ipToString(key.SrcIP), key.SrcPort,
			ipToString(key.DstIP), key.DstPort)

		// Buffer size recommendation: if utilization is consistently high,
		// the send buffer may be too small.
		if stats.BufferUtil > 0.9 {
			a.recommendations = append(a.recommendations, TCPRecommendation{
				Service:   svc,
				Type:      "buffer_size",
				Current:   "auto",
				Suggested: "increase net.core.wmem_max to 16MB",
				Reason:    fmt.Sprintf("buffer utilization %.0f%% indicates send buffer is saturated", stats.BufferUtil*100),
			})
		}

		// Congestion control: high RTT with small cwnd suggests suboptimal CC.
		if stats.AvgRTT > 10000 && stats.AvgCwnd < 20 { // RTT > 10ms, cwnd < 20 segments
			a.recommendations = append(a.recommendations, TCPRecommendation{
				Service:   svc,
				Type:      "congestion_control",
				Current:   "cubic",
				Suggested: "bbr",
				Reason:    fmt.Sprintf("high RTT (%.0fus) with small cwnd (%.0f) suggests loss-based CC is suboptimal", stats.AvgRTT, stats.AvgCwnd),
			})
		}

		// Receive window: if rcv_wnd is consistently small relative to BDP.
		bdp := (stats.AvgRTT / 1e6) * stats.AvgCwnd * 1448 // BDP in bytes (MSS=1448)
		if bdp > 0 && stats.AvgRcvWnd < bdp*0.5 {
			a.recommendations = append(a.recommendations, TCPRecommendation{
				Service:   svc,
				Type:      "buffer_size",
				Current:   "auto",
				Suggested: "increase net.core.rmem_max to 16MB",
				Reason:    fmt.Sprintf("receive window (%.0f) is less than 50%% of BDP (%.0f bytes)", stats.AvgRcvWnd, bdp),
			})
		}
	}

	result := make([]TCPRecommendation, len(a.recommendations))
	copy(result, a.recommendations)
	return result
}

// ServeHTTP handles GET /api/v1/tuning/tcp.
func (a *TCPAdvisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	recs := a.GenerateRecommendations()

	w.Header().Set("Content-Type", "application/json")
	resp := struct {
		Recommendations []TCPRecommendation `json:"recommendations"`
		ConnectionCount int                 `json:"connection_count"`
	}{
		Recommendations: recs,
		ConnectionCount: a.connectionCount(),
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *TCPAdvisor) connectionCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.connStats)
}

func ipToString(ip uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d",
		ip&0xFF, (ip>>8)&0xFF, (ip>>16)&0xFF, (ip>>24)&0xFF)
}
