// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// infer_latency.c - accept -> first recv latency on inference ports.
//
// kretprobe inet_csk_accept records the accepted socket when its LOCAL port is
// one of the ports userspace wrote into infer_ports (the collector's
// -infer-ports flag; default 8000 vLLM and 8001 Triton HTTP).  kprobe
// tcp_recvmsg on that socket emits one FABRIC_SIG_INFER_WAIT fabric_signal
// (latency_ns = accept -> first recv, rank = local port) and forgets the
// socket.  Sockets on any other port are never recorded, so tcp_recvmsg only
// pays a failed hash lookup for them.
//
// The interval includes the time the client takes to send its first request
// bytes, so it measures connection-to-request wait, not model latency.
// Entries older than INFER_MAX_WAIT_NS are discarded without a signal: a
// socket that is never read would otherwise pair with a later socket that
// reuses the same kernel address.  Observe-only.

#include "headers/fabric_signal.h"

#define INFER_MAX_PORTS    8
#define INFER_MAX_WAIT_NS  30000000000ULL   /* 30 s */
#define MAX_TRACKED_SOCKS  65536

// Userspace fills slots with watched local TCP ports (host order); 0 = empty.
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, INFER_MAX_PORTS);
	__type(key, __u32);
	__type(value, __u16);
} infer_ports SEC(".maps");

// Accepted sockets awaiting their first recv.  LRU: sockets that never read
// age out.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_TRACKED_SOCKS);
	__type(key, __u64);   /* struct sock * */
	__type(value, __u64); /* accept time, ns */
} infer_accept SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

GRYVIA_DECLARE_DROPS();

static __always_inline int port_watched(__u16 port)
{
	__u32 i;

	if (port == 0)
		return 0;
#pragma unroll
	for (i = 0; i < INFER_MAX_PORTS; i++) {
		__u32 k = i;
		__u16 *p = bpf_map_lookup_elem(&infer_ports, &k);

		if (p && *p == port)
			return 1;
	}
	return 0;
}

SEC("kretprobe/inet_csk_accept")
int BPF_KRETPROBE(infer_accept_exit, struct sock *sk)
{
	__u64 key = (__u64)sk;
	__u64 now;
	__u16 port;

	if (!sk)
		return 0;
	port = BPF_CORE_READ(sk, __sk_common.skc_num);
	if (!port_watched(port))
		return 0;
	now = bpf_ktime_get_ns();
	bpf_map_update_elem(&infer_accept, &key, &now, BPF_ANY);
	return 0;
}

SEC("kprobe/tcp_recvmsg")
int BPF_KPROBE(infer_first_recv, struct sock *sk)
{
	__u64 key = (__u64)sk;
	struct fabric_signal *ev;
	__u64 *start;
	__u64 now, lat;

	if (!sk)
		return 0;
	start = bpf_map_lookup_elem(&infer_accept, &key);
	if (!start)
		return 0;
	now = bpf_ktime_get_ns();
	lat = now - *start;
	bpf_map_delete_elem(&infer_accept, &key);
	if (lat > INFER_MAX_WAIT_NS)
		return 0;

	ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
	if (!ev) {
		GRYVIA_COUNT_DROP(GRYVIA_DROP_RINGBUF);
		return 0;
	}
	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = now;
	ev->pid = bpf_get_current_pid_tgid() >> 32;
	ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
	ev->signal_type = FABRIC_SIG_INFER_WAIT;
	ev->rank = BPF_CORE_READ(sk, __sk_common.skc_num);
	ev->latency_ns = lat;
	bpf_get_current_comm(&ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
