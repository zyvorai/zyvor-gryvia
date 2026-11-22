// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package security aggregates and serves security events detected by
// the eBPF security probes.
package security

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/decoder"
)

// SecurityAlert is an enriched, human-readable security alert derived
// from a raw eBPF security event.
type SecurityAlert struct {
	Timestamp   time.Time `json:"timestamp"`
	EventType   string    `json:"event_type"`
	Severity    string    `json:"severity"`
	PID         uint32    `json:"pid"`
	UID         uint32    `json:"uid"`
	ProcessName string    `json:"process_name"`
	Details     string    `json:"details"`
	SrcIP       string    `json:"src_ip,omitempty"`
	DstIP       string    `json:"dst_ip,omitempty"`
	DstPort     uint16    `json:"dst_port,omitempty"`
	Path        string    `json:"path,omitempty"`
}

// SecurityAggregator collects and categorises security events.
type SecurityAggregator struct {
	mu        sync.RWMutex
	alerts    []SecurityAlert
	maxAlerts int
	counters  map[uint8]int64 // per-event-type counters
}

// NewSecurityAggregator creates a SecurityAggregator with the given max
// number of alerts to retain in memory.
func NewSecurityAggregator(maxAlerts int) *SecurityAggregator {
	if maxAlerts <= 0 {
		maxAlerts = 10000
	}
	return &SecurityAggregator{
		maxAlerts: maxAlerts,
		counters:  make(map[uint8]int64),
	}
}

// Process ingests a raw security event, converts it to an alert, and stores it.
func (a *SecurityAggregator) Process(event decoder.SecurityEvent) {
	alert := SecurityAlert{
		Timestamp:   time.Now(),
		EventType:   decoder.SecurityEventTypeName(event.EventType),
		Severity:    decoder.SeverityName(event.Severity),
		PID:         event.PID,
		UID:         event.UID,
		ProcessName: event.CommString(),
		Path:        event.PathString(),
		SrcIP:       decoder.IPToString(event.SrcIP),
		DstIP:       decoder.IPToString(event.DstIP),
		DstPort:     event.DstPort,
	}

	// Build human-readable details.
	alert.Details = a.buildDetails(event)

	a.mu.Lock()
	defer a.mu.Unlock()

	a.counters[event.EventType]++
	a.alerts = append(a.alerts, alert)

	// Ring buffer: keep only the last maxAlerts.
	if len(a.alerts) > a.maxAlerts {
		a.alerts = a.alerts[len(a.alerts)-a.maxAlerts:]
	}
}

func (a *SecurityAggregator) buildDetails(event decoder.SecurityEvent) string {
	switch event.EventType {
	case decoder.SecEvtContainerEscape:
		return fmt.Sprintf("container escape attempt by PID %d (uid=%d)", event.PID, event.UID)
	case decoder.SecEvtPrivilegeEscalation:
		return fmt.Sprintf("privilege escalation: uid changed from %d to %d by PID %d",
			event.OldUID, event.NewUID, event.PID)
	case decoder.SecEvtCryptoMining:
		return fmt.Sprintf("crypto mining activity detected in process %s (PID %d)",
			event.CommString(), event.PID)
	case decoder.SecEvtDataExfiltration:
		return fmt.Sprintf("data exfiltration: %d bytes from PID %d (%s) to %s:%d",
			event.Bytes, event.PID, event.CommString(), decoder.IPToString(event.DstIP), event.DstPort)
	case decoder.SecEvtDriverTampering:
		if event.PathString() != "" {
			return fmt.Sprintf("GPU driver file %s modified by PID %d (%s)",
				event.PathString(), event.PID, event.CommString())
		}
		return fmt.Sprintf("kernel module operation (syscall %d) by PID %d (%s)",
			event.SyscallNr, event.PID, event.CommString())
	case decoder.SecEvtSuspiciousExec:
		return fmt.Sprintf("suspicious exec of %s by PID %d (uid=%d)",
			event.PathString(), event.PID, event.UID)
	case decoder.SecEvtNamespaceBreach:
		return fmt.Sprintf("namespace breach by PID %d (syscall %d)", event.PID, event.SyscallNr)
	default:
		return fmt.Sprintf("security event type=%d from PID %d", event.EventType, event.PID)
	}
}

// GetAlerts returns alerts filtered by severity. If severity is empty, all alerts
// are returned.
func (a *SecurityAggregator) GetAlerts(severity string) []SecurityAlert {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if severity == "" {
		result := make([]SecurityAlert, len(a.alerts))
		copy(result, a.alerts)
		return result
	}

	var result []SecurityAlert
	for _, alert := range a.alerts {
		if alert.Severity == severity {
			result = append(result, alert)
		}
	}
	return result
}

// GetCounts returns per-event-type counters with human-readable keys.
func (a *SecurityAggregator) GetCounts() map[string]int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make(map[string]int64, len(a.counters))
	for eventType, count := range a.counters {
		result[decoder.SecurityEventTypeName(eventType)] = count
	}
	return result
}

// ServeHTTP handles GET /api/v1/security/alerts.
// Query params: ?severity=critical|high|medium|low|info
func (a *SecurityAggregator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	severity := r.URL.Query().Get("severity")
	alerts := a.GetAlerts(severity)

	w.Header().Set("Content-Type", "application/json")
	resp := struct {
		Alerts []SecurityAlert  `json:"alerts"`
		Counts map[string]int64 `json:"counts"`
	}{
		Alerts: alerts,
		Counts: a.GetCounts(),
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
