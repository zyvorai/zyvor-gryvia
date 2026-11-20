// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// quota_pace.c - cap SO_MAX_PACING_RATE of over-budget cgroups.
//
// *** THE ONLY MUTATING PROGRAM IN THE SET.  Default OFF. ***
//
// cgroup sockops.  When a process opens an outbound TCP connection
// (BPF_SOCK_OPS_TCP_CONNECT_CB, run inside connect() in the caller's own
// context) the caller's cgroup id is looked up in pace_rate.  If, and only if,
// there is an entry the socket's SO_MAX_PACING_RATE is lowered to that rate.
// It never drops, delays or rewrites a packet itself; the kernel's pacing does
// the rate limiting.
//
// Why CONNECT_CB and only outbound: the ESTABLISHED callbacks (the bundle's
// choice) run from the TCP receive path, usually in softirq, where `current`
// is whatever task was interrupted.  Keying on bpf_get_current_cgroup_id()
// there would clamp sockets of an unrelated cgroup.  sockops programs cannot
// call bpf_sk_cgroup_id() (the verifier rejects it), so the only trustworthy
// cgroup id is the caller's during connect().  Accepted (passive) sockets are
// therefore not paced.
//
// Safety properties (each one is checked by the functional test in the PR):
//   - no entry, entry 0, or an entry above PACE_MAX_BPS: the socket is
//     untouched (the map is empty until userspace inserts a key).
//   - callers in cgroups without an entry are never touched: the key is the
//     FULL 64-bit cgroup id, an exact match.  Descendant cgroups are NOT
//     covered: key each (container) cgroup.
//   - the rate is clamped UP to PACE_MIN_BPS (1 Mbit/s), so a bad value can
//     slow a workload but never freeze it.
//   - the pacing rate is only ever lowered: a socket whose workload already
//     set a lower SO_MAX_PACING_RATE keeps it.
//   - fails open: every error path returns without changing anything; the
//     program always returns 1 and never writes skops->reply or the callback
//     flags, so it has no effect on any other sockops op.
//   - lease expiry is userspace (collector/pkg/fabric/lease.go): the entry is
//     deleted and the NEXT connection is unpaced.  A socket that was already
//     paced keeps its rate until it closes; the kernel offers no way to undo it.
//
// bpf_setsockopt(SO_MAX_PACING_RATE) takes a 32-bit value in bytes per second
// (about 4.29 GB/s max), so the map value is capped accordingly.  Pacing is
// enforced by the fq qdisc or, without it, by TCP's internal pacing.

#include "headers/fabric_signal.h"

#ifndef SOL_SOCKET
#define SOL_SOCKET 1
#endif
#ifndef SO_MAX_PACING_RATE
#define SO_MAX_PACING_RATE 47
#endif

#define PACE_MIN_BPS   125000ULL        /* 1 Mbit/s: never freeze a workload */
#define PACE_MAX_BPS   0xFFFFFFFEULL    /* 0xFFFFFFFF means "unlimited" to the kernel */
#define MAX_PACED_CGROUPS 4096

// Userspace fills this (collector/pkg/fabric).  No entry, no pacing.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_PACED_CGROUPS);
	__type(key, __u64);   /* full cgroup v2 id (the cgroup directory's inode number) */
	__type(value, __u64); /* bytes per second */
} pace_rate SEC(".maps");

SEC("sockops")
int gryvia_quota_pace(struct bpf_sock_ops *skops)
{
	__u64 cg, *rate, want;
	__u32 cur = 0, val;

	if (skops->op != BPF_SOCK_OPS_TCP_CONNECT_CB)
		return 1;
	cg = bpf_get_current_cgroup_id();
	if (!cg)
		return 1;
	rate = bpf_map_lookup_elem(&pace_rate, &cg);
	if (!rate)
		return 1;
	want = *rate;
	if (want == 0 || want > PACE_MAX_BPS)
		return 1; /* 0 and out-of-range mean "no clamp", never "clamp to 0" */
	if (want < PACE_MIN_BPS)
		want = PACE_MIN_BPS;
	val = (__u32)want;

	/* Only ever lower the socket's limit. */
	if (!bpf_getsockopt(skops, SOL_SOCKET, SO_MAX_PACING_RATE, &cur, sizeof(cur)) &&
	    cur != 0 && cur <= val)
		return 1;
	bpf_setsockopt(skops, SOL_SOCKET, SO_MAX_PACING_RATE, &val, sizeof(val));
	return 1;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
