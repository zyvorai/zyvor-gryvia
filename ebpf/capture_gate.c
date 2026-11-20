// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// capture_gate.c - cgroup-scoped Flight Recorder arm.
//
// The node-wide recorder is too wide for a training node.  Userspace inserts
// a lease into capture_lease[cgroup_id] (full 64-bit id, same idea as
// quota_pace's pace_rate).  This program does not capture packets.  It is
// the gate other programs and the collector consult:
//
//   lookup(bpf_get_current_cgroup_id()) -> expiry_ns
//   expiry in the past, or missing key -> not armed
//
// A GryviaTraceSession, or a fabric signal the collector decides is worth
// a trace, writes the lease.  Deleting the key disarms.  Fail open: no
// entry means no capture.  Observe only; this object does not attach a
// mutating hook.  The kprobe on tcp_sendmsg exists so `make check` can load
// a real program and so the collector can read gate_hits.

#include "headers/gryvia_core.h"

struct lease {
	__u64 expiry_ns;
	__u32 reason; /* collector-defined: 1 trace session, 2 straggler, 3 p2p */
	__u32 _pad;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, __u64); /* cgroup id */
	__type(value, struct lease);
} capture_lease SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 2); /* 0 = consulted, 1 = armed */
	__type(key, __u32);
	__type(value, __u64);
} gate_hits SEC(".maps");

static __always_inline void bump(__u32 slot)
{
	__u64 *v = bpf_map_lookup_elem(&gate_hits, &slot);

	if (v)
		*v += 1;
}

static __always_inline int armed(void)
{
	__u64 cg = bpf_get_current_cgroup_id();
	struct lease *l = bpf_map_lookup_elem(&capture_lease, &cg);
	__u64 now;

	bump(0);
	if (!l)
		return 0;
	now = bpf_ktime_get_ns();
	if (!l->expiry_ns || now > l->expiry_ns)
		return 0;
	bump(1);
	return 1;
}

SEC("kprobe/tcp_sendmsg")
int BPF_KPROBE(gate_tcp_sendmsg)
{
	(void)armed();
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
