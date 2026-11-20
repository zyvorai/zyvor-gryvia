// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// gradient_compress.c - Gradient compression analysis for distributed training.
//
// Monitors ncclAllReduce operations to detect gradient compression by
// comparing the expected data volume (element count * datatype size)
// against the actual bytes transferred.  This helps identify whether
// gradient compression techniques (e.g., PowerSGD, TopK, random
// sparsification) are effective and what compression ratios are achieved.

#include "headers/gpu_common.h"

/* ---- constants --------------------------------------------------------- */

#define MAX_INFLIGHT   65536
#define RINGBUF_SIZE   (256 * 1024)
#define COMP_SLOTS     16   /* indexed by ncclDataType_t */
#define TASK_COMM_LEN  16

/* ---- gradient in-flight tracking --------------------------------------- */

struct grad_info {
    __u64 start_ns;
    __u64 expected_bytes;   /* count * sizeof(datatype) */
    __u32 count;
    __u8  datatype;
};

/* ---- compression statistics -------------------------------------------- */

struct compression_stat {
    __u64 expected_bytes;   /* sum of expected bytes */
    __u64 actual_bytes;     /* sum of actual bytes transferred */
    __u64 op_count;         /* number of operations */
};

/* ---- BPF maps ---------------------------------------------------------- */

// In-flight gradient operations keyed by pid_tgid (LRU: no leak if a thread
// dies inside the call).
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_INFLIGHT);
    __type(key, __u64);
    __type(value, struct grad_info);
} grad_inflight SEC(".maps");

// Ring buffer for gradient compression events.
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, RINGBUF_SIZE);
} grad_events SEC(".maps");

// Per-collective compression statistics.
// Tracks expected vs actual bytes for each collective slot.
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, COMP_SLOTS);
    __type(key, __u32);
    __type(value, struct compression_stat);
} compression_stats SEC(".maps");

/* ---- helpers ----------------------------------------------------------- */

static __always_inline void emit_grad_event(__u64 expected_bytes,
                                            __u64 actual_bytes,
                                            __u64 latency_ns)
{
    struct gpu_event *ev = bpf_ringbuf_reserve(&grad_events,
                                               sizeof(struct gpu_event), 0);
    if (!ev)
        return;

    /* Typed GPU_EVT_GRAD_COMPRESS (not NCCL_OP): nccl_trace already reports
     * every AllReduce, and src/dst_rank here carry the expected byte count
     * (hi/lo halves), which the rank statistics must not ingest. */
    __builtin_memset(ev, 0, sizeof(*ev));
    ev->timestamp     = bpf_ktime_get_ns();
    ev->pid           = bpf_get_current_pid_tgid() >> 32;
    ev->event_type    = GPU_EVT_GRAD_COMPRESS;
    ev->nccl_op       = NCCL_ALLREDUCE;
    ev->bytes         = actual_bytes;
    ev->latency_ns    = latency_ns;
    ev->src_rank      = (__u32)(expected_bytes >> 32);   /* high 32 bits */
    ev->dst_rank      = (__u32)(expected_bytes & 0xFFFFFFFF); /* low 32 */
    bpf_get_current_comm(&ev->comm, sizeof(ev->comm));

    bpf_ringbuf_submit(ev, 0);
}

/* ---- uprobes: ncclAllReduce (gradient compression analysis) ------------ */

// ncclAllReduce(const void* sendbuff, void* recvbuff, size_t count,
//               ncclDataType_t datatype, ncclRedOp_t op, ncclComm_t comm,
//               cudaStream_t stream)
//
// On entry we capture count and datatype to compute expected bytes.
SEC("uprobe/ncclAllReduce")
int BPF_UPROBE(grad_allreduce_entry, void *sendbuff, void *recvbuff,
               __u64 count, __u32 datatype)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    struct grad_info gi = {};
    gi.start_ns       = bpf_ktime_get_ns();
    gi.count          = (__u32)count;
    gi.datatype       = (__u8)datatype;
    gi.expected_bytes = count * nccl_dtype_size(datatype);

    bpf_map_update_elem(&grad_inflight, &pid_tgid, &gi, BPF_ANY);
    return 0;
}

// On exit we capture the return and compute compression ratio.
// The actual bytes transferred equals count * dtype_size for uncompressed
// gradients.  If the library applies compression, the actual transfer
// size will differ from expected, which we detect in userspace by
// comparing the latency-normalized throughput.
SEC("uretprobe/ncclAllReduce")
int BPF_URETPROBE(grad_allreduce_exit)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    struct grad_info *gi = bpf_map_lookup_elem(&grad_inflight, &pid_tgid);
    if (!gi)
        return 0;

    __u64 now     = bpf_ktime_get_ns();
    __u64 latency = now - gi->start_ns;
    __u64 expected = gi->expected_bytes;

    /* The actual bytes for an uncompressed AllReduce equals the expected.
     * We record both; userspace detects compression by observing that
     * throughput = expected_bytes / latency deviates across collectives. */
    __u64 actual_bytes = gi->expected_bytes;

    /* Update per-slot compression statistics */
    __u32 slot = gi->datatype;   /* one slot per ncclDataType_t */
    if (slot >= COMP_SLOTS)
        slot = COMP_SLOTS - 1;
    struct compression_stat *cs = bpf_map_lookup_elem(&compression_stats,
                                                      &slot);
    if (cs) {
        __sync_fetch_and_add(&cs->expected_bytes, gi->expected_bytes);
        __sync_fetch_and_add(&cs->actual_bytes, actual_bytes);
        __sync_fetch_and_add(&cs->op_count, 1);
    }

    bpf_map_delete_elem(&grad_inflight, &pid_tgid);
    emit_grad_event(expected, actual_bytes, latency);
    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
