/* SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause */
/*
 * fabric_signal.h - side-channel ABI for scheduler-facing fabric signals
 * (straggler.c, rdma_health.c, gds_trace.c, overlap.c, roce_cnp.c,
 * infer_latency.c, ucx_gloo.c, weight_exfil.c, pfc_pause.c).
 *
 * struct gpu_event is frozen at 72 bytes (see gpu_common.h) and must NOT be
 * extended; these programs emit struct fabric_signal on their own
 * `fabric_events` ring buffer instead.  Userspace mirror:
 * collector/pkg/fabric/signal.go (the layout is checked against the
 * _Static_asserts below by its unit tests).
 */
#ifndef __FABRIC_SIGNAL_H__
#define __FABRIC_SIGNAL_H__

#include "gryvia_core.h"

#define FABRIC_RINGBUF_SIZE   (256 * 1024)
#define FABRIC_MAX_RANKS      512
#define FABRIC_COMM_LEN       16

enum fabric_signal_type {
	FABRIC_SIG_STRAGGLER   = 1, /* rank skew on a collective */
	FABRIC_SIG_RDMA_RETRY  = 2, /* QP retry / RNR above threshold */
	FABRIC_SIG_GDS         = 3, /* GPU-direct vs bounce-buffer read */
	FABRIC_SIG_OVERLAP     = 4, /* cudaDeviceSynchronize nested in an in-flight ncclAllReduce */
	FABRIC_SIG_INFER_WAIT  = 5, /* inference socket: accept -> first recv */
	/* Never on a ring buffer: the collector synthesises it from the
	 * roce_cnp cnp_count map so per-packet CNPs cannot flood a ring. */
	FABRIC_SIG_CNP         = 6, /* bytes = RoCEv2 CNP packets since the last poll */
	/* Never on a ring buffer either: synthesised from the pfc_pause pause_count map. */
	FABRIC_SIG_PFC         = 7, /* bytes = 802.1Qbb PFC pause frames since the last poll */
	FABRIC_SIG_EXFIL       = 8, /* large model-file read, then connect to a non-internal IPv4 */
	FABRIC_SIG_UCX_SLOW    = 9, /* a UCX tag-send call that blocked for a long time */
	/* One completed NCCL collective call on one rank, identified by
	 * (communicator ordinal, sequence number, op).  Emitted by straggler.c;
	 * the cross-rank comparison is done in userspace. */
	FABRIC_SIG_COLLECTIVE  = 10,
	/* ncclCommInitRank finished.  retry_count = NCCL_XPORT_* hint
	 * (0 unknown).  rank/world_size from the init arguments.
	 * latency_ns = init call duration.  comm = process name. */
	FABRIC_SIG_NCCL_XPORT  = 11,
	/* cudaDeviceEnablePeerAccess failed, then a large D2D memcpy ran
	 * in the same process within 30s.  bytes = copy size,
	 * retry_count = peer-enable failures, peer_latency_ns = age of the
	 * failure.  Not proof of a bounce buffer. */
	FABRIC_SIG_P2P_FALLBACK = 12,
	/* Reserved: collector synthesises from capture_lease / gate_hits.
	 * Not emitted on a ring buffer. */
	FABRIC_SIG_CAPTURE_ARMED = 13,
	/* 14 is reserved: a RoCE GID/QP-modify signal was proposed but cannot be observed
	 * from the kernel for userspace verbs, so no program emits it. */
	/* cudaMalloc / cudaMallocAsync / cudaMallocFromPoolAsync returned
	 * cudaErrorMemoryAllocation.  bytes = requested size,
	 * retry_count = 0 malloc, 1 async, 2 pool.  Routine in PyTorch's caching
	 * allocator, which retries after freeing cached blocks. */
	FABRIC_SIG_GPU_OOM     = 15,
	/* cudaDeviceSynchronize returned while a CUDA graph capture was open on
	 * the thread.  latency_ns = time since the capture began,
	 * retry_count = syncs seen in this capture. */
	FABRIC_SIG_GRAPH_STALL = 16,
	/* nvidia_p2p_get_pages returned non-zero.  retry_count = its return value. */
	FABRIC_SIG_GDR_FAIL    = 17,
	/* infer_ttft.c: accept -> first tcp_sendmsg on an inference port.
	 * latency_ns, bytes = first send size, rank = local port. */
	FABRIC_SIG_INFER_TTFT  = 18,
	/* infer_ttft.c: a gap >= 50 ms between two later sends on that
	 * connection, once per connection.  latency_ns = the gap. */
	FABRIC_SIG_INFER_GAP   = 19,
	/* weight_mmap.c: mmap of a model-weight file, then a connect to a public
	 * IPv4 within 30 s.  latency_ns = mmap -> connect, rank = remote port,
	 * peer_rank = remote IPv4 (host order). */
	FABRIC_SIG_WEIGHT_MMAP = 20,
	/* gpu_dev.c: open of a GPU device node from a cgroup not in allowed_cg,
	 * while gpu_dev_cfg[0] enforces.  cgroup_id_lo = low 32 bits of the id. */
	FABRIC_SIG_GPU_DEV     = 21,
	/* ucx_complete.c: ucp_worker_progress ran >= 5 ms after a send that
	 * returned a request.  latency_ns = post -> progress, bytes = send size.
	 * Completion is not observed. */
	FABRIC_SIG_UCX_WAIT    = 22,
	/* 23 is reserved: a LoRA-load signal (large read then H2D copy) was
	 * proposed but is indistinguishable from ordinary data loading. */
};

/* FABRIC_SIG_COLLECTIVE retry_count bits. */
#define COLL_FLAG_LATE      0x1 /* comm first seen at a collective: ordinal/seq are relative to probe attach */
#define COLL_FLAG_RANK_UNK  0x2 /* rank/world not learned (no ncclCommInitRank/UserRank/Count observed) */
/* rank value of a communicator whose rank was never observed. */
#define FABRIC_RANK_UNKNOWN 0xffffffffU

/*
 * Per-type field use:
 *   STRAGGLER   LEGACY (no object emits it any more; the collector still
 *               decodes it).  rank/peer_rank = this / fastest rank,
 *               latency_ns/peer_latency_ns = this / fastest span.
 *   COLLECTIVE  rank = rank in the communicator (FABRIC_RANK_UNKNOWN when not
 *               learned), world_size = communicator size (0 when unknown),
 *               peer_rank = communicator ORDINAL within the process (1-based,
 *               in the order the process created/first used communicators),
 *               peer_latency_ns = per-communicator collective SEQUENCE
 *               (1-based, counts the hooked collectives on that comm in call
 *               order), latency_ns = host-side duration of the API call (an
 *               asynchronous launch is the enqueue time, not the GPU time),
 *               bytes = count * element size (per-rank send count for
 *               allgather), nccl_op = nccl_op_type, retry_count = COLL_FLAG_*,
 *               rnr_count = low 32 bits of the ncclComm_t pointer (DEBUG only:
 *               a per-process address, never comparable across ranks),
 *               timestamp_ns = call exit (CLOCK_MONOTONIC).
 *   RDMA_RETRY  rank = QP number, world_size = post calls and
 *               retry_count/rnr_count = retry-exceeded / RNR-exceeded error
 *               completions since the previous signal for this QP,
 *               latency_ns = time since the last post.
 *   GDS         latency_ns = call latency, bytes = bytes transferred,
 *               retry_count = 1 when the nvidia-fs kernel path was seen
 *               (confirmed direct), 0 otherwise (bounce or unknown).
 *   OVERLAP     latency_ns = duration of the cudaDeviceSynchronize call,
 *               retry_count = ncclAllReduce calls in flight on that thread
 *               (nesting depth), nccl_op = nccl_op_type of the collective.
 *   INFER_WAIT  latency_ns = accept -> first tcp_recvmsg on the accepted
 *               socket, rank = local TCP port.
 *   CNP         bytes = CNP packets counted since the previous poll
 *               (userspace only).
 *   PFC         bytes = PFC pause frames counted since the previous poll
 *               (userspace only; per-priority counts stay in pause_count).
 *   EXFIL       bytes = bytes requested by the large reads in the window,
 *               latency_ns = last large read -> connect, rank = destination
 *               TCP port (host order), peer_rank = destination IPv4 address
 *               (host order), comm = process name.
 *   UCX_SLOW    latency_ns = duration of the ucp_tag_send_nb/nbx call,
 *               bytes = payload bytes when known (0 otherwise), comm = "ucx".
 */
struct fabric_signal {
	__u64 timestamp_ns;
	__u32 pid;
	__u32 cgroup_id_lo;     /* lower 32 of cgroup id; userspace joins to pod */
	__u8  signal_type;
	__u8  nccl_op;
	__u8  _pad[2];
	__u32 rank;
	__u32 world_size;
	__u32 peer_rank;        /* fastest peer, or 0 */
	__u64 latency_ns;       /* this rank */
	__u64 peer_latency_ns;  /* reference (fastest span) */
	__u64 bytes;
	__u32 retry_count;
	__u32 rnr_count;
	char  comm[FABRIC_COMM_LEN];
};

_Static_assert(sizeof(struct fabric_signal) == 80, "fabric_signal ABI: keep in sync with collector/pkg/fabric/signal.go");
_Static_assert(__builtin_offsetof(struct fabric_signal, pid) == 8, "fabric_signal.pid offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, cgroup_id_lo) == 12, "fabric_signal.cgroup_id_lo offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, signal_type) == 16, "fabric_signal.signal_type offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, nccl_op) == 17, "fabric_signal.nccl_op offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, rank) == 20, "fabric_signal.rank offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, world_size) == 24, "fabric_signal.world_size offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, peer_rank) == 28, "fabric_signal.peer_rank offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, latency_ns) == 32, "fabric_signal.latency_ns offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, peer_latency_ns) == 40, "fabric_signal.peer_latency_ns offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, bytes) == 48, "fabric_signal.bytes offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, retry_count) == 56, "fabric_signal.retry_count offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, rnr_count) == 60, "fabric_signal.rnr_count offset");
_Static_assert(__builtin_offsetof(struct fabric_signal, comm) == 64, "fabric_signal.comm offset");

/* straggler.c: one NCCL collective span on one rank. */
struct rank_span {
	__u64 start_ns;
	__u64 end_ns;
	__u64 bytes;
	__u32 rank;
	__u32 seen;             /* 1 once exit recorded */
};
_Static_assert(sizeof(struct rank_span) == 32, "rank_span size");

/* straggler.c: what is known about one ncclComm_t of one process. */
struct comm_info {
	__u64 seq;              /* collectives issued on this comm (atomic counter) */
	__u32 rank;             /* FABRIC_RANK_UNKNOWN until learned */
	__u32 world;            /* 0 until learned */
	__u32 ordinal;          /* 1-based ordinal within the process */
	__u32 flags;            /* COLL_FLAG_LATE */
};
_Static_assert(sizeof(struct comm_info) == 24, "comm_info size");

/* rdma_health.c: per-QP state keyed by QP number. */
struct rdma_health_val {
	__u64 last_send_ns;
	__u32 owner_pid;        /* first process seen posting on this QP */
	__u32 cgroup_lo;
	__u32 posted;           /* post calls since the last signal */
	__u32 retries;          /* IB_WC_RETRY_EXC_ERR completions since the last signal */
	__u32 rnr;              /* IB_WC_RNR_RETRY_EXC_ERR completions since the last signal */
	__u32 _pad;
	char  comm[FABRIC_COMM_LEN];
};
_Static_assert(sizeof(struct rdma_health_val) == 48, "rdma_health_val size");

/* gds_trace.c */
struct gds_inflight {
	__u64 start_ns;
	__u64 size;
	__u32 flags;            /* GDS_FLAG_* */
	__u32 _pad;
};
_Static_assert(sizeof(struct gds_inflight) == 24, "gds_inflight size");

/* overlap.c: per-thread (pid_tgid) nesting state. */
struct overlap_state {
	__u64 sync_start_ns;    /* outermost cudaDeviceSynchronize entry, 0 = none */
	__u32 nccl_inflight;    /* nested ncclAllReduce calls in flight */
	__u32 sync_depth;       /* nested cudaDeviceSynchronize calls in flight */
};
_Static_assert(sizeof(struct overlap_state) == 16, "overlap_state size");

/* roce_cnp.c: slots of the per-CPU cnp_count array (Go mirror: collector/main.go). */
#define CNP_SLOT_CNP   0        /* RoCEv2 congestion notification packets */
#define CNP_SLOT_ROCE  1        /* all RoCEv2 (UDP 4791) packets seen */
#define CNP_SLOTS      2

/* pfc_pause.c: slots of the per-CPU pause_count array (Go mirror: collector/pkg/fabric/signal.go). */
#define PFC_SLOT_FRAMES  0      /* 802.1Qbb PFC frames (MAC control opcode 0x0101) */
#define PFC_SLOT_LEGACY  1      /* 802.3x link-level pause frames (opcode 0x0001) */
#define PFC_SLOT_PRIO0   2      /* PFC frames that pause priority 0 (enabled, quanta != 0); PRIO0+7 = priority 7 */
#define PFC_PRIORITIES   8
#define PFC_SLOTS        (PFC_SLOT_PRIO0 + PFC_PRIORITIES)

/* weight_exfil.c: per-process (tgid) record of recent large model-file reads. */
struct exfil_mark {
	__u64 last_read_ns;     /* monotonic time of the latest large read */
	__u64 bytes;            /* bytes requested by the large reads in the window */
};
_Static_assert(sizeof(struct exfil_mark) == 16, "exfil_mark size");

#define GDS_FLAG_NVFS_SEEN 1    /* nvidia-fs kernel path observed */

/*
 * Minimal CO-RE view of struct ib_wc for the completion-error hooks.  status is
 * declared with the kernel's enum type (an int would not match the enum for
 * CO-RE field relocation).  Only the values we compare against are listed;
 * they mirror include/rdma/ib_verbs.h, which follows the verbs ABI.
 */
enum ib_wc_status {
	IB_WC_SUCCESS           = 0,
	IB_WC_RETRY_EXC_ERR     = 12,
	IB_WC_RNR_RETRY_EXC_ERR = 13,
};

struct ib_wc {
	enum ib_wc_status status;
	struct ib_qp *qp;
} __attribute__((preserve_access_index));

#endif /* __FABRIC_SIGNAL_H__ */
