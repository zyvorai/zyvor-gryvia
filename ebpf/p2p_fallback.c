// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// p2p_fallback.c - observe CUDA peer access and flag a likely host bounce.
//
// cudaDeviceEnablePeerAccess(peer, flags) returning non-zero means this
// device cannot DMA to `peer`.  Large cudaMemcpyAsync D2D (kind == 3) that
// follows a failed enable is the usual "GPUs look busy, step time doubled"
// pattern: NCCL or the framework fell back to a staging buffer in host RAM.
//
// Thresholds are uncalibrated heuristics, same as straggler.c.  Observe only.
// cudaMemcpyDefault (kind 4) is ignored: direction is unknown.

#include "headers/fabric_signal.h"

#define CUDA_MEMCPY_D2D 3
/* cudaErrorPeerAccessAlreadyEnabled: frameworks call cudaDeviceEnablePeerAccess repeatedly and ignore
 * this code, so it must not count as "this device cannot reach the peer". */
#define CUDA_ERR_PEER_ALREADY_ENABLED 704
#define P2P_FAIL_WINDOW_NS (30ULL * 1000000000ULL)
#define P2P_BIG_COPY (8ULL * 1024 * 1024)
#define MAX_PIDS 2048

struct p2p_state {
	__u64 fail_ns;
	__u32 fail_peer;
	__u32 fails;
};

struct copy_call {
	__u64 start_ns;
	__u64 bytes;
	__u32 kind;
	__u32 _pad;
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_PIDS);
	__type(key, __u32);
	__type(value, struct p2p_state);
} p2p_fail SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_PIDS);
	__type(key, __u64);
	__type(value, struct copy_call);
} copy_inflight SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

SEC("uretprobe/cudaDeviceEnablePeerAccess")
int BPF_URETPROBE(peer_enable_ret, int ret)
{
	__u32 pid = bpf_get_current_pid_tgid() >> 32;
	struct p2p_state *old;
	struct p2p_state st = {};

	if (ret == 0 || ret == CUDA_ERR_PEER_ALREADY_ENABLED)
		return 0;
	old = bpf_map_lookup_elem(&p2p_fail, &pid);
	st.fails = old ? old->fails + 1 : 1;
	st.fail_ns = bpf_ktime_get_ns();
	bpf_map_update_elem(&p2p_fail, &pid, &st, BPF_ANY);
	return 0;
}

/* cudaError_t cudaMemcpyAsync(void* dst, const void* src, size_t count,
 *                              enum cudaMemcpyKind kind, cudaStream_t stream)
 */
SEC("uprobe/cudaMemcpyAsync")
int BPF_UPROBE(memcpy_async, void *dst, void *src, __u64 count, __u64 kind)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct copy_call c = {};

	(void)dst;
	(void)src;
	/* kind is a 32-bit enum; the upper half of its register is undefined. */
	if ((__u32)kind != CUDA_MEMCPY_D2D || count < P2P_BIG_COPY)
		return 0;
	c.start_ns = bpf_ktime_get_ns();
	c.bytes = count;
	c.kind = (__u32)kind;
	bpf_map_update_elem(&copy_inflight, &id, &c, BPF_ANY);
	return 0;
}

SEC("uretprobe/cudaMemcpyAsync")
int BPF_URETPROBE(memcpy_async_ret, int ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 pid = id >> 32;
	struct copy_call *c;
	struct p2p_state *st;
	struct fabric_signal *ev;
	__u64 now;

	c = bpf_map_lookup_elem(&copy_inflight, &id);
	if (!c)
		return 0;
	now = bpf_ktime_get_ns();
	st = bpf_map_lookup_elem(&p2p_fail, &pid);
	if (ret == 0 && st && st->fail_ns && now - st->fail_ns < P2P_FAIL_WINDOW_NS) {
		ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
		if (ev) {
			__builtin_memset(ev, 0, sizeof(*ev));
			ev->timestamp_ns = now;
			ev->pid = pid;
			ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
			ev->signal_type = FABRIC_SIG_P2P_FALLBACK;
			ev->latency_ns = now - c->start_ns;
			ev->bytes = c->bytes;
			ev->retry_count = st->fails;
			ev->peer_latency_ns = now - st->fail_ns;
			bpf_get_current_comm(ev->comm, sizeof(ev->comm));
			bpf_ringbuf_submit(ev, 0);
		}
	}
	bpf_map_delete_elem(&copy_inflight, &id);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
