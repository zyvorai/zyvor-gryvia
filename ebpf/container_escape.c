// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// container_escape.c - Detect container escape attempts via syscall and
//                      file-access monitoring.
//
// Hooks raw_tracepoint/sys_enter for suspicious syscalls (unshare, setns,
// mount, pivot_root, ptrace, execve) and kprobe/security_file_open for
// access to sensitive host paths from container context.

#include "headers/common.h"
#include "headers/security_common.h"

/* Clone flags used for namespace manipulation */
#define CLONE_NEWNS   0x00020000
#define CLONE_NEWUSER 0x10000000

/* ptrace request codes */
#define PTRACE_ATTACH 16
#define PTRACE_SEIZE  0x4206

/* Escape counter indices */
#define ESCAPE_CTR_UNSHARE     0
#define ESCAPE_CTR_SETNS       1
#define ESCAPE_CTR_MOUNT       2
#define ESCAPE_CTR_PIVOT       3
#define ESCAPE_CTR_PTRACE      4
#define ESCAPE_CTR_EXECVE      5
#define ESCAPE_CTR_FILE_OPEN   6
#define ESCAPE_CTR_MAX         7

/* ---- BPF maps --------------------------------------------------------- */

// Ring buffer for emitting security events.
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 128 * 1024);  /* 128 KB */
} escape_events SEC(".maps");

// Hash of known container PIDs (populated from userspace via cgroup scanning).
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u32);   /* PID */
    __type(value, __u64); /* cgroup_id */
} container_pids SEC(".maps");

// Configurable syscall watch list (index -> syscall number).
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 32);
    __type(key, __u32);
    __type(value, __u32);
} sensitive_syscalls SEC(".maps");

// Per-type escape attempt counters.
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, ESCAPE_CTR_MAX);
    __type(key, __u32);
    __type(value, __u64);
} escape_counter SEC(".maps");

/* ---- helpers ---------------------------------------------------------- */

static __always_inline int is_container_pid(__u32 pid)
{
    return bpf_map_lookup_elem(&container_pids, &pid) != NULL;
}

static __always_inline void bump_counter(__u32 idx)
{
    __u64 *cnt = bpf_map_lookup_elem(&escape_counter, &idx);
    if (cnt)
        __sync_fetch_and_add(cnt, 1);
}

static __always_inline void emit_escape_event(__u16 syscall_nr, __u8 severity)
{
    struct security_event *evt;

    evt = bpf_ringbuf_reserve(&escape_events, sizeof(*evt), 0);
    if (!evt)
        return;

    sec_event_init(evt, SEC_CONTAINER_ESCAPE, severity);
    evt->syscall_nr = syscall_nr;

    bpf_ringbuf_submit(evt, 0);
}

/* ---- raw tracepoint on sys_enter -------------------------------------- */

/*
 * raw_tracepoint/sys_enter: ctx->args[0] is the struct pt_regs * of the
 * syscall, ctx->args[1] is the syscall id.
 */
SEC("raw_tracepoint/sys_enter")
int escape_syscall_monitor(struct bpf_raw_tracepoint_args *ctx)
{
    unsigned long regs = ctx->args[0];
    long nr = (long)ctx->args[1];
    __u32 pid = bpf_get_current_pid_tgid() >> 32;

    /* Only care about container processes. */
    if (!is_container_pid(pid))
        return 0;

    switch (nr) {
    case GRYVIA_NR_unshare:
        /* unshare(CLONE_NEWNS | CLONE_NEWUSER) - namespace manipulation */
        if (sec_sysarg(regs, 0) & (CLONE_NEWNS | CLONE_NEWUSER)) {
            bump_counter(ESCAPE_CTR_UNSHARE);
            emit_escape_event((__u16)nr, SEC_SEV_CRITICAL);
        }
        break;

    case GRYVIA_NR_setns:
        /* Any setns from a container is suspicious. */
        bump_counter(ESCAPE_CTR_SETNS);
        emit_escape_event((__u16)nr, SEC_SEV_CRITICAL);
        break;

    case GRYVIA_NR_mount:
        bump_counter(ESCAPE_CTR_MOUNT);
        emit_escape_event((__u16)nr, SEC_SEV_HIGH);
        break;

    case GRYVIA_NR_pivot_root:
        bump_counter(ESCAPE_CTR_PIVOT);
        emit_escape_event((__u16)nr, SEC_SEV_CRITICAL);
        break;

    case GRYVIA_NR_ptrace: {
        /* ptrace(request, pid, ...): attaching outside the container. */
        unsigned long req = sec_sysarg(regs, 0);

        if (req == PTRACE_ATTACH || req == PTRACE_SEIZE) {
            __u32 tpid = (__u32)sec_sysarg(regs, 1);

            if (!is_container_pid(tpid)) {
                bump_counter(ESCAPE_CTR_PTRACE);
                emit_escape_event((__u16)nr, SEC_SEV_CRITICAL);
            }
        }
        break;
    }

    case GRYVIA_NR_execve:
        bump_counter(ESCAPE_CTR_EXECVE);
        emit_escape_event((__u16)nr, SEC_SEV_MEDIUM);
        break;

    default:
        break;
    }

    return 0;
}

/* ---- kprobe on security_file_open ------------------------------------- */

/*
 * security_file_open is called on every file open.  We look for access to
 * paths that indicate a container escape attempt:
 *   - /proc/1/ns/mnt, /proc/1/ns/pid, /proc/1/ns/net
 *   - /proc/sys/kernel/core_pattern  (classic escape vector)
 *   - /dev/nvidia* or other host device files from container context
 */

#define NAME_LEN 64

SEC("kprobe/security_file_open")
int BPF_KPROBE(escape_file_open, struct file *filp)
{
    __u32 pid = bpf_get_current_pid_tgid() >> 32;

    if (!is_container_pid(pid))
        return 0;

    /* Short dentry name (file->f_path.dentry->d_name.name) and the name of
     * its parent directory. */
    struct dentry *dentry = BPF_CORE_READ(filp, f_path.dentry);
    if (!dentry)
        return 0;

    const unsigned char *name = BPF_CORE_READ(dentry, d_name.name);
    if (!name)
        return 0;

    char buf[NAME_LEN] = {};
    bpf_probe_read_kernel_str(buf, sizeof(buf), name);

    int suspicious = 0;
    __u8 severity = SEC_SEV_HIGH;

    if (SEC_STR_EQ(buf, "mnt") || SEC_STR_EQ(buf, "pid") ||
        SEC_STR_EQ(buf, "net")) {
        /* Namespace handle: only /proc/<pid>/ns/{mnt,pid,net}.  Require
         * the parent directory to be "ns" so ordinary files that happen to
         * be called "net" do not alert. */
        struct dentry *parent = BPF_CORE_READ(dentry, d_parent);
        const unsigned char *pname =
            parent ? BPF_CORE_READ(parent, d_name.name) : NULL;
        char pbuf[4] = {};

        if (pname)
            bpf_probe_read_kernel_str(pbuf, sizeof(pbuf), pname);
        if (SEC_STR_EQ(pbuf, "ns")) {
            suspicious = 1;
            severity = SEC_SEV_CRITICAL;
        }
    } else if (SEC_STR_EQ(buf, "core_pattern")) {
        /* classic container escape via /proc/sys/kernel/core_pattern */
        suspicious = 1;
        severity = SEC_SEV_CRITICAL;
    } else if (SEC_STR_PREFIX(buf, "nvidia")) {
        /* host GPU device nodes */
        suspicious = 1;
    }

    if (!suspicious)
        return 0;

    bump_counter(ESCAPE_CTR_FILE_OPEN);

    struct security_event *evt;
    evt = bpf_ringbuf_reserve(&escape_events, sizeof(*evt), 0);
    if (!evt)
        return 0;

    sec_event_init(evt, SEC_CONTAINER_ESCAPE, severity);
    __builtin_memcpy(evt->path, buf, sizeof(buf));

    bpf_ringbuf_submit(evt, 0);

    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
