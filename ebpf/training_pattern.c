// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// training_pattern.c - AI training communication pattern analysis.
//
// Detects distributed training patterns by correlating NCCL collective
// operations with TCP traffic for data loading.  Tracks the rank
// communication matrix (which ranks talk to which) and the compute-
// communication cycle timing to identify pipeline bubbles and
// synchronization inefficiencies.

#include "headers/gpu_common.h"

/* ---- constants --------------------------------------------------------- */

#define MAX_INFLIGHT     65536
#define RINGBUF_SIZE     (256 * 1024)
#define MAX_RANK_PAIRS   (MAX_RANKS * MAX_RANKS)
#define CYCLE_SLOTS      4
#define TASK_COMM_LEN    16

/* ---- rank communication matrix key ------------------------------------ */

struct rank_pair {
    __u32 src_rank;
    __u32 dst_rank;
};

/* ---- rank communication stats ----------------------------------------- */

struct rank_comm_stat {
    __u64 bytes;
    __u64 count;
    __u64 last_ns;
};

/* ---- compute/communication cycle tracking ------------------------------ */

// Per training process (tgid).  A "step" is one ncclAllReduce; the gap between
// the previous AllReduce returning and the next one starting is attributed to
// compute, the AllReduce call itself to communication.
struct cycle_timing {
    __u64 last_compute_end_ns;   /* when the previous AllReduce returned */
    __u64 last_comm_start_ns;
    __u64 total_compute_ns;
    __u64 total_comm_ns;
    __u64 last_gap_ns;           /* compute gap preceding the current step */
    __u64 data_recv_bytes;       /* TCP bytes received since the last step */
    __u64 data_send_bytes;
    __u64 steps;
};

/* ---- BPF maps ---------------------------------------------------------- */

// Rank communication matrix: bytes exchanged between each (src, dst) pair.
// Ranks are not observable from the uprobe (ncclComm_t is opaque), so all
// AllReduce traffic accumulates under the (0,0) sentinel.
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_INFLIGHT);
    __type(key, struct rank_pair);
    __type(value, struct rank_comm_stat);
} rank_comm_matrix SEC(".maps");

// Per-process compute/communication cycle timing.  Keyed by tgid rather than
// per-CPU: entry and exit of a step may run on different CPUs.
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_INFLIGHT);
    __type(key, __u32);
    __type(value, struct cycle_timing);
} compute_comm_cycle SEC(".maps");

// Ring buffer for training pattern events (one GPU_EVT_TRAIN_CYCLE per step).
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, RINGBUF_SIZE);
} pattern_events SEC(".maps");

// In-flight AllReduce calls (entry/exit correlation); LRU so calls whose
// thread died cannot leak slots.
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_INFLIGHT);
    __type(key, __u64);
    __type(value, struct nccl_inflight);
} pattern_nccl_inflight SEC(".maps");

/* ---- helpers ----------------------------------------------------------- */

static __always_inline struct cycle_timing *get_cycle(__u32 tgid, int create)
{
    struct cycle_timing *ct = bpf_map_lookup_elem(&compute_comm_cycle, &tgid);
    if (ct || !create)
        return ct;

    struct cycle_timing init = {};
    bpf_map_update_elem(&compute_comm_cycle, &tgid, &init, BPF_NOEXIST);
    return bpf_map_lookup_elem(&compute_comm_cycle, &tgid);
}

static __always_inline void emit_cycle_event(__u64 data_bytes, __u64 comm_ns,
                                             __u64 compute_gap_ns)
{
    struct gpu_event *ev = bpf_ringbuf_reserve(&pattern_events,
                                               sizeof(struct gpu_event), 0);
    if (!ev)
        return;

    __u64 us = compute_gap_ns / 1000;

    __builtin_memset(ev, 0, sizeof(*ev));
    ev->timestamp     = bpf_ktime_get_ns();
    ev->pid           = bpf_get_current_pid_tgid() >> 32;
    ev->event_type    = GPU_EVT_TRAIN_CYCLE;
    ev->nccl_op       = NCCL_ALLREDUCE;
    ev->bytes         = data_bytes;
    ev->latency_ns    = comm_ns;
    ev->collective_id = us > 0xffffffffULL ? 0xffffffffU : (__u32)us;
    bpf_get_current_comm(&ev->comm, sizeof(ev->comm));

    bpf_ringbuf_submit(ev, 0);
}

/* ---- uprobes: ncclAllReduce (pattern detection focus) ------------------ */

// Shares the hook with nccl_trace.c but tracks the compute/communication
// cycle and the rank matrix instead of per-call events.
SEC("uprobe/ncclAllReduce")
int BPF_UPROBE(pattern_allreduce_entry, void *sendbuff, void *recvbuff,
               __u64 count, __u64 datatype)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u32 tgid = pid_tgid >> 32;
    __u64 now = bpf_ktime_get_ns();
    __u64 bytes = count * nccl_dtype_size(datatype);

    struct nccl_inflight info = {};
    info.start_ns = now;
    info.op_type  = NCCL_ALLREDUCE;
    info.bytes    = bytes;
    bpf_map_update_elem(&pattern_nccl_inflight, &pid_tgid, &info, BPF_ANY);

    struct cycle_timing *ct = get_cycle(tgid, 1);
    if (ct) {
        ct->last_gap_ns = 0;
        if (ct->last_compute_end_ns) {
            ct->last_gap_ns = now - ct->last_compute_end_ns;
            ct->total_compute_ns += ct->last_gap_ns;
        }
        ct->last_comm_start_ns = now;
    }

    struct rank_pair rp = {};   /* (0,0) sentinel */
    struct rank_comm_stat init = {};
    bpf_map_update_elem(&rank_comm_matrix, &rp, &init, BPF_NOEXIST);
    struct rank_comm_stat *rcs = bpf_map_lookup_elem(&rank_comm_matrix, &rp);
    if (rcs) {
        __sync_fetch_and_add(&rcs->bytes, bytes);
        __sync_fetch_and_add(&rcs->count, 1);
        rcs->last_ns = now;
    }
    return 0;
}

SEC("uretprobe/ncclAllReduce")
int BPF_URETPROBE(pattern_allreduce_exit)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u32 tgid = pid_tgid >> 32;

    struct nccl_inflight *info = bpf_map_lookup_elem(&pattern_nccl_inflight,
                                                     &pid_tgid);
    if (!info)
        return 0;

    __u64 now = bpf_ktime_get_ns();
    __u64 comm_ns = now - info->start_ns;
    bpf_map_delete_elem(&pattern_nccl_inflight, &pid_tgid);

    struct cycle_timing *ct = get_cycle(tgid, 0);
    if (!ct)
        return 0;

    ct->total_comm_ns += comm_ns;
    ct->last_compute_end_ns = now;
    ct->steps++;

    __u64 data = ct->data_recv_bytes;
    __u64 gap = ct->last_gap_ns;
    ct->data_recv_bytes = 0;
    ct->data_send_bytes = 0;

    emit_cycle_event(data, comm_ns, gap);
    return 0;
}

/* ---- kretprobes: tcp_sendmsg / tcp_recvmsg (data loading patterns) ------ */

// Only processes that have already issued an AllReduce (i.e. are training)
// are accounted, and only their byte totals are kept: emitting one ring
// buffer event per TCP call system-wide would flood the buffer and be
// misread by the collector as GPU memory transfers.
//
// tcp_sendmsg(sk, msg, size) / tcp_recvmsg(sk, msg, len, ...) both return the
// byte count actually transferred (or a negative errno).
SEC("kretprobe/tcp_sendmsg")
int BPF_KRETPROBE(pattern_tcp_send, int ret)
{
    if (ret <= 0)
        return 0;
    struct cycle_timing *ct = get_cycle(bpf_get_current_pid_tgid() >> 32, 0);
    if (ct)
        __sync_fetch_and_add(&ct->data_send_bytes, (__u64)ret);
    return 0;
}

// Data ingestion between collectives: prefetch / data-loader traffic.
SEC("kretprobe/tcp_recvmsg")
int BPF_KRETPROBE(pattern_tcp_recv, int ret)
{
    if (ret <= 0)
        return 0;
    struct cycle_timing *ct = get_cycle(bpf_get_current_pid_tgid() >> 32, 0);
    if (ct)
        __sync_fetch_and_add(&ct->data_recv_bytes, (__u64)ret);
    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
