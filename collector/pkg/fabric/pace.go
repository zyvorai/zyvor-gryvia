// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cilium/ebpf"
)

// Limits shared with ebpf/quota_pace.c (PACE_MIN_BPS, PACE_MAX_BPS,
// MAX_PACED_CGROUPS); a unit test checks them against the C source.
const (
	// PaceMinBytesPerSec is 1 Mbit/s. A lower request is raised to it, so a bad
	// value can slow a workload down but never freeze it.
	PaceMinBytesPerSec uint64 = 125_000
	// PaceMaxBytesPerSec is the largest rate SO_MAX_PACING_RATE takes through
	// bpf_setsockopt (32-bit bytes per second, 0xFFFFFFFF means unlimited).
	PaceMaxBytesPerSec uint64 = 0xFFFF_FFFE
	// MaxPacedCgroups is the capacity of the pace_rate map.
	MaxPacedCgroups = 4096
)

var (
	// ErrInvalidCgroup rejects the id 0 and the root cgroup (id 1): pacing "everything" is never wanted.
	ErrInvalidCgroup = errors.New("pace: invalid cgroup id")
	// ErrInvalidRate rejects a zero or above-maximum rate. A rate below the minimum is clamped, not rejected.
	ErrInvalidRate = errors.New("pace: rate out of range")
	// ErrPacerClosed is returned by Grant after Shutdown: nothing may be re-armed while the collector exits.
	ErrPacerClosed = errors.New("pace: pacer is shut down")
	// ErrTooManyLeases means the pace_rate map is full.
	ErrTooManyLeases = errors.New("pace: too many paced cgroups")
)

// PaceMap is the pace_rate map of quota_pace.c: full cgroup id -> bytes per second.
// Delete of an absent key must not be an error.
type PaceMap interface {
	Put(cgroupID, bytesPerSec uint64) error
	Delete(cgroupID uint64) error
}

// EBPFPaceMap is a PaceMap backed by the loaded pace_rate *ebpf.Map.
type EBPFPaceMap struct{ M *ebpf.Map }

// NewEBPFPaceMap wraps the pace_rate map (loader.Manager.Map("quota_pace.o", "pace_rate")).
func NewEBPFPaceMap(m *ebpf.Map) *EBPFPaceMap { return &EBPFPaceMap{M: m} }

// Put sets the rate of a cgroup.
func (e *EBPFPaceMap) Put(cgroupID, bytesPerSec uint64) error {
	return e.M.Put(&cgroupID, &bytesPerSec)
}

// Delete removes a cgroup's entry; an absent key is fine.
func (e *EBPFPaceMap) Delete(cgroupID uint64) error {
	if err := e.M.Delete(&cgroupID); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	return nil
}

// Logger is the part of *zap.SugaredLogger the Pacer uses. Every grant, renewal,
// expiry, revoke and shutdown deletion is logged.
type Logger interface {
	Infow(msg string, keysAndValues ...interface{})
	Warnw(msg string, keysAndValues ...interface{})
}

type nopLogger struct{}

func (nopLogger) Infow(string, ...interface{}) {}
func (nopLogger) Warnw(string, ...interface{}) {}

// Pacer drives the pace_rate map of quota_pace.c under a Lease: Grant writes
// an entry, Expire deletes lapsed ones, Shutdown deletes every entry it wrote.
//
// The only caller of Grant in the collector is QuotaSyncer, and only with
// -quota-pace-sync (off by default); there is no HTTP or RPC entry point.
// Without it -quota-pace only attaches the program, builds a Pacer and runs its
// expiry loop, so pacing stays off until an operator (bpftool on the map id)
// inserts an entry.
type Pacer struct {
	mu     sync.Mutex
	m      PaceMap
	lease  *Lease
	rates  map[uint64]uint64
	log    Logger
	closed bool
}

// NewPacer creates a Pacer over m. A non-positive ttl means DefaultLeaseTTL; a nil log is silent.
func NewPacer(m PaceMap, ttl time.Duration, log Logger) *Pacer {
	if log == nil {
		log = nopLogger{}
	}
	return &Pacer{m: m, lease: NewLease(ttl), rates: map[uint64]uint64{}, log: log}
}

// Grant paces the cgroup at bytesPerSec until the lease lapses (calling it again
// renews the lease and updates the rate). A rate below PaceMinBytesPerSec is
// clamped up to it; 0 and rates above PaceMaxBytesPerSec are rejected. The map
// is written first: if that fails no lease is recorded. It returns the deadline
// and the rate that was actually programmed.
func (p *Pacer) Grant(cgroupID, bytesPerSec uint64) (time.Time, uint64, error) {
	if cgroupID <= 1 {
		return time.Time{}, 0, fmt.Errorf("%w: %d", ErrInvalidCgroup, cgroupID)
	}
	if bytesPerSec == 0 || bytesPerSec > PaceMaxBytesPerSec {
		return time.Time{}, 0, fmt.Errorf("%w: %d bytes/s (want %d..%d)", ErrInvalidRate, bytesPerSec, PaceMinBytesPerSec, PaceMaxBytesPerSec)
	}
	rate, clamped := bytesPerSec, false
	if rate < PaceMinBytesPerSec {
		rate, clamped = PaceMinBytesPerSec, true
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return time.Time{}, 0, ErrPacerClosed
	}
	if !p.lease.Has(cgroupID) && p.lease.Len() >= MaxPacedCgroups {
		return time.Time{}, 0, ErrTooManyLeases
	}
	if err := p.m.Put(cgroupID, rate); err != nil {
		return time.Time{}, 0, fmt.Errorf("pace: write pace_rate[%d]: %w", cgroupID, err)
	}
	renewed := p.lease.Has(cgroupID)
	exp := p.lease.Grant(cgroupID)
	p.rates[cgroupID] = rate
	p.log.Infow("quota pace granted", "cgroup_id", cgroupID, "requested_bytes_per_sec", bytesPerSec,
		"bytes_per_sec", rate, "clamped_to_min", clamped, "renewed", renewed, "ttl", p.lease.TTL().String())
	return exp, rate, nil
}

// Revoke deletes the entry of a cgroup early. If the delete fails the lease is kept so Expire retries.
func (p *Pacer) Revoke(cgroupID uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.lease.Has(cgroupID) {
		return nil
	}
	if err := p.m.Delete(cgroupID); err != nil {
		p.log.Warnw("quota pace revoke failed, will retry at expiry", "cgroup_id", cgroupID, "error", err)
		return fmt.Errorf("pace: delete pace_rate[%d]: %w", cgroupID, err)
	}
	p.lease.Release(cgroupID)
	delete(p.rates, cgroupID)
	p.log.Infow("quota pace revoked", "cgroup_id", cgroupID)
	return nil
}

// Expire deletes the entries whose lease has lapsed and returns their cgroup
// ids. An entry whose delete failed stays leased and is retried on the next call.
func (p *Pacer) Expire() []uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var done []uint64
	for _, cg := range p.lease.Expired() {
		if err := p.m.Delete(cg); err != nil {
			p.log.Warnw("quota pace expiry: delete failed, will retry", "cgroup_id", cg, "error", err)
			continue
		}
		p.lease.Release(cg)
		rate := p.rates[cg]
		delete(p.rates, cg)
		p.log.Infow("quota pace lease expired, entry removed", "cgroup_id", cg, "bytes_per_sec", rate)
		done = append(done, cg)
	}
	return done
}

// Shutdown deletes every entry this Pacer wrote (fail open) and refuses further
// grants. It is idempotent and returns the joined delete errors, if any.
// A socket that was already paced keeps its rate until it closes; only new
// connections are unpaced.
func (p *Pacer) Shutdown() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	var errs []error
	for _, cg := range p.lease.Keys() {
		if err := p.m.Delete(cg); err != nil {
			p.log.Warnw("quota pace shutdown: delete failed", "cgroup_id", cg, "error", err)
			errs = append(errs, fmt.Errorf("pace_rate[%d]: %w", cg, err))
			continue
		}
		p.lease.Release(cg)
		delete(p.rates, cg)
		p.log.Infow("quota pace entry removed at shutdown", "cgroup_id", cg)
	}
	return errors.Join(errs...)
}

// Active returns the cgroup id -> programmed rate of every current lease.
func (p *Pacer) Active() map[uint64]uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[uint64]uint64, len(p.rates))
	for k, v := range p.rates {
		out[k] = v
	}
	return out
}

// Run calls Expire every interval until ctx is done, then Shutdown.
func (p *Pacer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = p.Shutdown()
			return
		case <-t.C:
			p.Expire()
		}
	}
}
