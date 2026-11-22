// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package diagnosis turns the node-local observations Gryvia already collects
// (Flight Recorder events, folded fabric status, cgroup v2 pressure and
// throttling counters, eBPF probe attach results and drop counters) into a
// ranked, evidence-backed per-job report.
//
// The rules are plain thresholds (Config), evaluated by the pure function
// Evaluate. There is no learning and no hidden state: the same Input always
// yields the same Diagnosis. Telemetry that could not be collected is listed as
// Unavailable and is never treated as healthy.
package diagnosis

import (
	"encoding/json"
	"strconv"
	"time"
)

// Finding kinds.
const (
	KindNetwork   = "network"
	KindStorage   = "storage"
	KindCPU       = "cpu"
	KindMemory    = "memory"
	KindGPUComm   = "gpu-comm"
	KindStraggler = "straggler"
	KindRDMA      = "rdma"
	KindExfil     = "exfil"
)

// Severities, ordered.
const (
	SevInfo     = "info"
	SevWarning  = "warning"
	SevCritical = "critical"
)

// Confidence levels, ordered.
const (
	ConfLow    = "low"
	ConfMedium = "medium"
	ConfHigh   = "high"
)

// SevRank orders severities (unknown -> 0).
func SevRank(s string) int {
	switch s {
	case SevInfo:
		return 1
	case SevWarning:
		return 2
	case SevCritical:
		return 3
	}
	return 0
}

// ConfRank orders confidences (unknown -> 0).
func ConfRank(c string) int {
	switch c {
	case ConfLow:
		return 1
	case ConfMedium:
		return 2
	case ConfHigh:
		return 3
	}
	return 0
}

// Evidence is one measured value a finding rests on.
type Evidence struct {
	Source string  `json:"source"` // flight, fabric, cgroup
	Metric string  `json:"metric"`
	Value  float64 `json:"value"`
	Window string  `json:"window"`
	Node   string  `json:"node,omitempty"` // set by the gateway when merging nodes
}

// Finding is one suspected bottleneck. It is a statement about what was
// measured, not a proven root cause.
type Finding struct {
	Kind               string     `json:"kind"`
	Severity           string     `json:"severity"`
	Confidence         string     `json:"confidence"`
	Evidence           []Evidence `json:"evidence"`
	Summary            string     `json:"summary"`
	WhatWasNotMeasured []string   `json:"whatWasNotMeasured"`
}

// Unavailable names telemetry that could not be collected and why.
type Unavailable struct {
	Signal string `json:"signal"`
	Reason string `json:"reason"`
}

// ProbeStatus is the outcome of loading one eBPF program (a copy of the
// loader's status, kept here so the package has no kernel-specific imports).
type ProbeStatus struct {
	Object   string `json:"object"`
	Program  string `json:"program"`
	Kind     string `json:"kind,omitempty"`
	Attached bool   `json:"attached"`
	Reason   string `json:"reason,omitempty"`
}

// DropCount is a producer-side or consumer-side loss counter.
type DropCount struct {
	Source string `json:"source"` // e.g. straggler.o/drops or userspace/gpu-decoder
	Count  uint64 `json:"count"`
}

// Count is a number that may be unknown; unknown marshals as the string "unknown".
type Count struct {
	N     int
	Known bool
}

// MarshalJSON implements json.Marshaler.
func (c Count) MarshalJSON() ([]byte, error) {
	if !c.Known {
		return []byte(`"unknown"`), nil
	}
	return []byte(strconv.Itoa(c.N)), nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *Count) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		*c = Count{}
		return nil // "unknown" or anything else that is not a number
	}
	*c = Count{N: n, Known: true}
	return nil
}

// Sampling states whether events are sampled. Ratio 1 means every event that
// reaches a ring or perf buffer is decoded; Filters lists threshold filters in
// the probes (events under the threshold are never emitted, which is not the same
// as sampling and not the same as the event not happening).
type Sampling struct {
	Ratio   float64  `json:"ratio"`
	Note    string   `json:"note"`
	Filters []string `json:"filters,omitempty"`
}

// DroppedEvents totals known losses.
type DroppedEvents struct {
	Total  uint64      `json:"total"`
	Counts []DropCount `json:"counts"`
	// NotCounted lists attached diagnosis-relevant programs whose object has no
	// `drops` map (an older build), so their ring losses are unknown.
	NotCounted []string `json:"notCounted,omitempty"`
}

// Completeness says what the report does not cover. Complete is true only when
// Reasons is empty.
type Completeness struct {
	ProbesAttached int           `json:"probesAttached"`
	ProbesSkipped  []ProbeStatus `json:"probesSkipped"`
	DroppedEvents  DroppedEvents `json:"droppedEvents"`
	Sampling       Sampling      `json:"sampling"`
	NodesExpected  Count         `json:"nodesExpected"`
	NodesReporting int           `json:"nodesReporting"`
	MissingNodes   []string      `json:"missingNodes"`
	Complete       bool          `json:"complete"`
	Reasons        []string      `json:"reasons"`
}

// Diagnosis is the per-job report.
type Diagnosis struct {
	Namespace   string        `json:"namespace"`
	Job         string        `json:"job"`
	Node        string        `json:"node,omitempty"`
	Scope       string        `json:"scope"`
	GeneratedAt time.Time     `json:"generatedAt"`
	Window      string        `json:"window"`
	Summary     string        `json:"summary"`
	Findings    []Finding     `json:"findings"`
	Unavailable []Unavailable `json:"unavailable"`
	// Measured lists the signals that were collected (with a value, possibly zero).
	Measured []string `json:"measured"`
	// Metrics holds every measured value by name; the incident store keeps it for
	// before/after comparison. Only measured signals appear.
	Metrics      map[string]float64 `json:"metrics"`
	Completeness Completeness       `json:"measurementCompleteness"`
}
