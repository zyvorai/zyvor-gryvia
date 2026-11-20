// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// graph_stall.c - cudaDeviceSynchronize while a CUDA graph capture is open.
//
// cudaStreamBeginCapture then cudaDeviceSynchronize on the same thread
// before cudaStreamEndCapture.  A device-wide sync is not permitted during
// stream capture and normally invalidates the capture, so this points at a
// capture that will fail or fall back to eager execution.  latency_ns is the
// time since the capture began (not the duration of the sync); syncs within
// the first 1 ms of a capture are ignored, same order as overlap.c.  The return
// value of the sync is not checked.  Observe only.

#include "headers/fabric_signal.h"

#define STALL_NS 1000000ULL
#define MAX_INFLIGHT 2048

struct cap {
	__u64 start_ns;
	__u32 depth;
	__u32 syncs;
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_INFLIGHT);
	__type(key, __u64);
	__type(value, struct cap);
} capture SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

SEC("uprobe/cudaStreamBeginCapture")
int BPF_UPROBE(begin_cap, void *stream, __u32 mode)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct cap *c = bpf_map_lookup_elem(&capture, &id);
	struct cap init = {};

	(void)stream;
	(void)mode;
	if (!c) {
		init.start_ns = bpf_ktime_get_ns();
		init.depth = 1;
		bpf_map_update_elem(&capture, &id, &init, BPF_ANY);
		return 0;
	}
	c->depth += 1;
	return 0;
}

SEC("uprobe/cudaStreamEndCapture")
int BPF_UPROBE(end_cap, void *stream, void *graph)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct cap *c = bpf_map_lookup_elem(&capture, &id);

	(void)stream;
	(void)graph;
	if (!c)
		return 0;
	if (c->depth)
		c->depth -= 1;
	if (!c->depth)
		bpf_map_delete_elem(&capture, &id);
	return 0;
}

SEC("uretprobe/cudaDeviceSynchronize")
int BPF_URETPROBE(sync_ret, int ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct cap *c = bpf_map_lookup_elem(&capture, &id);
	struct fabric_signal *ev;
	__u64 now;

	(void)ret;
	if (!c || !c->depth)
		return 0;
	now = bpf_ktime_get_ns();
	if (now - c->start_ns < STALL_NS)
		return 0;
	c->syncs += 1;
	ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
	if (!ev)
		return 0;
	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = now;
	ev->pid = id >> 32;
	ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
	ev->signal_type = FABRIC_SIG_GRAPH_STALL;
	ev->latency_ns = now - c->start_ns;
	ev->retry_count = c->syncs;
	bpf_get_current_comm(ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
