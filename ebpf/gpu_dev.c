// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// gpu_dev.c - open of a GPU device node from a cgroup that is not expected to use it.
//
// security_file_open fires for every open on the node, so the program first keeps
// only character devices named nvidia* (nvidia0, nvidiactl, nvidia-uvm, ...) or
// renderD* (/dev/dri/renderD128).  Counting and signalling are then controlled
// from userspace through two maps:
//
//   gpu_dev_cfg[0]  non-zero = enforce.  0 (the default) fails open: opens are
//                   counted in gpu_open_count but nothing is signalled.
//   allowed_cg      cgroup id (the full 64-bit id) -> any value: cgroups that
//                   may open a GPU device.  The collector fills it, for example
//                   from the pods that hold a GPU on this node.
//
// With enforcement on, an open from a cgroup that is not in allowed_cg emits one
// FABRIC_SIG_GPU_DEV (cgroup_id_lo = low 32 bits of the cgroup id).  The device
// name is not reported.  Host processes and device plugins live in cgroups of
// their own and must be listed too.  Observe only.

#include "headers/fabric_signal.h"

#define S_IFMT_MASK 0170000
#define S_IFCHR_BITS 0020000

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u32);
} gpu_dev_cfg SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, __u64); /* cgroup id */
	__type(value, __u32);
} allowed_cg SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u64);
} gpu_open_count SEC(".maps");

static __always_inline int is_gpu_node(struct file *file)
{
	struct inode *ino = BPF_CORE_READ(file, f_inode);
	struct dentry *d;
	const unsigned char *name;
	char b[8] = {};

	if (!ino || (BPF_CORE_READ(ino, i_mode) & S_IFMT_MASK) != S_IFCHR_BITS)
		return 0;
	d = BPF_CORE_READ(file, f_path.dentry);
	if (!d)
		return 0;
	name = BPF_CORE_READ(d, d_name.name);
	if (!name || bpf_probe_read_kernel_str(b, sizeof(b), name) < 0)
		return 0;
	if (b[0] == 'n' && b[1] == 'v' && b[2] == 'i' && b[3] == 'd' && b[4] == 'i' && b[5] == 'a')
		return 1;
	return b[0] == 'r' && b[1] == 'e' && b[2] == 'n' && b[3] == 'd' && b[4] == 'e' && b[5] == 'r' &&
	       b[6] == 'D';
}

SEC("kprobe/security_file_open")
int BPF_KPROBE(gd_file_open, struct file *file)
{
	__u64 cg;
	__u32 zero = 0;
	__u32 *enforce;
	__u64 *n;
	struct fabric_signal *ev;

	if (!file || !is_gpu_node(file))
		return 0;
	n = bpf_map_lookup_elem(&gpu_open_count, &zero);
	if (n)
		*n += 1;
	enforce = bpf_map_lookup_elem(&gpu_dev_cfg, &zero);
	if (!enforce || !*enforce)
		return 0;
	cg = bpf_get_current_cgroup_id();
	if (bpf_map_lookup_elem(&allowed_cg, &cg))
		return 0;
	ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
	if (!ev)
		return 0;
	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = bpf_ktime_get_ns();
	ev->pid = bpf_get_current_pid_tgid() >> 32;
	ev->cgroup_id_lo = (__u32)cg;
	ev->signal_type = FABRIC_SIG_GPU_DEV;
	bpf_get_current_comm(ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
