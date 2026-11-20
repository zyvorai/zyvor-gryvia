// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// tcp_tuning.c - TCP tuning recommendations via tcp_probe tracepoint.
//
// Hooks the kernel's built-in tcp_probe tracepoint to capture per-connection
// TCP metrics including congestion window size, smoothed RTT, receive window,
// slow start threshold, and send window.  Maintains running averages per
// connection and histograms for cwnd and RTT distributions.

#include "headers/common.h"

/* ---- tracepoint args -------------------------------------------------- */

/* Matches /sys/kernel/tracing/events/tcp/tcp_probe/format (kernel >= 5.x
 * with sockaddr-based saddr/daddr).  saddr/daddr hold a struct sockaddr_in
 * (family, port, addr) or sockaddr_in6; sport/dport are host byte order. */
struct tcp_probe_args {
    __u64 pad;            /* common_* fields */
    __u8  saddr[28];
    __u8  daddr[28];
    __u16 sport;
    __u16 dport;
    __u16 family;
    __u32 mark;
    __u16 data_len;
    __u32 snd_nxt;
    __u32 snd_una;
    __u32 snd_cwnd;
    __u32 ssthresh;
    __u32 snd_wnd;
    __u32 srtt;
    __u32 rcv_wnd;
    __u64 sock_cookie;
    const void *skbaddr;
    const void *skaddr;
};

/* ---- event struct ----------------------------------------------------- */

struct tcp_tuning_event {
    __u64 timestamp;
    __u32 pid;
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
    __u32 snd_cwnd;
    __u32 srtt_us;
    __u32 rcv_wnd;
    __u32 ssthresh;
    __u32 snd_wnd;
    __u32 _pad;
    __u64 bytes_acked;
    char  comm[TASK_COMM_LEN];
};

/* ---- connection stats for running averages ---------------------------- */

struct conn_tuple {
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
};

struct conn_stats {
    __u64 avg_cwnd;
    __u64 avg_rtt_us;
    __u64 avg_rcv_wnd;
    __u64 sample_count;
    __u64 total_bytes;
};

/* ---- histogram buckets ------------------------------------------------ */

/* cwnd histogram: 8 buckets */
#define CWND_BUCKETS 8
/* <10, 10-50, 50-100, 100-500, 500-1000, 1000-5000, 5000-10000, >10000 */

/* RTT histogram: 6 buckets */
#define RTT_BUCKETS 6
/* <1ms, 1-5ms, 5-10ms, 10-50ms, 50-100ms, >100ms */

/* ---- BPF maps --------------------------------------------------------- */

/* Ring buffer for TCP tuning events */
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 128 * 1024);
} tcp_tuning_events SEC(".maps");

/* Per-connection running average stats keyed by 4-tuple */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, struct conn_tuple);
    __type(value, struct conn_stats);
} tcp_conn_stats SEC(".maps");

/* Congestion window histogram (percpu for lock-free updates) */
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, CWND_BUCKETS);
    __type(key, __u32);
    __type(value, __u64);
} cwnd_histogram SEC(".maps");

/* RTT histogram (percpu for lock-free updates) */
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, RTT_BUCKETS);
    __type(key, __u32);
    __type(value, __u64);
} rtt_histogram SEC(".maps");

/* ---- helpers ---------------------------------------------------------- */

static __always_inline __u32 cwnd_bucket(__u32 cwnd)
{
    if (cwnd < 10)        return 0;
    if (cwnd < 50)        return 1;
    if (cwnd < 100)       return 2;
    if (cwnd < 500)       return 3;
    if (cwnd < 1000)      return 4;
    if (cwnd < 5000)      return 5;
    if (cwnd < 10000)     return 6;
    return 7;
}

static __always_inline __u32 rtt_bucket(__u32 srtt_us)
{
    if (srtt_us < 1000)        return 0;  /* < 1ms */
    if (srtt_us < 5000)        return 1;  /* 1-5ms */
    if (srtt_us < 10000)       return 2;  /* 5-10ms */
    if (srtt_us < 50000)       return 3;  /* 10-50ms */
    if (srtt_us < 100000)      return 4;  /* 50-100ms */
    return 5;                              /* > 100ms */
}

/* ---- tracepoint hook -------------------------------------------------- */

SEC("tracepoint/tcp/tcp_probe")
int tcp_tuning_probe(struct tcp_probe_args *ctx)
{
    /* IPv4 only: saddr/daddr are sockaddr_in, address at offset 4. */
    if (ctx->family != AF_INET)
        return 0;

    __u32 snd_cwnd = ctx->snd_cwnd, ssthresh = ctx->ssthresh;
    __u32 snd_wnd = ctx->snd_wnd, srtt = ctx->srtt, rcv_wnd = ctx->rcv_wnd;
    __u16 sport = ctx->sport, dport = ctx->dport;
    __u32 saddr = 0, daddr = 0;

    __builtin_memcpy(&saddr, &ctx->saddr[4], sizeof(saddr));
    __builtin_memcpy(&daddr, &ctx->daddr[4], sizeof(daddr));

    /* Update cwnd histogram */
    __u32 cb = cwnd_bucket(snd_cwnd);
    __u64 *cwnd_cnt = bpf_map_lookup_elem(&cwnd_histogram, &cb);
    if (cwnd_cnt)
        (*cwnd_cnt)++;

    /* Update RTT histogram */
    __u32 rb = rtt_bucket(srtt);
    __u64 *rtt_cnt = bpf_map_lookup_elem(&rtt_histogram, &rb);
    if (rtt_cnt)
        (*rtt_cnt)++;

    /* Update per-connection running averages */
    struct conn_tuple key = {};
    key.src_ip   = saddr;
    key.dst_ip   = daddr;
    key.src_port = sport;
    key.dst_port = dport;

    struct conn_stats *stats = bpf_map_lookup_elem(&tcp_conn_stats, &key);
    if (stats) {
        __u64 n = stats->sample_count;
        if (n > 0) {
            stats->avg_cwnd    = (stats->avg_cwnd * n + snd_cwnd) / (n + 1);
            stats->avg_rtt_us  = (stats->avg_rtt_us * n + srtt) / (n + 1);
            stats->avg_rcv_wnd = (stats->avg_rcv_wnd * n + rcv_wnd) / (n + 1);
        }
        stats->sample_count = n + 1;
    } else {
        struct conn_stats new_stats = {};
        new_stats.avg_cwnd    = snd_cwnd;
        new_stats.avg_rtt_us  = srtt;
        new_stats.avg_rcv_wnd = rcv_wnd;
        new_stats.sample_count = 1;
        /* If another CPU inserted first this sample is dropped, which is fine
         * for a running average. */
        bpf_map_update_elem(&tcp_conn_stats, &key, &new_stats, BPF_NOEXIST);
    }

    /* Emit event to ring buffer */
    struct tcp_tuning_event *ev;
    ev = bpf_ringbuf_reserve(&tcp_tuning_events, sizeof(*ev), 0);
    if (!ev)
        return 0;

    ev->timestamp = bpf_ktime_get_ns();
    ev->src_ip    = saddr;
    ev->dst_ip    = daddr;
    ev->src_port  = sport;
    ev->dst_port  = dport;
    ev->snd_cwnd  = snd_cwnd;
    ev->srtt_us   = srtt;
    ev->rcv_wnd   = rcv_wnd;
    ev->ssthresh  = ssthresh;
    ev->snd_wnd   = snd_wnd;
    ev->_pad      = 0;
    ev->bytes_acked = 0;

    __u64 pid_tgid = bpf_get_current_pid_tgid();
    ev->pid = pid_tgid >> 32;
    bpf_get_current_comm(&ev->comm, sizeof(ev->comm));

    bpf_ringbuf_submit(ev, 0);

    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
