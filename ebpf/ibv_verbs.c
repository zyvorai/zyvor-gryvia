// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// ibv_verbs.c - libibverbs control-path counters seen from userspace.
//
// NCCL posts work requests and rings doorbells from userspace: ibv_post_send /
// ibv_post_recv / ibv_poll_cq are `static inline` wrappers in <infiniband/verbs.h>
// that call function pointers stored in the device context, so there is NO
// exported symbol to probe on the data path, and the kernel ib_post_send /
// ib_poll_cq kprobes of rdma_trace.c / rdma_health.c never fire for NCCL.
// Data-path visibility therefore has to come from the NIC hardware counters
// (collector/pkg/nic, -nic-counters).
//
// What IS attachable are the exported CONTROL-path functions of libibverbs.so:
//   ibv_create_qp        successful returns   -> queue pairs created
//   ibv_destroy_qp       returns of 0         -> queue pairs destroyed
//   ibv_reg_mr           successful returns   -> memory regions registered, and
//   ibv_reg_mr_iova2     (same, newer rdma-core) the bytes registered
// Node-wide counters only (a per-CPU array, polled by the collector like
// roce_cnp.c); no ring buffer, no per-process state beyond one in-flight
// length per thread.  Useful for connection-count and pinned-memory churn
// (a job that keeps re-registering buffers, QP counts that do not match the
// expected topology); it says nothing about retries or bandwidth.
//
// ibv_modify_qp (retry_cnt / timeout / rnr_retry attributes) is deliberately NOT
// probed: reading struct ibv_qp_attr needs the rdma-core header layout, which
// is not part of this build, and a guessed offset would report wrong values.
//
// Opt-in: the collector only attaches this object with -ibverbs-probes.  A
// missing symbol (versioned or renamed) is a skip, not an error.
// UNVERIFIED against real libibverbs: tested with a stand-in library exporting
// unversioned symbols (ebpf/test/collective_ident/README in docs).

#include "headers/fabric_signal.h"

#define MAX_THREADS  65536

/* Slots of ibv_counts (Go mirror: collector/main.go). */
#define IBV_SLOT_QP_CREATED    0
#define IBV_SLOT_QP_DESTROYED  1
#define IBV_SLOT_MR_REGISTERED 2
#define IBV_SLOT_MR_BYTES      3
#define IBV_SLOTS              4

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, IBV_SLOTS);
	__type(key, __u32);
	__type(value, __u64);
} ibv_counts SEC(".maps");

// length of the ibv_reg_mr call in flight on a thread (LRU: a missed return is recycled).
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_THREADS);
	__type(key, __u64);
	__type(value, __u64);
} reg_len SEC(".maps");

static __always_inline void bump(__u32 slot, __u64 n)
{
	__u64 *v = bpf_map_lookup_elem(&ibv_counts, &slot);

	if (v)
		*v += n;
}

// struct ibv_qp *ibv_create_qp(struct ibv_pd *pd, struct ibv_qp_init_attr *attr)
SEC("uretprobe/ibv_create_qp")
int BPF_URETPROBE(ibv_create_qp_exit, void *qp)
{
	if (qp)
		bump(IBV_SLOT_QP_CREATED, 1);
	return 0;
}

// int ibv_destroy_qp(struct ibv_qp *qp)
SEC("uretprobe/ibv_destroy_qp")
int BPF_URETPROBE(ibv_destroy_qp_exit, int ret)
{
	if (ret == 0)
		bump(IBV_SLOT_QP_DESTROYED, 1);
	return 0;
}

static __always_inline int reg_entry(__u64 length)
{
	__u64 id = bpf_get_current_pid_tgid();

	bpf_map_update_elem(&reg_len, &id, &length, BPF_ANY);
	return 0;
}

static __always_inline int reg_exit(void *mr)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u64 *len = bpf_map_lookup_elem(&reg_len, &id);

	if (!len)
		return 0;
	if (mr) {
		bump(IBV_SLOT_MR_REGISTERED, 1);
		bump(IBV_SLOT_MR_BYTES, *len);
	}
	bpf_map_delete_elem(&reg_len, &id);
	return 0;
}

// struct ibv_mr *ibv_reg_mr(struct ibv_pd *pd, void *addr, size_t length, int access)
SEC("uprobe/ibv_reg_mr")
int BPF_UPROBE(ibv_reg_mr_entry, void *pd, void *addr, __u64 length)
{
	return reg_entry(length);
}

SEC("uretprobe/ibv_reg_mr")
int BPF_URETPROBE(ibv_reg_mr_exit, void *mr)
{
	return reg_exit(mr);
}

// struct ibv_mr *ibv_reg_mr_iova2(pd, addr, length, iova, access)
SEC("uprobe/ibv_reg_mr_iova2")
int BPF_UPROBE(ibv_reg_mr_iova2_entry, void *pd, void *addr, __u64 length)
{
	return reg_entry(length);
}

SEC("uretprobe/ibv_reg_mr_iova2")
int BPF_URETPROBE(ibv_reg_mr_iova2_exit, void *mr)
{
	return reg_exit(mr);
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
