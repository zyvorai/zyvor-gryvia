// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeCaps struct {
	caps []EgressCap
	err  error
}

func (f *fakeCaps) EgressCaps(context.Context) ([]EgressCap, error) { return f.caps, f.err }

type fakePods struct {
	byNS  map[string][]PodRef
	errNS map[string]error
	asked []string
}

func (f *fakePods) NodePods(_ context.Context, ns string) ([]PodRef, error) {
	f.asked = append(f.asked, ns)
	if err := f.errNS[ns]; err != nil {
		return nil, err
	}
	return f.byNS[ns], nil
}

type fakeCG struct{ ids map[string][]uint64 } // key: ns/name

func (f *fakeCG) CgroupIDs(p PodRef) ([]uint64, error) {
	ids, ok := f.ids[p.Namespace+"/"+p.Name]
	if !ok {
		return nil, errors.New("no cgroup")
	}
	return ids, nil
}

type syncEnv struct {
	s     *QuotaSyncer
	caps  *fakeCaps
	pods  *fakePods
	cg    *fakeCG
	m     *fakeMap
	pacer *Pacer
	clock *fakeClock
	log   *recLog
}

func newSyncEnv(dry bool) *syncEnv {
	p, m, c, lg := newTestPacer()
	e := &syncEnv{caps: &fakeCaps{}, pods: &fakePods{byNS: map[string][]PodRef{}, errNS: map[string]error{}}, cg: &fakeCG{ids: map[string][]uint64{}},
		m: m, pacer: p, clock: c, log: lg}
	e.s = NewQuotaSyncer(e.caps, e.pods, e.cg, p, "gryvia-network", dry, lg)
	return e
}

func (e *syncEnv) pod(ns, name string, ids ...uint64) {
	e.pods.byNS[ns] = append(e.pods.byNS[ns], PodRef{Namespace: ns, Name: name, UID: name, QOS: "Burstable"})
	e.cg.ids[ns+"/"+name] = ids
}

const mbps100 = 100 * 125_000

func TestSyncGrantsOnlyPodsOfListedNamespaces(t *testing.T) {
	e := newSyncEnv(false)
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"team-a"}, Mbps: 100}}
	e.pod("team-a", "p1", 500, 501)
	e.pod("team-b", "p2", 600) // not in any quota
	if err := e.s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[uint64]uint64{500: mbps100, 501: mbps100}
	if len(e.m.m) != 2 || e.m.m[500] != mbps100 || e.m.m[501] != mbps100 {
		t.Fatalf("map %v, want %v", e.m.m, want)
	}
	if len(e.pods.asked) != 1 || e.pods.asked[0] != "team-a" {
		t.Fatalf("pods listed for %v", e.pods.asked)
	}
	if !e.log.has("quota pace granted") || !e.log.has("pacing pod cgroup") {
		t.Fatalf("grants not logged: %v", e.log.lines)
	}
}

func TestSyncNeverPacesProtectedNamespaces(t *testing.T) {
	e := newSyncEnv(false)
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"kube-system", "gryvia-system", "gryvia-network", "kube-public", "kube-node-lease", "team-a"}, Mbps: 50}}
	for _, ns := range []string{"kube-system", "gryvia-system", "gryvia-network", "kube-public", "kube-node-lease"} {
		e.pod(ns, "p-"+ns, uint64(len(ns))+1000)
	}
	e.pod("team-a", "ok", 42)
	if err := e.s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.m.m) != 1 || e.m.m[42] == 0 {
		t.Fatalf("only team-a may be paced: %v", e.m.m)
	}
	for _, ns := range e.pods.asked {
		if ns != "team-a" {
			t.Fatalf("protected namespace %s was even queried", ns)
		}
	}
	// A source that ignores the namespace it was asked for is not trusted either.
	e2 := newSyncEnv(false)
	e2.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"team-a"}, Mbps: 50}}
	e2.pods.byNS["team-a"] = []PodRef{{Namespace: "kube-system", Name: "evil", UID: "u", QOS: "Burstable"}}
	e2.cg.ids["kube-system/evil"] = []uint64{77}
	_ = e2.s.Sync(context.Background())
	if len(e2.m.m) != 0 {
		t.Fatalf("paced a pod outside the asked namespace: %v", e2.m.m)
	}
}

func TestSyncCapBounds(t *testing.T) {
	for _, mbps := range []int{0, -5, MaxEgressMbps + 1, 1 << 30} {
		e := newSyncEnv(false)
		e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a"}, Mbps: mbps}}
		e.pod("a", "p", 5)
		_ = e.s.Sync(context.Background())
		if len(e.m.m) != 0 {
			t.Errorf("cap %d must not grant, map %v", mbps, e.m.m)
		}
	}
	for mbps, want := range map[int]uint64{1: 125_000, MaxEgressMbps: 4_000_000_000} {
		e := newSyncEnv(false)
		e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a"}, Mbps: mbps}}
		e.pod("a", "p", 5)
		_ = e.s.Sync(context.Background())
		if e.m.m[5] != want {
			t.Errorf("cap %d -> %d, want %d", mbps, e.m.m[5], want)
		}
	}
	if uint64(MaxEgressMbps)*mbpsToBytesPerSec > PaceMaxBytesPerSec {
		t.Fatal("MaxEgressMbps exceeds what the program accepts")
	}
	// Strictest of two quotas wins.
	e := newSyncEnv(false)
	e.caps.caps = []EgressCap{{Quota: "loose", Namespaces: []string{"a"}, Mbps: 500}, {Quota: "tight", Namespaces: []string{"a"}, Mbps: 20}}
	e.pod("a", "p", 5)
	_ = e.s.Sync(context.Background())
	if e.m.m[5] != 20*125_000 {
		t.Fatalf("strictest cap must win: %v", e.m.m)
	}
}

func TestSyncAPIFailureChangesNothing(t *testing.T) {
	e := newSyncEnv(false)
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a", "b"}, Mbps: 100}}
	e.pod("a", "p", 5)
	if err := e.s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(e.m.m)
	e.m.deletes = nil

	// 1. quota list fails
	e.caps.err = errors.New("apiserver down")
	e.caps.caps = nil // a naive implementation would treat this as "all quotas gone"
	if err := e.s.Sync(context.Background()); err == nil {
		t.Fatal("error swallowed")
	}
	if len(e.m.m) != before || len(e.m.deletes) != 0 {
		t.Fatalf("quota-list failure revoked: %v %v", e.m.m, e.m.deletes)
	}
	// 2. one namespace's pod list fails while another works and a new pod exists
	e.caps.err = nil
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a", "b"}, Mbps: 100}}
	e.pod("b", "new", 9)
	e.pods.errNS["a"] = errors.New("boom")
	if err := e.s.Sync(context.Background()); err == nil {
		t.Fatal("error swallowed")
	}
	if _, granted := e.m.m[9]; granted || len(e.m.m) != before || len(e.m.deletes) != 0 {
		t.Fatalf("pod-list failure granted or revoked: %v %v", e.m.m, e.m.deletes)
	}
}

func TestSyncRevokesWhatIsNoLongerWanted(t *testing.T) {
	e := newSyncEnv(false)
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a"}, Mbps: 100}}
	e.pod("a", "p1", 5)
	e.pod("a", "p2", 6)
	_ = e.s.Sync(context.Background())
	if len(e.m.m) != 2 {
		t.Fatal(e.m.m)
	}
	// pod gone
	e.pods.byNS["a"] = e.pods.byNS["a"][:1]
	_ = e.s.Sync(context.Background())
	if _, ok := e.m.m[6]; ok || e.m.m[5] == 0 {
		t.Fatalf("pod gone: %v", e.m.m)
	}
	if !e.log.has("quota pace revoked") {
		t.Fatal("revoke not logged")
	}
	// cap set to 0 (drops out of EgressCaps in the real source; also invalid here)
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a"}, Mbps: 0}}
	_ = e.s.Sync(context.Background())
	if len(e.m.m) != 0 || len(e.s.Granted()) != 0 {
		t.Fatalf("cap 0: %v", e.m.m)
	}
	// quota removed
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a"}, Mbps: 100}}
	_ = e.s.Sync(context.Background())
	e.caps.caps = nil
	_ = e.s.Sync(context.Background())
	if len(e.m.m) != 0 {
		t.Fatalf("quota removed: %v", e.m.m)
	}
}

func TestSyncRenewsLeaseAndExpiryDeletes(t *testing.T) {
	e := newSyncEnv(false)
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a"}, Mbps: 100}}
	e.pod("a", "p", 5)
	_ = e.s.Sync(context.Background())
	e.clock.t = e.clock.t.Add(50 * time.Second) // lease is 1 minute in the test pacer
	_ = e.s.Sync(context.Background())          // renews
	e.clock.t = e.clock.t.Add(50 * time.Second)
	if got := e.pacer.Expire(); len(got) != 0 || e.m.m[5] == 0 {
		t.Fatalf("renewed lease expired early: %v", got)
	}
	// The API goes away: syncs fail, the lease runs out by itself and the entry is deleted.
	e.caps.err = errors.New("down")
	_ = e.s.Sync(context.Background())
	e.clock.t = e.clock.t.Add(2 * time.Minute)
	if got := e.pacer.Expire(); len(got) != 1 || len(e.m.m) != 0 {
		t.Fatalf("expiry did not delete: %v %v", got, e.m.m)
	}
	if !e.log.has("lease expired") {
		t.Fatal("expiry not logged")
	}
}

func TestSyncShutdownDeletesAllAndRefusesGrants(t *testing.T) {
	e := newSyncEnv(false)
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a"}, Mbps: 100}}
	e.pod("a", "p", 5, 6, 7)
	_ = e.s.Sync(context.Background())
	if len(e.m.m) != 3 {
		t.Fatal(e.m.m)
	}
	if err := e.pacer.Shutdown(); err != nil || len(e.m.m) != 0 {
		t.Fatalf("shutdown: %v %v", err, e.m.m)
	}
	_ = e.s.Sync(context.Background()) // a late sync must not re-arm anything
	if len(e.m.m) != 0 {
		t.Fatalf("grant after shutdown: %v", e.m.m)
	}
}

func TestSyncDryRunWritesNothing(t *testing.T) {
	e := newSyncEnv(true)
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a"}, Mbps: 100}}
	e.pod("a", "p", 5)
	if err := e.s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.m.m) != 0 || e.pacer.lease.Len() != 0 || len(e.s.Granted()) != 0 {
		t.Fatalf("dry-run wrote: %v", e.m.m)
	}
	if !e.log.has("dry-run: would grant") {
		t.Fatalf("dry-run did not log: %v", e.log.lines)
	}
	e.caps.caps = nil
	_ = e.s.Sync(context.Background())
	if !e.log.has("dry-run: would revoke") || len(e.m.m) != 0 {
		t.Fatalf("%v", e.log.lines)
	}
}

func TestSyncSkipsPodsWithoutCgroup(t *testing.T) {
	e := newSyncEnv(false)
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a"}, Mbps: 100}}
	e.pods.byNS["a"] = []PodRef{{Namespace: "a", Name: "nocg", UID: "u", QOS: "Burstable"}}
	e.pod("a", "ok", 5)
	if err := e.s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.m.m) != 1 || e.m.m[5] == 0 || !e.log.has("pod cgroup not resolved") {
		t.Fatalf("%v %v", e.m.m, e.log.lines)
	}
}

func TestSyncRevokeFailureKeepsLeaseForExpiry(t *testing.T) {
	e := newSyncEnv(false)
	e.caps.caps = []EgressCap{{Quota: "q", Namespaces: []string{"a"}, Mbps: 100}}
	e.pod("a", "p", 5)
	_ = e.s.Sync(context.Background())
	e.caps.caps = nil
	e.m.delErr = errors.New("map busy")
	if err := e.s.Sync(context.Background()); err == nil {
		t.Fatal("failed revoke not reported")
	}
	e.m.delErr = nil
	e.clock.t = e.clock.t.Add(2 * time.Minute)
	e.pacer.Expire()
	if len(e.m.m) != 0 {
		t.Fatalf("entry leaked: %v", e.m.m)
	}
}
