// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package graph builds and maintains an in-memory service dependency
// graph derived from observed network flow events.  The graph is
// exposed via an HTTP endpoint as JSON for consumption by the API
// server and web UI.
package graph

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/decoder"
)

// Node represents a service (or pod) in the dependency graph.
type Node struct {
	ID        string `json:"id"`
	Service   string `json:"service"`
	Namespace string `json:"namespace,omitempty"`
	PodCount  int    `json:"pod_count"`
}

// Edge represents observed traffic between two services.
type Edge struct {
	Source     string    `json:"source"`
	Target     string    `json:"target"`
	Protocol   string    `json:"protocol"`
	Port       uint16    `json:"port"`
	BytesTotal uint64    `json:"bytes_total"`
	Latency    float64   `json:"latency_p50_ms"`
	LastSeen   time.Time `json:"last_seen"`
	FlowCount  uint64    `json:"flow_count"`
}

// Graph is the full dependency graph snapshot.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// edgeKey uniquely identifies a directional edge.
type edgeKey struct {
	source string
	target string
	port   uint16
}

// Builder maintains the live service dependency graph.
type Builder struct {
	mu       sync.RWMutex
	nodes    map[string]*Node
	edges    map[edgeKey]*Edge
	staleTTL time.Duration
}

// NewBuilder creates a Builder with a default stale-edge TTL of 10 minutes.
func NewBuilder() *Builder {
	b := &Builder{
		nodes:    make(map[string]*Node),
		edges:    make(map[edgeKey]*Edge),
		staleTTL: 10 * time.Minute,
	}
	go b.pruneLoop()
	return b
}

// RecordFlow updates the graph with a single observed flow event.
func (b *Builder) RecordFlow(ev decoder.FlowEvent) {
	srcID := ev.SrcService
	if srcID == "" {
		srcID = decoder.IPToString(ev.SrcIP)
	}
	dstID := ev.DstService
	if dstID == "" {
		dstID = decoder.IPToString(ev.DstIP)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Upsert source node.
	if _, ok := b.nodes[srcID]; !ok {
		b.nodes[srcID] = &Node{
			ID:        srcID,
			Service:   srcID,
			Namespace: ev.Namespace,
			PodCount:  1,
		}
	}
	// Upsert destination node.
	if _, ok := b.nodes[dstID]; !ok {
		b.nodes[dstID] = &Node{
			ID:        dstID,
			Service:   dstID,
			Namespace: ev.Namespace,
			PodCount:  1,
		}
	}

	// Upsert edge.
	key := edgeKey{source: srcID, target: dstID, port: ev.DstPort}
	edge, ok := b.edges[key]
	if !ok {
		edge = &Edge{
			Source:   srcID,
			Target:   dstID,
			Protocol: protoName(ev.Protocol),
			Port:     ev.DstPort,
		}
		b.edges[key] = edge
	}

	edge.BytesTotal += uint64(ev.Bytes)
	edge.FlowCount++
	edge.LastSeen = time.Now()
	if ev.LatencyNs > 0 {
		// Simple exponential moving average for display latency.
		newLatency := float64(ev.LatencyNs) / 1e6 // ns -> ms
		if edge.Latency == 0 {
			edge.Latency = newLatency
		} else {
			edge.Latency = edge.Latency*0.9 + newLatency*0.1
		}
	}
}

// Snapshot returns a point-in-time copy of the graph.
func (b *Builder) Snapshot() Graph {
	b.mu.RLock()
	defer b.mu.RUnlock()

	g := Graph{
		Nodes: make([]Node, 0, len(b.nodes)),
		Edges: make([]Edge, 0, len(b.edges)),
	}
	for _, n := range b.nodes {
		g.Nodes = append(g.Nodes, *n)
	}
	for _, e := range b.edges {
		g.Edges = append(g.Edges, *e)
	}
	return g
}

// ServeHTTP handles GET /api/v1/graph and returns the graph as JSON.
func (b *Builder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	snap := b.Snapshot()
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (b *Builder) pruneLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		b.pruneStale()
	}
}

func (b *Builder) pruneStale() {
	cutoff := time.Now().Add(-b.staleTTL)

	b.mu.Lock()
	defer b.mu.Unlock()

	// Remove stale edges.
	for key, edge := range b.edges {
		if edge.LastSeen.Before(cutoff) {
			delete(b.edges, key)
		}
	}

	// Remove orphan nodes (no edges reference them).
	referenced := make(map[string]bool)
	for _, edge := range b.edges {
		referenced[edge.Source] = true
		referenced[edge.Target] = true
	}
	for id := range b.nodes {
		if !referenced[id] {
			delete(b.nodes, id)
		}
	}
}

func protoName(proto uint8) string {
	switch proto {
	case 6:
		return "TCP"
	case 17:
		return "UDP"
	default:
		return "unknown"
	}
}
