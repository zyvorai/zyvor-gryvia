// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// gpu_oom.c - cudaMalloc / cudaMallocAsync failure.
//
// cudaErrorMemoryAllocation is 2.  A non-zero return on the malloc family
// emits FABRIC_SIG_GPU_OOM.  Bytes is the requested size.  Observe only.
// malloc_stats counts completed calls per kind (0 malloc, 1 async, 2 pool).
//
// Not proof that the job failed: PyTorch's caching allocator treats a cudaMalloc
// out-of-memory as routine, frees cached blocks and retries.  A burst of these
// is memory pressure; one is not an incident.

#include "headers/fabric_signal.h"

#define CUDA_ERROR_OUT_OF_MEMORY 2
#define MAX_INFLIGHT 4096

struct alloc_call {
	__u64 bytes;
	__u32 kind; /* 0 malloc, 1 async, 2 pool */
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_INFLIGHT);
	__type(key, __u64);
	__type(value, struct alloc_call);
} alloc_inflight SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 4);
	__type(key, __u32);
	__type(value, __u64);
} malloc_stats SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

static __always_inline int note(__u64 bytes, __u32 kind)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct alloc_call c = { .bytes = bytes, .kind = kind };

	bpf_map_update_elem(&alloc_inflight, &id, &c, BPF_ANY);
	return 0;
}

static __always_inline int done(int ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct alloc_call *c = bpf_map_lookup_elem(&alloc_inflight, &id);
	__u32 slot;
	__u64 *v;
	struct fabric_signal *ev;

	if (!c)
		return 0;
	slot = c->kind;
	v = bpf_map_lookup_elem(&malloc_stats, &slot);
	if (v)
		*v += 1;
	if (ret == CUDA_ERROR_OUT_OF_MEMORY) {
		ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
		if (ev) {
			__builtin_memset(ev, 0, sizeof(*ev));
			ev->timestamp_ns = bpf_ktime_get_ns();
			ev->pid = id >> 32;
			ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
			ev->signal_type = FABRIC_SIG_GPU_OOM;
			ev->bytes = c->bytes;
			ev->retry_count = c->kind;
			bpf_get_current_comm(ev->comm, sizeof(ev->comm));
			bpf_ringbuf_submit(ev, 0);
		}
	}
	bpf_map_delete_elem(&alloc_inflight, &id);
	return 0;
}

SEC("uprobe/cudaMalloc")
int BPF_UPROBE(cuda_malloc, void *devPtr, __u64 size)
{
	(void)devPtr;
	return note(size, 0);
}
SEC("uretprobe/cudaMalloc")
int BPF_URETPROBE(cuda_malloc_ret, int ret) { return done(ret); }

SEC("uprobe/cudaMallocAsync")
int BPF_UPROBE(cuda_malloc_async, void *devPtr, __u64 size, void *stream)
{
	(void)devPtr;
	(void)stream;
	return note(size, 1);
}
SEC("uretprobe/cudaMallocAsync")
int BPF_URETPROBE(cuda_malloc_async_ret, int ret) { return done(ret); }

SEC("uprobe/cudaMallocFromPoolAsync")
int BPF_UPROBE(cuda_malloc_pool, void *ptr, __u64 size, void *pool, void *stream)
{
	(void)ptr;
	(void)pool;
	(void)stream;
	return note(size, 2);
}
SEC("uretprobe/cudaMallocFromPoolAsync")
int BPF_URETPROBE(cuda_malloc_pool_ret, int ret) { return done(ret); }

char LICENSE[] SEC("license") = "Dual BSD/GPL";
