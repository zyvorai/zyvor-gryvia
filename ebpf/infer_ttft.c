// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// infer_ttft.c - accept to first response send, and a later send gap.
//
// infer_latency.c measures accept -> first recv (connection-to-request wait).
// This program measures tcp_sendmsg on the same accepted socket: accept -> first
// send (FABRIC_SIG_INFER_TTFT, latency_ns, bytes = first send size, rank = local
// port) and, once per connection, a gap >= 50 ms between two later sends
// (FABRIC_SIG_INFER_GAP).  Only sockets accepted on a port in infer_ports are
// tracked (same map contract as infer_latency: the collector fills the local TCP
// ports, 0 = empty slot, an empty map records nothing).  State is per struct sock
// pointer, so a response written from a different thread than the accept is
// still found.
//
// Limits: accept -> first send includes the time the client takes to send its
// request, so this is not tokenizer time-to-first-token.  The send gap cannot
// tell a stall inside one response from the idle time between two requests on a
// keep-alive connection, so treat it as a hint.  A socket that never sends is
// dropped after 30 s without a signal.  Observe only.

#include "headers/fabric_signal.h"

#define INFER_MAX_PORTS   8
#define INFER_MAX_WAIT_NS 30000000000ULL /* 30 s: accept -> first send */
#define GAP_NS            50000000ULL    /* 50 ms */
#define MAX_TRACKED_SOCKS 65536

struct sock_st {
	__u64 accept_ns;
	__u64 last_send_ns;
	__u16 port;
	__u8  sent;
	__u8  gap_reported;
	__u32 _pad;
};

// Userspace fills slots with watched local TCP ports (host order); 0 = empty.
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, INFER_MAX_PORTS);
	__type(key, __u32);
	__type(value, __u16);
} infer_ports SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_TRACKED_SOCKS);
	__type(key, __u64); /* struct sock * */
	__type(value, struct sock_st);
} infer_sock SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

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

static __always_inline void emit(__u8 type, __u16 port, __u64 now, __u64 latency, __u64 bytes)
{
	struct fabric_signal *ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);

	if (!ev)
		return;
	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = now;
	ev->pid = bpf_get_current_pid_tgid() >> 32;
	ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
	ev->signal_type = type;
	ev->rank = port;
	ev->latency_ns = latency;
	ev->bytes = bytes;
	bpf_get_current_comm(ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
}

SEC("kretprobe/inet_csk_accept")
int BPF_KRETPROBE(ttft_accept_ret, struct sock *sk)
{
	__u64 key = (__u64)sk;
	struct sock_st st = {};
	__u16 port;

	if (!sk)
		return 0;
	port = BPF_CORE_READ(sk, __sk_common.skc_num);
	if (!port_watched(port))
		return 0;
	st.accept_ns = bpf_ktime_get_ns();
	st.port = port;
	bpf_map_update_elem(&infer_sock, &key, &st, BPF_ANY);
	return 0;
}

SEC("kprobe/tcp_sendmsg")
int BPF_KPROBE(ttft_sendmsg, struct sock *sk, void *msg, __u64 size)
{
	__u64 key = (__u64)sk;
	struct sock_st *st;
	__u64 now;

	if (!sk || !size)
		return 0;
	st = bpf_map_lookup_elem(&infer_sock, &key);
	if (!st)
		return 0;
	now = bpf_ktime_get_ns();
	if (!st->sent) {
		__u64 lat = now - st->accept_ns;

		if (lat > INFER_MAX_WAIT_NS) {
			bpf_map_delete_elem(&infer_sock, &key);
			return 0;
		}
		st->sent = 1;
		st->last_send_ns = now;
		emit(FABRIC_SIG_INFER_TTFT, st->port, now, lat, size);
		return 0;
	}
	if (!st->gap_reported && now - st->last_send_ns >= GAP_NS) {
		st->gap_reported = 1;
		emit(FABRIC_SIG_INFER_GAP, st->port, now, now - st->last_send_ns, size);
	}
	st->last_send_ns = now;
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
