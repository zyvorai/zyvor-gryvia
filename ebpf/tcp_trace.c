// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// tcp_trace.c - TCP connection lifecycle tracing via kprobes.
//
// Hooks tcp_connect, tcp_accept, tcp_close, and tcp_retransmit_skb to
// track every TCP connection from establishment through teardown.  Each
// connection emits a flow_event to a perf ring buffer consumed by the
// userspace collector.

#include "headers/common.h"

/* ---- BPF maps --------------------------------------------------------- */

// Per-connection state keyed by the struct sock pointer.  The pointer is the
// only identifier that is stable across connect/accept/close/retransmit
// (the source port is not yet bound when tcp_v4_connect is entered and
// retransmits run in softirq context under an unrelated pid).
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u64);
    __type(value, struct conn_info);
} conn_map SEC(".maps");

// Perf ring buffer for flow events.
struct {
    __uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
    __uint(key_size, sizeof(__u32));
    __uint(value_size, sizeof(__u32));
} events SEC(".maps");

GRYVIA_DECLARE_DROPS();

/* ---- helpers ---------------------------------------------------------- */

static __always_inline void emit_event(void *ctx, __u32 src_ip, __u32 dst_ip,
                                       __u16 src_port, __u16 dst_port,
                                       __u8 protocol, __u8 verdict,
                                       __u32 bytes, __u64 latency_ns,
                                       __u32 owner_pid)
{
    struct flow_event ev = {};

    ev.timestamp  = bpf_ktime_get_ns();
    ev.src_ip     = src_ip;
    ev.dst_ip     = dst_ip;
    ev.src_port   = src_port;
    ev.dst_port   = dst_port;
    ev.protocol   = protocol;
    ev.verdict    = verdict;
    ev.bytes      = bytes;
    ev.latency_ns = latency_ns;

    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u32 cur_pid = pid_tgid >> 32;
    ev.pid = owner_pid ? owner_pid : cur_pid;
    /* Close and retransmit run in an arbitrary task or softirq; only report
     * a command name when it belongs to the PID being reported. */
    if (!owner_pid || owner_pid == cur_pid)
        bpf_get_current_comm(&ev.comm, sizeof(ev.comm));

    if (bpf_perf_event_output(ctx, &events, BPF_F_CURRENT_CPU, &ev, sizeof(ev)))
        GRYVIA_COUNT_DROP(GRYVIA_DROP_PERF);
}

/* ---- kprobes ---------------------------------------------------------- */

// Start tracking a connection and emit its "connect/accept" event.
static __always_inline void track_open(void *ctx, struct sock *sk,
                                       __u32 dst_ip_hint, __u16 dst_port_hint)
{
    __u64 key = (__u64)sk;
    struct conn_info ci = {};
    ci.start_ns = bpf_ktime_get_ns();
    ci.owner_pid = bpf_get_current_pid_tgid() >> 32;
    bpf_map_update_elem(&conn_map, &key, &ci, BPF_ANY);

    __u32 src_ip, dst_ip;
    __u16 sport, dport;
    gryvia_sock_v4_tuple(sk, &src_ip, &dst_ip, &sport, &dport);
    if (dst_ip == 0)
        dst_ip = dst_ip_hint;
    if (dport == 0)
        dport = dst_port_hint;

    emit_event(ctx, src_ip, dst_ip, sport, dport, IPPROTO_TCP,
               0 /* forward */, 0, 0, ci.owner_pid);
}

// tcp_v4_connect(struct sock *sk, struct sockaddr *uaddr, int addr_len)
// On entry the socket is not connected yet, so the destination is taken from
// the caller-supplied sockaddr_in.
SEC("kprobe/tcp_v4_connect")
int BPF_KPROBE(tcp_connect, struct sock *sk, struct sockaddr *uaddr)
{
    struct sockaddr_in sin = {};

    if (uaddr &&
        bpf_probe_read_kernel(&sin, sizeof(sin), uaddr) == 0 &&
        sin.sin_family == AF_INET)
        track_open(ctx, sk, sin.sin_addr.s_addr, bpf_ntohs(sin.sin_port));
    else
        track_open(ctx, sk, 0, 0);
    return 0;
}

// inet_csk_accept returns a connected sock *.
SEC("kretprobe/inet_csk_accept")
int BPF_KRETPROBE(tcp_accept, struct sock *sk)
{
    if (!sk)
        return 0;

    track_open(ctx, sk, 0, 0);
    return 0;
}

// tcp_close(struct sock *sk, long timeout)
SEC("kprobe/tcp_close")
int BPF_KPROBE(tcp_conn_close, struct sock *sk)
{
    __u64 key = (__u64)sk;
    struct conn_info *ci = bpf_map_lookup_elem(&conn_map, &key);
    if (!ci)
        return 0;

    __u64 latency = bpf_ktime_get_ns() - ci->start_ns;
    __u32 bytes = (__u32)(ci->bytes_sent + ci->bytes_recv);

    __u32 src_ip, dst_ip;
    __u16 sport, dport;
    gryvia_sock_v4_tuple(sk, &src_ip, &dst_ip, &sport, &dport);

    emit_event(ctx, src_ip, dst_ip, sport, dport, IPPROTO_TCP, 0, bytes,
               latency, ci->owner_pid);

    /* Verdict 2 is an observation, not a packet drop. The bytes field is
     * the retransmission count for this connection. Emit once at close so
     * softirq retransmits retain the process identity captured at open. */
    if (ci->retransmits)
        emit_event(ctx, src_ip, dst_ip, sport, dport, IPPROTO_TCP, 2,
                   ci->retransmits, 0, ci->owner_pid);

    bpf_map_delete_elem(&conn_map, &key);
    return 0;
}

// tcp_retransmit_skb(struct sock *sk, struct sk_buff *skb, int segs)
SEC("kprobe/tcp_retransmit_skb")
int BPF_KPROBE(tcp_retransmit, struct sock *sk)
{
    __u64 key = (__u64)sk;
    struct conn_info *ci = bpf_map_lookup_elem(&conn_map, &key);
    if (!ci)
        return 0;

    __sync_fetch_and_add(&ci->retransmits, 1);
    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
