// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// exfil_detect.c - Detect data exfiltration from containers.
//
// Tracks per-(PID, dst_ip) outbound byte volume over a sliding window.
// Flags when outbound traffic exceeds a configurable threshold to a
// single external (non-RFC1918) IP address.  Uses kprobe/tcp_sendmsg
// for byte counting and kprobe/tcp_v4_connect for connection tracking.

#include "headers/common.h"
#include "headers/security_common.h"

/* Default threshold: 1 GB in bytes */
#define DEFAULT_EXFIL_THRESHOLD_BYTES  (1ULL * 1024 * 1024 * 1024)

/* Default time window: 5 minutes in nanoseconds */
#define DEFAULT_EXFIL_WINDOW_NS        (5ULL * 60 * 1000000000ULL)

/* Threshold config indices */
#define EXFIL_CFG_THRESHOLD_HI  0  /* upper 32 bits of byte threshold */
#define EXFIL_CFG_THRESHOLD_LO  1  /* lower 32 bits of byte threshold */
#define EXFIL_CFG_WINDOW_SECS   2  /* window in seconds */
#define EXFIL_CFG_MAX           4

/* Internal range config indices */
#define INTERNAL_RANGE_10     0  /* 10.0.0.0/8 */
#define INTERNAL_RANGE_172    1  /* 172.16.0.0/12 */
#define INTERNAL_RANGE_192    2  /* 192.168.0.0/16 */
#define INTERNAL_RANGE_MAX    3

/* Stat counter indices */
#define EXFIL_CTR_BYTES_TRACKED  0
#define EXFIL_CTR_CONNECTIONS    1
#define EXFIL_CTR_ALERTS         2
#define EXFIL_CTR_MAX            3

/* Key for per-destination byte tracking */
struct dest_key {
    __u32 pid;
    __u32 dst_ip;
};

/* Value for per-destination byte tracking */
struct dest_bytes {
    __u64 total_bytes;
    __u64 first_seen_ns;
    __u64 last_alert_ns;  /* rate-limit alerts */
};

/* ---- BPF maps --------------------------------------------------------- */

// Ring buffer for exfiltration events.
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 64 * 1024);  /* 64 KB */
} exfil_events SEC(".maps");

// Per-(pid, dst_ip) outbound byte tracking.
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 16384);
    __type(key, struct dest_key);
    __type(value, struct dest_bytes);
} per_dest_bytes SEC(".maps");

// Configurable thresholds (populated from userspace).
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, EXFIL_CFG_MAX);
    __type(key, __u32);
    __type(value, __u64);
} exfil_thresholds SEC(".maps");

// Internal IP ranges configuration.
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, INTERNAL_RANGE_MAX);
    __type(key, __u32);
    __type(value, __u64);  /* packed: upper 32 = network, lower 32 = mask */
} internal_ranges SEC(".maps");

// Detection counters.
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, EXFIL_CTR_MAX);
    __type(key, __u32);
    __type(value, __u64);
} exfil_stats SEC(".maps");

/* ---- helpers ---------------------------------------------------------- */

static __always_inline void bump_exfil_counter(__u32 idx)
{
    __u64 *cnt = bpf_map_lookup_elem(&exfil_stats, &idx);
    if (cnt)
        __sync_fetch_and_add(cnt, 1);
}

/*
 * Check if an IP is in RFC1918 private range.
 * IP is in network byte order.
 *
 * 10.0.0.0/8:      0x0A000000 mask 0xFF000000
 * 172.16.0.0/12:    0xAC100000 mask 0xFFF00000
 * 192.168.0.0/16:   0xC0A80000 mask 0xFFFF0000
 */
static __always_inline int is_internal_ip(__u32 ip_be)
{
    __u32 ip = bpf_ntohl(ip_be);

    /* 10.0.0.0/8 */
    if ((ip & 0xFF000000) == 0x0A000000)
        return 1;
    /* 172.16.0.0/12 */
    if ((ip & 0xFFF00000) == 0xAC100000)
        return 1;
    /* 192.168.0.0/16 */
    if ((ip & 0xFFFF0000) == 0xC0A80000)
        return 1;
    /* 127.0.0.0/8 loopback */
    if ((ip & 0xFF000000) == 0x7F000000)
        return 1;
    /* 169.254.0.0/16 link-local */
    if ((ip & 0xFFFF0000) == 0xA9FE0000)
        return 1;

    return 0;
}

static __always_inline __u64 get_threshold(void)
{
    __u32 idx_hi = EXFIL_CFG_THRESHOLD_HI;
    __u32 idx_lo = EXFIL_CFG_THRESHOLD_LO;

    __u64 *hi = bpf_map_lookup_elem(&exfil_thresholds, &idx_hi);
    __u64 *lo = bpf_map_lookup_elem(&exfil_thresholds, &idx_lo);

    if (hi && lo && (*hi || *lo))
        return ((*hi) << 32) | (*lo);

    return DEFAULT_EXFIL_THRESHOLD_BYTES;
}

static __always_inline __u64 get_window_ns(void)
{
    __u32 idx = EXFIL_CFG_WINDOW_SECS;
    __u64 *secs = bpf_map_lookup_elem(&exfil_thresholds, &idx);

    if (secs && *secs > 0)
        return (*secs) * 1000000000ULL;

    return DEFAULT_EXFIL_WINDOW_NS;
}

/* ---- kprobe/tcp_sendmsg ----------------------------------------------- */

/*
 * Track outbound bytes per (PID, dst_ip).
 *
 * tcp_sendmsg(struct sock *sk, struct msghdr *msg, size_t size)
 *
 * We read dst IP from sock->__sk_common.skc_daddr and track cumulative
 * bytes sent.
 */
SEC("kprobe/tcp_sendmsg")
int BPF_KPROBE(exfil_tcp_sendmsg, struct sock *sk, void *msg, __u64 size)
{
    __u32 dst_ip = 0, src_ip = 0;
    __u16 dst_port = 0;
    unsigned short family = BPF_CORE_READ(sk, __sk_common.skc_family);

    /* IPv4 sockets only; skc_daddr is meaningless for AF_INET6. */
    if (family != AF_INET)
        return 0;
    gryvia_sock_v4_tuple(sk, &src_ip, &dst_ip, NULL, &dst_port);

    /* Ignore internal traffic. */
    if (is_internal_ip(dst_ip))
        return 0;

    /* Ignore zero/broadcast. */
    if (dst_ip == 0 || dst_ip == 0xFFFFFFFF)
        return 0;

    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u32 pid = pid_tgid >> 32;
    __u64 now = bpf_ktime_get_ns();

    struct dest_key key = {};
    key.pid    = pid;
    key.dst_ip = dst_ip;

    bump_exfil_counter(EXFIL_CTR_BYTES_TRACKED);

    struct dest_bytes *db = bpf_map_lookup_elem(&per_dest_bytes, &key);
    if (db) {
        __u64 window = get_window_ns();

        /* Reset window if expired. */
        if ((now - db->first_seen_ns) > window) {
            db->first_seen_ns = now;
            db->total_bytes   = size;
            db->last_alert_ns = 0;
            return 0;
        }

        __sync_fetch_and_add(&db->total_bytes, size);

        /* Check threshold. */
        __u64 threshold = get_threshold();
        if (db->total_bytes >= threshold) {
            /* Rate limit: only alert once per window. */
            if (db->last_alert_ns == 0 ||
                (now - db->last_alert_ns) > window) {
                db->last_alert_ns = now;
                bump_exfil_counter(EXFIL_CTR_ALERTS);

                struct security_event *evt;
                evt = bpf_ringbuf_reserve(&exfil_events, sizeof(*evt), 0);
                if (!evt)
                    return 0;

                sec_event_init(evt, SEC_DATA_EXFILTRATION, SEC_SEV_HIGH);
                evt->src_ip     = src_ip;
                evt->dst_ip     = dst_ip;
                evt->dst_port   = dst_port;
                evt->bytes      = db->total_bytes;

                bpf_ringbuf_submit(evt, 0);
            }
        }
    } else {
        /* First send to this destination. */
        struct dest_bytes new_db = {};
        new_db.total_bytes   = size;
        new_db.first_seen_ns = now;
        bpf_map_update_elem(&per_dest_bytes, &key, &new_db, BPF_NOEXIST);
    }

    return 0;
}

/* ---- kprobe/tcp_v4_connect -------------------------------------------- */

/*
 * Track new outbound connections to external IPs for correlation.
 */
SEC("kprobe/tcp_v4_connect")
int BPF_KPROBE(exfil_tcp_connect, struct sock *sk, struct sockaddr_in *uaddr)
{
    /* uaddr is a kernel copy here: read it with the kernel helper. */
    struct sockaddr_in sin = {};
    if (bpf_probe_read_kernel(&sin, sizeof(sin), uaddr))
        return 0;
    if (sin.sin_family != AF_INET)
        return 0;
    __u32 dst_ip = sin.sin_addr.s_addr;

    if (is_internal_ip(dst_ip))
        return 0;

    if (dst_ip == 0 || dst_ip == 0xFFFFFFFF)
        return 0;

    bump_exfil_counter(EXFIL_CTR_CONNECTIONS);

    /* Pre-create tracking entry so tcp_sendmsg has a record ready. */
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u32 pid = pid_tgid >> 32;

    struct dest_key key = {};
    key.pid    = pid;
    key.dst_ip = dst_ip;

    struct dest_bytes *existing = bpf_map_lookup_elem(&per_dest_bytes, &key);
    if (!existing) {
        struct dest_bytes new_db = {};
        new_db.first_seen_ns = bpf_ktime_get_ns();
        bpf_map_update_elem(&per_dest_bytes, &key, &new_db, BPF_NOEXIST);
    }

    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
