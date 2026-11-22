// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// maxCgroupsPerPod bounds the container cgroups enumerated under one pod.
const maxCgroupsPerPod = 64

var podUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// HostCgroups finds pod cgroups below Root, the cgroup v2 directory the
// quota_pace program is attached to (-cgroup-path, e.g. /host/sys/fs/cgroup).
// Both kubelet cgroup drivers are understood. A cgroup id on a 64-bit kernel is
// the inode number of the cgroup directory.
//
// Nothing outside Root is ever returned: candidate paths are built from a
// validated pod UID and a fixed QoS class, symlinks are resolved and must stay
// under the resolved Root, and Root itself is never returned.
type HostCgroups struct {
	Root string
}

func (h *HostCgroups) candidates(p PodRef) ([]string, error) {
	if !podUIDRe.MatchString(p.UID) {
		return nil, fmt.Errorf("invalid pod uid %q", p.UID)
	}
	u := p.UID
	us := strings.ReplaceAll(u, "-", "_")
	switch p.QOS {
	case "Guaranteed":
		return []string{"kubepods/pod" + u, "kubepods.slice/kubepods-pod" + us + ".slice"}, nil
	case "Burstable":
		return []string{"kubepods/burstable/pod" + u, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + us + ".slice"}, nil
	case "BestEffort":
		return []string{"kubepods/besteffort/pod" + u, "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + us + ".slice"}, nil
	}
	return nil, fmt.Errorf("unknown QoS class %q", p.QOS)
}

func inodeOf(fi os.FileInfo) (uint64, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no inode available")
	}
	return uint64(st.Ino), nil
}

// under reports whether path is strictly below root (both already resolved).
func under(root, path string) bool {
	return strings.HasPrefix(path, strings.TrimRight(root, string(filepath.Separator))+string(filepath.Separator))
}

// CgroupIDs returns the ids of the pod's cgroup and of its direct child
// directories (the container cgroups). It returns an error when the pod's
// cgroup does not exist (yet), so the pod is skipped.
func (h *HostCgroups) CgroupIDs(p PodRef) ([]uint64, error) {
	cands, err := h.candidates(p)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(h.Root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("%s is not a cgroup v2 mount: %w", h.Root, err)
	}
	rootFI, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	rootID, err := inodeOf(rootFI)
	if err != nil {
		return nil, err
	}
	for _, rel := range cands {
		dir := filepath.Join(root, rel)
		fi, err := os.Lstat(dir)
		if err != nil {
			continue
		}
		if !fi.IsDir() { // a symlink or file is never followed
			continue
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || !under(root, real) {
			return nil, fmt.Errorf("%s resolves outside %s", dir, root)
		}
		ids, err := collectIDs(real, rootID)
		if err != nil {
			return nil, err
		}
		return ids, nil
	}
	return nil, fmt.Errorf("no cgroup for pod %s/%s under %s", p.Namespace, p.Name, h.Root)
}

// Dirs returns the pod's cgroup directory followed by its direct child
// (container) directories, every one resolved and confined under Root exactly as
// CgroupIDs does. Userspace readers (the flight diagnosis) use it to read
// cpu.stat / memory.events / *.pressure; nothing is written. At most
// maxCgroupsPerPod children are returned.
func (h *HostCgroups) Dirs(p PodRef) ([]string, error) {
	cands, err := h.candidates(p)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(h.Root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("%s is not a cgroup v2 mount: %w", h.Root, err)
	}
	for _, rel := range cands {
		dir := filepath.Join(root, rel)
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() {
			continue
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || !under(root, real) {
			return nil, fmt.Errorf("%s resolves outside %s", dir, root)
		}
		dirs := []string{real}
		entries, err := os.ReadDir(real)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() { // DirEntry.IsDir is false for symlinks
				continue
			}
			if len(dirs) > maxCgroupsPerPod {
				break
			}
			dirs = append(dirs, filepath.Join(real, e.Name()))
		}
		return dirs, nil
	}
	return nil, fmt.Errorf("no cgroup for pod %s/%s under %s", p.Namespace, p.Name, h.Root)
}

func collectIDs(podDir string, rootID uint64) ([]uint64, error) {
	var ids []uint64
	add := func(dir string) error {
		fi, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		id, err := inodeOf(fi)
		if err != nil {
			return err
		}
		if id <= 1 || id == rootID {
			return fmt.Errorf("refusing cgroup id %d of %s", id, dir)
		}
		ids = append(ids, id)
		return nil
	}
	if err := add(podDir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(podDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() { // DirEntry.IsDir is false for symlinks
			continue
		}
		if len(ids) > maxCgroupsPerPod {
			break
		}
		if err := add(filepath.Join(podDir, e.Name())); err != nil {
			continue // a container that vanished meanwhile
		}
	}
	return ids, nil
}
