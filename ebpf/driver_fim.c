// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// driver_fim.c - GPU driver file integrity monitoring.
//
// Monitors write access to NVIDIA driver files, CUDA libraries, and
// kernel module loading.  Uses kprobe/security_file_open for file-level
// monitoring and raw_tracepoint/sys_enter for init_module/finit_module
// syscall detection.

#include "headers/common.h"
#include "headers/security_common.h"

#define NAME_LEN 64

/* Stat counter indices */
#define FIM_CTR_FILE_WRITE  0
#define FIM_CTR_MOD_LOAD    1
#define FIM_CTR_ALERTS      2
#define FIM_CTR_MAX         3

/* ---- BPF maps --------------------------------------------------------- */

// Ring buffer for FIM events.
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 64 * 1024);  /* 64 KB */
} fim_events SEC(".maps");

// Configurable watched path patterns (populated from userspace).
// Key: index, Value: null-terminated path prefix string.
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 64);
    __type(key, __u32);
    __type(value, char[MAX_PATH_LEN]);
} watched_paths SEC(".maps");

// Detection counters.
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, FIM_CTR_MAX);
    __type(key, __u32);
    __type(value, __u64);
} fim_stats SEC(".maps");

/* ---- helpers ---------------------------------------------------------- */

static __always_inline void bump_fim_counter(__u32 idx)
{
    __u64 *cnt = bpf_map_lookup_elem(&fim_stats, &idx);
    if (cnt)
        __sync_fetch_and_add(cnt, 1);
}

/* nvidia*, libnvidia*, libcuda*: device nodes, driver and CUDA libraries. */
static __always_inline int matches_driver_pattern(const char *name)
{
    return SEC_STR_PREFIX(name, "nvidia") ||
           SEC_STR_PREFIX(name, "libnvidia") ||
           SEC_STR_PREFIX(name, "libcuda");
}

/* Kernel module files: *.ko, *.ko.xz, *.ko.zst, *.ko.gz (bounded scan). */
static __always_inline int matches_module_pattern(const char *name)
{
    for (int i = 0; i < NAME_LEN - 4; i++) {
        if (name[i] == '\0')
            return 0;
        if (name[i] == '.' && name[i + 1] == 'k' && name[i + 2] == 'o' &&
            (name[i + 3] == '\0' || name[i + 3] == '.'))
            return 1;
    }
    return 0;
}

/*
 * modprobe.d style config: only the dentry name is visible (not the parent
 * directory), so flag the conventional "blacklist-*" file names.
 */
static __always_inline int matches_modprobe_config(const char *name)
{
    return SEC_STR_PREFIX(name, "blacklist");
}

/* ---- kprobe/security_file_open ---------------------------------------- */

/*
 * Monitor file opens with write intent targeting GPU driver files.
 *
 * security_file_open(struct file *file)
 */
SEC("kprobe/security_file_open")
int BPF_KPROBE(fim_file_open, struct file *filp)
{
    /* Write intent: FMODE_WRITE, or an access mode other than O_RDONLY. */
    unsigned int f_mode = BPF_CORE_READ(filp, f_mode);
    unsigned int f_flags = BPF_CORE_READ(filp, f_flags);

    if (!(f_mode & FMODE_WRITE) && !(f_flags & O_ACCMODE))
        return 0;

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

    if (matches_driver_pattern(buf) || matches_module_pattern(buf)) {
        suspicious = 1;
        severity = SEC_SEV_CRITICAL;
    } else if (matches_modprobe_config(buf)) {
        suspicious = 1;
    }

    if (!suspicious)
        return 0;

    bump_fim_counter(FIM_CTR_FILE_WRITE);
    bump_fim_counter(FIM_CTR_ALERTS);

    struct security_event *evt;
    evt = bpf_ringbuf_reserve(&fim_events, sizeof(*evt), 0);
    if (!evt)
        return 0;

    sec_event_init(evt, SEC_DRIVER_TAMPERING, severity);
    __builtin_memcpy(evt->path, buf, sizeof(buf));

    bpf_ringbuf_submit(evt, 0);

    return 0;
}

/* ---- raw tracepoint on sys_enter for module loading ------------------- */

/* ctx->args[1] is the syscall id (args[0] is the pt_regs pointer). */
SEC("raw_tracepoint/sys_enter")
int fim_module_monitor(struct bpf_raw_tracepoint_args *ctx)
{
    long nr = (long)ctx->args[1];

    /* init_module, finit_module and delete_module (rmmod nvidia). */
    if (nr != GRYVIA_NR_init_module && nr != GRYVIA_NR_finit_module &&
        nr != GRYVIA_NR_delete_module)
        return 0;

    bump_fim_counter(FIM_CTR_MOD_LOAD);
    bump_fim_counter(FIM_CTR_ALERTS);

    struct security_event *evt;
    evt = bpf_ringbuf_reserve(&fim_events, sizeof(*evt), 0);
    if (!evt)
        return 0;

    sec_event_init(evt, SEC_DRIVER_TAMPERING, SEC_SEV_HIGH);
    evt->syscall_nr = (__u16)nr;

    bpf_ringbuf_submit(evt, 0);

    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
