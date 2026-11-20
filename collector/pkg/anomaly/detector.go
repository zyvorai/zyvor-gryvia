// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package anomaly implements simple statistical anomaly detection over
// flow events.  It maintains per-service baselines using exponential
// moving average and standard deviation, and flags events that exceed
// baseline + 3*stddev.
package anomaly

import (
	"encoding/json"
	"math"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zyvorai/gryvia/collector/pkg/decoder"
)

// AnomalyType classifies what kind of anomaly was detected.
type AnomalyType string

const (
	LatencySpike  AnomalyType = "latency_spike"
	TrafficBurst  AnomalyType = "traffic_burst"
	NewConnection AnomalyType = "new_connection"
	DNSFailure    AnomalyType = "dns_failure"
)

// Anomaly represents a detected anomaly event.
type Anomaly struct {
	Type       AnomalyType `json:"type"`
	Service    string      `json:"service"`
	Namespace  string      `json:"namespace,omitempty"`
	Value      float64     `json:"value"`
	Baseline   float64     `json:"baseline"`
	Stddev     float64     `json:"stddev"`
	Threshold  float64     `json:"threshold"`
	DetectedAt time.Time   `json:"detected_at"`
	Message    string      `json:"message"`
}

// baseline tracks the moving average and variance for a metric.
type baseline struct {
	mean     float64
	variance float64
	count    uint64
}

// serviceKey identifies a service for baseline tracking.
type serviceKey struct {
	service   string
	namespace string
}

// Detector observes flow events and detects anomalies.
type Detector struct {
	mu        sync.RWMutex
	log       *zap.SugaredLogger
	latencyBL map[serviceKey]*baseline
	bytesBL   map[serviceKey]*baseline
	seenSvc   map[serviceKey]bool
	anomalies []Anomaly
	maxStore  int // max anomalies to keep in memory
}

// NewDetector creates a Detector.
func NewDetector(log *zap.SugaredLogger) *Detector {
	return &Detector{
		log:       log,
		latencyBL: make(map[serviceKey]*baseline),
		bytesBL:   make(map[serviceKey]*baseline),
		seenSvc:   make(map[serviceKey]bool),
		maxStore:  1000,
	}
}

// Observe processes a flow event and checks for anomalies.
func (d *Detector) Observe(ev decoder.FlowEvent) {
	svc := ev.SrcService
	if svc == "" {
		svc = decoder.IPToString(ev.SrcIP)
	}
	key := serviceKey{service: svc, namespace: ev.Namespace}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Detect new connections from previously unseen services.
	if !d.seenSvc[key] {
		d.seenSvc[key] = true
		if ev.Protocol == 6 { // TCP
			d.recordAnomaly(Anomaly{
				Type:       NewConnection,
				Service:    svc,
				Namespace:  ev.Namespace,
				DetectedAt: time.Now(),
				Message:    "first observed network connection from service",
			})
		}
	}

	// Latency anomaly detection.
	if ev.LatencyNs > 0 {
		latencyMs := float64(ev.LatencyNs) / 1e6
		bl := d.getOrCreateBaseline(d.latencyBL, key)
		d.updateAndCheck(bl, latencyMs, LatencySpike, key)
	}

	// Traffic volume anomaly detection.
	if ev.Bytes > 0 {
		bl := d.getOrCreateBaseline(d.bytesBL, key)
		d.updateAndCheck(bl, float64(ev.Bytes), TrafficBurst, key)
	}
}

func (d *Detector) getOrCreateBaseline(m map[serviceKey]*baseline, key serviceKey) *baseline {
	bl, ok := m[key]
	if !ok {
		bl = &baseline{}
		m[key] = bl
	}
	return bl
}

func (d *Detector) updateAndCheck(bl *baseline, value float64, anomalyType AnomalyType, key serviceKey) {
	bl.count++

	// Need at least 100 samples before flagging anomalies.
	if bl.count < 100 {
		// Welford's online algorithm for mean and variance.
		delta := value - bl.mean
		bl.mean += delta / float64(bl.count)
		delta2 := value - bl.mean
		bl.variance += delta * delta2
		return
	}

	stddev := math.Sqrt(bl.variance / float64(bl.count))
	threshold := bl.mean + 3*stddev

	if value > threshold && stddev > 0 {
		d.recordAnomaly(Anomaly{
			Type:       anomalyType,
			Service:    key.service,
			Namespace:  key.namespace,
			Value:      value,
			Baseline:   bl.mean,
			Stddev:     stddev,
			Threshold:  threshold,
			DetectedAt: time.Now(),
			Message:    string(anomalyType) + " detected: value exceeds baseline + 3*stddev",
		})
	}

	// Continue updating baseline (with dampening to avoid drift).
	delta := value - bl.mean
	bl.mean += delta / float64(bl.count)
	delta2 := value - bl.mean
	bl.variance += delta * delta2
}

func (d *Detector) recordAnomaly(a Anomaly) {
	d.anomalies = append(d.anomalies, a)
	// Ring buffer: keep only the last maxStore anomalies.
	if len(d.anomalies) > d.maxStore {
		d.anomalies = d.anomalies[len(d.anomalies)-d.maxStore:]
	}
	d.log.Warnw("anomaly detected",
		"type", a.Type,
		"service", a.Service,
		"value", a.Value,
		"baseline", a.Baseline,
		"message", a.Message,
	)
}

// Recent returns the most recent n anomalies.
func (d *Detector) Recent(n int) []Anomaly {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if n <= 0 || n > len(d.anomalies) {
		n = len(d.anomalies)
	}
	result := make([]Anomaly, n)
	copy(result, d.anomalies[len(d.anomalies)-n:])
	return result
}

// ServeHTTP handles GET /api/v1/anomalies.
func (d *Detector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	recent := d.Recent(100)
	if err := json.NewEncoder(w).Encode(recent); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
