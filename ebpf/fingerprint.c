// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// fingerprint.c - Workload behavioral fingerprinting.
//
// Builds per-process behavioral feature vectors by tracking syscall
// frequency and network connection patterns.  Compares runtime behavior
// against known-good baselines and emits alerts when significant
// deviations are detected, indicating potential compromise or
// misconfiguration.

#include "headers/common.h"

/* ---- constants -------------------------------------------------------- */

#define NUM_SYSCALL_BUCKETS 32  /* group syscalls into 32 categories */
#define MAX_TRACKED_PROCS   8192

/* Drift alert thresholds */
#define DRIFT_SYSCALL_THRESHOLD  100  /* >100 calls in a new category */
#define DRIFT_PORT_THRESHOLD     5    /* >5 new destination ports */
#define DRIFT_IP_THRESHOLD       10   /* >10 new destination IPs */

/* Alert event types */
#define ALERT_NEW_SYSCALL_CAT   1
#define ALERT_NEW_DST_PORT      2
#define ALERT_NEW_DST_IP        3
#define ALERT_CONN_SPIKE        4

/* ---- structs ---------------------------------------------------------- */

struct behavior_profile {
    __u64 syscall_counts[NUM_SYSCALL_BUCKETS];
    __u64 unique_dst_ips;
    __u64 unique_dst_ports;
    __u64 total_bytes_out;
    __u64 total_connections;
    __u64 last_updated;
};

struct fingerprint_event {
    __u64 timestamp;
    __u32 pid;
    __u32 alert_type;
    __u64 current_value;
    __u64 baseline_value;
    char  comm[TASK_COMM_LEN];
};

/* ---- BPF maps --------------------------------------------------------- */

/* Per-process behavior profiles */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_TRACKED_PROCS);
    __type(key, __u32);    /* pid */
    __type(value, struct behavior_profile);
} process_profiles SEC(".maps");

/* Ring buffer for drift alert events */
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 64 * 1024);
} fingerprint_events SEC(".maps");

/* Known-good baseline profiles (populated from userspace after learning) */
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u32);    /* pid or process group id */
    __type(value, struct behavior_profile);
} baseline_profiles SEC(".maps");

/* Destination IP tracking per process: hash of (pid, dst_ip) -> count */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u64);    /* pid << 32 | dst_ip_hash */
    __type(value, __u64);  /* count */
} dst_ip_tracker SEC(".maps");

/* Destination port tracking per process: hash of (pid, dst_port) -> count */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u64);    /* pid << 32 | dst_port */
    __type(value, __u64);  /* count */
} dst_port_tracker SEC(".maps");

/* ---- helpers ---------------------------------------------------------- */

/* Map syscall number to one of 32 buckets/categories.
 * Categories roughly follow Linux syscall groupings:
 *  0: process mgmt (fork, execve, exit, wait)
 *  1: file I/O (read, write, open, close)
 *  2: file stat/metadata
 *  3: memory management (mmap, mprotect, brk)
 *  4: signal handling
 *  5: IPC (shmget, semget, msgget)
 *  6: network socket ops (socket, bind, listen, accept)
 *  7: network connect/send/recv
 *  8: filesystem ops (mount, stat, chmod)
 *  9: timer/clock ops
 * 10: epoll/poll/select
 * 11-31: other categories
 */
static __always_inline __u32 syscall_to_bucket(long syscall_nr)
{
    if (syscall_nr < 0)
        return 31;

    /* Syscall numbers differ per architecture (x86_64 legacy table vs the
     * asm-generic table used by arm64). */
    switch (syscall_nr) {
#if defined(__TARGET_ARCH_x86)
    /* File I/O: read, write, open, close, pread64, pwrite64, readv, writev, openat */
    case 0: case 1: case 2: case 3: case 17: case 18: case 19: case 20: case 257:
        return 1;
    /* Stat/metadata: stat, fstat, lstat, lseek, newfstatat */
    case 4: case 5: case 6: case 8: case 262:
        return 2;
    /* Memory: mmap, mprotect, munmap, brk, mremap, msync, mincore, madvise */
    case 9: case 10: case 11: case 12: case 25: case 26: case 27: case 28:
        return 3;
    /* Process: clone, fork, vfork, execve, exit, wait4, kill, exit_group */
    case 56: case 57: case 58: case 59: case 60: case 61: case 62: case 231:
        return 0;
    /* Signals/sleep: rt_sigaction, rt_sigprocmask, rt_sigreturn, pause, nanosleep */
    case 13: case 14: case 15: case 34: case 35:
        return 4;
    /* Socket ops: socket, accept, bind, listen, getsockname, getpeername,
     * socketpair, setsockopt, getsockopt, accept4 */
    case 41: case 43: case 49: case 50: case 51: case 52: case 53: case 54:
    case 55: case 288:
        return 6;
    /* Data transfer: connect, sendto, recvfrom, sendmsg, recvmsg, shutdown,
     * recvmmsg, sendmmsg */
    case 42: case 44: case 45: case 46: case 47: case 48: case 299: case 307:
        return 7;
    /* Filesystem: rename, mkdir, rmdir, creat, link, unlink, symlink, readlink, chmod */
    case 82: case 83: case 84: case 85: case 86: case 87: case 88: case 89: case 90:
        return 8;
    /* Timers: gettimeofday, clock_gettime, clock_getres, clock_nanosleep */
    case 96: case 228: case 229: case 230:
        return 9;
    /* poll/select/epoll: poll, select, ppoll, pselect6, epoll_wait, epoll_ctl, epoll_pwait */
    case 7: case 23: case 232: case 233: case 270: case 271: case 281:
        return 10;
    /* SysV IPC: shmget, shmat, shmctl, semget, semop, semctl, msgget, msgsnd, msgrcv, msgctl */
    case 29: case 30: case 31: case 64: case 65: case 66: case 67: case 68: case 69: case 70:
        return 5;
#elif defined(__TARGET_ARCH_arm64)
    /* File I/O: openat, close, lseek, read, write, readv, writev, pread64, pwrite64 */
    case 56: case 57: case 62: case 63: case 64: case 65: case 66: case 67: case 68:
        return 1;
    /* Stat/metadata: readlinkat, newfstatat, fstat */
    case 78: case 79: case 80:
        return 2;
    /* Memory: brk, munmap, mremap, mmap, mprotect, msync, mincore, madvise */
    case 214: case 215: case 216: case 222: case 226: case 227: case 232: case 233:
        return 3;
    /* Process: exit, exit_group, kill, clone, execve, wait4 */
    case 93: case 94: case 129: case 220: case 221: case 260:
        return 0;
    /* Signals/sleep: nanosleep, rt_sigaction, rt_sigprocmask, rt_sigreturn */
    case 101: case 134: case 135: case 139:
        return 4;
    /* Socket ops: socket, socketpair, bind, listen, accept, accept4, setsockopt, getsockopt */
    case 198: case 199: case 200: case 201: case 202: case 242: case 208: case 209:
        return 6;
    /* Data transfer: connect, sendto, recvfrom, shutdown, sendmsg, recvmsg */
    case 203: case 206: case 207: case 210: case 211: case 212:
        return 7;
    /* Filesystem: mkdirat, unlinkat, symlinkat, linkat, renameat, fchmodat */
    case 34: case 35: case 36: case 37: case 38: case 53:
        return 8;
    /* Timers: clock_gettime, clock_getres, clock_nanosleep, gettimeofday */
    case 113: case 114: case 115: case 169:
        return 9;
    /* epoll_ctl, epoll_pwait, pselect6, ppoll, epoll_pwait2 */
    case 21: case 22: case 72: case 73: case 441:
        return 10;
    /* SysV IPC: msgget, msgctl, msgrcv, msgsnd, semget, semctl, semop, shmget, shmctl, shmat */
    case 186: case 187: case 188: case 189: case 190: case 191: case 193: case 194:
    case 195: case 196:
        return 5;
#endif
    default:
        /* Hash remaining syscalls across the remaining buckets 11-31 */
        return 11 + ((__u32)syscall_nr % 21);
    }
}

static __always_inline void emit_drift_alert(__u32 pid, __u32 alert_type,
                                              __u64 current, __u64 baseline)
{
    struct fingerprint_event *ev;
    ev = bpf_ringbuf_reserve(&fingerprint_events, sizeof(*ev), 0);
    if (!ev)
        return;

    ev->timestamp      = bpf_ktime_get_ns();
    ev->pid            = pid;
    ev->alert_type     = alert_type;
    ev->current_value  = current;
    ev->baseline_value = baseline;
    bpf_get_current_comm(&ev->comm, sizeof(ev->comm));

    bpf_ringbuf_submit(ev, 0);
}

/* ---- raw tracepoint: syscall profiling -------------------------------- */

/* raw sys_enter: args[0] is a struct pt_regs *, args[1] is the syscall id. */
SEC("raw_tracepoint/sys_enter")
int fingerprint_syscall(struct bpf_raw_tracepoint_args *raw_ctx)
{
    long syscall_nr = (long)raw_ctx->args[1];

    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u32 pid = pid_tgid >> 32;

    __u32 bucket = syscall_to_bucket(syscall_nr);
    /* NUM_SYSCALL_BUCKETS is a power of two: the mask also gives the verifier
     * a provable bound for the array index below. */
    asm volatile("" : "+r"(bucket));   /* stop clang folding the mask away */
    bucket &= NUM_SYSCALL_BUCKETS - 1;

    /* Update process profile */
    struct behavior_profile *prof = bpf_map_lookup_elem(&process_profiles, &pid);
    if (prof) {
        __sync_fetch_and_add(&prof->syscall_counts[bucket], 1);
        prof->last_updated = bpf_ktime_get_ns();

        /* Check against baseline for drift */
        struct behavior_profile *baseline = bpf_map_lookup_elem(&baseline_profiles, &pid);
        if (baseline) {
            /* Alert if a previously unused syscall category now has significant activity */
            if (baseline->syscall_counts[bucket] == 0 &&
                prof->syscall_counts[bucket] >= DRIFT_SYSCALL_THRESHOLD) {
                emit_drift_alert(pid, ALERT_NEW_SYSCALL_CAT,
                                 prof->syscall_counts[bucket], 0);
            }
        }
    } else {
        struct behavior_profile new_prof = {};
        new_prof.syscall_counts[bucket] = 1;
        new_prof.last_updated = bpf_ktime_get_ns();
        bpf_map_update_elem(&process_profiles, &pid, &new_prof, BPF_NOEXIST);
    }

    return 0;
}

/* ---- kprobe: connection pattern profiling ----------------------------- */

/* tcp_v4_connect(struct sock *sk, struct sockaddr *uaddr, int addr_len)
 * The destination is not in sk yet at entry: take it from uaddr. */
SEC("kprobe/tcp_v4_connect")
int BPF_KPROBE(fingerprint_connect, struct sock *sk, struct sockaddr *uaddr)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u32 pid = pid_tgid >> 32;

    __u32 dst_ip = 0;
    __u16 dst_port = 0;
    if (gryvia_uaddr_v4(uaddr, &dst_ip, &dst_port))
        return 0;

    /* Update process profile */
    struct behavior_profile *prof = bpf_map_lookup_elem(&process_profiles, &pid);
    if (!prof) {
        struct behavior_profile new_prof = {};
        new_prof.last_updated = bpf_ktime_get_ns();
        bpf_map_update_elem(&process_profiles, &pid, &new_prof, BPF_NOEXIST);
        prof = bpf_map_lookup_elem(&process_profiles, &pid);
        if (!prof)
            return 0;
    }

    __sync_fetch_and_add(&prof->total_connections, 1);
    prof->last_updated = bpf_ktime_get_ns();

    /* Track unique destination IPs */
    __u64 ip_key = ((__u64)pid << 32) | (__u64)dst_ip;
    __u64 *ip_cnt = bpf_map_lookup_elem(&dst_ip_tracker, &ip_key);
    if (!ip_cnt) {
        __u64 one = 1;
        /* Only count a new IP if we won the insert (races with other CPUs). */
        if (!bpf_map_update_elem(&dst_ip_tracker, &ip_key, &one, BPF_NOEXIST))
            __sync_fetch_and_add(&prof->unique_dst_ips, 1);

        /* Check against baseline */
        struct behavior_profile *baseline = bpf_map_lookup_elem(&baseline_profiles, &pid);
        if (baseline && prof->unique_dst_ips > baseline->unique_dst_ips + DRIFT_IP_THRESHOLD) {
            emit_drift_alert(pid, ALERT_NEW_DST_IP,
                             prof->unique_dst_ips, baseline->unique_dst_ips);
        }
    } else {
        __sync_fetch_and_add(ip_cnt, 1);
    }

    /* Track unique destination ports */
    __u64 port_key = ((__u64)pid << 32) | (__u64)dst_port;
    __u64 *port_cnt = bpf_map_lookup_elem(&dst_port_tracker, &port_key);
    if (!port_cnt) {
        __u64 one = 1;
        if (!bpf_map_update_elem(&dst_port_tracker, &port_key, &one, BPF_NOEXIST))
            __sync_fetch_and_add(&prof->unique_dst_ports, 1);

        /* Check against baseline */
        struct behavior_profile *baseline = bpf_map_lookup_elem(&baseline_profiles, &pid);
        if (baseline && prof->unique_dst_ports > baseline->unique_dst_ports + DRIFT_PORT_THRESHOLD) {
            emit_drift_alert(pid, ALERT_NEW_DST_PORT,
                             prof->unique_dst_ports, baseline->unique_dst_ports);
        }
    } else {
        __sync_fetch_and_add(port_cnt, 1);
    }

    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
