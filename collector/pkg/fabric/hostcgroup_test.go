// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const testUID = "12345678-1234-1234-1234-123456789abc"

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Ino)
}

func mkdirs(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, r := range rels {
		if err := os.MkdirAll(filepath.Join(root, r), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func newCgroupRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestHostCgroupsLayouts(t *testing.T) {
	us := strings.ReplaceAll(testUID, "-", "_")
	cases := []struct {
		name, qos string
		dirs      []string
		pod       string
	}{
		{"cgroupfs burstable", "Burstable", []string{"kubepods/burstable/pod" + testUID + "/c1", "kubepods/burstable/pod" + testUID + "/c2"}, "kubepods/burstable/pod" + testUID},
		{"cgroupfs besteffort", "BestEffort", []string{"kubepods/besteffort/pod" + testUID + "/c1"}, "kubepods/besteffort/pod" + testUID},
		{"cgroupfs guaranteed", "Guaranteed", []string{"kubepods/pod" + testUID + "/c1"}, "kubepods/pod" + testUID},
		{"systemd burstable", "Burstable", []string{"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + us + ".slice/cri-containerd-a.scope"},
			"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + us + ".slice"},
		{"systemd guaranteed", "Guaranteed", []string{"kubepods.slice/kubepods-pod" + us + ".slice/crio-b.scope"}, "kubepods.slice/kubepods-pod" + us + ".slice"},
		{"systemd besteffort", "BestEffort", []string{"kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + us + ".slice/x.scope"},
			"kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + us + ".slice"},
	}
	for _, c := range cases {
		root := newCgroupRoot(t)
		mkdirs(t, root, c.dirs...)
		// A sibling pod that must never appear.
		mkdirs(t, root, "kubepods/burstable/pod99999999-1234-1234-1234-123456789abc/c1")
		h := &HostCgroups{Root: root}
		ids, err := h.CgroupIDs(PodRef{Namespace: "n", Name: "p", UID: testUID, QOS: c.qos})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		want := map[uint64]bool{inode(t, filepath.Join(root, c.pod)): true}
		for _, d := range c.dirs {
			want[inode(t, filepath.Join(root, d))] = true
		}
		if len(ids) != len(want) {
			t.Errorf("%s: ids %v want %v", c.name, ids, want)
		}
		for _, id := range ids {
			if !want[id] {
				t.Errorf("%s: unexpected id %d", c.name, id)
			}
		}
	}
}

func TestHostCgroupsRefusesEverythingOutsideRoot(t *testing.T) {
	// invalid uid / qos: never turned into a path
	root := newCgroupRoot(t)
	h := &HostCgroups{Root: root}
	for _, p := range []PodRef{
		{UID: "../../etc", QOS: "Burstable"},
		{UID: testUID + "/../..", QOS: "Burstable"},
		{UID: strings.ToUpper(testUID), QOS: "Burstable"},
		{UID: testUID, QOS: "../x"},
		{UID: testUID, QOS: ""},
	} {
		if ids, err := h.CgroupIDs(p); err == nil {
			t.Errorf("%+v resolved to %v", p, ids)
		}
	}

	// pod directory is a symlink to a directory OUTSIDE root
	outside := t.TempDir()
	mkdirs(t, outside, "c1")
	mkdirs(t, root, "kubepods/burstable")
	if err := os.Symlink(outside, filepath.Join(root, "kubepods/burstable/pod"+testUID)); err != nil {
		t.Fatal(err)
	}
	if ids, err := h.CgroupIDs(PodRef{UID: testUID, QOS: "Burstable"}); err == nil {
		t.Fatalf("followed a symlink out of the root: %v", ids)
	}

	// a symlinked parent that escapes
	root2 := newCgroupRoot(t)
	mkdirs(t, outside, "burstable/pod"+testUID+"/c1")
	if err := os.Symlink(outside, filepath.Join(root2, "kubepods")); err != nil {
		t.Fatal(err)
	}
	if ids, err := (&HostCgroups{Root: root2}).CgroupIDs(PodRef{UID: testUID, QOS: "Burstable"}); err == nil {
		t.Fatalf("followed a symlinked parent out of the root: %v", ids)
	}

	// a symlinked container child is not enumerated
	root3 := newCgroupRoot(t)
	mkdirs(t, root3, "kubepods/burstable/pod"+testUID+"/real")
	if err := os.Symlink(outside, filepath.Join(root3, "kubepods/burstable/pod"+testUID, "escape")); err != nil {
		t.Fatal(err)
	}
	ids, err := (&HostCgroups{Root: root3}).CgroupIDs(PodRef{UID: testUID, QOS: "Burstable"})
	if err != nil || len(ids) != 2 {
		t.Fatalf("want pod + real container only, got %v %v", ids, err)
	}
	for _, id := range ids {
		if id == inode(t, outside) || id == inode(t, filepath.Join(outside, "c1")) {
			t.Fatal("id of a directory outside the root returned")
		}
	}

	// not a cgroup v2 mount
	if _, err := (&HostCgroups{Root: t.TempDir()}).CgroupIDs(PodRef{UID: testUID, QOS: "Burstable"}); err == nil {
		t.Fatal("accepted a root without cgroup.controllers")
	}
	// pod cgroup missing
	if _, err := h.CgroupIDs(PodRef{UID: "aaaaaaaa-1234-1234-1234-123456789abc", QOS: "Guaranteed"}); err == nil {
		t.Fatal("missing pod cgroup must be an error")
	}
	// a file where the pod dir should be
	root4 := newCgroupRoot(t)
	mkdirs(t, root4, "kubepods/burstable")
	_ = os.WriteFile(filepath.Join(root4, "kubepods/burstable/pod"+testUID), nil, 0o644)
	if _, err := (&HostCgroups{Root: root4}).CgroupIDs(PodRef{UID: testUID, QOS: "Burstable"}); err == nil {
		t.Fatal("file accepted as cgroup")
	}
}

func TestUnder(t *testing.T) {
	if !under("/a/b", "/a/b/c") || under("/a/b", "/a/b") || under("/a/b", "/a/bc") || under("/a/b", "/a") || !under("/a/b/", "/a/b/c") {
		t.Fatal("under() wrong")
	}
}

// End to end: fake pods + real inodes of a fake cgroup tree + real Pacer with a fake map.
func TestSyncWithHostCgroupsOnlyGrantsUnderRoot(t *testing.T) {
	root := newCgroupRoot(t)
	mkdirs(t, root, "kubepods/burstable/pod"+testUID+"/c1")
	podID := inode(t, filepath.Join(root, "kubepods/burstable/pod"+testUID))
	cID := inode(t, filepath.Join(root, "kubepods/burstable/pod"+testUID, "c1"))

	p, m, _, lg := newTestPacer()
	pods := &fakePods{byNS: map[string][]PodRef{"a": {
		{Namespace: "a", Name: "in", UID: testUID, QOS: "Burstable"},
		{Namespace: "a", Name: "no-cgroup", UID: "aaaaaaaa-1234-1234-1234-123456789abc", QOS: "Burstable"},
		{Namespace: "a", Name: "traversal", UID: "../../..", QOS: "Burstable"},
	}}, errNS: map[string]error{}}
	s := NewQuotaSyncer(&fakeCaps{caps: []EgressCap{{Quota: "q", Namespaces: []string{"a"}, Mbps: 10}}}, pods, &HostCgroups{Root: root}, p, "own", false, lg)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.m) != 2 || m.m[podID] == 0 || m.m[cID] == 0 {
		t.Fatalf("map %v, want exactly ids %d and %d", m.m, podID, cID)
	}
}

func TestHostCgroupsDirs(t *testing.T) {
	us := strings.ReplaceAll(testUID, "-", "_")
	root := newCgroupRoot(t)
	pod := "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + us + ".slice"
	mkdirs(t, root, pod+"/cri-containerd-a.scope", pod+"/cri-containerd-b.scope")
	_ = os.WriteFile(filepath.Join(root, pod, "not-a-dir"), nil, 0o644)
	h := &HostCgroups{Root: root}
	dirs, err := h.Dirs(PodRef{Namespace: "ml", Name: "p", UID: testUID, QOS: "Burstable"})
	if err != nil || len(dirs) != 3 || !strings.HasSuffix(dirs[0], pod) {
		t.Fatalf("%v %v", dirs, err)
	}
	if _, err := h.Dirs(PodRef{UID: "../../etc", QOS: "Burstable"}); err == nil {
		t.Fatal("bad uid accepted")
	}
	if _, err := h.Dirs(PodRef{UID: testUID, QOS: "Guaranteed"}); err == nil {
		t.Fatal("missing pod cgroup must be an error")
	}
	outside := t.TempDir()
	root2 := newCgroupRoot(t)
	mkdirs(t, root2, "kubepods.slice/kubepods-burstable.slice")
	if err := os.Symlink(outside, filepath.Join(root2, pod)); err != nil {
		t.Fatal(err)
	}
	if _, err := (&HostCgroups{Root: root2}).Dirs(PodRef{UID: testUID, QOS: "Burstable"}); err == nil {
		t.Fatal("symlinked pod dir must not be followed")
	}
}
