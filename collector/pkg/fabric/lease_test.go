// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestLease(ttl time.Duration) (*Lease, *fakeClock) {
	c := &fakeClock{t: time.Now()}
	l := NewLease(ttl)
	l.now = c.now
	return l, c
}

func TestLeaseDefaultTTL(t *testing.T) {
	if got := NewLease(0).TTL(); got != 15*time.Minute {
		t.Errorf("default ttl = %v, want 15m", got)
	}
	if got := NewLease(-time.Second).TTL(); got != DefaultLeaseTTL {
		t.Errorf("negative ttl = %v", got)
	}
	if got := NewLease(time.Second).TTL(); got != time.Second {
		t.Errorf("explicit ttl = %v", got)
	}
}

func TestLeaseExpiryAndRelease(t *testing.T) {
	l, c := newTestLease(time.Minute)
	if len(l.Expired()) != 0 || l.Len() != 0 {
		t.Fatal("new lease must be empty")
	}
	exp := l.Grant(42)
	if !exp.Equal(c.t.Add(time.Minute)) {
		t.Errorf("deadline = %v", exp)
	}
	c.t = c.t.Add(59 * time.Second)
	if len(l.Expired()) != 0 {
		t.Error("must not expire before the deadline")
	}
	c.t = c.t.Add(time.Second) // exactly at the deadline: lapsed
	if got := l.Expired(); !reflect.DeepEqual(got, []uint64{42}) {
		t.Errorf("expired = %v", got)
	}
	// Expired does not forget: a failed delete is retried.
	if got := l.Expired(); !reflect.DeepEqual(got, []uint64{42}) {
		t.Errorf("second call = %v, want the lease to be kept until Release", got)
	}
	l.Release(42)
	if len(l.Expired()) != 0 || l.Has(42) {
		t.Error("released lease must be gone")
	}
}

func TestLeaseRenewAndFullCgroupID(t *testing.T) {
	l, c := newTestLease(time.Minute)
	const hi = uint64(1)<<40 | 7 // differs from `lo` only above bit 32
	const lo = uint64(7)
	l.Grant(hi)
	l.Grant(lo)
	if l.Len() != 2 {
		t.Fatalf("64-bit ids must not collide on their low 32 bits: len=%d", l.Len())
	}
	c.t = c.t.Add(50 * time.Second)
	l.Grant(lo) // renew one
	c.t = c.t.Add(20 * time.Second)
	if got := l.Expired(); !reflect.DeepEqual(got, []uint64{hi}) {
		t.Errorf("expired = %v, want only the un-renewed cgroup", got)
	}
	if got := l.Keys(); !reflect.DeepEqual(got, []uint64{lo, hi}) {
		t.Errorf("keys = %v", got)
	}
}

func TestLeaseUsesMonotonicClock(t *testing.T) {
	exp := NewLease(time.Hour).Grant(5)
	if !strings.Contains(exp.String(), "m=") {
		t.Errorf("deadline %q carries no monotonic reading: a wall-clock step could move it", exp.String())
	}
}
