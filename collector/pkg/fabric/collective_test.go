// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/dcgm"
)

// TestCollectiveConstantsMatchC pins the Go mirror of the C enum/flags/struct.
func TestCollectiveConstantsMatchC(t *testing.T) {
	src, err := os.ReadFile("../../../ebpf/headers/fabric_signal.h")
	if err != nil {
		t.Skipf("C header not available: %v", err)
	}
	num := func(re string) uint64 {
		m := regexp.MustCompile(re).FindSubmatch(src)
		if m == nil {
			t.Fatalf("no match for %s", re)
		}
		v, err := strconv.ParseUint(string(m[1]), 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := num(`FABRIC_SIG_COLLECTIVE\s*=\s*(\d+)`); got != uint64(SigCollective) {
		t.Errorf("SigCollective = %d, header %d", SigCollective, got)
	}
	if got := num(`COLL_FLAG_LATE\s+(0x\w+)`); got != uint64(CollFlagLate) {
		t.Errorf("CollFlagLate mismatch: header %d", got)
	}
	if got := num(`COLL_FLAG_RANK_UNK\s+(0x\w+)`); got != uint64(CollFlagRankUnknown) {
		t.Errorf("CollFlagRankUnknown mismatch: header %d", got)
	}
	if got := num(`FABRIC_RANK_UNKNOWN\s+(0x\w+)U`); got != uint64(RankUnknown) {
		t.Errorf("RankUnknown mismatch: header %d", got)
	}
	if got := num(`sizeof\(struct comm_info\) == (\d+)`); got != 24 {
		t.Errorf("comm_info size %d changed: update straggler.c users and this test", got)
	}
}

func span(rank uint32, ord uint32, seq uint64, op uint8, bytes, dur uint64) CollSpan {
	return CollSpan{Rank: rank, World: 4, Ordinal: ord, Seq: seq, Op: op, Bytes: bytes, DurationNS: dur}
}

func TestMatchCollectivesSkewAndSlowestRank(t *testing.T) {
	spans := []CollSpan{
		span(0, 1, 1, 0, 1024, 100), span(1, 1, 1, 0, 1024, 900), span(2, 1, 1, 0, 1024, 150),
		// a different collective on the same comm must not mix with seq 1
		span(0, 1, 2, 0, 1024, 500), span(1, 1, 2, 0, 1024, 510),
		// the same seq on ANOTHER communicator is a different collective
		span(0, 2, 1, 1, 64, 10),
	}
	gs := MatchCollectives(spans)
	if len(gs) != 3 {
		t.Fatalf("want 3 groups, got %d: %+v", len(gs), gs)
	}
	g := gs[0]
	if g.Ordinal != 1 || g.Seq != 1 || g.SkewNS != 800 || g.SlowestRank != 1 || g.FastestRank != 0 {
		t.Errorf("group 1/1 = %+v", g)
	}
	if len(g.Missing) != 1 || g.Missing[0] != 3 {
		t.Errorf("missing ranks = %v, want [3] (rank 3 of world 4 never reported)", g.Missing)
	}
	if gs[1].SkewNS != 10 || gs[1].SlowestRank != 1 {
		t.Errorf("group 1/2 = %+v", gs[1])
	}
	if gs[2].Comparable() || gs[2].SkewNS != 0 || len(gs[2].Ranks) != 1 {
		t.Errorf("single-rank group must not be compared: %+v", gs[2])
	}
}

func TestMatchCollectivesNeverInvents(t *testing.T) {
	// unknown rank dropped; unknown world -> no missing list at all
	gs := MatchCollectives([]CollSpan{
		{Rank: RankUnknown, World: 0, Ordinal: 1, Seq: 1, DurationNS: 5},
		{Rank: 3, World: 0, Ordinal: 1, Seq: 1, DurationNS: 7},
		{Rank: 5, World: 0, Ordinal: 1, Seq: 1, DurationNS: 9},
	})
	if len(gs) != 1 || len(gs[0].Ranks) != 2 || gs[0].Missing != nil {
		t.Fatalf("got %+v", gs)
	}
	if gs[0].SlowestRank != 5 || gs[0].SkewNS != 2 {
		t.Errorf("got %+v", gs[0])
	}
}

func TestMatchCollectivesInconsistentIsNotCompared(t *testing.T) {
	for name, spans := range map[string][]CollSpan{
		"bytes differ": {span(0, 1, 1, 0, 1024, 100), span(1, 1, 1, 0, 2048, 900)},
		"world differs": {span(0, 1, 1, 0, 1024, 100),
			{Rank: 1, World: 8, Ordinal: 1, Seq: 1, Op: 0, Bytes: 1024, DurationNS: 900}},
		"rank twice": {span(0, 1, 1, 0, 1024, 100), span(0, 1, 1, 0, 1024, 900)},
	} {
		gs := MatchCollectives(spans)
		if len(gs) != 1 || !gs[0].Inconsistent || gs[0].Comparable() || gs[0].SkewNS != 0 {
			t.Errorf("%s: %+v", name, gs)
		}
	}
	// a rank that did not know the world size adopts the known one
	gs := MatchCollectives([]CollSpan{
		{Rank: 0, World: 0, Ordinal: 1, Seq: 1, Bytes: 8, DurationNS: 1},
		{Rank: 1, World: 4, Ordinal: 1, Seq: 1, Bytes: 8, DurationNS: 2},
	})
	if gs[0].Inconsistent || gs[0].World != 4 {
		t.Errorf("%+v", gs[0])
	}
}

func collSig(pid, rank uint32, ord uint32, seq uint64, dur uint64) Signal {
	return Signal{Type: SigCollective, PID: pid, Rank: rank, WorldSize: 2, PeerRank: ord, PeerLatencyNS: seq,
		LatencyNS: dur, Bytes: 4096, NCCLOp: 0, Comm: "python"}
}

func TestFolderMatchedStraggler(t *testing.T) {
	f, c := newTestFolder()
	f.Bind(10, "ml", "train")
	f.Bind(11, "ml", "train")
	for seq := uint64(1); seq <= 12; seq++ {
		f.Add(collSig(10, 0, 1, seq, 1_000_000))
		f.Add(collSig(11, 1, 1, seq, 30_000_000)) // rank 1 is 30x slower
		c.t = c.t.Add(time.Second)
	}
	st := f.Snapshot()[JobKey{"ml", "train"}]
	if st.CollectivesCompared != 12 || st.StragglerHits != 12 || st.StragglerRank != 1 || st.StragglerPID != 11 {
		t.Fatalf("status %+v", st)
	}
	if st.CollectiveMaxSkewMS != 29 || st.NCCLP99MS != 30 {
		t.Errorf("skew %v p99 %v", st.CollectiveMaxSkewMS, st.NCCLP99MS)
	}
	if st.ScoreDelta != penaltyStraggler {
		t.Errorf("score %v", st.ScoreDelta)
	}
}

func TestFolderUnmatchedCollectivesAreNotStragglers(t *testing.T) {
	// The old heuristic compared unrelated calls: same payload, different seq must not pair up.
	f, _ := newTestFolder()
	f.Bind(10, "ml", "train")
	f.Bind(11, "ml", "train")
	f.Add(collSig(10, 0, 1, 1, 1_000_000))
	f.Add(collSig(11, 1, 1, 2, 90_000_000))
	st := only(t, f)
	if st.CollectivesCompared != 0 || st.StragglerHits != 0 {
		t.Errorf("%+v", st)
	}
}

func TestFolderCollectivesDoNotEvictOtherSignals(t *testing.T) {
	f, _ := newTestFolder()
	f.Bind(10, "ml", "train")
	f.Add(Signal{Type: SigOverlap, PID: 10, LatencyNS: 1000})
	for i := 0; i < maxSamples+10; i++ {
		f.Add(collSig(10, 0, 1, uint64(i+1), 1000))
	}
	if f.Snapshot()[JobKey{"ml", "train"}].OverlapIdleRatio == 0 {
		t.Error("collective flood evicted the overlap sample")
	}
}

func TestFolderGPUCorrelation(t *testing.T) {
	f, c := newTestFolder()
	f.Bind(10, "ml", "train")
	start := c.t
	// one 2 s NCCL call ending at start+2s
	c.t = start.Add(2 * time.Second)
	f.Add(collSig(10, 0, 1, 1, 2_000_000_000))
	var samples []dcgm.GPUSample
	for i := 1; i <= 2; i++ { // 1 s cadence: (start,start+1s] idle, (start+1s,start+2s] busy
		v := 0.02
		if i == 2 {
			v = 0.9
		}
		samples = append(samples, dcgm.GPUSample{At: start.Add(time.Duration(i) * time.Second),
			Values: map[string]float64{dcgm.FieldSMActive: v}})
	}
	samples = append(samples, dcgm.GPUSample{At: start, Values: map[string]float64{dcgm.FieldSMActive: 0.02}})
	f.SetGPUSource(func(k JobKey, from time.Time) []dcgm.GPUSample {
		if k != (JobKey{"ml", "train"}) {
			t.Errorf("wrong job %v", k)
		}
		return samples
	}, 0.1)
	st := only(t, f)
	if !st.GPUCorrelationMeasured || st.GPUIdleDuringCommRatio < 0.49 || st.GPUIdleDuringCommRatio > 0.51 {
		t.Fatalf("%+v", st)
	}
	if st.GPUCorrelationCoverage < 0.99 {
		t.Errorf("coverage %v", st.GPUCorrelationCoverage)
	}
	// informational only: the GPU fields must not move the score
	if st.ScoreDelta != 0 {
		t.Errorf("GPU correlation changed the score: %v", st.ScoreDelta)
	}
}

func TestFolderNICPreferredOverXDP(t *testing.T) {
	f, c := newTestFolder() // window 1 min
	// XDP-derived CNPs and pause frames first
	f.AddCNP(6000)
	f.AddPFC(60000)
	_ = c
	st := f.Snapshot()[CNPJob]
	if st.CNPRate != 100 {
		t.Fatalf("XDP-only CNP rate = %v, want 6000/60s", st.CNPRate)
	}
	// NIC counters present (CNP = 600 in the window, pause counter present but 0)
	f.AddNIC(30, 6, 600, 0)
	snap := f.Snapshot()
	if got := snap[CNPJob].CNPRate; got != 10 {
		t.Errorf("CNP rate with NIC counters = %v, want 10 (NIC only, no double counting)", got)
	}
	if got := snap[PFCJob].PFCRate; got != 0 {
		t.Errorf("PFC rate with NIC pause counter = %v, want 0 (NIC authoritative)", got)
	}
	n := snap[NICJob]
	if n.NICRetryRate != 0.5 || n.NICErrorRate != 0.1 {
		t.Errorf("nic rates %+v", n)
	}
	// NIC that exports no CNP counter (-1) leaves XDP in charge
	f2, _ := newTestFolder()
	f2.AddCNP(6000)
	f2.AddNIC(1, 0, -1, -1)
	if got := f2.Snapshot()[CNPJob].CNPRate; got != 100 {
		t.Errorf("no NIC CNP counter: rate %v, want XDP's 100", got)
	}
}
