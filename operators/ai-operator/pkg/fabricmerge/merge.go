// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package fabricmerge folds the per-node entries collectors write into a GryviaFabricSignal's
// status.nodes[] into the top-level status fields (opt-in: ai-operator --merge-fabric-signals).
//
// Rules: degradation metrics take the maximum over fresh nodes; ratios are means weighted by the
// node's sampleCount (minimum weight 1), skipping nodes that did not measure them; stragglerRank
// comes from the node with the highest ncclP99ms; updatedAt is the newest measuredAt; an entry
// older than its ttlSeconds is ignored, and when every entry is stale nothing is folded so the
// existing top-level status is left as it is.
package fabricmerge

import (
	"math"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// DefaultTTL applies to an entry with ttlSeconds unset.
const DefaultTTL = 300 * time.Second

// Fresh reports whether the entry is still valid at now (an entry stamped in the future, from a
// skewed clock, counts as fresh).
func Fresh(n gryviav1.FabricNodeStatus, now time.Time) bool {
	ttl := DefaultTTL
	if n.TTLSeconds > 0 {
		ttl = time.Duration(n.TTLSeconds) * time.Second
	}
	return !n.MeasuredAt.IsZero() && now.Sub(n.MeasuredAt.Time) <= ttl
}

// Expiry returns when the entry stops being fresh.
func Expiry(n gryviav1.FabricNodeStatus) time.Time {
	ttl := DefaultTTL
	if n.TTLSeconds > 0 {
		ttl = time.Duration(n.TTLSeconds) * time.Second
	}
	return n.MeasuredAt.Add(ttl)
}

type wmean struct{ sum, weight float64 }

func (w *wmean) add(v *float64, weight float64) {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return
	}
	w.sum += *v * weight
	w.weight += weight
}

func (w wmean) value() float64 {
	if w.weight == 0 {
		return 0
	}
	return w.sum / w.weight
}

func maxF(cur float64, v *float64) float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return cur
	}
	return math.Max(cur, *v)
}

// Merge folds the fresh entries of nodes into the top-level status fields. The returned status
// carries every merged field (zero/nil when no fresh node measured it) and Nodes = nodes; ok is
// false when no entry is fresh, in which case the caller must leave the status untouched.
func Merge(now time.Time, nodes []gryviav1.FabricNodeStatus) (out gryviav1.GryviaFabricSignalStatus, ok bool) {
	var (
		gds, overlap, idle, smActive, cover wmean
		maxNCCL                             = -1.0
		rankNode                            string
		rank                                *int32
		latest                              time.Time
		engine                              string
		mixed                               bool
		fallbackRank                        *int32
		fallbackAt                          time.Time
		requests                            *int64
	)
	for _, n := range nodes {
		if !Fresh(n, now) {
			continue
		}
		ok = true
		w := math.Max(1, float64(n.SampleCount))
		if n.MeasuredAt.Time.After(latest) {
			latest = n.MeasuredAt.Time
		}

		out.ScoreDelta = maxF(out.ScoreDelta, n.ScoreDelta)
		out.NCCLP99Ms = maxF(out.NCCLP99Ms, n.NCCLP99Ms)
		out.RDMARetryRate = maxF(out.RDMARetryRate, n.RDMARetryRate)
		out.PFCRate = maxF(out.PFCRate, n.PFCRate)
		out.CNPRate = maxF(out.CNPRate, n.CNPRate)
		out.InferWaitP99Ms = maxF(out.InferWaitP99Ms, n.InferWaitP99Ms)
		out.CollectiveMaxSkewMs = maxF(out.CollectiveMaxSkewMs, n.CollectiveMaxSkewMs)
		if n.ExfilEvents != nil && *n.ExfilEvents > out.ExfilEvents {
			out.ExfilEvents = *n.ExfilEvents
		}

		gds.add(n.GDSHitRatio, w)
		overlap.add(n.OverlapIdleRatio, w)
		idle.add(n.GPUIdleDuringCommRatio, w)
		smActive.add(n.SMActiveDuringCompute, w)
		cover.add(n.GPUCorrelationCoverage, w)

		// Straggler: the node with the highest NCCL p99 names the rank (ties: lowest node name,
		// so the result does not depend on list order).
		if n.StragglerRank != nil {
			if n.NCCLP99Ms != nil && (*n.NCCLP99Ms > maxNCCL || (*n.NCCLP99Ms == maxNCCL && n.Node < rankNode)) {
				maxNCCL, rankNode, rank = *n.NCCLP99Ms, n.Node, n.StragglerRank
			}
			if fallbackRank == nil || n.MeasuredAt.Time.After(fallbackAt) {
				fallbackRank, fallbackAt = n.StragglerRank, n.MeasuredAt.Time
			}
		}

		// Inference engine fields: worst node.
		out.TTFTP99Ms = maxPtr(out.TTFTP99Ms, n.TTFTP99Ms)
		out.ITLP99Ms = maxPtr(out.ITLP99Ms, n.ITLP99Ms)
		out.QueueTimeP99Ms = maxPtr(out.QueueTimeP99Ms, n.QueueTimeP99Ms)
		out.E2EP99Ms = maxPtr(out.E2EP99Ms, n.E2EP99Ms)
		out.QueueTimeMeanMs = maxPtr(out.QueueTimeMeanMs, n.QueueTimeMeanMs)
		out.E2EMeanMs = maxPtr(out.E2EMeanMs, n.E2EMeanMs)
		out.KVCacheUsage = maxPtr(out.KVCacheUsage, n.KVCacheUsage)
		if n.RequestsWaiting != nil && (requests == nil || *n.RequestsWaiting > *requests) {
			v := *n.RequestsWaiting
			requests = &v
		}
		if n.Engine != nil && *n.Engine != "" {
			switch {
			case engine == "":
				engine = *n.Engine
			case engine != *n.Engine:
				mixed = true
			}
		}
	}
	if !ok {
		return gryviav1.GryviaFabricSignalStatus{}, false
	}
	switch {
	case rank != nil:
		out.StragglerRank = *rank
	case fallbackRank != nil:
		out.StragglerRank = *fallbackRank
	}
	out.GDSHitRatio = gds.value()
	out.OverlapIdleRatio = overlap.value()
	out.GPUIdleDuringCommRatio = idle.value()
	out.SMActiveDuringCompute = smActive.value()
	out.GPUCorrelationCoverage = cover.value()
	out.RequestsWaiting = requests
	if mixed {
		engine = "mixed"
	}
	out.Engine = engine
	if !latest.IsZero() {
		t := metav1.NewTime(latest.Truncate(time.Second))
		out.UpdatedAt = &t
	}
	out.Nodes = nodes
	return out, true
}

func maxPtr(cur, v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return cur
	}
	if cur == nil || *v > *cur {
		c := *v
		return &c
	}
	return cur
}

// Apply writes the merged fields over st, keeping st.Nodes as it is. Fields not measured by any
// fresh node are cleared, so a recovered value disappears.
func Apply(st *gryviav1.GryviaFabricSignalStatus, merged gryviav1.GryviaFabricSignalStatus) {
	nodes := st.Nodes
	*st = merged
	st.Nodes = nodes
}
