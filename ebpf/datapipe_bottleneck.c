// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// datapipe_bottleneck.c - Data pipeline bottleneck detection for AI training.
//
// Correlates storage I/O completions (block layer), network data ingestion
// (TCP), and GPU compute phases (CUDA kernel launch/sync) to detect
// bottlenecks in the training data pipeline.  When the GPU is idle
// waiting for data, the program emits bottleneck alert events.

#include "headers/gpu_common.h"

/* ---- constants --------------------------------------------------------- */

#define MAX_INFLIGHT   65536
#define RINGBUF_SIZE   (256 * 1024)
#define IO_STAT_SLOTS  4
#define NET_STAT_SLOTS 2
#define TASK_COMM_LEN  16

/* ---- I/O statistics ---------------------------------------------------- */

struct io_stat {
    __u64 total_bytes;
    __u64 total_ops;
    __u64 total_latency_ns;
};

/* ---- network ingestion statistics -------------------------------------- */

struct net_stat {
    __u64 total_bytes;
    __u64 total_ops;
};

/* ---- GPU busy/idle tracking -------------------------------------------- */

struct gpu_busy_info {
    __u64 last_launch_ns;
    __u64 last_sync_ns;
    __u64 total_busy_ns;
    __u64 total_idle_ns;
    __u8  is_busy;
};

/* ---- BPF maps ---------------------------------------------------------- */

// Per-CPU storage I/O throughput counters.
// Slot 0 = reads, slot 1 = writes (total_bytes/total_ops of each; slots 2-3
// are reserved).
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, IO_STAT_SLOTS);
    __type(key, __u32);
    __type(value, struct io_stat);
} io_stats SEC(".maps");

// Per-CPU network ingestion rate counters.
// Slots: 0=bytes, 1=ops
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, NET_STAT_SLOTS);
    __type(key, __u32);
    __type(value, struct net_stat);
} net_ingest_stats SEC(".maps");

// Per-PID GPU busy/idle state tracking (LRU: exited processes age out).
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_INFLIGHT);
    __type(key, __u32);
    __type(value, struct gpu_busy_info);
} gpu_busy_map SEC(".maps");

// Ring buffer for pipeline bottleneck alert events.
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, RINGBUF_SIZE);
} pipeline_events SEC(".maps");

GRYVIA_DECLARE_DROPS();

/* ---- helpers ----------------------------------------------------------- */

static __always_inline void emit_pipeline_event(__u8 event_type,
                                                __u64 bytes,
                                                __u64 latency_ns)
{
    struct gpu_event *ev = bpf_ringbuf_reserve(&pipeline_events,
                                               sizeof(struct gpu_event), 0);
    if (!ev) {
    	GRYVIA_COUNT_DROP(GRYVIA_DROP_RINGBUF);
    	return;
    }

    __builtin_memset(ev, 0, sizeof(*ev));
    ev->timestamp     = bpf_ktime_get_ns();
    ev->pid           = bpf_get_current_pid_tgid() >> 32;
    ev->event_type    = event_type;
    ev->bytes         = bytes;
    ev->latency_ns    = latency_ns;
    bpf_get_current_comm(&ev->comm, sizeof(ev->comm));

    bpf_ringbuf_submit(ev, 0);
}

/* ---- tp_btf: block I/O completion --------------------------------------- */

#define REQ_OP_MASK   0xffU
#define REQ_OP_READ   0U
#define REQ_OP_WRITE  1U

// TP_PROTO(struct request *rq, blk_status_t error, unsigned int nr_bytes).
// tp_btf gives typed, CO-RE-safe access to the arguments (a classic
// tracepoint would need hard-coded field offsets that move between kernels).
// System-wide, per-CPU counters only: emitting a ring buffer event for every
// block I/O would flood the buffer and be misread as GPU copies.
SEC("tp_btf/block_rq_complete")
int BPF_PROG(block_rq_complete, struct request *rq, int error,
             unsigned int nr_bytes)
{
    __u32 op = BPF_CORE_READ(rq, cmd_flags) & REQ_OP_MASK;
    __u32 key;

    if (op == REQ_OP_READ)
        key = 0;
    else if (op == REQ_OP_WRITE)
        key = 1;
    else
        return 0;   /* flush / discard / zone ops carry no data throughput */

    struct io_stat *stat = bpf_map_lookup_elem(&io_stats, &key);
    if (stat) {
        stat->total_bytes += nr_bytes;   /* per-CPU slot: no atomics needed */
        stat->total_ops   += 1;
    }
    return 0;
}

/* ---- kretprobe: tcp_recvmsg (network data ingestion rate) --------------- */

// tcp_recvmsg(...) returns the byte count actually received (or -errno);
// the `len` argument would only be the caller's buffer size.
SEC("kretprobe/tcp_recvmsg")
int BPF_KRETPROBE(datapipe_tcp_recv, int ret)
{
    if (ret <= 0)
        return 0;

    __u32 key = 0;
    struct net_stat *ns = bpf_map_lookup_elem(&net_ingest_stats, &key);
    if (ns) {
        ns->total_bytes += ret;
        ns->total_ops   += 1;
    }
    return 0;
}

/* ---- uprobe: cudaLaunchKernel (GPU compute start) ---------------------- */

// cudaLaunchKernel(const void *func, dim3 gridDim, dim3 blockDim,
//                  void **args, size_t sharedMem, cudaStream_t stream)
SEC("uprobe/cudaLaunchKernel")
int BPF_UPROBE(datapipe_cuda_launch)
{
    __u32 pid = bpf_get_current_pid_tgid() >> 32;
    __u64 now = bpf_ktime_get_ns();

    struct gpu_busy_info *gbi = bpf_map_lookup_elem(&gpu_busy_map, &pid);
    if (!gbi) {
        struct gpu_busy_info init = {};
        /* NOEXIST: another thread of the process may have created it. */
        bpf_map_update_elem(&gpu_busy_map, &pid, &init, BPF_NOEXIST);
        gbi = bpf_map_lookup_elem(&gpu_busy_map, &pid);
        if (!gbi)
            return 0;
    }

    /* Idle -> busy transition: account the idle time; a gap longer than 1ms
     * means the GPU was starved (likely by the data pipeline). */
    if (!gbi->is_busy && gbi->last_sync_ns > 0) {
        __u64 idle_ns = now - gbi->last_sync_ns;
        __sync_fetch_and_add(&gbi->total_idle_ns, idle_ns);
        if (idle_ns > 1000000)
            emit_pipeline_event(GPU_EVT_PIPE_STALL, 0, idle_ns);
    }
    if (!gbi->is_busy)
        gbi->last_launch_ns = now;   /* start of this busy span */
    gbi->is_busy = 1;
    return 0;
}

/* ---- uretprobe: cudaDeviceSynchronize (GPU compute end) ---------------- */

// cudaDeviceSynchronize(void): everything launched so far has finished.
SEC("uretprobe/cudaDeviceSynchronize")
int BPF_URETPROBE(datapipe_cuda_sync)
{
    __u32 pid = bpf_get_current_pid_tgid() >> 32;
    __u64 now = bpf_ktime_get_ns();

    struct gpu_busy_info *gbi = bpf_map_lookup_elem(&gpu_busy_map, &pid);
    if (!gbi)
        return 0;

    if (gbi->is_busy && gbi->last_launch_ns > 0) {
        __u64 busy_ns = now - gbi->last_launch_ns;
        __sync_fetch_and_add(&gbi->total_busy_ns, busy_ns);
        /* Per-span delta (not the running total) so consumers can sum it. */
        emit_pipeline_event(GPU_EVT_PIPE_BUSY, 0, busy_ns);
    }
    gbi->last_sync_ns = now;
    gbi->is_busy = 0;
    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
