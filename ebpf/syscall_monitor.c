// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// syscall_monitor.c - Syscall-level network activity monitoring.
//
// Uses raw tracepoints on sys_enter to capture connect(), sendto(), and
// recvfrom() invocations.  Tracks which PIDs make network calls and
// detects when a process that has never performed network IO suddenly
// starts connecting -- a potential indicator of compromise or
// misconfiguration.

#include "headers/common.h"

/* Syscall numbers are per-arch; see GRYVIA_NR_* in headers/gryvia_core.h. */
#define SYS_CONNECT  GRYVIA_NR_connect
#define SYS_SENDTO   GRYVIA_NR_sendto
#define SYS_RECVFROM GRYVIA_NR_recvfrom

/* Process network activity record */
struct proc_net_info {
    __u64 first_seen_ns;
    __u64 last_seen_ns;
    __u64 connect_count;
    __u64 sendto_count;
    __u64 recvfrom_count;
};

/* Alert event for unexpected network activity */
struct alert_event {
    __u64 timestamp;
    __u32 pid;
    __u32 uid;
    __u32 syscall_nr;
    char  comm[TASK_COMM_LEN];
};

/* ---- BPF maps --------------------------------------------------------- */

// Per-PID network activity tracker.
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u32);   // PID
    __type(value, struct proc_net_info);
} proc_net_map SEC(".maps");

// Set of PIDs seen doing network IO (for baseline).
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u32);   // PID
    __type(value, __u8);  // 1 = has baseline
} baseline_pids SEC(".maps");

// Perf ring buffer for alert events.
struct {
    __uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
    __uint(key_size, sizeof(__u32));
    __uint(value_size, sizeof(__u32));
} syscall_events SEC(".maps");

// Per-syscall global counters (index = syscall_nr mapped to 0,1,2).
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 3);
    __type(key, __u32);
    __type(value, __u64);
} syscall_counts SEC(".maps");

/* ---- tracepoint ------------------------------------------------------- */

// raw_tracepoint/sys_enter: args[0] = struct pt_regs *, args[1] = syscall id.
SEC("raw_tracepoint/sys_enter")
int syscall_monitor(struct bpf_raw_tracepoint_args *raw_ctx)
{
    long syscall_nr = (long)raw_ctx->args[1];

    /* Filter to network-related syscalls only. */
    int sc_idx = -1;
    switch (syscall_nr) {
    case SYS_CONNECT:  sc_idx = 0; break;
    case SYS_SENDTO:   sc_idx = 1; break;
    case SYS_RECVFROM: sc_idx = 2; break;
    default:
        return 0;
    }

    /* Update global counter. */
    __u32 idx = (__u32)sc_idx;
    __u64 *cnt = bpf_map_lookup_elem(&syscall_counts, &idx);
    if (cnt)
        __sync_fetch_and_add(cnt, 1);

    /* Gather process info. */
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u32 pid = pid_tgid >> 32;
    __u64 uid_gid = bpf_get_current_uid_gid();
    __u32 uid = (__u32)uid_gid;

    /* Update per-PID activity record. */
    struct proc_net_info *info = bpf_map_lookup_elem(&proc_net_map, &pid);
    if (info) {
        info->last_seen_ns = bpf_ktime_get_ns();
        switch (syscall_nr) {
        case SYS_CONNECT:  __sync_fetch_and_add(&info->connect_count, 1); break;
        case SYS_SENDTO:   __sync_fetch_and_add(&info->sendto_count, 1);  break;
        case SYS_RECVFROM: __sync_fetch_and_add(&info->recvfrom_count, 1); break;
        }
    } else {
        /* First network syscall from this PID -- check baseline. */
        struct proc_net_info new_info = {};
        new_info.first_seen_ns = bpf_ktime_get_ns();
        new_info.last_seen_ns  = new_info.first_seen_ns;
        switch (syscall_nr) {
        case SYS_CONNECT:  new_info.connect_count = 1; break;
        case SYS_SENDTO:   new_info.sendto_count  = 1; break;
        case SYS_RECVFROM: new_info.recvfrom_count = 1; break;
        }
        bpf_map_update_elem(&proc_net_map, &pid, &new_info, BPF_NOEXIST);

        /* If not in baseline set, emit an alert. */
        __u8 *seen = bpf_map_lookup_elem(&baseline_pids, &pid);
        if (!seen && syscall_nr == SYS_CONNECT) {
            struct alert_event alert = {};
            alert.timestamp  = bpf_ktime_get_ns();
            alert.pid        = pid;
            alert.uid        = uid;
            alert.syscall_nr = (__u32)syscall_nr;
            bpf_get_current_comm(&alert.comm, sizeof(alert.comm));

            bpf_perf_event_output(raw_ctx, &syscall_events,
                                  BPF_F_CURRENT_CPU,
                                  &alert, sizeof(alert));
        }
    }

    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
