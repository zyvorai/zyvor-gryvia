// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// privesc_monitor.c - Detect privilege escalation attempts.
//
// Monitors UID/GID transitions to root, credential changes, and
// capability acquisitions.  Uses raw_tracepoint/sys_enter for
// set*uid/set*gid/capset syscalls and kprobe/commit_creds for
// catching all credential changes at the kernel level.

#include "headers/common.h"
#include "headers/security_common.h"

/* Dangerous capabilities (CAP_NET_ADMIN/SYS_PTRACE/SYS_ADMIN are in gryvia_core.h) */
#ifndef CAP_DAC_OVERRIDE
#define CAP_DAC_OVERRIDE  1
#endif

/* Stat counter indices */
#define PRIVESC_CTR_SETUID     0
#define PRIVESC_CTR_SETGID     1
#define PRIVESC_CTR_CAPSET     2
#define PRIVESC_CTR_CRED_CHG   3
#define PRIVESC_CTR_ALERTS     4
#define PRIVESC_CTR_MAX        5

/* Pre-change credential snapshot */
struct cred_snapshot {
    __u32 uid;
    __u32 gid;
    __u64 timestamp;
};

/* ---- BPF maps --------------------------------------------------------- */

// Ring buffer for privilege escalation events.
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 64 * 1024);  /* 64 KB */
} privesc_events SEC(".maps");

// Per-PID credential tracking (stores pre-change UID/GID).
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);  /* entries are never deleted */
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u64);  /* pid_tgid */
    __type(value, struct cred_snapshot);
} cred_tracking SEC(".maps");

// Detection counters.
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, PRIVESC_CTR_MAX);
    __type(key, __u32);
    __type(value, __u64);
} privesc_stats SEC(".maps");

/* ---- helpers ---------------------------------------------------------- */

static __always_inline void bump_privesc_counter(__u32 idx)
{
    __u64 *cnt = bpf_map_lookup_elem(&privesc_stats, &idx);
    if (cnt)
        __sync_fetch_and_add(cnt, 1);
}

static __always_inline void emit_privesc_event(__u16 syscall_nr, __u8 severity,
                                               __u32 old_uid, __u32 new_uid)
{
    struct security_event *evt;

    evt = bpf_ringbuf_reserve(&privesc_events, sizeof(*evt), 0);
    if (!evt)
        return;

    sec_event_init(evt, SEC_PRIVILEGE_ESCALATION, severity);
    evt->syscall_nr = syscall_nr;
    evt->old_uid    = old_uid;
    evt->new_uid    = new_uid;

    bpf_ringbuf_submit(evt, 0);
}

/* Non-root -> root transition requested through a set*id syscall. */
static __always_inline void check_root_target(__u32 target, __u32 current_id,
                                              int ctr, __u16 nr, __u8 severity,
                                              __u32 current_uid)
{
    if (target == 0 && current_id != 0) {
        bump_privesc_counter(ctr);
        bump_privesc_counter(PRIVESC_CTR_ALERTS);
        emit_privesc_event(nr, severity, current_uid, target);
    }
}

/* ---- raw tracepoint on sys_enter -------------------------------------- */

/* ctx->args[0] is the syscall's struct pt_regs *, ctx->args[1] the id. */
SEC("raw_tracepoint/sys_enter")
int privesc_syscall_monitor(struct bpf_raw_tracepoint_args *ctx)
{
    unsigned long regs = ctx->args[0];
    long nr = (long)ctx->args[1];
    __u16 sysnr = (__u16)nr;

    __u64 uid_gid = bpf_get_current_uid_gid();
    __u32 uid = (__u32)uid_gid;
    __u32 gid = (__u32)(uid_gid >> 32);

    switch (nr) {
    case GRYVIA_NR_setuid: {
        /* setuid(uid) */
        __u64 pid_tgid = bpf_get_current_pid_tgid();
        struct cred_snapshot snap = {};

        check_root_target((__u32)sec_sysarg(regs, 0), uid,
                          PRIVESC_CTR_SETUID, sysnr, SEC_SEV_CRITICAL, uid);

        /* Store pre-change snapshot for correlation. */
        snap.uid = uid;
        snap.gid = gid;
        snap.timestamp = bpf_ktime_get_ns();
        bpf_map_update_elem(&cred_tracking, &pid_tgid, &snap, BPF_ANY);
        break;
    }

    case GRYVIA_NR_setgid:
        check_root_target((__u32)sec_sysarg(regs, 0), gid,
                          PRIVESC_CTR_SETGID, sysnr, SEC_SEV_HIGH, uid);
        break;

    case GRYVIA_NR_setreuid:
        /* setreuid(ruid, euid): either may become root */
        check_root_target((__u32)sec_sysarg(regs, 1), uid,
                          PRIVESC_CTR_SETUID, sysnr, SEC_SEV_CRITICAL, uid);
        break;

    case GRYVIA_NR_setregid:
        check_root_target((__u32)sec_sysarg(regs, 1), gid,
                          PRIVESC_CTR_SETGID, sysnr, SEC_SEV_HIGH, uid);
        break;

    case GRYVIA_NR_setresuid:
        /* setresuid(ruid, euid, suid): check euid */
        check_root_target((__u32)sec_sysarg(regs, 1), uid,
                          PRIVESC_CTR_SETUID, sysnr, SEC_SEV_CRITICAL, uid);
        break;

    case GRYVIA_NR_setresgid:
        check_root_target((__u32)sec_sysarg(regs, 1), gid,
                          PRIVESC_CTR_SETGID, sysnr, SEC_SEV_HIGH, uid);
        break;

    case GRYVIA_NR_capset:
        /* The capability header/data live in user memory; flag the call and
         * let userspace correlate. */
        bump_privesc_counter(PRIVESC_CTR_CAPSET);
        bump_privesc_counter(PRIVESC_CTR_ALERTS);
        emit_privesc_event(sysnr, SEC_SEV_HIGH, uid, uid);
        break;

    default:
        break;
    }

    return 0;
}

/* ---- kprobe/commit_creds ---------------------------------------------- */

/*
 * commit_creds(struct cred *new) applies new credentials to the current
 * task, so it sees every change, including setuid binaries and capability
 * inheritance.  The task's current credentials are still the OLD ones at
 * kprobe time, which lets us alert only on real transitions:
 *   - euid non-root -> root
 *   - dangerous effective capabilities that were NOT held before
 */
SEC("kprobe/commit_creds")
int BPF_KPROBE(privesc_commit_creds, struct cred *new_cred)
{
    struct task_struct *task = (struct task_struct *)bpf_get_current_task();
    const struct cred *old_cred = BPF_CORE_READ(task, cred);
    __u32 old_euid = BPF_CORE_READ(old_cred, euid.val);
    __u32 new_euid = BPF_CORE_READ(new_cred, euid.val);
    __u64 old_caps = 0, new_caps = 0;

    if (new_euid == 0 && old_euid != 0) {
        bump_privesc_counter(PRIVESC_CTR_CRED_CHG);
        bump_privesc_counter(PRIVESC_CTR_ALERTS);
        emit_privesc_event(0 /* no specific syscall */, SEC_SEV_CRITICAL,
                           old_euid, new_euid);
    }

    BPF_CORE_READ_INTO(&old_caps, old_cred, cap_effective);
    BPF_CORE_READ_INTO(&new_caps, new_cred, cap_effective);

    __u64 dangerous = (1ULL << CAP_SYS_ADMIN) | (1ULL << CAP_NET_ADMIN) |
                      (1ULL << CAP_SYS_PTRACE) | (1ULL << CAP_DAC_OVERRIDE);

    if ((new_caps & ~old_caps) & dangerous) {
        /* Only alert in a container context; cgroup id 1 is the root. */
        if (bpf_get_current_cgroup_id() > 1) {
            bump_privesc_counter(PRIVESC_CTR_CRED_CHG);
            emit_privesc_event(0, SEC_SEV_HIGH, old_euid, new_euid);
        }
    }

    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
