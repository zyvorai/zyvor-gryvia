// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// gpu_mem_trace.c - GPU memory transfer monitoring via uprobes.
//
// Hooks CUDA runtime functions (cudaMemcpy, cudaMemcpyAsync, cudaMalloc,
// cudaFree, cudaLaunchKernel, cudaDeviceSynchronize) to trace GPU memory
// operations.  Each transfer emits a gpu_event to a ring buffer with
// direction, size, and latency information.

#include "headers/gpu_common.h"

/* ---- constants --------------------------------------------------------- */

#define MAX_INFLIGHT   65536
#define RINGBUF_SIZE   (256 * 1024)
#define MAX_ALLOCS     65536
#define MEM_DIR_SLOTS  4      /* H2D, D2H, D2D, PEER */
#define TASK_COMM_LEN  16

/* cudaMemcpyKind enum values from CUDA runtime */
#define CUDA_MEMCPY_H2H  0
#define CUDA_MEMCPY_H2D  1
#define CUDA_MEMCPY_D2H  2
#define CUDA_MEMCPY_D2D  3
#define CUDA_MEMCPY_DEFAULT 4

/* ---- per-direction byte counters --------------------------------------- */

struct mem_dir_stat {
    __u64 count;
    __u64 total_bytes;
};

/* ---- allocation tracking ----------------------------------------------- */

// Key includes the tgid: device virtual addresses are per process.
struct alloc_key {
    __u32 tgid;
    __u32 _pad;
    __u64 ptr;
};

struct alloc_info {
    __u64 size;
    __u64 alloc_ns;
};

/* ---- BPF maps ---------------------------------------------------------- */

// In-flight cudaMemcpy*/cudaMalloc calls keyed by pid_tgid.  LRU: a thread
// that dies (or a missed uretprobe) between entry and exit would otherwise
// leak its slot forever.
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_INFLIGHT);
    __type(key, __u64);
    __type(value, struct cuda_mem_inflight);
} cuda_inflight_map SEC(".maps");

// In-flight cudaDeviceSynchronize calls (separate: a sync must not consume
// or clobber a memcpy entry of the same thread).
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_INFLIGHT);
    __type(key, __u64);
    __type(value, __u64);
} cuda_sync_map SEC(".maps");

// Ring buffer for gpu_event emission to userspace.
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, RINGBUF_SIZE);
} cuda_events SEC(".maps");

GRYVIA_DECLARE_DROPS();

// Per-CPU memory direction byte counters (one per mem_direction).
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, MEM_DIR_SLOTS);
    __type(key, __u32);
    __type(value, struct mem_dir_stat);
} cuda_mem_stats SEC(".maps");

// Live GPU allocations: (tgid, device pointer) -> size.  LRU so allocations
// of processes that exit without cudaFree are eventually recycled.
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ALLOCS);
    __type(key, struct alloc_key);
    __type(value, struct alloc_info);
} cuda_alloc_map SEC(".maps");

/* ---- helpers ----------------------------------------------------------- */

static __always_inline __u8 cuda_kind_to_direction(__u32 kind)
{
    switch (kind) {
    case CUDA_MEMCPY_H2D:
        return MEM_H2D;
    case CUDA_MEMCPY_D2H:
        return MEM_D2H;
    case CUDA_MEMCPY_D2D:
        return MEM_D2D;
    case CUDA_MEMCPY_H2H:
        return 0xff;          /* host-only copy: not GPU traffic */
    default:
        return MEM_UNKNOWN;   /* cudaMemcpyDefault */
    }
}

static __always_inline void emit_cuda_event(__u8 event_type, __u8 direction,
                                            __u64 bytes, __u64 latency_ns)
{
    struct gpu_event *ev = bpf_ringbuf_reserve(&cuda_events,
                                               sizeof(struct gpu_event), 0);
    if (!ev) {
    	GRYVIA_COUNT_DROP(GRYVIA_DROP_RINGBUF);
    	return;
    }

    __builtin_memset(ev, 0, sizeof(*ev));
    ev->timestamp  = bpf_ktime_get_ns();
    ev->pid        = bpf_get_current_pid_tgid() >> 32;
    ev->event_type = event_type;
    ev->direction  = direction;
    ev->bytes      = bytes;
    ev->latency_ns = latency_ns;
    bpf_get_current_comm(&ev->comm, sizeof(ev->comm));

    bpf_ringbuf_submit(ev, 0);
}

static __always_inline void update_mem_stats(__u8 direction, __u64 bytes)
{
    __u32 key = direction;
    if (key >= MEM_DIR_SLOTS)
        return;

    struct mem_dir_stat *stat = bpf_map_lookup_elem(&cuda_mem_stats, &key);
    if (stat) {
        __sync_fetch_and_add(&stat->count, 1);
        __sync_fetch_and_add(&stat->total_bytes, bytes);
    }
}

static __always_inline void memcpy_entry(__u64 count, __u32 kind)
{
    __u8 dir = cuda_kind_to_direction(kind);
    if (dir == 0xff)
        return;

    __u64 pid_tgid = bpf_get_current_pid_tgid();
    struct cuda_mem_inflight info = {};
    info.start_ns  = bpf_ktime_get_ns();
    info.size      = count;
    info.direction = dir;
    bpf_map_update_elem(&cuda_inflight_map, &pid_tgid, &info, BPF_ANY);
}

static __always_inline void memcpy_exit(void)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    struct cuda_mem_inflight *info = bpf_map_lookup_elem(&cuda_inflight_map,
                                                         &pid_tgid);
    if (!info)
        return;

    /* Copy out before deleting: the map value dies with the element. */
    __u64 latency = bpf_ktime_get_ns() - info->start_ns;
    __u64 size = info->size;
    __u8 dir = info->direction;
    bpf_map_delete_elem(&cuda_inflight_map, &pid_tgid);

    emit_cuda_event(GPU_EVT_MEM_TRANSFER, dir, size, latency);
    update_mem_stats(dir, size);
}

/* ---- uprobes: cudaMemcpy / cudaMemcpyAsync ------------------------------ */

// cudaMemcpy(void *dst, const void *src, size_t count,
//            enum cudaMemcpyKind kind)
// The async variant only measures the enqueue time, not the DMA itself.
SEC("uprobe/cudaMemcpy")
int BPF_UPROBE(cuda_memcpy_entry, void *dst, void *src, __u64 count,
               __u32 kind)
{
    memcpy_entry(count, kind);
    return 0;
}

SEC("uretprobe/cudaMemcpy")
int BPF_URETPROBE(cuda_memcpy_exit)
{
    memcpy_exit();
    return 0;
}

// cudaMemcpyAsync(void *dst, const void *src, size_t count,
//                 enum cudaMemcpyKind kind, cudaStream_t stream)
SEC("uprobe/cudaMemcpyAsync")
int BPF_UPROBE(cuda_memcpy_async_entry, void *dst, void *src, __u64 count,
               __u32 kind)
{
    memcpy_entry(count, kind);
    return 0;
}

SEC("uretprobe/cudaMemcpyAsync")
int BPF_URETPROBE(cuda_memcpy_async_exit)
{
    memcpy_exit();
    return 0;
}

/* ---- uprobes: cudaMalloc / cudaFree ------------------------------------- */

// cudaMalloc(void **devPtr, size_t size): the pointer is only written on
// return, so stash devPtr at entry and read *devPtr at exit.
SEC("uprobe/cudaMalloc")
int BPF_UPROBE(cuda_malloc_entry, void **devPtr, __u64 size)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    struct cuda_mem_inflight info = {};
    info.start_ns = bpf_ktime_get_ns();
    info.size     = size;
    info.aux      = (__u64)devPtr;
    bpf_map_update_elem(&cuda_inflight_map, &pid_tgid, &info, BPF_ANY);
    return 0;
}

SEC("uretprobe/cudaMalloc")
int BPF_URETPROBE(cuda_malloc_exit, int ret)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    struct cuda_mem_inflight *info = bpf_map_lookup_elem(&cuda_inflight_map,
                                                         &pid_tgid);
    if (!info)
        return 0;

    __u64 size = info->size;
    __u64 latency = bpf_ktime_get_ns() - info->start_ns;
    void **devPtr = (void **)info->aux;
    __u64 start_ns = info->start_ns;
    bpf_map_delete_elem(&cuda_inflight_map, &pid_tgid);

    if (ret != 0)   /* cudaSuccess == 0 */
        return 0;

    void *ptr = NULL;
    if (bpf_probe_read_user(&ptr, sizeof(ptr), devPtr) || !ptr)
        return 0;

    struct alloc_key key = {};
    key.tgid = pid_tgid >> 32;
    key.ptr  = (__u64)ptr;
    struct alloc_info ai = {};
    ai.size     = size;
    ai.alloc_ns = start_ns;
    bpf_map_update_elem(&cuda_alloc_map, &key, &ai, BPF_ANY);

    emit_cuda_event(GPU_EVT_CUDA_ALLOC, 0, size, latency);
    return 0;
}

// cudaFree(void *devPtr)
SEC("uprobe/cudaFree")
int BPF_UPROBE(cuda_free_entry, void *devPtr)
{
    struct alloc_key key = {};
    key.tgid = bpf_get_current_pid_tgid() >> 32;
    key.ptr  = (__u64)devPtr;

    struct alloc_info *ai = bpf_map_lookup_elem(&cuda_alloc_map, &key);
    if (!ai)
        return 0;   /* not tracked (allocated before attach, or cudaFree(NULL)) */

    __u64 size = ai->size;
    bpf_map_delete_elem(&cuda_alloc_map, &key);
    emit_cuda_event(GPU_EVT_CUDA_ALLOC, 1, size, 0);
    return 0;
}

/* ---- uprobe: cudaLaunchKernel ------------------------------------------ */

// cudaLaunchKernel(const void *func, dim3 gridDim, dim3 blockDim,
//                  void **args, size_t sharedMem, cudaStream_t stream)
// Launches are fire-and-forget: emit a marker only (no in-flight state, there
// is no exit-side consumer).
SEC("uprobe/cudaLaunchKernel")
int BPF_UPROBE(cuda_launch_kernel_entry)
{
    emit_cuda_event(GPU_EVT_CUDA_LAUNCH, 0, 0, 0);
    return 0;
}

/* ---- uprobes: cudaDeviceSynchronize ------------------------------------ */

// cudaDeviceSynchronize(void): latency = how long the host blocked.
SEC("uprobe/cudaDeviceSynchronize")
int BPF_UPROBE(cuda_device_sync_entry)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u64 now = bpf_ktime_get_ns();
    bpf_map_update_elem(&cuda_sync_map, &pid_tgid, &now, BPF_ANY);
    return 0;
}

SEC("uretprobe/cudaDeviceSynchronize")
int BPF_URETPROBE(cuda_device_sync_exit)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    __u64 *start = bpf_map_lookup_elem(&cuda_sync_map, &pid_tgid);
    if (!start)
        return 0;

    __u64 latency = bpf_ktime_get_ns() - *start;
    bpf_map_delete_elem(&cuda_sync_map, &pid_tgid);

    emit_cuda_event(GPU_EVT_CUDA_SYNC, 0, 0, latency);
    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
