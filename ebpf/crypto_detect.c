// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// crypto_detect.c - Detect cryptocurrency mining activity in containers.
//
// Uses kprobe/tcp_v4_connect to detect outbound connections to known mining
// pool ports and tracepoint/sched/sched_process_exec to detect known miner
// binary execution.  Emits HIGH severity events when a PID connects to 2+
// mining ports within 60 seconds or when a known miner binary is executed.

#include "headers/common.h"
#include "headers/security_common.h"

/* Detection stat counter indices */
#define MINING_CTR_PORT_HIT      0
#define MINING_CTR_BINARY_HIT    1
#define MINING_CTR_ALERT_EMITTED 2
#define MINING_CTR_MAX           3

/* Time window for multi-port detection (60 seconds in nanoseconds) */
#define MINING_WINDOW_NS  (60ULL * 1000000000ULL)

/* Suspect PID tracking record */
struct suspect_info {
    __u64 first_seen_ns;     /* timestamp of first suspicious connection */
    __u32 port_hit_count;    /* number of distinct mining port hits */
    __u16 last_port;         /* last suspicious port connected to */
    __u16 _pad;
};

/* ---- BPF maps --------------------------------------------------------- */

// Ring buffer for mining detection events.
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 64 * 1024);  /* 64 KB */
} mining_events SEC(".maps");

// Known mining pool ports (populated from userspace).
// Key: port number, Value: 1 (present).
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 64);
    __type(key, __u16);
    __type(value, __u8);
} mining_ports SEC(".maps");

// Per-PID suspicious connection tracking (LRU to auto-evict stale entries).
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 8192);
    __type(key, __u32);  /* PID */
    __type(value, struct suspect_info);
} suspect_pids SEC(".maps");

// Detection counters.
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, MINING_CTR_MAX);
    __type(key, __u32);
    __type(value, __u64);
} mining_stats SEC(".maps");

/* ---- helpers ---------------------------------------------------------- */

static __always_inline void bump_mining_counter(__u32 idx)
{
    __u64 *cnt = bpf_map_lookup_elem(&mining_stats, &idx);
    if (cnt)
        __sync_fetch_and_add(cnt, 1);
}

static __always_inline void emit_mining_event(__u32 dst_ip, __u16 dst_port)
{
    struct security_event *evt;

    evt = bpf_ringbuf_reserve(&mining_events, sizeof(*evt), 0);
    if (!evt)
        return;

    sec_event_init(evt, SEC_CRYPTO_MINING, SEC_SEV_HIGH);
    evt->dst_ip   = dst_ip;
    evt->dst_port = dst_port;

    bpf_ringbuf_submit(evt, 0);
}

/* ---- kprobe/tcp_v4_connect -------------------------------------------- */

/*
 * Intercept outbound TCP connections.  Check if the destination port
 * matches a known mining pool port.  Track per-PID suspicious hit count
 * and alert when threshold is reached.
 *
 * tcp_v4_connect(struct sock *sk, struct sockaddr *uaddr, int addr_len)
 */
SEC("kprobe/tcp_v4_connect")
int BPF_KPROBE(detect_mining_connect, struct sock *sk, struct sockaddr_in *uaddr)
{
    /* uaddr is already a KERNEL copy of the sockaddr (tcp_v4_connect runs
     * after the syscall layer copied it in), so it must be read with
     * bpf_probe_read_kernel, not bpf_probe_read_user. */
    struct sockaddr_in sin = {};
    if (bpf_probe_read_kernel(&sin, sizeof(sin), uaddr))
        return 0;
    if (sin.sin_family != AF_INET)
        return 0;
    __u16 dst_port = bpf_ntohs(sin.sin_port);

    /* Check if the port is in our mining ports map. */
    __u8 *is_mining = bpf_map_lookup_elem(&mining_ports, &dst_port);
    if (!is_mining)
        return 0;

    bump_mining_counter(MINING_CTR_PORT_HIT);

    __u32 pid = bpf_get_current_pid_tgid() >> 32;
    __u64 now = bpf_ktime_get_ns();
    __u32 dst_ip = sin.sin_addr.s_addr;

    /* Update suspect tracking. */
    struct suspect_info *info = bpf_map_lookup_elem(&suspect_pids, &pid);
    if (info) {
        /* Check if still within the detection window. */
        if ((now - info->first_seen_ns) > MINING_WINDOW_NS) {
            /* Window expired -- reset. */
            info->first_seen_ns  = now;
            info->port_hit_count = 1;
            info->last_port      = dst_port;
        } else {
            /* Within window.  Only count distinct ports. */
            if (info->last_port != dst_port)
                info->port_hit_count++;
            info->last_port = dst_port;

            /* Alert when 2+ distinct mining ports hit within window. */
            if (info->port_hit_count >= 2) {
                bump_mining_counter(MINING_CTR_ALERT_EMITTED);
                emit_mining_event(dst_ip, dst_port);
                /* Reset to avoid spamming. */
                info->port_hit_count = 0;
                info->first_seen_ns  = now;
            }
        }
    } else {
        /* First suspicious connection from this PID. */
        struct suspect_info new_info = {};
        new_info.first_seen_ns  = now;
        new_info.port_hit_count = 1;
        new_info.last_port      = dst_port;
        bpf_map_update_elem(&suspect_pids, &pid, &new_info, BPF_NOEXIST);
    }

    return 0;
}

/* ---- tracepoint/sched/sched_process_exec ------------------------------ */

/*
 * Detect execution of known miner binaries by checking the process
 * comm name after exec.
 *
 * Known miner process names checked via prefix matching:
 *   xmrig, ethminer, t-rex, nbminer, gminer, phoenixm, lolminer
 */
SEC("tracepoint/sched/sched_process_exec")
int detect_miner_exec(void *ctx)
{
    char comm[TASK_COMM_LEN];
    bpf_get_current_comm(&comm, sizeof(comm));

    /* comm is truncated to 15 chars, so match prefixes. */
    if (!SEC_STR_PREFIX(comm, "xmrig") &&
        !SEC_STR_PREFIX(comm, "ethmin") &&
        !SEC_STR_PREFIX(comm, "t-rex") &&
        !SEC_STR_PREFIX(comm, "nbmin") &&
        !SEC_STR_PREFIX(comm, "gminer") &&
        !SEC_STR_PREFIX(comm, "phoenixm") &&
        !SEC_STR_PREFIX(comm, "lolmin"))
        return 0;

    bump_mining_counter(MINING_CTR_BINARY_HIT);
    bump_mining_counter(MINING_CTR_ALERT_EMITTED);

    struct security_event *evt;
    evt = bpf_ringbuf_reserve(&mining_events, sizeof(*evt), 0);
    if (!evt)
        return 0;

    sec_event_init(evt, SEC_CRYPTO_MINING, SEC_SEV_HIGH);

    bpf_ringbuf_submit(evt, 0);

    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
