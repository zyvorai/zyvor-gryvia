// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import (
	"context"
	"errors"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
)

type fakeMap struct {
	m       map[uint64]uint64
	putErr  error
	delErr  error
	deletes []uint64
}

func newFakeMap() *fakeMap { return &fakeMap{m: map[uint64]uint64{}} }

func (f *fakeMap) Put(k, v uint64) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.m[k] = v
	return nil
}

func (f *fakeMap) Delete(k uint64) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deletes = append(f.deletes, k)
	delete(f.m, k)
	return nil
}

type recLog struct{ lines []string }

func (r *recLog) Infow(msg string, kv ...interface{}) { r.lines = append(r.lines, "info "+msg) }
func (r *recLog) Warnw(msg string, kv ...interface{}) { r.lines = append(r.lines, "warn "+msg) }
func (r *recLog) has(sub string) bool {
	for _, l := range r.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func newTestPacer() (*Pacer, *fakeMap, *fakeClock, *recLog) {
	fm := newFakeMap()
	lg := &recLog{}
	p := NewPacer(fm, time.Minute, lg)
	c := &fakeClock{t: time.Now()}
	p.lease.now = c.now
	return p, fm, c, lg
}

func TestPacerDefaultsToNothing(t *testing.T) {
	p, fm, _, _ := newTestPacer()
	if len(fm.m) != 0 || len(p.Active()) != 0 {
		t.Fatal("a new Pacer must not write anything")
	}
	if got := p.Expire(); len(got) != 0 {
		t.Errorf("expire on empty = %v", got)
	}
}

func TestPacerGrantWritesEntryAndLogs(t *testing.T) {
	p, fm, c, lg := newTestPacer()
	exp, rate, err := p.Grant(100, 2_000_000)
	if err != nil || rate != 2_000_000 || fm.m[100] != 2_000_000 {
		t.Fatalf("grant: exp=%v rate=%d err=%v map=%v", exp, rate, err, fm.m)
	}
	if !exp.Equal(c.t.Add(time.Minute)) {
		t.Errorf("deadline = %v", exp)
	}
	if !lg.has("quota pace granted") {
		t.Errorf("grant not logged: %v", lg.lines)
	}
	if !reflect.DeepEqual(p.Active(), map[uint64]uint64{100: 2_000_000}) {
		t.Errorf("active = %v", p.Active())
	}
}

func TestPacerClampsLowAndRejectsBad(t *testing.T) {
	p, fm, _, _ := newTestPacer()
	if _, rate, err := p.Grant(100, 1000); err != nil || rate != PaceMinBytesPerSec || fm.m[100] != PaceMinBytesPerSec {
		t.Errorf("low rate must be clamped up: rate=%d err=%v map=%v", rate, err, fm.m)
	}
	for _, bad := range []uint64{0, PaceMaxBytesPerSec + 1, 1 << 40} {
		if _, _, err := p.Grant(200, bad); !errors.Is(err, ErrInvalidRate) {
			t.Errorf("rate %d: err=%v, want ErrInvalidRate", bad, err)
		}
	}
	for _, cg := range []uint64{0, 1} {
		if _, _, err := p.Grant(cg, 1_000_000); !errors.Is(err, ErrInvalidCgroup) {
			t.Errorf("cgroup %d: err=%v, want ErrInvalidCgroup", cg, err)
		}
	}
	if _, ok := fm.m[200]; ok || len(fm.m) != 1 {
		t.Errorf("rejected grants must not touch the map: %v", fm.m)
	}
	if _, _, err := p.Grant(300, PaceMaxBytesPerSec); err != nil {
		t.Errorf("max rate must be accepted: %v", err)
	}
}

func TestPacerMapFailureLeavesNoLease(t *testing.T) {
	p, fm, _, _ := newTestPacer()
	fm.putErr = errors.New("boom")
	if _, _, err := p.Grant(100, 1_000_000); err == nil {
		t.Fatal("want error")
	}
	if p.lease.Len() != 0 || len(p.Active()) != 0 {
		t.Error("a failed map write must not record a lease")
	}
}

func TestPacerExpiryDeletesEntry(t *testing.T) {
	p, fm, c, lg := newTestPacer()
	p.Grant(100, 1_000_000)
	p.Grant(101, 1_000_000)
	c.t = c.t.Add(30 * time.Second)
	p.Grant(101, 1_000_000) // renewed
	c.t = c.t.Add(31 * time.Second)
	got := p.Expire()
	if !reflect.DeepEqual(got, []uint64{100}) {
		t.Fatalf("expired = %v", got)
	}
	if _, ok := fm.m[100]; ok {
		t.Error("expired entry must be deleted from the map")
	}
	if _, ok := fm.m[101]; !ok {
		t.Error("renewed entry must stay")
	}
	if !lg.has("quota pace lease expired") {
		t.Errorf("expiry not logged: %v", lg.lines)
	}
	c.t = c.t.Add(time.Minute)
	p.Expire()
	if len(fm.m) != 0 {
		t.Errorf("map must be empty once every lease lapsed: %v", fm.m)
	}
}

func TestPacerExpiryRetriesFailedDelete(t *testing.T) {
	p, fm, c, lg := newTestPacer()
	p.Grant(100, 1_000_000)
	c.t = c.t.Add(2 * time.Minute)
	fm.delErr = errors.New("busy")
	if got := p.Expire(); len(got) != 0 {
		t.Errorf("failed delete must not count as expired: %v", got)
	}
	if !lg.has("delete failed") || fm.m[100] == 0 {
		t.Errorf("entry must stay and the failure be logged: %v %v", fm.m, lg.lines)
	}
	fm.delErr = nil
	if got := p.Expire(); !reflect.DeepEqual(got, []uint64{100}) || len(fm.m) != 0 {
		t.Errorf("retry: got=%v map=%v", got, fm.m)
	}
}

func TestPacerRevoke(t *testing.T) {
	p, fm, _, _ := newTestPacer()
	p.Grant(100, 1_000_000)
	if err := p.Revoke(100); err != nil || len(fm.m) != 0 || len(p.Active()) != 0 {
		t.Errorf("revoke: err=%v map=%v", err, fm.m)
	}
	if err := p.Revoke(999); err != nil {
		t.Errorf("revoking an unknown cgroup must be a no-op: %v", err)
	}
}

func TestPacerShutdownDeletesAllAndRefusesGrants(t *testing.T) {
	p, fm, _, lg := newTestPacer()
	p.Grant(100, 1_000_000)
	p.Grant(101, 2_000_000)
	if err := p.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if len(fm.m) != 0 || len(p.Active()) != 0 {
		t.Errorf("shutdown must remove every entry: %v", fm.m)
	}
	if !lg.has("removed at shutdown") {
		t.Errorf("shutdown deletions not logged: %v", lg.lines)
	}
	if _, _, err := p.Grant(102, 1_000_000); !errors.Is(err, ErrPacerClosed) {
		t.Errorf("grant after shutdown: %v", err)
	}
	if err := p.Shutdown(); err != nil {
		t.Errorf("shutdown must be idempotent: %v", err)
	}
}

func TestPacerShutdownReportsFailedDeletes(t *testing.T) {
	p, fm, _, _ := newTestPacer()
	p.Grant(100, 1_000_000)
	fm.delErr = errors.New("gone")
	if err := p.Shutdown(); err == nil {
		t.Error("want the delete error reported")
	}
}

func TestPacerCapacity(t *testing.T) {
	p, _, _, _ := newTestPacer()
	for i := 0; i < MaxPacedCgroups; i++ {
		if _, _, err := p.Grant(uint64(i)+2, 1_000_000); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := p.Grant(1<<40, 1_000_000); !errors.Is(err, ErrTooManyLeases) {
		t.Errorf("err = %v, want ErrTooManyLeases", err)
	}
	if _, _, err := p.Grant(2, 3_000_000); err != nil {
		t.Errorf("renewing an existing lease at capacity must work: %v", err)
	}
}

func TestPacerRunExpiresAndShutsDownOnCancel(t *testing.T) {
	fm := newFakeMap()
	p := NewPacer(fm, 30*time.Millisecond, nil)
	p.Grant(100, 1_000_000)
	p.Grant(101, 1_000_000)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx, 5*time.Millisecond); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.lease.Len() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p.lease.Len() != 0 {
		t.Fatal("Run did not expire the leases")
	}
	p2 := NewPacer(fm, time.Hour, nil)
	p2.Grant(200, 1_000_000)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { p2.Run(ctx2, time.Hour); close(done2) }()
	cancel2()
	<-done2
	if _, ok := fm.m[200]; ok {
		t.Error("cancelling Run must delete the entries (fail open)")
	}
	cancel()
	<-done
}

// The limits must agree with ebpf/quota_pace.c.
func TestPaceLimitsMatchC(t *testing.T) {
	src, err := os.ReadFile("../../../ebpf/quota_pace.c")
	if err != nil {
		t.Skipf("C source not available: %v", err)
	}
	for name, want := range map[string]uint64{
		"PACE_MIN_BPS": PaceMinBytesPerSec, "PACE_MAX_BPS": PaceMaxBytesPerSec, "MAX_PACED_CGROUPS": MaxPacedCgroups,
	} {
		m := regexp.MustCompile(`#define\s+` + name + `\s+(0x[0-9A-Fa-f]+|\d+)`).FindSubmatch(src)
		if m == nil {
			t.Errorf("%s missing from quota_pace.c", name)
			continue
		}
		got, err := strconv.ParseUint(string(m[1]), 0, 64)
		if err != nil || got != want {
			t.Errorf("%s: C %s, Go %d", name, m[1], want)
		}
	}
}

// EBPFPaceMap against a real hash map with the layout of pace_rate. Needs
// CAP_BPF; skipped otherwise.
func TestEBPFPaceMap(t *testing.T) {
	m, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Hash, KeySize: 8, ValueSize: 8, MaxEntries: 8})
	if err != nil {
		t.Skipf("cannot create a BPF map here: %v", err)
	}
	defer m.Close()
	pm := NewEBPFPaceMap(m)
	cg := uint64(1)<<40 | 9
	if err := pm.Put(cg, 777_000); err != nil {
		t.Fatal(err)
	}
	var v uint64
	if err := m.Lookup(&cg, &v); err != nil || v != 777_000 {
		t.Fatalf("lookup: v=%d err=%v", v, err)
	}
	if err := pm.Delete(cg); err != nil {
		t.Fatal(err)
	}
	if err := pm.Delete(cg); err != nil {
		t.Errorf("deleting an absent key must not fail: %v", err)
	}
	if err := m.Lookup(&cg, &v); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Errorf("entry must be gone: %v", err)
	}
}
