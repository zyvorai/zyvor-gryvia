// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// nccl_trace.c - NCCL collective communication tracing via uprobes.
//
// Hooks NCCL library functions (ncclAllReduce, ncclAllGather, ncclBroadcast,
// ncclReduce, ncclReduceScatter, ncclSend, ncclRecv, ncclGroupStart,
// ncclGroupEnd) to trace distributed GPU collective operations.  Each
// collective emits a gpu_event to a ring buffer consumed by the userspace
// collector, with latency computed from entry/exit timestamps.

#include "headers/gpu_common.h"

/* ---- constants --------------------------------------------------------- */

#define MAX_INFLIGHT   65536
#define RINGBUF_SIZE   (256 * 1024)
#define LATENCY_SLOTS  6
#define TASK_COMM_LEN  16

/* Latency histogram bucket boundaries (nanoseconds) */
#define BUCKET_100US   100000ULL
#define BUCKET_1MS     1000000ULL
#define BUCKET_10MS    10000000ULL
#define BUCKET_100MS   100000000ULL
#define BUCKET_1S      1000000000ULL

/* ---- per-op statistics ------------------------------------------------- */

struct nccl_op_stat {
    __u64 count;
    __u64 total_bytes;
};

/* ---- BPF maps ---------------------------------------------------------- */

// In-flight NCCL operations keyed by pid_tgid for entry/exit correlation.
// LRU so entries orphaned by a thread dying mid-call (or a missed uretprobe)
// are recycled instead of filling the map.
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_INFLIGHT);
    __type(key, __u64);
    __type(value, struct nccl_inflight);
} nccl_inflight_map SEC(".maps");

// ncclGroupStart/End nesting state per thread (kept apart from the per-call
// map: collectives issued inside a group would otherwise overwrite it).
struct nccl_group {
    __u64 start_ns;
    __u32 depth;
    __u32 _pad;
};

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_INFLIGHT);
    __type(key, __u64);
    __type(value, struct nccl_group);
} nccl_group_map SEC(".maps");

// Ring buffer for gpu_event emission to userspace.
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, RINGBUF_SIZE);
} nccl_events SEC(".maps");

GRYVIA_DECLARE_DROPS();

// Per-CPU operation histogram: one entry per nccl_op_type (8 entries).
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, NCCL_OP_NAMES_LEN);
    __type(key, __u32);
    __type(value, struct nccl_op_stat);
} nccl_op_hist SEC(".maps");

// Latency distribution buckets: <100us, 100us-1ms, 1-10ms, 10-100ms,
// 100ms-1s, >1s.
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, LATENCY_SLOTS);
    __type(key, __u32);
    __type(value, __u64);
} nccl_latency_hist SEC(".maps");

/* ---- helpers ----------------------------------------------------------- */

// Entry: stash start time and payload size keyed by pid_tgid (a thread makes
// one NCCL call at a time, so the exit probe on the same thread finds it).
static __always_inline void record_nccl_entry(__u8 op_type, __u64 count,
                                              __u64 dtype)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    struct nccl_inflight info = {};
    info.start_ns = bpf_ktime_get_ns();
    info.op_type  = op_type;
    info.bytes    = count * nccl_dtype_size(dtype);
    info.rank     = 0;   /* ncclComm_t is opaque; rank is not readable */

    bpf_map_update_elem(&nccl_inflight_map, &pid_tgid, &info, BPF_ANY);
}

static __always_inline void update_latency_hist(__u64 latency_ns)
{
    __u32 bucket;

    if (latency_ns < BUCKET_100US)
        bucket = 0;
    else if (latency_ns < BUCKET_1MS)
        bucket = 1;
    else if (latency_ns < BUCKET_10MS)
        bucket = 2;
    else if (latency_ns < BUCKET_100MS)
        bucket = 3;
    else if (latency_ns < BUCKET_1S)
        bucket = 4;
    else
        bucket = 5;

    __u64 *slot = bpf_map_lookup_elem(&nccl_latency_hist, &bucket);
    if (slot)
        __sync_fetch_and_add(slot, 1);
}

static __always_inline void emit_nccl_exit(void)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    struct nccl_inflight *info = bpf_map_lookup_elem(&nccl_inflight_map,
                                                     &pid_tgid);
    if (!info)
        return;

    __u64 now = bpf_ktime_get_ns();
    __u64 latency = now - info->start_ns;

    /* Emit event via ring buffer */
    struct gpu_event *ev = bpf_ringbuf_reserve(&nccl_events,
                                               sizeof(struct gpu_event), 0);
    if (ev) {
        ev->timestamp     = now;
        ev->pid           = pid_tgid >> 32;
        ev->gpu_id        = 0;
        ev->event_type    = GPU_EVT_NCCL_OP;
        ev->direction     = 0;
        ev->nccl_op       = info->op_type;
        ev->_pad          = 0;
        ev->bytes         = info->bytes;
        ev->_pad2         = 0;
        ev->latency_ns    = latency;
        ev->src_rank      = info->rank;
        ev->dst_rank      = 0;
        ev->collective_id = 0;
        ev->world_size    = 0;
        bpf_get_current_comm(&ev->comm, sizeof(ev->comm));

        bpf_ringbuf_submit(ev, 0);
    } else {
        GRYVIA_COUNT_DROP(GRYVIA_DROP_RINGBUF);
    }

    /* Update per-op histogram */
    __u32 op_key = info->op_type;
    if (op_key < NCCL_OP_NAMES_LEN) {
        struct nccl_op_stat *stat = bpf_map_lookup_elem(&nccl_op_hist,
                                                        &op_key);
        if (stat) {
            __sync_fetch_and_add(&stat->count, 1);
            __sync_fetch_and_add(&stat->total_bytes, info->bytes);
        }
    }

    /* Update latency histogram */
    update_latency_hist(latency);

    bpf_map_delete_elem(&nccl_inflight_map, &pid_tgid);
}

/* ---- uprobes: ncclAllReduce -------------------------------------------- */

// ncclAllReduce(const void* sendbuff, void* recvbuff, size_t count,
//               ncclDataType_t datatype, ncclRedOp_t op, ncclComm_t comm,
//               cudaStream_t stream)
SEC("uprobe/ncclAllReduce")
int BPF_UPROBE(nccl_allreduce_entry, void *sendbuff, void *recvbuff,
               __u64 count, __u64 datatype)
{
    record_nccl_entry(NCCL_ALLREDUCE, count, datatype);
    return 0;
}

SEC("uretprobe/ncclAllReduce")
int BPF_URETPROBE(nccl_allreduce_exit)
{
    emit_nccl_exit();
    return 0;
}

/* ---- uprobes: ncclAllGather -------------------------------------------- */

// ncclAllGather(const void* sendbuff, void* recvbuff, size_t sendcount,
//               ncclDataType_t datatype, ncclComm_t comm, cudaStream_t stream)
SEC("uprobe/ncclAllGather")
int BPF_UPROBE(nccl_allgather_entry, void *sendbuff, void *recvbuff,
               __u64 count, __u64 datatype)
{
    record_nccl_entry(NCCL_ALLGATHER, count, datatype);
    return 0;
}

SEC("uretprobe/ncclAllGather")
int BPF_URETPROBE(nccl_allgather_exit)
{
    emit_nccl_exit();
    return 0;
}

/* ---- uprobes: ncclBroadcast -------------------------------------------- */

// ncclBroadcast(const void* sendbuff, void* recvbuff, size_t count,
//               ncclDataType_t datatype, int root, ncclComm_t comm,
//               cudaStream_t stream)
SEC("uprobe/ncclBroadcast")
int BPF_UPROBE(nccl_broadcast_entry, void *sendbuff, void *recvbuff,
               __u64 count, __u64 datatype)
{
    record_nccl_entry(NCCL_BROADCAST, count, datatype);
    return 0;
}

SEC("uretprobe/ncclBroadcast")
int BPF_URETPROBE(nccl_broadcast_exit)
{
    emit_nccl_exit();
    return 0;
}

/* ---- uprobes: ncclReduce ----------------------------------------------- */

// ncclReduce(const void* sendbuff, void* recvbuff, size_t count,
//            ncclDataType_t datatype, ncclRedOp_t op, int root,
//            ncclComm_t comm, cudaStream_t stream)
SEC("uprobe/ncclReduce")
int BPF_UPROBE(nccl_reduce_entry, void *sendbuff, void *recvbuff,
               __u64 count, __u64 datatype)
{
    record_nccl_entry(NCCL_REDUCE, count, datatype);
    return 0;
}

SEC("uretprobe/ncclReduce")
int BPF_URETPROBE(nccl_reduce_exit)
{
    emit_nccl_exit();
    return 0;
}

/* ---- uprobes: ncclReduceScatter ---------------------------------------- */

// ncclReduceScatter(const void* sendbuff, void* recvbuff, size_t recvcount,
//                   ncclDataType_t datatype, ncclRedOp_t op, ncclComm_t comm,
//                   cudaStream_t stream)
SEC("uprobe/ncclReduceScatter")
int BPF_UPROBE(nccl_reducescatter_entry, void *sendbuff, void *recvbuff,
               __u64 count, __u64 datatype)
{
    record_nccl_entry(NCCL_REDUCESCATTER, count, datatype);
    return 0;
}

SEC("uretprobe/ncclReduceScatter")
int BPF_URETPROBE(nccl_reducescatter_exit)
{
    emit_nccl_exit();
    return 0;
}

/* ---- uprobes: ncclSend ------------------------------------------------- */

// ncclSend(const void* sendbuff, size_t count, ncclDataType_t datatype,
//          int peer, ncclComm_t comm, cudaStream_t stream)
SEC("uprobe/ncclSend")
int BPF_UPROBE(nccl_send_entry, void *sendbuff, __u64 count, __u64 datatype)
{
    record_nccl_entry(NCCL_SEND, count, datatype);
    return 0;
}

SEC("uretprobe/ncclSend")
int BPF_URETPROBE(nccl_send_exit)
{
    emit_nccl_exit();
    return 0;
}

/* ---- uprobes: ncclRecv ------------------------------------------------- */

// ncclRecv(void* recvbuff, size_t count, ncclDataType_t datatype,
//          int peer, ncclComm_t comm, cudaStream_t stream)
SEC("uprobe/ncclRecv")
int BPF_UPROBE(nccl_recv_entry, void *recvbuff, __u64 count, __u64 datatype)
{
    record_nccl_entry(NCCL_RECV, count, datatype);
    return 0;
}

SEC("uretprobe/ncclRecv")
int BPF_URETPROBE(nccl_recv_exit)
{
    emit_nccl_exit();
    return 0;
}

/* ---- uprobes: ncclGroupStart / ncclGroupEnd ---------------------------- */

// A group batches several async calls; the outermost Start..End(return) span
// is reported as one NCCL_ALLTOALL-typed event ("group").  Nested groups only
// bump the depth counter.
SEC("uprobe/ncclGroupStart")
int BPF_UPROBE(nccl_group_start)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    struct nccl_group *g = bpf_map_lookup_elem(&nccl_group_map, &pid_tgid);
    if (g) {
        g->depth++;
        return 0;
    }
    struct nccl_group ng = {};
    ng.start_ns = bpf_ktime_get_ns();
    ng.depth = 1;
    bpf_map_update_elem(&nccl_group_map, &pid_tgid, &ng, BPF_NOEXIST);
    return 0;
}

SEC("uprobe/ncclGroupEnd")
int BPF_UPROBE(nccl_group_end_entry)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    struct nccl_group *g = bpf_map_lookup_elem(&nccl_group_map, &pid_tgid);
    if (g && g->depth > 0)
        g->depth--;
    return 0;
}

SEC("uretprobe/ncclGroupEnd")
int BPF_URETPROBE(nccl_group_end)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    struct nccl_group *g = bpf_map_lookup_elem(&nccl_group_map, &pid_tgid);
    if (!g || g->depth > 0)
        return 0;   /* unmatched, or still inside an outer group */

    __u64 now = bpf_ktime_get_ns();
    __u64 latency = now - g->start_ns;
    bpf_map_delete_elem(&nccl_group_map, &pid_tgid);

    struct gpu_event *ev = bpf_ringbuf_reserve(&nccl_events,
                                               sizeof(struct gpu_event), 0);
    if (!ev) {
        GRYVIA_COUNT_DROP(GRYVIA_DROP_RINGBUF);
        return 0;
    }
    __builtin_memset(ev, 0, sizeof(*ev));
    ev->timestamp  = now;
    ev->pid        = pid_tgid >> 32;
    ev->event_type = GPU_EVT_NCCL_OP;
    ev->nccl_op    = NCCL_ALLTOALL;
    ev->latency_ns = latency;
    bpf_get_current_comm(&ev->comm, sizeof(ev->comm));
    bpf_ringbuf_submit(ev, 0);
    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
