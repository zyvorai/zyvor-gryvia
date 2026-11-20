// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// overlap.c - GPU sync while an NCCL collective is in flight.
//
// A cudaDeviceSynchronize() that runs nested inside an in-flight
// ncclAllReduce() on the same thread means the host is blocked on the GPU
// while communication is outstanding: the GPU is idle-while-comm from the
// scheduler's point of view.  When such a sync returns and took at least
// OVERLAP_MIN_NS, one FABRIC_SIG_OVERLAP fabric_signal is emitted
// (latency_ns = sync duration, retry_count = nested ncclAllReduce depth).
//
// State is per thread (pid_tgid), so entry and exit pair on the same thread
// and nesting is counted exactly: ncclAllReduce entry/exit adjust
// nccl_inflight, cudaDeviceSynchronize entry/exit adjust sync_depth, and the
// map element is deleted on every path that leaves both counters at zero.
// A sync that starts outside any ncclAllReduce creates no state at all.
//
// Scope: only the same-thread nesting is visible.  NCCL launched
// asynchronously from one thread and synchronised from another is not seen.
// Observe-only.  Needs libnccl.so and libcudart.so (same resolver as
// straggler.c); the collector skips the probes it cannot resolve.

#include "headers/gpu_common.h"
#include "headers/fabric_signal.h"

#define OVERLAP_MIN_NS   1000000ULL     /* 1 ms */
#define MAX_THREADS      65536

// LRU so entries orphaned by a missed uretprobe (or a dead thread) are recycled.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_THREADS);
	__type(key, __u64); /* pid_tgid */
	__type(value, struct overlap_state);
} overlap_state_map SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

GRYVIA_DECLARE_DROPS();

static __always_inline struct overlap_state *get_state(__u64 id)
{
	struct overlap_state init = {};
	struct overlap_state *st = bpf_map_lookup_elem(&overlap_state_map, &id);

	if (st)
		return st;
	/* Only this thread writes its own key; NOEXIST keeps it race-free anyway. */
	bpf_map_update_elem(&overlap_state_map, &id, &init, BPF_NOEXIST);
	return bpf_map_lookup_elem(&overlap_state_map, &id);
}

static __always_inline void drop_if_idle(__u64 id, const struct overlap_state *st)
{
	if (st->nccl_inflight == 0 && st->sync_depth == 0)
		bpf_map_delete_elem(&overlap_state_map, &id);
}

SEC("uprobe/ncclAllReduce")
int BPF_UPROBE(overlap_allreduce_entry)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct overlap_state *st = get_state(id);

	if (st && st->nccl_inflight < 0xffff)
		st->nccl_inflight++;
	return 0;
}

// Runs for every return, successful or not, so the counter always unwinds.
SEC("uretprobe/ncclAllReduce")
int BPF_URETPROBE(overlap_allreduce_exit, int ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct overlap_state *st = bpf_map_lookup_elem(&overlap_state_map, &id);

	if (!st)
		return 0;
	if (st->nccl_inflight > 0)
		st->nccl_inflight--;
	drop_if_idle(id, st);
	return 0;
}

SEC("uprobe/cudaDeviceSynchronize")
int BPF_UPROBE(overlap_sync_entry)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct overlap_state *st = bpf_map_lookup_elem(&overlap_state_map, &id);

	/* Not nested inside an ncclAllReduce: nothing to track. */
	if (!st || st->nccl_inflight == 0)
		return 0;
	if (st->sync_depth == 0)
		st->sync_start_ns = bpf_ktime_get_ns();
	if (st->sync_depth < 0xffff)
		st->sync_depth++;
	return 0;
}

SEC("uretprobe/cudaDeviceSynchronize")
int BPF_URETPROBE(overlap_sync_exit, int ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct overlap_state *st = bpf_map_lookup_elem(&overlap_state_map, &id);
	struct fabric_signal *ev;
	__u64 start, now, lat;
	__u32 depth;

	if (!st || st->sync_depth == 0)
		return 0;
	st->sync_depth--;
	if (st->sync_depth > 0)
		return 0; /* inner sync of a nested pair: the outermost one reports */

	start = st->sync_start_ns;
	depth = st->nccl_inflight;
	st->sync_start_ns = 0;
	drop_if_idle(id, st); /* st is not used past this point */

	now = bpf_ktime_get_ns();
	if (depth == 0 || start == 0 || now <= start)
		return 0;
	lat = now - start;
	if (lat < OVERLAP_MIN_NS)
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
	ev->signal_type = FABRIC_SIG_OVERLAP;
	ev->nccl_op = NCCL_ALLREDUCE;
	ev->latency_ns = lat;
	ev->retry_count = depth;
	bpf_get_current_comm(&ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
