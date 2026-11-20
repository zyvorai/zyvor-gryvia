// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package sources reads the data the network-intelligence controllers report: the per-node eBPF
// collector (HTTP, signed) and, optionally, Netra (HTTP, bearer). Nothing here invents data: a source
// that is not configured or not reachable is reported as a typed *SourceError so a controller can
// put an honest condition on its object instead of an empty status.
//
// Written from the collector's own code (collector/pkg/{graph,anomaly,security}) and the gateway
// client (services/api-gateway/routers/collector*.py). Verified against httptest servers only; it has
// NOT been run against a real collector DaemonSet or a live netrad.
package sources

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Reasons of a SourceError (used verbatim as the reason of the SourceAvailable condition).
const (
	ReasonNotConfigured = "NotConfigured"
	ReasonNoCollectors  = "NoCollectors"
	ReasonUnreachable   = "Unreachable"
	ReasonMisconfigured = "Misconfigured"
	ReasonNoData        = "NoData"
	ReasonPartial       = "PartialData"
	ReasonOK            = "Available"
)

// SourceError says why a source produced no data.
type SourceError struct {
	Source  string // "collector", "netra", "kube"
	Reason  string
	Message string
}

func (e *SourceError) Error() string { return fmt.Sprintf("%s: %s: %s", e.Source, e.Reason, e.Message) }

// AsSourceError returns err as a *SourceError (wrapping unknown errors as Unreachable).
func AsSourceError(source string, err error) *SourceError {
	var se *SourceError
	if errors.As(err, &se) {
		return se
	}
	return &SourceError{Source: source, Reason: ReasonUnreachable, Message: err.Error()}
}

// Stats says how many collectors answered.
type Stats struct {
	Reachable int
	Total     int
}

// Node is a service (or, when the collector could not resolve one, an IP) in the collector graph.
type Node struct {
	ID        string `json:"id"`
	Service   string `json:"service"`
	Namespace string `json:"namespace,omitempty"`
	PodCount  int    `json:"pod_count"`
}

// Edge is a directional service connection (collector/pkg/graph.Edge JSON).
type Edge struct {
	Source     string    `json:"source"`
	Target     string    `json:"target"`
	Protocol   string    `json:"protocol"`
	Port       uint16    `json:"port"`
	BytesTotal uint64    `json:"bytes_total"`
	LatencyMs  float64   `json:"latency_p50_ms"`
	LastSeen   time.Time `json:"last_seen"`
	FlowCount  uint64    `json:"flow_count"`
}

// Graph is the /api/v1/graph body.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Anomaly is one /api/v1/anomalies element (collector/pkg/anomaly.Anomaly JSON).
type Anomaly struct {
	Type       string    `json:"type"`
	Service    string    `json:"service"`
	Namespace  string    `json:"namespace,omitempty"`
	Value      float64   `json:"value"`
	Baseline   float64   `json:"baseline"`
	Stddev     float64   `json:"stddev"`
	Threshold  float64   `json:"threshold"`
	DetectedAt time.Time `json:"detected_at"`
	Message    string    `json:"message"`
}

// SecurityAlert is one /api/v1/security/alerts alert (collector/pkg/security.SecurityAlert JSON:
// event_type, process_name, src_ip, details ... not type/process/sourceIP/message).
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

// FlowRecord is one Netra flow-history record (fields as read by the gateway, routers/netra.py).
type FlowRecord struct {
	ObservedAt   time.Time `json:"observedAt"`
	Namespace    string    `json:"namespace"`
	Pod          string    `json:"pod"`
	WorkloadName string    `json:"workloadName"`
	Comm         string    `json:"comm"`
	Node         string    `json:"node"`
	Peer         string    `json:"peer"`
	Protocol     string    `json:"protocol"`
	Port         int       `json:"port"`
	Bytes        int64     `json:"bytes"`
	Blocked      bool      `json:"blocked"`
	SrttUs       int64     `json:"srttUs"`
	Direction    string    `json:"direction"`
}

// Collector is what the controllers need from the collector fleet. Every method merges the answers
// of all reachable nodes; a *SourceError means no data at all.
type Collector interface {
	Graph(ctx context.Context) (Graph, Stats, error)
	Anomalies(ctx context.Context) ([]Anomaly, Stats, error)
	SecurityAlerts(ctx context.Context) ([]SecurityAlert, Stats, error)
}

// FlowHistory is the optional Netra source.
type FlowHistory interface {
	Configured() bool
	History(ctx context.Context, since time.Duration, limit int) ([]FlowRecord, error)
}
