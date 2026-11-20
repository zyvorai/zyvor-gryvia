// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import (
	"sort"
	"sync"
	"time"
)

// DefaultLeaseTTL is how long a pace grant lives unless it is renewed.
const DefaultLeaseTTL = 15 * time.Minute

// Lease tracks which cgroups are currently pace-limited and until when. It is
// the bookkeeping half of quota_pace.c: a Pacer writes the pace_rate map entry
// when it grants a lease and deletes it when the lease lapses, so the default
// state (no lease, no entry) is "observe: nothing is clamped".
//
// Expiry is measured on the monotonic clock: the deadline is time.Now().Add(ttl)
// and is compared with time.Now(), both carrying Go's monotonic reading, so a
// wall-clock step (NTP, manual date change, VM resume) can neither extend nor
// prematurely end a lease. A Lease is safe for concurrent use.
type Lease struct {
	mu    sync.Mutex
	until map[uint64]time.Time // full 64-bit cgroup id -> deadline
	ttl   time.Duration
	now   func() time.Time // time.Now; replaced by tests
}

// NewLease creates a Lease. A non-positive ttl means DefaultLeaseTTL.
func NewLease(ttl time.Duration) *Lease {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	return &Lease{until: map[uint64]time.Time{}, ttl: ttl, now: time.Now}
}

// TTL is the lifetime of one grant.
func (l *Lease) TTL() time.Duration { return l.ttl }

// Grant starts (or renews) the lease of a cgroup and returns its deadline.
// It only does bookkeeping; the caller writes the map entry (see Pacer).
func (l *Lease) Grant(cgroupID uint64) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	exp := l.now().Add(l.ttl)
	l.until[cgroupID] = exp
	return exp
}

// Has reports whether the cgroup holds a lease (expired or not).
func (l *Lease) Has(cgroupID uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.until[cgroupID]
	return ok
}

// Len is the number of leases held.
func (l *Lease) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.until)
}

// Expired returns the cgroups whose lease has lapsed, oldest deadline first. It
// does not forget them: the caller deletes each map entry and then calls Release,
// so a failed delete is retried on the next call instead of leaking the entry.
func (l *Lease) Expired() []uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var out []uint64
	for cg, exp := range l.until {
		if !now.Before(exp) {
			out = append(out, cg)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if l.until[out[i]].Equal(l.until[out[j]]) {
			return out[i] < out[j]
		}
		return l.until[out[i]].Before(l.until[out[j]])
	})
	return out
}

// Release forgets a cgroup's lease (after its map entry was deleted).
func (l *Lease) Release(cgroupID uint64) {
	l.mu.Lock()
	delete(l.until, cgroupID)
	l.mu.Unlock()
}

// Keys returns every cgroup that holds a lease, in ascending order.
func (l *Lease) Keys() []uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]uint64, 0, len(l.until))
	for cg := range l.until {
		out = append(out, cg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
