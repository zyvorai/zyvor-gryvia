// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package scheduler

import (
	"context"
	"math"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// Fabric-aware node ranking (opt-in). The per-node health comes from
// GryviaNodeFabric objects written by the eBPF collector. Everything here fails
// open: a missing, unreadable, malformed or expired signal never penalises.
// Signal quality depends on the collector's probes, which are unverified on
// real GPU/RDMA hardware.

const (
	// AnnotationFabricAware is the per-job override: "true" opts the job in even
	// when the operator flag is off, "false" opts it out even when it is on.
	AnnotationFabricAware = "gryvia.io/fabric-aware"

	// DefaultSignalTTL is used when a GryviaNodeFabric carries no ttlSeconds.
	DefaultSignalTTL = 5 * time.Minute
	maxSignalTTL     = 24 * time.Hour
	// maxClockSkew is how far in the future measuredAt may lie before the signal
	// is treated as invalid instead of "fresh".
	maxClockSkew = time.Minute

	// MaxExplained bounds how many top-ranked nodes are listed in the explanation;
	// penalised nodes of the pre-fabric top ranking are always included on top.
	MaxExplained = 8
	maxReasons   = 8

	listTimeout = 5 * time.Second
)

// NodeSignal is one node's fabric measurement.
type NodeSignal struct {
	Node       string
	ScoreDelta float64
	Reasons    []string
	MeasuredAt time.Time
	TTL        time.Duration // 0 = DefaultSignalTTL
}

func (s NodeSignal) ttl() time.Duration {
	if s.TTL <= 0 {
		return DefaultSignalTTL
	}
	if s.TTL > maxSignalTTL {
		return maxSignalTTL
	}
	return s.TTL
}

// Age is now - MeasuredAt.
func (s NodeSignal) Age(now time.Time) time.Duration { return now.Sub(s.MeasuredAt) }

// Fresh reports whether the signal may be used at now: it has a measurement
// time, is not from the future (beyond a small skew) and is no older than its ttl.
func (s NodeSignal) Fresh(now time.Time) bool {
	if s.Node == "" || s.MeasuredAt.IsZero() || math.IsNaN(s.ScoreDelta) {
		return false
	}
	age := s.Age(now)
	if age < -maxClockSkew {
		return false
	}
	return age <= s.ttl()
}

// FreshSignals keeps the usable signals by node name (a later duplicate of the
// same node replaces an earlier one only if it is newer).
func FreshSignals(sigs []NodeSignal, now time.Time) map[string]NodeSignal {
	out := map[string]NodeSignal{}
	for _, s := range sigs {
		if !s.Fresh(now) {
			continue
		}
		if cur, ok := out[s.Node]; ok && !s.MeasuredAt.After(cur.MeasuredAt) {
			continue
		}
		out[s.Node] = s
	}
	return out
}

// LoadNodeSignals lists GryviaNodeFabric objects. An error (missing CRD, no RBAC)
// is returned for logging; callers treat it as "no signals".
func LoadNodeSignals(ctx context.Context, c client.Client) ([]NodeSignal, error) {
	// The cached client blocks until an informer for the kind syncs; without the
	// CRD or RBAC that never happens, so bound the wait and fail open.
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	var l gryviav1.GryviaNodeFabricList
	if err := c.List(ctx, &l); err != nil {
		return nil, err
	}
	out := make([]NodeSignal, 0, len(l.Items))
	for _, it := range l.Items {
		name := it.Spec.NodeName
		if name == "" {
			name = it.Name
		}
		out = append(out, NodeSignal{
			Node:       name,
			ScoreDelta: it.Spec.ScoreDelta,
			Reasons:    it.Spec.Reasons,
			MeasuredAt: it.Spec.MeasuredAt.Time,
			TTL:        time.Duration(it.Spec.TTLSeconds) * time.Second,
		})
	}
	return out, nil
}

// FabricOptions configures fabric-aware ranking for one FindOptimalNodesFabric call.
type FabricOptions struct {
	// MaxPenalty limits the points subtracted per node; <=0 or above
	// MaxFabricPenalty means MaxFabricPenalty.
	MaxPenalty float64
	// Now is the clock (nil = time.Now).
	Now func() time.Time
	// Signals overrides the cluster lookup (tests).
	Signals func(ctx context.Context) ([]NodeSignal, error)
	// OnError receives a signal-lookup failure (the ranking then stays unchanged).
	OnError func(error)
}

// Placement is the result of a fabric-aware node choice.
type Placement struct {
	Nodes []string
	// Explanation is empty unless a fresh signal actually changed a node's score.
	Explanation []gryviav1.PlacementExplanation
}

// FabricEnabled resolves the opt-in: the job annotation wins over the operator
// default. Anything other than "true"/"false" falls back to the operator flag.
func FabricEnabled(job *gryviav1.GryviaAIJob, operatorDefault bool) bool {
	if job != nil {
		switch job.Annotations[AnnotationFabricAware] {
		case "true":
			return true
		case "false":
			return false
		}
	}
	return operatorDefault
}

// rankWithFabric ranks scored nodes (highest first) and returns the explanation.
// With no penalty in effect it returns exactly the ordering FindOptimalNodes
// always produced (sort.Slice on the untouched scores) and a nil explanation.
func rankWithFabric(scored []NodeScore, sigs map[string]NodeSignal, opt FabricOptions, now time.Time) ([]NodeScore, []gryviav1.PlacementExplanation) {
	deltas := make(map[string]float64, len(sigs))
	for n, s := range sigs {
		deltas[n] = s.ScoreDelta
	}
	max := opt.MaxPenalty
	if !(max > 0) || max > MaxFabricPenalty {
		max = MaxFabricPenalty
	}
	rescored := rescoreNodesCapped(scored, deltas, max)
	changed := false
	for i := range scored {
		if rescored[i].Score != scored[i].Score {
			changed = true
			break
		}
	}
	if !changed {
		base := append([]NodeScore(nil), scored...)
		sort.Slice(base, func(i, j int) bool { return base[i].Score > base[j].Score })
		return base, nil
	}

	// Pre-fabric ranking, to keep penalised nodes of the original top visible.
	baseRank := append([]NodeScore(nil), scored...)
	sort.SliceStable(baseRank, func(i, j int) bool { return baseRank[i].Score > baseRank[j].Score })
	baseScore := make(map[string]int, len(scored))
	for _, n := range scored {
		baseScore[n.NodeName] = n.Score
	}

	final := append([]NodeScore(nil), rescored...)
	// Stable, with node name as the final tie-break, so equal scores never flap.
	sort.SliceStable(final, func(i, j int) bool {
		if final[i].Score != final[j].Score {
			return final[i].Score > final[j].Score
		}
		return final[i].NodeName < final[j].NodeName
	})

	include := map[string]bool{}
	for i := 0; i < len(final) && i < MaxExplained; i++ {
		include[final[i].NodeName] = true
	}
	for i := 0; i < len(baseRank) && i < MaxExplained; i++ {
		n := baseRank[i].NodeName
		if s, ok := sigs[n]; ok && s.ScoreDelta > 0 {
			include[n] = true
		}
	}
	var expl []gryviav1.PlacementExplanation
	for _, n := range final {
		if !include[n.NodeName] {
			continue
		}
		e := gryviav1.PlacementExplanation{
			Node:          n.NodeName,
			BaseScore:     int32(baseScore[n.NodeName]),
			FinalScore:    int32(n.Score),
			FabricPenalty: int32(baseScore[n.NodeName] - n.Score),
		}
		if e.FabricPenalty > 0 {
			if s, ok := sigs[n.NodeName]; ok {
				e.Reasons = boundedReasons(s.Reasons)
				e.SignalAgeSeconds = int64(s.Age(now) / time.Second)
			}
		}
		expl = append(expl, e)
	}
	return final, expl
}

func boundedReasons(in []string) []string {
	if len(in) > maxReasons {
		in = in[:maxReasons]
	}
	out := make([]string, 0, len(in))
	for _, r := range in {
		if len(r) > 200 {
			r = r[:200]
		}
		out = append(out, r)
	}
	return out
}

// PreferredNodeTerms turns a fabric-aware decision into SOFT node affinity: the
// nodes the operator selected (nodesAllocated) get the maximum preference weight.
// It returns nil unless the explanation shows at least one penalised node, so a
// job the feature did not affect gets nothing. Preferred terms never make a pod
// unschedulable: if no selected node fits, the default scheduler places it
// elsewhere exactly as before.
func PreferredNodeTerms(selected []string, expl []gryviav1.PlacementExplanation) []corev1.PreferredSchedulingTerm {
	penalised := false
	for _, e := range expl {
		if e.FabricPenalty > 0 {
			penalised = true
			break
		}
	}
	if !penalised || len(selected) == 0 {
		return nil
	}
	terms := make([]corev1.PreferredSchedulingTerm, 0, len(selected))
	for _, n := range selected {
		terms = append(terms, corev1.PreferredSchedulingTerm{
			Weight: 100,
			Preference: corev1.NodeSelectorTerm{MatchFields: []corev1.NodeSelectorRequirement{{
				Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{n},
			}}},
		})
	}
	return terms
}

// AnnotationPinPlacement opts a job in to REQUIRED node affinity on the nodes the operator
// placed it on ("true"). Without it placement stays advisory (label selectors only).
const AnnotationPinPlacement = "gryvia.io/pin-placement"

// PinToNodes returns a copy of aff whose required node affinity also demands one of the given
// nodes. NodeSelectorTerms are ORed, so the node requirement is added to every existing term
// (a user's own required terms keep applying); with no terms one is created. nil/empty nodes
// returns aff unchanged. The input is never mutated.
func PinToNodes(aff *corev1.Affinity, nodes []string) *corev1.Affinity {
	if len(nodes) == 0 {
		return aff
	}
	out := aff.DeepCopy()
	if out == nil {
		out = &corev1.Affinity{}
	}
	if out.NodeAffinity == nil {
		out.NodeAffinity = &corev1.NodeAffinity{}
	}
	req := corev1.NodeSelectorRequirement{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: append([]string(nil), nodes...)}
	na := out.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}
	sel := na.RequiredDuringSchedulingIgnoredDuringExecution
	if len(sel.NodeSelectorTerms) == 0 {
		sel.NodeSelectorTerms = []corev1.NodeSelectorTerm{{}}
	}
	for i := range sel.NodeSelectorTerms {
		sel.NodeSelectorTerms[i].MatchFields = append(sel.NodeSelectorTerms[i].MatchFields, req)
	}
	return out
}
