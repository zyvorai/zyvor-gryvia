// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/flight"
)

func fakeBinder(t *testing.T, m map[uint32][2]string) (*Binder, *Folder, *fakeClock, *int) {
	t.Helper()
	f := NewFolder(time.Minute)
	calls := 0
	b := NewBinder(f, func(pid uint32) (string, string, bool) {
		calls++
		v, ok := m[pid]
		return v[0], v[1], ok
	})
	c := &fakeClock{t: time.Now()}
	b.now = c.now
	return b, f, c, &calls
}

func TestBinderFoldsResolvedUnderJobAndOthersUnattributed(t *testing.T) {
	b, f, _, _ := fakeBinder(t, map[uint32][2]string{10: {"ml", "train"}})
	b.Add(Signal{PID: 10, Type: SigStraggler, Comm: "python"})
	b.Add(Signal{PID: 20, Type: SigStraggler, Comm: "stranger"})
	snap := f.Snapshot()
	if _, ok := snap[JobKey{"ml", "train"}]; !ok {
		t.Fatalf("resolved pid not under its job: %v", snap)
	}
	if _, ok := snap[JobKey{"_unattributed", "stranger"}]; !ok {
		t.Fatalf("unresolved pid must stay unattributed: %v", snap)
	}
	if len(snap) != 2 {
		t.Fatalf("%v", snap)
	}
}

func TestBinderCachesThenRechecks(t *testing.T) {
	m := map[uint32][2]string{10: {"ml", "train"}}
	b, f, c, calls := fakeBinder(t, m)
	b.Add(Signal{PID: 10, Type: SigStraggler})
	b.Add(Signal{PID: 10, Type: SigStraggler})
	if *calls != 1 {
		t.Fatalf("resolver called %d times inside the TTL", *calls)
	}
	// The pid is recycled by another (non-job) process: after the TTL it must be unbound.
	delete(m, 10)
	c.t = c.t.Add(BindTTL + time.Second)
	b.Add(Signal{PID: 10, Type: SigStraggler, Comm: "recycled"})
	if _, ok := f.Snapshot()[JobKey{"_unattributed", "recycled"}]; !ok {
		t.Fatal("recycled pid still attributed to the old job")
	}
	// Negative answers are cached too.
	n := *calls
	b.Add(Signal{PID: 10, Type: SigStraggler})
	if *calls != n {
		t.Fatal("negative answer not cached")
	}
}

func TestBinderIsBounded(t *testing.T) {
	m := map[uint32][2]string{}
	for i := uint32(1); i <= MaxBindings+10; i++ {
		m[i] = [2]string{"ml", "j"}
	}
	b, f, _, _ := fakeBinder(t, m)
	for i := uint32(1); i <= MaxBindings+10; i++ {
		b.Add(Signal{PID: i, Type: SigStraggler})
		if b.Len() > MaxBindings {
			t.Fatalf("cache grew to %d", b.Len())
		}
	}
	f.mu.Lock()
	n := len(f.pidToJob)
	f.mu.Unlock()
	if n > MaxBindings {
		t.Fatalf("folder pid map grew to %d", n)
	}
}

func TestBinderNilResolverAndPidZero(t *testing.T) {
	f := NewFolder(time.Minute)
	b := NewBinder(f, nil)
	b.Add(Signal{PID: 5, Comm: "x", Type: SigStraggler})
	if _, ok := f.Snapshot()[JobKey{"_unattributed", "x"}]; !ok {
		t.Fatal("nil resolver must leave signals unattributed")
	}
}

// The real flight.Resolver on a fake /proc tree: identical fail-closed rules.
func TestBinderWithFlightResolverFakeProc(t *testing.T) {
	d := t.TempDir()
	r := flight.NewResolver("gpu-1", d)
	uid := "12345678-1234-1234-1234-123456789abc"
	pods := `{"items":[{"metadata":{"uid":"` + uid + `","name":"train-0","namespace":"ml","labels":{"gryvia.io/job":"train"}},"spec":{"nodeName":"gpu-1"}},
	{"metadata":{"uid":"87654321-1234-1234-1234-123456789abc","name":"o","namespace":"ml","labels":{"gryvia.io/job":"remote"}},"spec":{"nodeName":"gpu-2"}}]}`
	if err := r.SetPods([]byte(pods)); err != nil {
		t.Fatal(err)
	}
	write := func(pid, cg string) {
		if err := os.MkdirAll(filepath.Join(d, pid), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, pid, "cgroup"), []byte(cg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("42", "0::/kubepods.slice/kubepods-pod"+strings.ReplaceAll(uid, "-", "_")+".slice/cri-containerd-x.scope\n")
	write("43", "0::/system.slice/sshd.service\n")                          // not a pod
	write("44", "0::/kubepods/pod87654321-1234-1234-1234-123456789abc/c\n") // pod on another node
	write("45", "0::/kubepods/pod00000000-0000-0000-0000-000000000000/c\n") // unknown pod
	f := NewFolder(time.Minute)
	b := NewBinder(f, func(pid uint32) (string, string, bool) {
		id, ok := r.Resolve(pid)
		return id.Namespace, id.Job, ok
	})
	for _, pid := range []uint32{42, 43, 44, 45, 46} {
		b.Add(Signal{PID: pid, Type: SigStraggler, Comm: "p" + itoa(int(pid))})
	}
	snap := f.Snapshot()
	if _, ok := snap[JobKey{"ml", "train"}]; !ok || len(snap) != 5 {
		t.Fatalf("want 1 job + 4 unattributed, got %v", snap)
	}
	for _, c := range []string{"p43", "p44", "p45", "p46"} {
		if _, ok := snap[JobKey{"_unattributed", c}]; !ok {
			t.Errorf("%s must stay unattributed: %v", c, snap)
		}
	}
	// A pod-list refresh that no longer has the pod fails closed after the TTL.
	if err := r.SetPods([]byte(`{"items":[]}`)); err != nil {
		t.Fatal(err)
	}
	c := &fakeClock{t: time.Now().Add(BindTTL + time.Second)}
	b.now = c.now
	b.Add(Signal{PID: 42, Type: SigStraggler, Comm: "later"})
	if _, ok := f.Snapshot()[JobKey{"_unattributed", "later"}]; !ok {
		t.Fatal("stale pod stayed attributed")
	}
}
