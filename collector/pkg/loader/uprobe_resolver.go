// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package loader provides uprobe resolution for attaching to libraries
// inside container filesystems via /proc.
package loader

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// UprobeResolver resolves library paths inside container filesystems
// by inspecting /proc/<pid>/maps.
type UprobeResolver struct {
	procFS string // default "/proc"
}

// NewUprobeResolver creates a UprobeResolver using the given procFS path.
// If procFS is empty, it defaults to "/proc".
func NewUprobeResolver(procFS string) *UprobeResolver {
	if procFS == "" {
		procFS = "/proc"
	}
	return &UprobeResolver{procFS: procFS}
}

// FindLibrary finds the path to a shared library inside a container's filesystem
// by reading /proc/<pid>/maps and looking for the library name.
func (r *UprobeResolver) FindLibrary(pid int, libName string) (string, error) {
	mapsPath := filepath.Join(r.procFS, strconv.Itoa(pid), "maps")

	f, err := os.Open(mapsPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", mapsPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// /proc/<pid>/maps format:
		// address perms offset dev inode pathname
		// We look for lines whose pathname contains the libName.
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		pathname := fields[5]
		if strings.Contains(filepath.Base(pathname), libName) {
			// Return host-accessible path via /proc/<pid>/root
			return r.ResolveContainerPath(pid, pathname), nil
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading maps for pid %d: %w", pid, err)
	}

	return "", fmt.Errorf("library %s not found in /proc/%d/maps", libName, pid)
}

// FindNCCLLibrary finds libnccl.so for a given container PID.
func (r *UprobeResolver) FindNCCLLibrary(pid int) (string, error) {
	return r.FindLibrary(pid, "libnccl.so")
}

// FindCUDALibrary finds libcudart.so for a given container PID.
func (r *UprobeResolver) FindCUDALibrary(pid int) (string, error) {
	return r.FindLibrary(pid, "libcudart.so")
}

// FindCuFileLibrary finds libcufile.so (GPUDirect Storage) for a given container PID.
func (r *UprobeResolver) FindCuFileLibrary(pid int) (string, error) {
	return r.FindLibrary(pid, "libcufile.so")
}

// FindUCXLibrary finds libucp.so (UCX; exports ucp_tag_send_nb/nbx) for a given
// container PID. libucs.so is the UCX service layer and does not export them.
func (r *UprobeResolver) FindUCXLibrary(pid int) (string, error) {
	return r.FindLibrary(pid, "libucp.so")
}

// FindIBVerbsLibrary finds libibverbs.so (exports ibv_create_qp, ibv_destroy_qp,
// ibv_reg_mr; the data-path calls are inline and have no symbol) for a given PID.
func (r *UprobeResolver) FindIBVerbsLibrary(pid int) (string, error) {
	return r.FindLibrary(pid, "libibverbs.so")
}

// FindContainerPIDs finds PIDs for pods matching a label selector by scanning
// /proc for processes whose cgroup path contains the pod name.
func (r *UprobeResolver) FindContainerPIDs(namespace, podName string) ([]int, error) {
	entries, err := os.ReadDir(r.procFS)
	if err != nil {
		return nil, fmt.Errorf("reading procfs: %w", err)
	}

	var pids []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // not a PID directory
		}

		cgroupPath := filepath.Join(r.procFS, entry.Name(), "cgroup")
		data, err := os.ReadFile(cgroupPath)
		if err != nil {
			continue
		}

		cgroupContent := string(data)
		// Kubernetes cgroup paths contain the pod UID and namespace.
		// Match on podName appearing in the cgroup hierarchy.
		if strings.Contains(cgroupContent, podName) {
			pids = append(pids, pid)
		}
	}

	if len(pids) == 0 {
		return nil, fmt.Errorf("no PIDs found for pod %s/%s", namespace, podName)
	}
	return pids, nil
}

// ResolveContainerPath converts a container-internal path to a host-accessible
// path via /proc/<pid>/root/<path>.
func (r *UprobeResolver) ResolveContainerPath(pid int, containerPath string) string {
	return filepath.Join(r.procFS, strconv.Itoa(pid), "root", containerPath)
}
