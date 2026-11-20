// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// connpool_analyze.c - Connection pooling analysis via kprobe/kretprobe.
//
// Tracks TCP connection lifecycle (creation, establishment, destruction)
// to analyze connection pooling behavior.  Detects short-lived connections
// and excessive connection churn that indicate missing or broken connection
// pools.

#include "headers/common.h"

/* ---- event types ------------------------------------------------------ */

#define EVENT_CONNECT 0
#define EVENT_CLOSE   1

/* ---- structs ---------------------------------------------------------- */

struct connpool_event {
    __u64 timestamp;
    __u32 pid;
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
    __u8  event_type;  /* 0=connect, 1=close */
    __u8  _pad[3];
    __u32 _pad2;
    __u64 lifetime_ns; /* only for close events */
    __u32 active_count;
    char  comm[TASK_COMM_LEN];
    __u32 _pad3;
};

/* In flight (between tcp_v4_connect entry and return), keyed by pid_tgid */
struct conn_inflight {
    __u64 sk;
    __u32 dst_ip;
    __u16 dst_port;
    __u16 _pad;
};

/* Established connection, keyed by struct sock pointer until tcp_close */
struct conn_live {
    __u64 start_ns;
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
};

/* Service pair key */
struct service_pair_key {
    __u32 src_ip;
    __u32 dst_ip;
    __u16 dst_port;
    __u16 _pad;
};

/* Service pair stats */
struct service_pair_value {
    __u64 active;
    __u64 total_created;
    __u64 short_lived;
    __u64 total_lifetime_ns;
};

/* Connection lifetime histogram: 8 buckets */
#define LIFETIME_BUCKETS 8
/* <100ms, 100ms-1s, 1-10s, 10-60s, 1-5min, 5-30min, 30min-1hr, >1hr */

#define SHORT_LIVED_THRESHOLD_NS 1000000000ULL  /* 1 second */

/* ---- BPF maps --------------------------------------------------------- */

/* Ring buffer for connection pool events */
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 64 * 1024);
} connpool_events SEC(".maps");

/* In-flight connects keyed by pid_tgid (LRU: a missed kretprobe must not leak) */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u64);   /* pid_tgid */
    __type(value, struct conn_inflight);
} conn_start_map SEC(".maps");

/* Live tracked connections keyed by struct sock pointer (LRU: sockets that
 * are closed without us seeing tcp_close age out) */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u64);   /* struct sock * */
    __type(value, struct conn_live);
} conn_live_map SEC(".maps");

/* Per-service-pair statistics */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, struct service_pair_key);
    __type(value, struct service_pair_value);
} service_pair_stats SEC(".maps");

/* Connection lifetime histogram */
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, LIFETIME_BUCKETS);
    __type(key, __u32);
    __type(value, __u64);
} connpool_histogram SEC(".maps");

/* ---- helpers ---------------------------------------------------------- */

static __always_inline __u32 lifetime_bucket(__u64 lifetime_ns)
{
    if (lifetime_ns < 100000000ULL)           return 0; /* < 100ms */
    if (lifetime_ns < 1000000000ULL)          return 1; /* 100ms-1s */
    if (lifetime_ns < 10000000000ULL)         return 2; /* 1-10s */
    if (lifetime_ns < 60000000000ULL)         return 3; /* 10-60s */
    if (lifetime_ns < 300000000000ULL)        return 4; /* 1-5min */
    if (lifetime_ns < 1800000000000ULL)       return 5; /* 5-30min */
    if (lifetime_ns < 3600000000000ULL)       return 6; /* 30min-1hr */
    return 7;                                            /* > 1hr */
}

/* ---- kprobes ---------------------------------------------------------- */

/* tcp_v4_connect(struct sock *sk, struct sockaddr *uaddr, int addr_len)
 * The destination is not in sk at entry, so take it from uaddr. */
SEC("kprobe/tcp_v4_connect")
int BPF_KPROBE(connpool_tcp_connect, struct sock *sk, struct sockaddr *uaddr)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    struct conn_inflight info = {};

    if (gryvia_uaddr_v4(uaddr, &info.dst_ip, &info.dst_port))
        return 0;
    info.sk = (__u64)sk;

    bpf_map_update_elem(&conn_start_map, &pid_tgid, &info, BPF_ANY);
    return 0;
}

/* kretprobe for tcp_v4_connect - the connect was started (0 = SYN sent) */
SEC("kretprobe/tcp_v4_connect")
int BPF_KRETPROBE(connpool_tcp_connect_ret, int ret)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    struct conn_inflight *info = bpf_map_lookup_elem(&conn_start_map, &pid_tgid);
    if (!info)
        return 0;

    struct conn_inflight in = *info;
    bpf_map_delete_elem(&conn_start_map, &pid_tgid);

    /* Connection failed */
    if (ret != 0)
        return 0;

    struct conn_live live = {};
    live.start_ns = bpf_ktime_get_ns();
    live.dst_ip   = in.dst_ip;
    live.dst_port = in.dst_port;
    /* Local address/port are chosen inside tcp_v4_connect: valid now. */
    gryvia_sock_v4_tuple((struct sock *)in.sk, &live.src_ip, NULL,
                         &live.src_port, NULL);
    bpf_map_update_elem(&conn_live_map, &in.sk, &live, BPF_ANY);

    /* Update service pair stats for the new connection */
    struct service_pair_key sp_key = {};
    sp_key.src_ip   = live.src_ip;
    sp_key.dst_ip   = live.dst_ip;
    sp_key.dst_port = live.dst_port;

    struct service_pair_value *sp = bpf_map_lookup_elem(&service_pair_stats, &sp_key);
    if (!sp) {
        struct service_pair_value new_sp = {};
        bpf_map_update_elem(&service_pair_stats, &sp_key, &new_sp, BPF_NOEXIST);
        sp = bpf_map_lookup_elem(&service_pair_stats, &sp_key);
    }
    if (sp) {
        __sync_fetch_and_add(&sp->active, 1);
        __sync_fetch_and_add(&sp->total_created, 1);
    }

    /* Emit connect event */
    struct connpool_event *ev;
    ev = bpf_ringbuf_reserve(&connpool_events, sizeof(*ev), 0);
    if (!ev)
        return 0;

    ev->timestamp    = live.start_ns;
    ev->pid          = pid_tgid >> 32;
    ev->src_ip       = live.src_ip;
    ev->dst_ip       = live.dst_ip;
    ev->src_port     = live.src_port;
    ev->dst_port     = live.dst_port;
    ev->event_type   = EVENT_CONNECT;
    ev->_pad[0]      = 0;
    ev->_pad[1]      = 0;
    ev->_pad[2]      = 0;
    ev->_pad2        = 0;
    ev->lifetime_ns  = 0;
    ev->active_count = sp ? (__u32)sp->active : 1;
    ev->_pad3        = 0;

    bpf_get_current_comm(&ev->comm, sizeof(ev->comm));
    bpf_ringbuf_submit(ev, 0);

    return 0;
}

/* tcp_close(struct sock *sk, long timeout) */
SEC("kprobe/tcp_close")
int BPF_KPROBE(connpool_tcp_close, struct sock *sk)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u64 now = bpf_ktime_get_ns();
    __u64 skey = (__u64)sk;

    /* Only connections whose connect we observed are accounted (this skips
     * listeners, accepted sockets and connections that pre-date the probe). */
    struct conn_live *live = bpf_map_lookup_elem(&conn_live_map, &skey);
    if (!live)
        return 0;

    struct conn_live lv = *live;
    bpf_map_delete_elem(&conn_live_map, &skey);

    __u64 lifetime_ns = now > lv.start_ns ? now - lv.start_ns : 0;

    struct service_pair_key sp_key = {};
    sp_key.src_ip   = lv.src_ip;
    sp_key.dst_ip   = lv.dst_ip;
    sp_key.dst_port = lv.dst_port;

    struct service_pair_value *sp = bpf_map_lookup_elem(&service_pair_stats, &sp_key);
    if (sp) {
        if (sp->active > 0)
            __sync_fetch_and_add(&sp->active, -1);
        __sync_fetch_and_add(&sp->total_lifetime_ns, lifetime_ns);
        if (lifetime_ns < SHORT_LIVED_THRESHOLD_NS)
            __sync_fetch_and_add(&sp->short_lived, 1);
    }

    __u32 bucket = lifetime_bucket(lifetime_ns);
    __u64 *cnt = bpf_map_lookup_elem(&connpool_histogram, &bucket);
    if (cnt)
        (*cnt)++;

    /* Emit close event */
    struct connpool_event *ev;
    ev = bpf_ringbuf_reserve(&connpool_events, sizeof(*ev), 0);
    if (!ev)
        return 0;

    ev->timestamp    = now;
    ev->pid          = pid_tgid >> 32;
    ev->src_ip       = lv.src_ip;
    ev->dst_ip       = lv.dst_ip;
    ev->src_port     = lv.src_port;
    ev->dst_port     = lv.dst_port;
    ev->event_type   = EVENT_CLOSE;
    ev->_pad[0]      = 0;
    ev->_pad[1]      = 0;
    ev->_pad[2]      = 0;
    ev->_pad2        = 0;
    ev->lifetime_ns  = lifetime_ns;
    ev->active_count = sp ? (__u32)sp->active : 0;
    ev->_pad3        = 0;

    bpf_get_current_comm(&ev->comm, sizeof(ev->comm));
    bpf_ringbuf_submit(ev, 0);

    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
