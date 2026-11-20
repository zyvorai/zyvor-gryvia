// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// latency_breakdown.c - Multi-phase request latency breakdown.
//
// Tracks per-request lifecycle across multiple phases: DNS resolution,
// TCP handshake, TLS handshake, and application request/response.
// Provides a complete latency breakdown showing where time is spent
// in each phase of a network request.

#include "headers/common.h"

#ifndef EINPROGRESS
#define EINPROGRESS 115
#endif

/* ---- phase tracking --------------------------------------------------- */

#define PHASE_DNS_START       0
#define PHASE_DNS_END         1
#define PHASE_TCP_START       2
#define PHASE_TCP_END         3
#define PHASE_TLS_START       4
#define PHASE_TLS_END         5
#define PHASE_APP_SEND        6
#define PHASE_APP_RECV        7

/* Phase histogram indices */
#define HIST_DNS              0
#define HIST_TCP              1
#define HIST_TLS              2
#define HIST_APP              3
#define HIST_TOTAL            4
#define NUM_PHASE_HISTOGRAMS  5

/* Histogram buckets per phase (8 buckets each) */
#define BUCKETS_PER_PHASE     8
#define TOTAL_HIST_ENTRIES    (NUM_PHASE_HISTOGRAMS * BUCKETS_PER_PHASE)

/* ---- structs ---------------------------------------------------------- */

struct latency_breakdown_event {
    __u64 timestamp;
    __u32 pid;
    __u32 dst_ip;
    __u16 dst_port;
    __u16 _pad;
    __u32 _pad2;
    __u64 dns_ns;           /* DNS resolution time */
    __u64 tcp_connect_ns;   /* TCP handshake time */
    __u64 tls_handshake_ns; /* TLS handshake time */
    __u64 app_rtt_ns;       /* Application request/response time */
    __u64 total_ns;         /* Total end-to-end */
    char  comm[TASK_COMM_LEN];
};

/* Per-request tracking state, keyed by pid_tgid */
struct request_state {
    __u64 dns_start;
    __u64 dns_end;
    __u64 tcp_start;
    __u64 tcp_end;
    __u64 tls_start;
    __u64 tls_end;
    __u64 app_send;
    __u64 app_recv;
    __u32 dst_ip;
    __u16 dst_port;
    __u16 _pad;
    __u64 sk;         /* the socket being connected: filters send/recv */
    __u32 in_recv;    /* tcp_recvmsg entered on sk, waiting for its return */
    __u32 _pad2;
};

/* ---- BPF maps --------------------------------------------------------- */

/* Per-request phase timestamps keyed by pid_tgid */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u64);   /* pid_tgid */
    __type(value, struct request_state);
} request_tracking SEC(".maps");

/* Ring buffer for breakdown events */
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 128 * 1024);
} breakdown_events SEC(".maps");

/* Per-phase latency histogram (percpu for lock-free updates) */
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, TOTAL_HIST_ENTRIES);
    __type(key, __u32);
    __type(value, __u64);
} phase_histogram SEC(".maps");

/* ---- helpers ---------------------------------------------------------- */

/* Map latency to histogram bucket (8 buckets):
 * <100us, 100us-1ms, 1-5ms, 5-10ms, 10-50ms, 50-100ms, 100-500ms, >500ms */
static __always_inline __u32 latency_bucket(__u64 ns)
{
    if (ns < 100000ULL)           return 0;  /* < 100us */
    if (ns < 1000000ULL)          return 1;  /* 100us-1ms */
    if (ns < 5000000ULL)          return 2;  /* 1-5ms */
    if (ns < 10000000ULL)         return 3;  /* 5-10ms */
    if (ns < 50000000ULL)         return 4;  /* 10-50ms */
    if (ns < 100000000ULL)        return 5;  /* 50-100ms */
    if (ns < 500000000ULL)        return 6;  /* 100-500ms */
    return 7;                                 /* > 500ms */
}

static __always_inline void update_phase_histogram(__u32 phase, __u64 latency_ns)
{
    __u32 bucket = phase * BUCKETS_PER_PHASE + latency_bucket(latency_ns);
    if (bucket >= TOTAL_HIST_ENTRIES)
        return;
    __u64 *cnt = bpf_map_lookup_elem(&phase_histogram, &bucket);
    if (cnt)
        (*cnt)++;
}

static __always_inline void emit_breakdown(struct request_state *rs)
{
    __u64 dns_ns = 0, tcp_ns = 0, tls_ns = 0, app_ns = 0, total_ns = 0;
    __u64 first_ts = 0, last_ts = 0;

    if (rs->dns_start && rs->dns_end) {
        dns_ns = rs->dns_end - rs->dns_start;
        update_phase_histogram(HIST_DNS, dns_ns);
    }

    if (rs->tcp_start && rs->tcp_end) {
        tcp_ns = rs->tcp_end - rs->tcp_start;
        update_phase_histogram(HIST_TCP, tcp_ns);
    }

    if (rs->tls_start && rs->tls_end) {
        tls_ns = rs->tls_end - rs->tls_start;
        update_phase_histogram(HIST_TLS, tls_ns);
    }

    if (rs->app_send && rs->app_recv) {
        app_ns = rs->app_recv - rs->app_send;
        update_phase_histogram(HIST_APP, app_ns);
    }

    /* Compute total from first non-zero timestamp to last */
    first_ts = rs->dns_start;
    if (!first_ts || (rs->tcp_start && rs->tcp_start < first_ts))
        first_ts = rs->tcp_start;

    last_ts = rs->app_recv;
    if (!last_ts)
        last_ts = rs->tcp_end;

    if (first_ts && last_ts && last_ts > first_ts) {
        total_ns = last_ts - first_ts;
        update_phase_histogram(HIST_TOTAL, total_ns);
    }

    struct latency_breakdown_event *ev;
    ev = bpf_ringbuf_reserve(&breakdown_events, sizeof(*ev), 0);
    if (!ev)
        return;

    ev->timestamp       = bpf_ktime_get_ns();
    ev->pid             = bpf_get_current_pid_tgid() >> 32;
    ev->dst_ip          = rs->dst_ip;
    ev->dst_port        = rs->dst_port;
    ev->_pad            = 0;
    ev->_pad2           = 0;
    ev->dns_ns          = dns_ns;
    ev->tcp_connect_ns  = tcp_ns;
    ev->tls_handshake_ns = tls_ns;
    ev->app_rtt_ns      = app_ns;
    ev->total_ns        = total_ns;
    bpf_get_current_comm(&ev->comm, sizeof(ev->comm));

    bpf_ringbuf_submit(ev, 0);
}

/* ---- kprobes: DNS phase ----------------------------------------------- */

/* udp_sendmsg(struct sock *sk, struct msghdr *msg, size_t len)
 * Only connected UDP sockets (what glibc/musl resolvers use) have skc_dport
 * set; unconnected sendto() queries are not seen. */
SEC("kprobe/udp_sendmsg")
int BPF_KPROBE(breakdown_dns_start, struct sock *sk)
{
    __u16 dport_be = 0;
    BPF_CORE_READ_INTO(&dport_be, sk, __sk_common.skc_dport);
    if (bpf_ntohs(dport_be) != 53)
        return 0;

    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u64 now = bpf_ktime_get_ns();

    struct request_state *rs = bpf_map_lookup_elem(&request_tracking, &pid_tgid);
    if (rs && rs->dns_start && !rs->dns_end)
        return 0;   /* lookup already in flight (e.g. A then AAAA): keep first */

    /* Start a fresh request (drops any stale state of a previous one). */
    struct request_state new_rs = {};
    new_rs.dns_start = now;
    bpf_map_update_elem(&request_tracking, &pid_tgid, &new_rs, BPF_ANY);

    return 0;
}

/* ---- kprobes: TCP handshake phase ------------------------------------- */

/* tcp_v4_connect(struct sock *sk, struct sockaddr *uaddr, int addr_len)
 * skc_daddr/skc_dport are not set yet at entry: read the destination from
 * uaddr. */
SEC("kprobe/tcp_v4_connect")
int BPF_KPROBE(breakdown_tcp_connect, struct sock *sk, struct sockaddr *uaddr)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u64 now = bpf_ktime_get_ns();

    struct request_state new_rs = {};
    struct request_state *old = bpf_map_lookup_elem(&request_tracking, &pid_tgid);

    /* Keep only an in-progress DNS phase; everything else is a stale request. */
    if (old && old->dns_start) {
        new_rs.dns_start = old->dns_start;
        new_rs.dns_end   = old->dns_end ? old->dns_end : now;
    }
    new_rs.tcp_start = now;
    new_rs.sk        = (__u64)sk;
    if (gryvia_uaddr_v4(uaddr, &new_rs.dst_ip, &new_rs.dst_port))
        return 0;

    bpf_map_update_elem(&request_tracking, &pid_tgid, &new_rs, BPF_ANY);
    return 0;
}

/* tcp_v4_connect only sends the SYN; a blocking connect() finishes the
 * handshake inside inet_stream_connect, so its return marks the handshake end.
 * (For O_NONBLOCK sockets it returns -EINPROGRESS immediately and this
 * degrades to the SYN-send time.) */
SEC("kretprobe/inet_stream_connect")
int BPF_KRETPROBE(breakdown_tcp_connect_ret, int ret)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    struct request_state *rs = bpf_map_lookup_elem(&request_tracking, &pid_tgid);
    if (!rs || !rs->tcp_start)
        return 0;

    if (ret != 0 && ret != -EINPROGRESS) {
        bpf_map_delete_elem(&request_tracking, &pid_tgid);
        return 0;
    }

    rs->tcp_end = bpf_ktime_get_ns();
    return 0;
}

/* ---- kprobes: TLS handshake phase ------------------------------------- */

/* tls_sw_sendmsg only fires for kernel TLS (kTLS) sockets; userspace TLS
 * libraries never reach it, so on those the TLS phase is simply absent. */
SEC("kprobe/tls_sw_sendmsg")
int BPF_KPROBE(breakdown_tls_handshake)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u64 now = bpf_ktime_get_ns();

    struct request_state *rs = bpf_map_lookup_elem(&request_tracking, &pid_tgid);
    if (!rs)
        return 0;

    /* Record first TLS send as handshake start, subsequent as end */
    if (!rs->tls_start)
        rs->tls_start = now;
    else
        rs->tls_end = now;

    return 0;
}

/* ---- kprobes: Application phase --------------------------------------- */

/* tcp_sendmsg(struct sock *sk, struct msghdr *msg, size_t size) */
SEC("kprobe/tcp_sendmsg")
int BPF_KPROBE(breakdown_app_send, struct sock *sk)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u64 now = bpf_ktime_get_ns();

    struct request_state *rs = bpf_map_lookup_elem(&request_tracking, &pid_tgid);
    if (!rs || rs->sk != (__u64)sk)
        return 0;

    /* First send after the handshake starts the application phase */
    if (rs->tcp_end && !rs->app_send) {
        rs->app_send = now;
        /* If TLS was started but not ended, mark it as ended now */
        if (rs->tls_start && !rs->tls_end)
            rs->tls_end = now;
    }

    return 0;
}

/* tcp_recvmsg(struct sock *sk, struct msghdr *msg, ...): the response has only
 * arrived when tcp_recvmsg RETURNS data, so entry just arms the kretprobe. */
SEC("kprobe/tcp_recvmsg")
int BPF_KPROBE(breakdown_app_recv, struct sock *sk)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    struct request_state *rs = bpf_map_lookup_elem(&request_tracking, &pid_tgid);
    if (!rs || rs->sk != (__u64)sk || !rs->app_send)
        return 0;

    rs->in_recv = 1;
    return 0;
}

SEC("kretprobe/tcp_recvmsg")
int BPF_KRETPROBE(breakdown_app_recv_ret, long ret)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    struct request_state *rs = bpf_map_lookup_elem(&request_tracking, &pid_tgid);
    if (!rs || !rs->in_recv)
        return 0;

    rs->in_recv = 0;
    if (ret <= 0)   /* EAGAIN / error / EOF: no response data yet */
        return 0;

    rs->app_recv = bpf_ktime_get_ns();

    /* Emit the complete breakdown event */
    emit_breakdown(rs);

    /* Clean up tracking state for this request */
    bpf_map_delete_elem(&request_tracking, &pid_tgid);

    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
