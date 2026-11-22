// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package sources

import (
	"sort"
	"time"
)

type edgeKey struct {
	s, t, proto string
	port        uint16
}

// MergeGraphs merges per-node graphs. Nodes are unioned by ID (pod counts add). Edges with the same
// (source, target, protocol, port) add their bytes and flow counts; latency is the flow-count weighted
// mean of the nodes' smoothed latencies (unweighted when no node counted flows); last_seen is the newest.
// The output is sorted, so equal inputs give equal outputs.
func MergeGraphs(gs []Graph) Graph {
	nodes := map[string]Node{}
	type acc struct {
		Edge
		latWeighted, weight float64
		latSum              float64
		latN                int
	}
	edges := map[edgeKey]*acc{}
	for _, g := range gs {
		for _, n := range g.Nodes {
			id := n.ID
			if id == "" {
				id = n.Service
			}
			if id == "" {
				continue
			}
			cur, ok := nodes[id]
			if !ok {
				n.ID = id
				nodes[id] = n
				continue
			}
			cur.PodCount += n.PodCount
			if cur.Namespace == "" {
				cur.Namespace = n.Namespace
			}
			nodes[id] = cur
		}
		for _, e := range g.Edges {
			if e.Source == "" || e.Target == "" {
				continue
			}
			k := edgeKey{e.Source, e.Target, e.Protocol, e.Port}
			a := edges[k]
			if a == nil {
				a = &acc{Edge: Edge{Source: e.Source, Target: e.Target, Protocol: e.Protocol, Port: e.Port}}
				edges[k] = a
			}
			a.BytesTotal += e.BytesTotal
			a.FlowCount += e.FlowCount
			if e.LastSeen.After(a.LastSeen) {
				a.LastSeen = e.LastSeen
			}
			if e.LatencyMs > 0 {
				a.latWeighted += e.LatencyMs * float64(e.FlowCount)
				a.weight += float64(e.FlowCount)
				a.latSum += e.LatencyMs
				a.latN++
			}
		}
	}
	out := Graph{Nodes: make([]Node, 0, len(nodes)), Edges: make([]Edge, 0, len(edges))}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, n)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	for _, a := range edges {
		e := a.Edge
		switch {
		case a.weight > 0:
			e.LatencyMs = a.latWeighted / a.weight
		case a.latN > 0:
			e.LatencyMs = a.latSum / float64(a.latN)
		}
		out.Edges = append(out.Edges, e)
	}
	sort.Slice(out.Edges, func(i, j int) bool {
		a, b := out.Edges[i], out.Edges[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Protocol < b.Protocol
	})
	return out
}

// MergeAnomalies concatenates per-node anomaly lists, drops exact duplicates and sorts by detection time.
func MergeAnomalies(lists [][]Anomaly) []Anomaly {
	type key struct {
		t, svc, ns string
		at         time.Time
	}
	seen := map[key]bool{}
	var out []Anomaly
	for _, l := range lists {
		for _, a := range l {
			k := key{a.Type, a.Service, a.Namespace, a.DetectedAt}
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].DetectedAt.Equal(out[j].DetectedAt) {
			return out[i].DetectedAt.Before(out[j].DetectedAt)
		}
		return out[i].Type+out[i].Service < out[j].Type+out[j].Service
	})
	return out
}

// MergeAlerts concatenates per-node alert lists, drops exact duplicates and sorts by time.
func MergeAlerts(lists [][]SecurityAlert) []SecurityAlert {
	type key struct {
		at            time.Time
		t, proc, path string
		pid           uint32
		dst           string
	}
	seen := map[key]bool{}
	var out []SecurityAlert
	for _, l := range lists {
		for _, a := range l {
			k := key{a.Timestamp, a.EventType, a.ProcessName, a.Path, a.PID, a.DstIP}
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}
