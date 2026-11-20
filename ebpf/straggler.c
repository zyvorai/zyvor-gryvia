// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// straggler.c - NCCL collective identity for cross-rank comparison.
//
// (The file name is kept for Makefile/loader stability; it replaces the old
// heuristic that compared the fastest recent ncclAllReduce of one payload size
// with unrelated later calls and could not know the rank.)
//
// What it does: emits one FABRIC_SIG_COLLECTIVE fabric_signal per completed
// NCCL collective call, carrying (communicator ordinal, per-communicator
// sequence number, op, bytes, rank, world size, host-side call duration).
// Userspace (collector/pkg/fabric folder, then the gateway's cross-node view)
// compares the SAME (communicator ordinal, seq, op) across ranks.  Nothing is
// decided in the kernel any more.
//
// How identity is learned WITHOUT reading NCCL private structs (no offsets):
//   - uprobe+uretprobe on the public API ncclCommInitRank / ncclCommInitRankConfig
//     (comm*, nranks, ncclUniqueId by value, myrank): on success the ncclComm_t
//     written to *comm is registered with (rank, world size) and the next
//     communicator ordinal of the process.  ncclUniqueId is a 128-byte struct
//     passed by value: on x86-64 it goes on the stack (myrank is integer arg 3),
//     on arm64 (AAPCS64) it is passed by reference in a register (myrank is
//     arg 4).  Both are handled with __TARGET_ARCH_*; only x86-64 was exercised.
//   - ncclCommInitAll(comms*, ndev, devlist): comm i gets rank i, world ndev.
//   - ncclCommUserRank(comm, int*rank) and ncclCommCount(comm, int*nranks): if
//     the workload calls them, rank/world are (re)learned from the out value.
//   - ncclCommDestroy / ncclCommAbort: the communicator is forgotten.
//   Not hooked: ncclCommSplit/ncclCommInitRankScalable (rank stays unknown
//   unless the workload calls ncclCommUserRank), ncclGetUniqueId (carries no
//   comm identity), ncclReduce (comm is a stack argument on x86-64), ncclSend/
//   ncclRecv (point to point: no matching seq across ranks).
//
// Sequence numbers: incremented at collective ENTRY for ncclAllReduce,
// ncclBroadcast, ncclAllGather, ncclReduceScatter and ncclAlltoAll in call
// order, so rank r's Nth collective on a communicator lines up with rank s's
// Nth as long as both ranks issue the same hooked collectives (NCCL requires
// this).  The counter is a plain (non-atomic) increment: two threads issuing on
// the SAME communicator concurrently could lose a count.  LIMITS, all reported
// to userspace instead of hidden:
//   - a communicator first seen at a collective (probes attached after
//     ncclCommInitRank) gets COLL_FLAG_LATE: its ordinal and seq are relative
//     to attach time and are only comparable with other ranks attached at the
//     same moment.  Its rank stays FABRIC_RANK_UNKNOWN until UserRank/Count.
//   - the ncclComm_t pointer is per process (and per node): it is emitted for
//     local debugging only.  Cross-node identity is the job plus the
//     communicator ORDINAL, which assumes every process creates communicators
//     in the same order (true for the usual DP/TP/PP init flow, not guaranteed).
//   - the span is the host-side duration of the API call; for asynchronous
//     launches (the normal case) that is enqueue time, not on-GPU time.
//   - inside ncclGroupStart/End calls only enqueue; the duration is small.
//
// Volume: one 80-byte record per collective.  collective_cfg[0] = N emits
// only calls whose seq is a multiple of N (same on every rank, so sampled
// records still line up); 0 or 1 = all.
//
// Observe-only.  Verified on x86-64 with a stand-in libnccl (tests/ebpf notes in
// docs/nccl-rdma-gpu-correlation.md); unverified against real NCCL.

#include "headers/gpu_common.h"
#include "headers/fabric_signal.h"

#define MAX_INFLIGHT   65536
#define MAX_COMMS      16384
#define MAX_PIDS       4096
#define INITALL_MAX    16

struct comm_key {
	__u32 pid;
	__u32 _pad;
	__u64 comm;             /* ncclComm_t value in that process */
};
_Static_assert(sizeof(struct comm_key) == 16, "comm_key size");

// Live communicators.  LRU: a missed ncclCommDestroy is recycled eventually.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_COMMS);
	__type(key, struct comm_key);
	__type(value, struct comm_info);
} comm_map SEC(".maps");

// Next communicator ordinal per process.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_PIDS);
	__type(key, __u32);
	__type(value, __u32);
} pid_ordinal SEC(".maps");

#define ARG_INIT   1
#define ARG_RANK   2
#define ARG_COUNT  3
#define ARG_INITALL 4

struct arg_key {
	__u64 id;               /* pid_tgid */
	__u32 which;
	__u32 _pad;
};
struct arg_val {
	__u64 ptr;              /* ncclComm_t* / comms[] / int* out */
	__u64 comm;             /* ncclComm_t (UserRank/Count) */
	__u32 nranks;
	__u32 rank;
};
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_INFLIGHT);
	__type(key, struct arg_key);
	__type(value, struct arg_val);
} api_args SEC(".maps");

struct coll_call {
	__u64 start_ns;
	__u64 bytes;
	__u64 seq;
	__u64 comm;
	__u32 rank;
	__u32 world;
	__u32 ordinal;
	__u32 flags;
	__u8  op;
	__u8  _pad[7];
};
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_INFLIGHT);
	__type(key, __u64);     /* pid_tgid: one thread makes one NCCL call at a time */
	__type(value, struct coll_call);
} coll_inflight SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

GRYVIA_DECLARE_DROPS();

// Userspace may write N here to emit only every Nth collective (by seq).
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u32);
} collective_cfg SEC(".maps");

static __always_inline __u32 next_ordinal(__u32 pid)
{
	__u32 one = 1, next;
	__u32 *cur = bpf_map_lookup_elem(&pid_ordinal, &pid);

	if (!cur) {
		bpf_map_update_elem(&pid_ordinal, &pid, &one, BPF_NOEXIST);
		cur = bpf_map_lookup_elem(&pid_ordinal, &pid);
		if (!cur)
			return 0;
	}
	next = *cur;
	*cur = next + 1;
	return next;
}

// Register (or re-register, e.g. after a missed destroy) a communicator with a known rank.
static __always_inline void comm_register(__u32 pid, __u64 comm, __u32 rank, __u32 world)
{
	struct comm_key key = { .pid = pid, .comm = comm };
	struct comm_info ci = {};
	__u32 ord;

	if (!comm)
		return;
	ord = next_ordinal(pid);
	if (!ord)
		return;
	ci.rank = rank;
	ci.world = world;
	ci.ordinal = ord;
	bpf_map_update_elem(&comm_map, &key, &ci, BPF_ANY);
}

// Find a communicator; create it lazily (flagged LATE, rank unknown) when the
// probes were attached after its creation.
static __always_inline struct comm_info *comm_get(__u32 pid, __u64 comm)
{
	struct comm_key key = { .pid = pid, .comm = comm };
	struct comm_info init = {};
	struct comm_info *ci = bpf_map_lookup_elem(&comm_map, &key);

	if (ci)
		return ci;
	init.rank = FABRIC_RANK_UNKNOWN;
	init.ordinal = next_ordinal(pid);
	if (!init.ordinal)
		return 0;
	init.flags = COLL_FLAG_LATE;
	bpf_map_update_elem(&comm_map, &key, &init, BPF_NOEXIST);
	return bpf_map_lookup_elem(&comm_map, &key);
}

static __always_inline void args_put(__u32 which, const struct arg_val *v)
{
	struct arg_key k = { .id = bpf_get_current_pid_tgid(), .which = which };

	bpf_map_update_elem(&api_args, &k, v, BPF_ANY);
}

// Returns a copy and removes the entry; false when the entry probe was missed.
static __always_inline int args_take(__u32 which, struct arg_val *out)
{
	struct arg_key k = { .id = bpf_get_current_pid_tgid(), .which = which };
	struct arg_val *v = bpf_map_lookup_elem(&api_args, &k);

	if (!v)
		return 0;
	*out = *v;
	bpf_map_delete_elem(&api_args, &k);
	return 1;
}

/* ---- communicator lifetime and identity ---- */

// ncclCommInitRank(ncclComm_t* comm, int nranks, ncclUniqueId id, int myrank)
// ncclCommInitRankConfig(comm, nranks, id, myrank, config): same first 4 args.
static __always_inline int init_rank_entry(struct pt_regs *ctx)
{
	struct arg_val v = {};

	v.ptr = (__u64)PT_REGS_PARM1(ctx);
	v.nranks = (__u32)PT_REGS_PARM2(ctx);
#if defined(__TARGET_ARCH_arm64)
	v.rank = (__u32)PT_REGS_PARM4(ctx);     /* x2 holds the by-reference ncclUniqueId */
#else
	v.rank = (__u32)PT_REGS_PARM3(ctx);     /* the 128-byte id is on the stack */
#endif
	args_put(ARG_INIT, &v);
	return 0;
}

static __always_inline int init_rank_exit(int ret)
{
	struct arg_val v;
	__u64 comm = 0;

	if (!args_take(ARG_INIT, &v) || ret != 0)
		return 0;
	if (bpf_probe_read_user(&comm, sizeof(comm), (void *)v.ptr))
		return 0;
	if (v.nranks == 0 || v.rank >= v.nranks)
		return 0;
	comm_register(bpf_get_current_pid_tgid() >> 32, comm, v.rank, v.nranks);
	return 0;
}

SEC("uprobe/ncclCommInitRank")
int BPF_UPROBE(ident_initrank_entry)
{
	return init_rank_entry(ctx);
}

SEC("uretprobe/ncclCommInitRank")
int BPF_URETPROBE(ident_initrank_exit, int ret)
{
	return init_rank_exit(ret);
}

SEC("uprobe/ncclCommInitRankConfig")
int BPF_UPROBE(ident_initrankcfg_entry)
{
	return init_rank_entry(ctx);
}

SEC("uretprobe/ncclCommInitRankConfig")
int BPF_URETPROBE(ident_initrankcfg_exit, int ret)
{
	return init_rank_exit(ret);
}

// ncclCommInitAll(ncclComm_t* comms, int ndev, const int* devlist): one process,
// ndev communicators, comm i has rank i.
SEC("uprobe/ncclCommInitAll")
int BPF_UPROBE(ident_initall_entry, void *comms, int ndev)
{
	struct arg_val v = {};

	v.ptr = (__u64)comms;
	v.nranks = (__u32)ndev;
	args_put(ARG_INITALL, &v);
	return 0;
}

struct initall_ctx {
	__u64 ptr;
	__u32 nranks;
	__u32 pid;
};

/* bpf_loop callback: register communicator i of ncclCommInitAll (comm i has rank i). Returning 1 stops the loop. */
static long initall_cb(__u64 idx, void *data)
{
	struct initall_ctx *c = data;
	__u64 comm = 0;

	if ((__u32)idx >= c->nranks)
		return 1;
	if (bpf_probe_read_user(&comm, sizeof(comm), (void *)(c->ptr + idx * sizeof(comm))))
		return 1;
	comm_register(c->pid, comm, (__u32)idx, c->nranks);
	return 0;
}

SEC("uretprobe/ncclCommInitAll")
int BPF_URETPROBE(ident_initall_exit, int ret)
{
	struct arg_val v;
	struct initall_ctx c;

	if (!args_take(ARG_INITALL, &v) || ret != 0)
		return 0;
	if (v.nranks == 0 || v.nranks > INITALL_MAX)
		return 0;
	c.ptr = v.ptr;
	c.nranks = v.nranks;
	c.pid = bpf_get_current_pid_tgid() >> 32;
	/* bpf_loop (Linux 5.17+) verifies the callback body once instead of once per device: an unrolled loop with map
	 * updates in the body exceeded the verifier's limits on Linux 6.17. */
	bpf_loop(INITALL_MAX, initall_cb, &c, 0);
	return 0;
}

// ncclCommUserRank(comm, int* rank) / ncclCommCount(comm, int* nranks).
static __always_inline int rank_count_entry(__u32 which, void *comm, void *out)
{
	struct arg_val v = {};

	v.comm = (__u64)comm;
	v.ptr = (__u64)out;
	args_put(which, &v);
	return 0;
}

static __always_inline int rank_count_exit(__u32 which, int ret)
{
	struct arg_val v;
	struct comm_info *ci;
	__s32 val = 0;

	if (!args_take(which, &v) || ret != 0)
		return 0;
	if (bpf_probe_read_user(&val, sizeof(val), (void *)v.ptr) || val < 0)
		return 0;
	ci = comm_get(bpf_get_current_pid_tgid() >> 32, v.comm);
	if (!ci)
		return 0;
	if (which == ARG_RANK)
		ci->rank = (__u32)val;
	else
		ci->world = (__u32)val;
	return 0;
}

SEC("uprobe/ncclCommUserRank")
int BPF_UPROBE(ident_userrank_entry, void *comm, void *rank)
{
	return rank_count_entry(ARG_RANK, comm, rank);
}

SEC("uretprobe/ncclCommUserRank")
int BPF_URETPROBE(ident_userrank_exit, int ret)
{
	return rank_count_exit(ARG_RANK, ret);
}

SEC("uprobe/ncclCommCount")
int BPF_UPROBE(ident_count_entry, void *comm, void *nranks)
{
	return rank_count_entry(ARG_COUNT, comm, nranks);
}

SEC("uretprobe/ncclCommCount")
int BPF_URETPROBE(ident_count_exit, int ret)
{
	return rank_count_exit(ARG_COUNT, ret);
}

static __always_inline void comm_forget(__u64 comm)
{
	struct comm_key key = { .pid = bpf_get_current_pid_tgid() >> 32, .comm = comm };

	bpf_map_delete_elem(&comm_map, &key);
}

SEC("uprobe/ncclCommDestroy")
int BPF_UPROBE(ident_destroy, void *comm)
{
	comm_forget((__u64)comm);
	return 0;
}

SEC("uprobe/ncclCommAbort")
int BPF_UPROBE(ident_abort, void *comm)
{
	comm_forget((__u64)comm);
	return 0;
}

/* ---- collectives ---- */

static __always_inline int coll_entry(__u8 op, __u64 count, __u64 datatype, void *comm)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct coll_call call = {};
	struct comm_info *ci = comm_get(id >> 32, (__u64)comm);

	call.start_ns = bpf_ktime_get_ns();
	call.bytes = count * nccl_dtype_size(datatype);
	call.comm = (__u64)comm;
	call.op = op;
	if (ci) {
		ci->seq++;
		call.seq = ci->seq;
		call.rank = ci->rank;
		call.world = ci->world;
		call.ordinal = ci->ordinal;
		call.flags = ci->flags;
	} else {
		call.rank = FABRIC_RANK_UNKNOWN;
	}
	bpf_map_update_elem(&coll_inflight, &id, &call, BPF_ANY);
	return 0;
}

static __always_inline int coll_exit(int ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct coll_call *call = bpf_map_lookup_elem(&coll_inflight, &id);
	struct coll_call c;
	struct fabric_signal *ev;
	__u32 key = 0, *every;
	__u64 now;

	if (!call)
		return 0;
	c = *call;
	bpf_map_delete_elem(&coll_inflight, &id);

	/* ncclResult_t != ncclSuccess: not a completed collective. */
	if (ret != 0 || c.seq == 0 || c.ordinal == 0)
		return 0;
	now = bpf_ktime_get_ns();
	if (now <= c.start_ns)
		return 0;
	every = bpf_map_lookup_elem(&collective_cfg, &key);
	if (every && *every > 1 && (c.seq % *every) != 0)
		return 0;

	ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
	if (!ev) {
		GRYVIA_COUNT_DROP(GRYVIA_DROP_RINGBUF);
		return 0;
	}
	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = now;
	ev->pid = id >> 32;
	ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
	ev->signal_type = FABRIC_SIG_COLLECTIVE;
	ev->nccl_op = c.op;
	ev->rank = c.rank;
	ev->world_size = c.world;
	ev->peer_rank = c.ordinal;
	ev->latency_ns = now - c.start_ns;
	ev->peer_latency_ns = c.seq;
	ev->bytes = c.bytes;
	ev->retry_count = c.flags;
	if (c.rank == FABRIC_RANK_UNKNOWN)
		ev->retry_count |= COLL_FLAG_RANK_UNK;
	ev->rnr_count = (__u32)c.comm;
	bpf_get_current_comm(&ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
	return 0;
}

// ncclAllReduce(sendbuff, recvbuff, count, datatype, op, comm, stream)
SEC("uprobe/ncclAllReduce")
int BPF_UPROBE(coll_allreduce_entry, void *s, void *r, __u64 count, __u64 dt, __u64 op, void *comm)
{
	return coll_entry(NCCL_ALLREDUCE, count, dt, comm);
}

SEC("uretprobe/ncclAllReduce")
int BPF_URETPROBE(coll_allreduce_exit, int ret)
{
	return coll_exit(ret);
}

// ncclBroadcast(sendbuff, recvbuff, count, datatype, root, comm, stream)
SEC("uprobe/ncclBroadcast")
int BPF_UPROBE(coll_broadcast_entry, void *s, void *r, __u64 count, __u64 dt, __u64 root, void *comm)
{
	return coll_entry(NCCL_BROADCAST, count, dt, comm);
}

SEC("uretprobe/ncclBroadcast")
int BPF_URETPROBE(coll_broadcast_exit, int ret)
{
	return coll_exit(ret);
}

// ncclAllGather(sendbuff, recvbuff, sendcount, datatype, comm, stream)
SEC("uprobe/ncclAllGather")
int BPF_UPROBE(coll_allgather_entry, void *s, void *r, __u64 count, __u64 dt, void *comm)
{
	return coll_entry(NCCL_ALLGATHER, count, dt, comm);
}

SEC("uretprobe/ncclAllGather")
int BPF_URETPROBE(coll_allgather_exit, int ret)
{
	return coll_exit(ret);
}

// ncclReduceScatter(sendbuff, recvbuff, recvcount, datatype, op, comm, stream)
SEC("uprobe/ncclReduceScatter")
int BPF_UPROBE(coll_reducescatter_entry, void *s, void *r, __u64 count, __u64 dt, __u64 op, void *comm)
{
	return coll_entry(NCCL_REDUCESCATTER, count, dt, comm);
}

SEC("uretprobe/ncclReduceScatter")
int BPF_URETPROBE(coll_reducescatter_exit, int ret)
{
	return coll_exit(ret);
}

// ncclAlltoAll(sendbuff, recvbuff, count, datatype, comm, stream), NCCL >= 2.28
SEC("uprobe/ncclAlltoAll")
int BPF_UPROBE(coll_alltoall_entry, void *s, void *r, __u64 count, __u64 dt, void *comm)
{
	return coll_entry(NCCL_ALLTOALL, count, dt, comm);
}

SEC("uretprobe/ncclAlltoAll")
int BPF_URETPROBE(coll_alltoall_exit, int ret)
{
	return coll_exit(ret);
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
