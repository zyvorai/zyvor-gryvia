/* SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause */
#ifndef __SECURITY_COMMON_H__
#define __SECURITY_COMMON_H__

#include "gryvia_core.h"

#define MAX_PATH_LEN 256
#define CGROUP_PATH_LEN 128

/* Security event severity */
enum sec_severity {
    SEC_SEV_INFO = 0,
    SEC_SEV_LOW = 1,
    SEC_SEV_MEDIUM = 2,
    SEC_SEV_HIGH = 3,
    SEC_SEV_CRITICAL = 4,
};

/* Security event types */
enum sec_event_type {
    SEC_CONTAINER_ESCAPE = 1,
    SEC_PRIVILEGE_ESCALATION = 2,
    SEC_CRYPTO_MINING = 3,
    SEC_DATA_EXFILTRATION = 4,
    SEC_DRIVER_TAMPERING = 5,
    SEC_SUSPICIOUS_EXEC = 6,
    SEC_NAMESPACE_BREACH = 7,
};

/* Security event structure */
struct security_event {
    __u64 timestamp;
    __u32 pid;
    __u32 uid;
    __u32 gid;
    __u8  event_type;    /* sec_event_type */
    __u8  severity;      /* sec_severity */
    __u16 syscall_nr;
    __u64 cgroup_id;
    char  comm[16];
    char  path[MAX_PATH_LEN];
    __u32 src_ip;
    __u32 dst_ip;
    __u16 dst_port;
    __u16 _pad;
    __u32 _pad2;         /* explicit: bytes is 8-aligned (offset 320) */
    __u64 bytes;         /* for exfiltration tracking */
    __u32 old_uid;       /* for privesc: UID before change */
    __u32 new_uid;       /* for privesc: UID after change */
};

/*
 * Wire layout shared with collector/pkg/decoder/security_decoder.go
 * (SecurityEvent, 336 bytes).  Keep both sides and the Go layout test in sync.
 */
_Static_assert(__builtin_offsetof(struct security_event, event_type) == 20, "event_type");
_Static_assert(__builtin_offsetof(struct security_event, cgroup_id) == 24, "cgroup_id");
_Static_assert(__builtin_offsetof(struct security_event, comm) == 32, "comm");
_Static_assert(__builtin_offsetof(struct security_event, path) == 48, "path");
_Static_assert(__builtin_offsetof(struct security_event, src_ip) == 304, "src_ip");
_Static_assert(__builtin_offsetof(struct security_event, bytes) == 320, "bytes");
_Static_assert(__builtin_offsetof(struct security_event, old_uid) == 328, "old_uid");
_Static_assert(sizeof(struct security_event) == 336, "security_event size");

/* Zero an event and fill the fields every detector sets. */
static __always_inline void sec_event_init(struct security_event *evt,
                                           __u8 type, __u8 severity)
{
    __u64 uid_gid = bpf_get_current_uid_gid();

    __builtin_memset(evt, 0, sizeof(*evt));
    evt->timestamp  = bpf_ktime_get_ns();
    evt->pid        = bpf_get_current_pid_tgid() >> 32;
    evt->uid        = (__u32)uid_gid;
    evt->gid        = (__u32)(uid_gid >> 32);
    evt->event_type = type;
    evt->severity   = severity;
    evt->cgroup_id  = bpf_get_current_cgroup_id();
    bpf_get_current_comm(&evt->comm, sizeof(evt->comm));
}

/*
 * Byte-wise literal comparison that the verifier accepts (no memcmp call).
 * SEC_STR_EQ matches the whole string, SEC_STR_PREFIX only the literal.
 */
static __always_inline int sec_match(const char *s, const char *lit, int n)
{
#pragma unroll
    for (int i = 0; i < n; i++) {
        if (s[i] != lit[i])
            return 0;
    }
    return 1;
}
#define SEC_STR_EQ(s, lit)     sec_match((s), (lit), (int)sizeof(lit))
#define SEC_STR_PREFIX(s, lit) sec_match((s), (lit), (int)sizeof(lit) - 1)

/*
 * Argument n (0-based, n < 3) of the syscall described by the pt_regs pointer
 * that raw_tracepoint/sys_enter passes as ctx->args[0].  The syscall id is
 * ctx->args[1].
 */
static __always_inline unsigned long sec_sysarg(unsigned long regs_ptr, int n)
{
#if defined(__TARGET_ARCH_x86)
    const struct pt_regs *r = (const struct pt_regs *)regs_ptr;

    if (n == 0)
        return BPF_CORE_READ(r, di);
    if (n == 1)
        return BPF_CORE_READ(r, si);
    return BPF_CORE_READ(r, dx);
#else
    const struct user_pt_regs *r = (const struct user_pt_regs *)regs_ptr;
    __u64 v = 0;

    if (n == 0)
        bpf_core_read(&v, sizeof(v), &r->regs[0]);
    else if (n == 1)
        bpf_core_read(&v, sizeof(v), &r->regs[1]);
    else
        bpf_core_read(&v, sizeof(v), &r->regs[2]);
    return v;
#endif
}

#endif /* __SECURITY_COMMON_H__ */
