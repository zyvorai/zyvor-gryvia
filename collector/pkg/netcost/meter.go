// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package netcost

import (
	"net/netip"
	"sort"
	"sync"
	"time"
)

// Limits keep memory bounded no matter what the network does.
const (
	MaxPairs   = 65536 // (local, remote) delta states over both address families, same order as one kernel map
	MaxBuckets = 20000 // open (hour, tenant, peer, zone) accumulators
)

// Sample is one cumulative counter pair read from the traffic_costs (IPv4) or traffic_costs6 (IPv6) map.
type Sample struct {
	Local, Remote netip.Addr
	BytesSent     uint64 // egress from Local to Remote
	BytesRecv     uint64 // ingress to Local from Remote
}

// Key identifies one accumulator. Direction is not part of it: a Totals holds both directions.
type Key struct {
	Hour   time.Time // UTC hour start
	Tenant string
	Peer   PeerClass
	Zone   ZoneClass
}

// Totals are the bytes of one Key.
type Totals struct {
	Egress  uint64
	Ingress uint64
}

type pairState struct{ sent, recv uint64 }

type pairKey struct{ local, remote netip.Addr }

// Stats are counters for observability; none of them is billed.
type Stats struct {
	Skipped            map[string]uint64 // by skip reason: bytes (egress + ingress) not attributed
	FlowEventsIgnored  uint64
	DroppedBucketBytes uint64 // bytes lost because MaxBuckets was reached
}

// Meter turns cumulative counters into per-hour tenant totals.
//
// Idempotence: only deltas are added. Observing the same cumulative sample again adds 0; a counter that went
// down (kernel LRU eviction and re-creation, or a program reload) is a reset and its new value is the delta.
// The first sight of a pair counts its whole value, correct because the collector loads the program itself,
// so counters start at 0 when it starts.
type Meter struct {
	mu      sync.Mutex
	res     Resolver
	node    string
	pairs   map[pairKey]pairState
	buckets map[Key]*Totals
	stats   Stats
}

// NewMeter builds a Meter for one node.
func NewMeter(res Resolver, node string) *Meter {
	return &Meter{res: res, node: node, pairs: map[pairKey]pairState{}, buckets: map[Key]*Totals{},
		stats: Stats{Skipped: map[string]uint64{}}}
}

func delta(cur, last uint64) uint64 {
	if cur < last {
		return cur // reset
	}
	return cur - last
}

// Observe folds one full scan of the counter map, taken at now, into the current hour bucket. Pairs missing
// from the scan were evicted from the kernel map and are forgotten (they restart from 0 if they return).
func (m *Meter) Observe(samples []Sample, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hour := now.UTC().Truncate(time.Hour)
	seen := make(map[pairKey]pairState, len(samples))
	for _, s := range samples {
		k := pairKey{s.Local, s.Remote}
		if _, dup := seen[k]; dup {
			continue // a pair listed twice in one scan is never counted twice
		}
		last, known := m.pairs[k]
		if !known && len(seen) >= MaxPairs {
			continue // bounded: a new pair beyond the limit is not tracked (the kernel map is bounded too)
		}
		st := pairState{s.BytesSent, s.BytesRecv}
		seen[k] = st
		dSent, dRecv := delta(st.sent, last.sent), delta(st.recv, last.recv)
		if dSent == 0 && dRecv == 0 {
			continue
		}
		v, ok, why := Classify(m.res, m.node, s.Local.String(), s.Remote.String())
		if !ok {
			m.stats.Skipped[why] += dSent + dRecv
			continue
		}
		key := Key{Hour: hour, Tenant: v.Tenant, Peer: v.Peer, Zone: v.Zone}
		t := m.buckets[key]
		if t == nil {
			if len(m.buckets) >= MaxBuckets {
				m.stats.DroppedBucketBytes += dSent + dRecv
				continue
			}
			t = &Totals{}
			m.buckets[key] = t
		}
		t.Egress += dSent
		t.Ingress += dRecv
	}
	m.pairs = seen
}

// NoteFlowEvent records that a tcp_trace flow event was seen and deliberately not billed: connect/close
// events carry no reliable byte counts and their bytes would duplicate the counter deltas.
func (m *Meter) NoteFlowEvent() {
	m.mu.Lock()
	m.stats.FlowEventsIgnored++
	m.mu.Unlock()
}

// Bucket is one accumulator with its key.
type Bucket struct {
	Key
	Totals
}

// Snapshot returns all open accumulators sorted for stable output.
func (m *Meter) Snapshot() []Bucket {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

func (m *Meter) snapshotLocked() []Bucket {
	out := make([]Bucket, 0, len(m.buckets))
	for k, t := range m.buckets {
		out = append(out, Bucket{k, *t})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		if !a.Hour.Equal(b.Hour) {
			return a.Hour.Before(b.Hour)
		}
		if a.Tenant != b.Tenant {
			return a.Tenant < b.Tenant
		}
		if a.Peer != b.Peer {
			return a.Peer < b.Peer
		}
		return a.Zone < b.Zone
	})
	return out
}

// Evict drops the accumulators of the given hour (after they were published as final).
func (m *Meter) Evict(k Key) {
	m.mu.Lock()
	delete(m.buckets, k)
	m.mu.Unlock()
}

// Stats returns a copy of the counters.
func (m *Meter) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stats
	s.Skipped = make(map[string]uint64, len(m.stats.Skipped))
	for k, v := range m.stats.Skipped {
		s.Skipped[k] = v
	}
	return s
}
