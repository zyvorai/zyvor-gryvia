// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// nccl_transport.c - record how an NCCL communicator was created.
//
// nccl_trace times collectives.  This program answers a different question
// the scheduler needs: did the job land on NVLink/P2P, SHM, or IB/RoCE?
//
// Hooks:
//   ncclCommInitRank / ncclCommInitRankConfig entry (count + comm pointer)
//   ncclGetUniqueId                                 (process is multi-node capable)
//
// Transport is not in the C signature.  Userspace (collector) reads
// /proc/<pid>/environ for NCCL_P2P_DISABLE, NCCL_SHM_DISABLE, NCCL_NET and
// writes the result into transport_hint[pid] before the init returns.
// If the hint is absent the signal is still emitted with transport 0
// (unknown) so the collector can fill it from the process environment
// after the fact.  Observe only.

#include "headers/fabric_signal.h"

#define NCCL_XPORT_UNKNOWN 0
#define NCCL_XPORT_P2P     1
#define NCCL_XPORT_SHM     2
#define NCCL_XPORT_NET     3
#define NCCL_XPORT_NET_IB  4
#define NCCL_XPORT_NET_ROCE 5

#define MAX_HINTS 1024

struct init_key {
	__u64 id; /* pid_tgid */
};

struct init_val {
	__u64 start_ns;
	__u32 nranks;
	__u32 rank;
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_HINTS);
	__type(key, __u32); /* pid */
	__type(value, __u32); /* NCCL_XPORT_* written by the collector */
} transport_hint SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_HINTS);
	__type(key, __u64);
	__type(value, struct init_val);
} init_inflight SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

static __always_inline int record_init(__u64 nranks, __u64 rank)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct init_val v = {};

	v.start_ns = bpf_ktime_get_ns();
	v.nranks = (__u32)nranks;
	v.rank = (__u32)rank;
	bpf_map_update_elem(&init_inflight, &id, &v, BPF_ANY);
	return 0;
}

static __always_inline int finish_init(int ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 pid = id >> 32;
	struct init_val *v;
	struct fabric_signal *ev;
	__u32 *hint;
	__u32 zero = 0;

	v = bpf_map_lookup_elem(&init_inflight, &id);
	if (!v)
		return 0;
	if (ret != 0) {
		bpf_map_delete_elem(&init_inflight, &id);
		return 0;
	}
	ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
	if (!ev) {
		bpf_map_delete_elem(&init_inflight, &id);
		return 0;
	}
	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = bpf_ktime_get_ns();
	ev->pid = pid;
	ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
	ev->signal_type = FABRIC_SIG_NCCL_XPORT;
	ev->rank = v->rank;
	ev->world_size = v->nranks;
	ev->latency_ns = ev->timestamp_ns - v->start_ns;
	hint = bpf_map_lookup_elem(&transport_hint, &pid);
	ev->retry_count = hint ? *hint : zero;
	bpf_get_current_comm(ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
	bpf_map_delete_elem(&init_inflight, &id);
	return 0;
}

/* ncclResult_t ncclCommInitRank(ncclComm_t* comm, int nranks, ncclUniqueId id, int myrank)
 * ncclUniqueId is a 128-byte struct passed BY VALUE, so `myrank` is not the 4th register on
 * x86-64: the id goes on the stack and myrank is integer arg 3.  On arm64 (AAPCS64) the id is
 * passed by reference in a register and myrank is arg 4.  Same handling as straggler.c; only
 * x86-64 has been exercised there. */
static __always_inline int init_rank_entry(struct pt_regs *ctx)
{
	__u64 nranks = PT_REGS_PARM2(ctx);
#if defined(__TARGET_ARCH_arm64)
	__u64 rank = PT_REGS_PARM4(ctx);
#else
	__u64 rank = PT_REGS_PARM3(ctx);
#endif
	return record_init((__u32)nranks, (__u32)rank);
}

SEC("uprobe/ncclCommInitRank")
int BPF_UPROBE(nccl_init_rank)
{
	return init_rank_entry(ctx);
}

SEC("uretprobe/ncclCommInitRank")
int BPF_URETPROBE(nccl_init_rank_ret, int ret)
{
	return finish_init(ret);
}

SEC("uprobe/ncclCommInitRankConfig")
int BPF_UPROBE(nccl_init_rank_cfg)
{
	return init_rank_entry(ctx);
}

SEC("uretprobe/ncclCommInitRankConfig")
int BPF_URETPROBE(nccl_init_rank_cfg_ret, int ret)
{
	return finish_init(ret);
}

/* Presence of GetUniqueId means this process expects a multi-process comm. */
SEC("uprobe/ncclGetUniqueId")
int BPF_UPROBE(nccl_get_uid, void *uid)
{
	(void)uid;
	return record_init(0, 0);
}

SEC("uretprobe/ncclGetUniqueId")
int BPF_URETPROBE(nccl_get_uid_ret, int ret)
{
	return finish_init(ret);
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
