// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// ucx_complete.c - a UCX tag send was still outstanding when progress ran later.
//
// ucx_gloo.c times the posting call only.  This records the latest
// ucp_tag_send_nbx per thread that returned a request (not an inline
// completion or an error) and emits when ucp_worker_progress runs on the same
// thread more than 5 ms later.  Completion is not observed: a request that
// finished in between still counts, so this is "a send was posted and progress
// ran >= 5 ms later", not proof of a stalled transfer.  One send per thread is
// tracked.  Observe only.  Skipped when libucp.so is absent.

#include "headers/fabric_signal.h"

#define SLOW_NS 5000000ULL
#define MAX_INFLIGHT 2048

struct send_st {
	__u64 start_ns;
	__u64 bytes;
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_INFLIGHT);
	__type(key, __u64);
	__type(value, struct send_st);
} ucx_inflight SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

SEC("uprobe/ucp_tag_send_nbx")
int BPF_UPROBE(send_nbx, void *ep, void *buffer, __u64 count)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct send_st s = {};

	(void)ep;
	(void)buffer;
	s.start_ns = bpf_ktime_get_ns();
	s.bytes = count;
	bpf_map_update_elem(&ucx_inflight, &id, &s, BPF_ANY);
	return 0;
}

SEC("uretprobe/ucp_tag_send_nbx")
int BPF_URETPROBE(send_nbx_ret, void *req)
{
	/* ucs_status_ptr_t: NULL = completed inline, an error status is encoded
	 * as a pointer in the last page, anything else is a request in flight. */
	__u64 id = bpf_get_current_pid_tgid();

	if (!req || (unsigned long)req >= (unsigned long)-4096)
		bpf_map_delete_elem(&ucx_inflight, &id);
	return 0;
}

SEC("uprobe/ucp_worker_progress")
int BPF_UPROBE(progress, void *worker)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct send_st *s = bpf_map_lookup_elem(&ucx_inflight, &id);
	struct fabric_signal *ev;
	__u64 now;

	(void)worker;
	if (!s)
		return 0;
	now = bpf_ktime_get_ns();
	if (now - s->start_ns < SLOW_NS)
		return 0;
	ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
	if (!ev)
		return 0;
	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = now;
	ev->pid = id >> 32;
	ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
	ev->signal_type = FABRIC_SIG_UCX_WAIT;
	ev->latency_ns = now - s->start_ns;
	ev->bytes = s->bytes;
	bpf_get_current_comm(ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
	bpf_map_delete_elem(&ucx_inflight, &id);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
