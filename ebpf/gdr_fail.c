// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// gdr_fail.c - GPUDirect RDMA page pinning failure.
//
// nvidia_p2p_get_pages (exported by the NVIDIA driver, called by nvidia-peermem
// when an RDMA NIC registers GPU memory) returning non-zero means GPUDirect RDMA
// registration failed and the job will bounce through host memory even when GDS
// looks enabled.  retry_count carries the return value as returned.
//
// Only this function is probed.  There is no kernel ib_reg_mr, and the memory
// registration functions that do exist report errors as ERR_PTR rather than NULL,
// so a NULL check on them would never fire; they are not covered.  The kretprobe
// attaches only while the nvidia module is loaded; the collector skips it
// otherwise.  gdr_stats: 0 = calls, 1 = failures.  Observe only.  Pairs with
// gds_trace.c.

#include "headers/fabric_signal.h"

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 2);
	__type(key, __u32);
	__type(value, __u64);
} gdr_stats SEC(".maps");

static __always_inline void bump(__u32 slot)
{
	__u64 *v = bpf_map_lookup_elem(&gdr_stats, &slot);

	if (v)
		*v += 1;
}

SEC("kretprobe/nvidia_p2p_get_pages")
int BPF_KRETPROBE(p2p_pages_ret, int ret)
{
	struct fabric_signal *ev;
	__u64 id;

	bump(0);
	if (!ret)
		return 0;
	bump(1);
	id = bpf_get_current_pid_tgid();
	ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
	if (!ev)
		return 0;
	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = bpf_ktime_get_ns();
	ev->pid = id >> 32;
	ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
	ev->signal_type = FABRIC_SIG_GDR_FAIL;
	ev->retry_count = (__u32)ret;
	bpf_get_current_comm(ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
