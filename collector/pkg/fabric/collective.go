// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"math"
	"sort"
)

// SigCollective is one completed NCCL collective call on one rank, emitted by
// ebpf/straggler.c (FABRIC_SIG_COLLECTIVE). Field use:
//
//	Rank      rank in the communicator (RankUnknown when the probes never saw it)
//	WorldSize communicator size (0 when unknown)
//	PeerRank  communicator ORDINAL within the process (1-based)
//	PeerLatencyNS collective SEQUENCE on that communicator (1-based)
//	LatencyNS host-side duration of the API call (enqueue time for async launches)
//	Bytes     count * element size, NCCLOp the operation, Retries = CollFlag* bits,
//	RNR       low 32 bits of the ncclComm_t pointer (debug only; never comparable
//	          across processes or nodes).
const SigCollective uint8 = 10

// FABRIC_RANK_UNKNOWN and COLL_FLAG_* of fabric_signal.h.
const (
	RankUnknown         uint32 = 0xffffffff
	CollFlagLate        uint32 = 0x1 // comm first seen at a collective: ordinal/seq relative to attach time
	CollFlagRankUnknown uint32 = 0x2
)

// CommOrdinal is the communicator ordinal of a SigCollective signal.
func (s Signal) CommOrdinal() uint32 { return s.PeerRank }

// CollSeq is the per-communicator collective sequence of a SigCollective signal.
func (s Signal) CollSeq() uint64 { return s.PeerLatencyNS }

// CollSpan is one rank's view of one collective call, the input of MatchCollectives.
type CollSpan struct {
	Rank       uint32
	World      uint32 // 0 = unknown
	Ordinal    uint32
	Seq        uint64
	Op         uint8
	Bytes      uint64
	DurationNS uint64
	Late       bool
	PID        uint32
}

// RankDuration is one rank's duration inside a CollGroup.
type RankDuration struct {
	Rank       uint32 `json:"rank"`
	DurationNS uint64 `json:"durationNs"`
	PID        uint32 `json:"pid,omitempty"`
}

// CollGroup is the set of spans of ONE collective (same communicator ordinal,
// sequence and operation) seen on different ranks.
type CollGroup struct {
	Ordinal uint32         `json:"commOrdinal"`
	Seq     uint64         `json:"seq"`
	Op      uint8          `json:"op"`
	Bytes   uint64         `json:"bytes"`
	World   uint32         `json:"world"`
	Ranks   []RankDuration `json:"ranks"`
	// SkewNS = slowest - fastest duration among the ranks present (0 with one rank).
	SkewNS      uint64 `json:"skewNs"`
	SlowestRank uint32 `json:"slowestRank"`
	FastestRank uint32 `json:"fastestRank"`
	// Missing lists the ranks of [0, World) that were not observed. Ranks are
	// never invented: with an unknown World it stays empty.
	Missing []uint32 `json:"missingRanks,omitempty"`
	// Inconsistent: ranks disagreed on bytes or world size, or one rank appears
	// twice. Such a group is NOT compared (no skew, no slowest rank): the most
	// likely cause is misaligned sequence numbers (probes attached at different
	// times, a different communicator creation order), not a slow rank.
	Inconsistent bool `json:"inconsistent,omitempty"`
	// Late: at least one span came from a communicator first seen mid-run.
	Late bool `json:"late,omitempty"`
}

// Comparable reports whether the group has a trustworthy skew (>= 2 distinct
// ranks, all consistent).
func (g CollGroup) Comparable() bool { return !g.Inconsistent && len(g.Ranks) >= 2 }

type collKey struct {
	ordinal uint32
	seq     uint64
	op      uint8
}

// maxMissingRanks bounds CollGroup.Missing.
const maxMissingRanks = 64

// MatchCollectives groups spans by (communicator ordinal, seq, op) and computes
// per-group skew. Spans whose rank is unknown are dropped (they cannot be
// attributed); the ordinal is job-scoped, so callers must pass spans of ONE
// job only. Output is sorted by (ordinal, seq, op).
func MatchCollectives(spans []CollSpan) []CollGroup {
	type acc struct {
		g    CollGroup
		seen map[uint32]bool
	}
	by := map[collKey]*acc{}
	for _, s := range spans {
		if s.Rank == RankUnknown || s.Ordinal == 0 || s.Seq == 0 {
			continue
		}
		k := collKey{s.Ordinal, s.Seq, s.Op}
		a := by[k]
		if a == nil {
			a = &acc{g: CollGroup{Ordinal: s.Ordinal, Seq: s.Seq, Op: s.Op, Bytes: s.Bytes, World: s.World}, seen: map[uint32]bool{}}
			by[k] = a
		} else {
			if s.Bytes != a.g.Bytes {
				a.g.Inconsistent = true
			}
			switch {
			case a.g.World == 0:
				a.g.World = s.World // an earlier span did not know it: keep the known value
			case s.World != 0 && s.World != a.g.World:
				a.g.Inconsistent = true
			}
		}
		if a.seen[s.Rank] {
			a.g.Inconsistent = true
			continue
		}
		a.seen[s.Rank] = true
		a.g.Late = a.g.Late || s.Late
		a.g.Ranks = append(a.g.Ranks, RankDuration{Rank: s.Rank, DurationNS: s.DurationNS, PID: s.PID})
	}
	out := make([]CollGroup, 0, len(by))
	for _, a := range by {
		g := a.g
		sort.Slice(g.Ranks, func(i, j int) bool { return g.Ranks[i].Rank < g.Ranks[j].Rank })
		if g.World > 0 {
			for r := uint32(0); r < g.World && len(g.Missing) < maxMissingRanks; r++ {
				if !a.seen[r] {
					g.Missing = append(g.Missing, r)
				}
			}
		}
		if g.Comparable() {
			slow, fast := g.Ranks[0], g.Ranks[0]
			for _, r := range g.Ranks[1:] {
				if r.DurationNS > slow.DurationNS {
					slow = r
				}
				if r.DurationNS < fast.DurationNS {
					fast = r
				}
			}
			g.SkewNS = slow.DurationNS - fast.DurationNS
			g.SlowestRank, g.FastestRank = slow.Rank, fast.Rank
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ordinal != out[j].Ordinal {
			return out[i].Ordinal < out[j].Ordinal
		}
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].Op < out[j].Op
	})
	return out
}

// Thresholds of the matched-collective straggler rule (the old in-kernel rule
// kept its numbers: 2x the fastest rank and at least 5 ms).
const (
	stragglerRatio   = 2.0
	stragglerFloorNS = 5_000_000
)

// straggling reports whether a comparable group has a straggler.
func straggling(g CollGroup) bool {
	if !g.Comparable() {
		return false
	}
	var fast, slow uint64 = math.MaxUint64, 0
	for _, r := range g.Ranks {
		if r.DurationNS < fast {
			fast = r.DurationNS
		}
		if r.DurationNS > slow {
			slow = r.DurationNS
		}
	}
	return fast > 0 && slow > stragglerFloorNS && float64(slow) > stragglerRatio*float64(fast)
}

func spanOf(s Signal) CollSpan {
	return CollSpan{
		Rank: s.Rank, World: s.WorldSize, Ordinal: s.CommOrdinal(), Seq: s.CollSeq(), Op: s.NCCLOp,
		Bytes: s.Bytes, DurationNS: s.LatencyNS, Late: s.Retries&CollFlagLate != 0, PID: s.PID,
	}
}

// Signals synthesised in userspace from NIC hardware counters (pkg/nic); they
// never appear on a ring. Bytes = the event count since the previous poll. They
// are numbered from 128 so they cannot collide with the ring types in
// fabric_signal.h (11-22 are real ring signals; they used to share 11-14).
const (
	SigNICRetry uint8 = 128 // transport retries / sequence errors
	SigNICError uint8 = 129 // link, symbol, discard errors
	SigNICCNP   uint8 = 130 // CNPs handled by the NIC (rp_cnp_handled); zero counts are recorded to mark "NIC counters present"
	SigNICPause uint8 = 131 // pause frames counted by the NIC; zero counts recorded too
)

// NICJob is the node-level key under which NIC retry/error events are folded.
var NICJob = JobKey{Namespace: "_node", Job: "nic"}

// AddNIC records the NIC hardware counter events counted since the previous
// poll. cnp/pause < 0 means the NIC does not export that counter (nothing is
// recorded, so the XDP-derived value stays in use); >= 0 (even 0) records it,
// which makes it authoritative for CNPRate/PFCRate (no double counting).
// retry/errors are recorded when > 0.
func (f *Folder) AddNIC(retry, errors, cnp, pause int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if retry > 0 {
		f.addLocked(NICJob, Signal{Type: SigNICRetry, Bytes: uint64(retry), Comm: "nic"})
	}
	if errors > 0 {
		f.addLocked(NICJob, Signal{Type: SigNICError, Bytes: uint64(errors), Comm: "nic"})
	}
	if cnp >= 0 {
		f.addLocked(CNPJob, Signal{Type: SigNICCNP, Bytes: uint64(cnp), Comm: "cnp"})
	}
	if pause >= 0 {
		f.addLocked(PFCJob, Signal{Type: SigNICPause, Bytes: uint64(pause), Comm: "pfc"})
	}
}
