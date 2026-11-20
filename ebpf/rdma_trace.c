// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// rdma_trace.c - RDMA/InfiniBand traffic analysis via kprobes and tracepoints.
//
// Hooks RDMA verbs layer functions (ib_post_send, ib_post_recv, ib_poll_cq)
// and QP lifecycle functions (ib_create_qp_kernel, ib_destroy_qp_user) to monitor
// InfiniBand traffic patterns used by GPU-direct RDMA and NCCL network
// transports.  Events are emitted to a ring buffer for the userspace
// collector.

#include "headers/gpu_common.h"

/* ---- constants --------------------------------------------------------- */

#define MAX_INFLIGHT   65536
#define MAX_QPS        8192
#define RINGBUF_SIZE   (256 * 1024)
#define RDMA_STAT_SLOTS 4    /* send_bytes, recv_bytes, send_ops, recv_ops */
#define TASK_COMM_LEN  16

/* ---- aggregate stats --------------------------------------------------- */

struct rdma_agg_stat {
    __u64 value;
};

#define MAX_WR_CHAIN   4     /* work requests walked per post call */
#define MAX_SGE        4     /* scatter/gather entries summed per WR */

/* NOTE: ib_post_send/ib_post_recv/ib_poll_cq are `static inline` wrappers in
 * include/rdma/ib_verbs.h, so on most kernels there is no symbol to kprobe
 * and these three attach only where the compiler emitted an out-of-line copy.
 * Only in-kernel RDMA consumers (NFS-RDMA, SRP, ...) reach them anyway:
 * NCCL/libibverbs post from user space through the NIC doorbell. */

/* ---- in-flight poll tracking -------------------------------------------- */

struct rdma_poll_info {
    __u64 start_ns;
};

/* ---- BPF maps ---------------------------------------------------------- */

// Per-QP statistics keyed by QP number.  LRU: QPs whose destroy we missed
// (attached late, or the process was killed) age out.
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_QPS);
    __type(key, __u32);
    __type(value, struct rdma_qp_info);
} rdma_qp_map SEC(".maps");

// Ring buffer for RDMA events.
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, RINGBUF_SIZE);
} rdma_events SEC(".maps");

// ib_poll_cq entry timestamps keyed by pid_tgid (LRU: no leak if the exit
// probe is missed).
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_INFLIGHT);
    __type(key, __u64);
    __type(value, struct rdma_poll_info);
} rdma_poll_inflight SEC(".maps");

// Per-CPU aggregate send/recv byte counters.
// Slots: 0=send_bytes, 1=recv_bytes, 2=send_ops, 3=recv_ops
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, RDMA_STAT_SLOTS);
    __type(key, __u32);
    __type(value, struct rdma_agg_stat);
} rdma_stats SEC(".maps");

/* ---- helpers ----------------------------------------------------------- */

// gpu_id carries the QP number and collective_id the poll completion count
// for RDMA events (documented in the collector decoder).
static __always_inline void emit_rdma_event(__u8 event_type, __u64 bytes,
                                            __u64 latency_ns, __u32 qp_num,
                                            __u32 completions)
{
    struct gpu_event *ev = bpf_ringbuf_reserve(&rdma_events,
                                               sizeof(struct gpu_event), 0);
    if (!ev)
        return;

    __builtin_memset(ev, 0, sizeof(*ev));
    ev->timestamp     = bpf_ktime_get_ns();
    ev->pid           = bpf_get_current_pid_tgid() >> 32;
    ev->gpu_id        = qp_num;
    ev->event_type    = event_type;
    ev->bytes         = bytes;
    ev->latency_ns    = latency_ns;
    ev->collective_id = completions;
    bpf_get_current_comm(&ev->comm, sizeof(ev->comm));

    bpf_ringbuf_submit(ev, 0);
}

static __always_inline void update_rdma_stats(__u32 slot, __u64 value)
{
    if (slot >= RDMA_STAT_SLOTS)
        return;

    struct rdma_agg_stat *stat = bpf_map_lookup_elem(&rdma_stats, &slot);
    if (stat)
        __sync_fetch_and_add(&stat->value, value);
}

// Sum the sge lengths of a (bounded) chain of work requests.  Generated for
// both ib_send_wr and ib_recv_wr, which share the fields we need.
#define DEFINE_WR_BYTES(fn, wr_type)                                         \
static __always_inline __u64 fn(struct wr_type *wr)                          \
{                                                                            \
    __u64 total = 0;                                                         \
    _Pragma("unroll")                                                        \
    for (int w = 0; w < MAX_WR_CHAIN; w++) {                                 \
        if (!wr)                                                             \
            break;                                                           \
        struct ib_sge *sg = BPF_CORE_READ(wr, sg_list);                      \
        int n = BPF_CORE_READ(wr, num_sge);                                  \
        if (sg && n > 0) {                                                   \
            if (n > MAX_SGE)                                                 \
                n = MAX_SGE;                                                 \
            for (int i = 0; i < MAX_SGE; i++) {                              \
                if (i >= n)                                                  \
                    break;                                                   \
                total += BPF_CORE_READ(sg, length);                          \
                sg++;                                                        \
            }                                                                \
        }                                                                    \
        wr = BPF_CORE_READ(wr, next);                                        \
    }                                                                        \
    return total;                                                            \
}

DEFINE_WR_BYTES(send_wr_bytes, ib_send_wr)
DEFINE_WR_BYTES(recv_wr_bytes, ib_recv_wr)

static __always_inline void qp_account(__u32 qp_num, __u64 bytes, int is_send)
{
    if (!qp_num)
        return;
    struct rdma_qp_info *qpi = bpf_map_lookup_elem(&rdma_qp_map, &qp_num);
    if (!qpi)
        return;   /* QP created before attach: not tracked */
    if (is_send) {
        __sync_fetch_and_add(&qpi->bytes_sent, bytes);
        qpi->last_send_ns = bpf_ktime_get_ns();
    } else {
        __sync_fetch_and_add(&qpi->bytes_recv, bytes);
    }
}

/* ---- kprobes: ib_post_send / ib_post_recv ------------------------------- */

// ib_post_send(struct ib_qp *qp, const struct ib_send_wr *send_wr,
//              const struct ib_send_wr **bad_send_wr)
SEC("kprobe/ib_post_send")
int BPF_KPROBE(rdma_post_send, struct ib_qp *qp, struct ib_send_wr *send_wr)
{
    __u32 qp_num = BPF_CORE_READ(qp, qp_num);
    __u64 bytes = send_wr_bytes(send_wr);

    update_rdma_stats(0, bytes);
    update_rdma_stats(2, 1);
    qp_account(qp_num, bytes, 1);

    emit_rdma_event(GPU_EVT_RDMA_SEND, bytes, 0, qp_num, 0);
    return 0;
}

// ib_post_recv(struct ib_qp *qp, const struct ib_recv_wr *recv_wr,
//              const struct ib_recv_wr **bad_recv_wr)
SEC("kprobe/ib_post_recv")
int BPF_KPROBE(rdma_post_recv, struct ib_qp *qp, struct ib_recv_wr *recv_wr)
{
    __u32 qp_num = BPF_CORE_READ(qp, qp_num);
    __u64 bytes = recv_wr_bytes(recv_wr);

    update_rdma_stats(1, bytes);
    update_rdma_stats(3, 1);
    qp_account(qp_num, bytes, 0);

    emit_rdma_event(GPU_EVT_RDMA_RECV, bytes, 0, qp_num, 0);
    return 0;
}

/* ---- kprobes: ib_poll_cq ----------------------------------------------- */

// ib_poll_cq(struct ib_cq *cq, int num_entries, struct ib_wc *wc)
SEC("kprobe/ib_poll_cq")
int BPF_KPROBE(rdma_poll_cq_entry, void *cq, int num_entries)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    struct rdma_poll_info pi = { .start_ns = bpf_ktime_get_ns() };

    bpf_map_update_elem(&rdma_poll_inflight, &pid_tgid, &pi, BPF_ANY);
    return 0;
}

// Exit: only completions > 0 are interesting (busy-polling returns 0 almost
// always).  Reported as an RDMA_SEND-typed event with bytes=0 and the number
// of completions in collective_id, so consumers do not mistake the count for
// a byte total.
SEC("kretprobe/ib_poll_cq")
int BPF_KRETPROBE(rdma_poll_cq_exit, int ret)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    struct rdma_poll_info *pi = bpf_map_lookup_elem(&rdma_poll_inflight,
                                                    &pid_tgid);
    if (!pi)
        return 0;

    __u64 latency = bpf_ktime_get_ns() - pi->start_ns;
    bpf_map_delete_elem(&rdma_poll_inflight, &pid_tgid);

    if (ret <= 0)
        return 0;

    emit_rdma_event(GPU_EVT_RDMA_SEND, 0, latency, 0, (__u32)ret);
    return 0;
}

/* ---- QP lifecycle ------------------------------------------------------- */

// struct ib_qp *ib_create_qp_kernel(struct ib_pd *pd,
//                                   struct ib_qp_init_attr *attr,
//                                   const char *caller)
// (ib_create_qp() is a macro around it.)  The QP number exists only in the
// returned object.
SEC("kretprobe/ib_create_qp_kernel")
int BPF_KRETPROBE(rdma_create_qp, struct ib_qp *qp)
{
    if ((unsigned long)qp >= (unsigned long)-4095L)   /* IS_ERR */
        return 0;

    __u32 qp_num = BPF_CORE_READ(qp, qp_num);
    if (!qp_num)
        return 0;

    struct rdma_qp_info qpi = {};
    qpi.last_send_ns = bpf_ktime_get_ns();
    bpf_map_update_elem(&rdma_qp_map, &qp_num, &qpi, BPF_ANY);
    return 0;
}

// int ib_destroy_qp_user(struct ib_qp *qp, struct ib_udata *udata)
SEC("kprobe/ib_destroy_qp_user")
int BPF_KPROBE(rdma_destroy_qp, struct ib_qp *qp)
{
    __u32 qp_num = BPF_CORE_READ(qp, qp_num);

    struct rdma_qp_info *qpi = bpf_map_lookup_elem(&rdma_qp_map, &qp_num);
    if (qpi) {
        __u64 total = qpi->bytes_sent + qpi->bytes_recv;
        bpf_map_delete_elem(&rdma_qp_map, &qp_num);
        /* Final per-QP volume, reported as a RECV-typed event by convention. */
        emit_rdma_event(GPU_EVT_RDMA_RECV, total, 0, qp_num, 0);
    }
    return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
