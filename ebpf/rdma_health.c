// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// rdma_health.c - QP retry / RNR signal for the scheduler.
//
// Counts send posts per QP and looks at the work completions returned by the
// CQ poll path for IB_WC_RETRY_EXC_ERR / IB_WC_RNR_RETRY_EXC_ERR.  When the
// error count of a QP reaches a threshold it emits one fabric_signal and
// restarts the window for that QP.
//
// ib_post_send()/ib_poll_cq() are `static inline` in include/rdma/ib_verbs.h,
// so they only exist as kprobe targets where the compiler emitted an
// out-of-line copy.  The driver entry points (mlx5_ib_post_send,
// mlx5_ib_poll_cq) are hooked as well; every one of the four kprobes is
// optional and the collector skips the ones whose symbol is absent.  When both
// the inline wrapper and the mlx5 function exist, `posted` counts a call
// twice; it only feeds a ratio.  Only in-kernel RDMA consumers (NFS-RDMA,
// SRP, ...) reach these paths; user-space verbs (NCCL) post through the NIC
// doorbell and are not visible here.
//
// Thresholds live in rdma_thresh so the operator can change them without
// rebuilding:
//   [0] retry threshold (default 8)
//   [1] rnr threshold   (default 4)

#include "headers/fabric_signal.h"

#define MAX_QPS          16384
#define MAX_INFLIGHT     65536
#define RDMA_MAX_WC      16     /* completions inspected per poll */
#define RETRY_THRESH_DEF 8
#define RNR_THRESH_DEF   4

// Per-QP health.  LRU: QPs that vanish age out.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_QPS);
	__type(key, __u32); /* qp_num */
	__type(value, struct rdma_health_val);
} rdma_health_map SEC(".maps");

// ib_wc array pointer of a poll in flight, keyed by pid_tgid (LRU: no leak if
// the exit probe is missed).
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_INFLIGHT);
	__type(key, __u64);
	__type(value, __u64);
} rdma_poll_wc SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 2);
	__type(key, __u32);
	__type(value, __u32);
} rdma_thresh SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

GRYVIA_DECLARE_DROPS();

static __always_inline __u32 thresh(__u32 idx, __u32 def)
{
	__u32 *v = bpf_map_lookup_elem(&rdma_thresh, &idx);

	if (!v || *v == 0)
		return def;
	return *v;
}

static __always_inline void emit_rdma(__u32 qpn, struct rdma_health_val *v)
{
	struct fabric_signal *ev;
	__u64 now = bpf_ktime_get_ns();

	ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
	if (!ev) {
		GRYVIA_COUNT_DROP(GRYVIA_DROP_RINGBUF);
		return; /* keep the counts; retry on the next error */
	}

	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = now;
	ev->pid = v->owner_pid;
	ev->cgroup_id_lo = v->cgroup_lo;
	ev->signal_type = FABRIC_SIG_RDMA_RETRY;
	ev->rank = qpn;
	ev->world_size = v->posted;
	ev->retry_count = v->retries;
	ev->rnr_count = v->rnr;
	ev->latency_ns = now > v->last_send_ns ? now - v->last_send_ns : 0;
	__builtin_memcpy(ev->comm, v->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);

	v->posted = 0;
	v->retries = 0;
	v->rnr = 0;
}

static __always_inline int post_send(struct ib_qp *qp)
{
	struct rdma_health_val *v, init = {};
	__u32 qpn;

	if (!qp)
		return 0;
	qpn = BPF_CORE_READ(qp, qp_num);
	if (!qpn)
		return 0;
	v = bpf_map_lookup_elem(&rdma_health_map, &qpn);
	if (!v) {
		init.last_send_ns = bpf_ktime_get_ns();
		init.owner_pid = bpf_get_current_pid_tgid() >> 32;
		init.cgroup_lo = (__u32)bpf_get_current_cgroup_id();
		init.posted = 1;
		bpf_get_current_comm(&init.comm, sizeof(init.comm));
		bpf_map_update_elem(&rdma_health_map, &qpn, &init, BPF_NOEXIST);
		return 0;
	}
	__sync_fetch_and_add(&v->posted, 1);
	v->last_send_ns = bpf_ktime_get_ns();
	return 0;
}

static __always_inline int poll_enter(struct ib_wc *wc)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u64 p = (__u64)wc;

	if (!p)
		return 0;
	bpf_map_update_elem(&rdma_poll_wc, &id, &p, BPF_ANY);
	return 0;
}

// Inspect the completions of a finished poll (ret = number returned).
static __always_inline int poll_exit(long ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u64 *wcp = bpf_map_lookup_elem(&rdma_poll_wc, &id);
	__u64 wcsz = bpf_core_type_size(struct ib_wc);
	__u64 base;
	int n;

	if (!wcp)
		return 0;
	base = *wcp;
	bpf_map_delete_elem(&rdma_poll_wc, &id);
	if (ret <= 0 || !base || !wcsz)
		return 0;
	n = ret > RDMA_MAX_WC ? RDMA_MAX_WC : ret;

	_Pragma("unroll")
	for (int i = 0; i < RDMA_MAX_WC; i++) {
		struct rdma_health_val *v;
		struct ib_wc *w;
		struct ib_qp *qp;
		enum ib_wc_status status;
		__u32 qpn;

		if (i >= n)
			break;
		w = (struct ib_wc *)(base + (__u64)i * wcsz);
		status = BPF_CORE_READ(w, status);
		if (status != IB_WC_RETRY_EXC_ERR &&
		    status != IB_WC_RNR_RETRY_EXC_ERR)
			continue;
		qp = BPF_CORE_READ(w, qp);
		if (!qp)
			continue;
		qpn = BPF_CORE_READ(qp, qp_num);
		v = bpf_map_lookup_elem(&rdma_health_map, &qpn);
		if (!v)
			continue; /* QP whose posts were not seen */
		if (status == IB_WC_RETRY_EXC_ERR)
			__sync_fetch_and_add(&v->retries, 1);
		else
			__sync_fetch_and_add(&v->rnr, 1);
		if (v->retries >= thresh(0, RETRY_THRESH_DEF) ||
		    v->rnr >= thresh(1, RNR_THRESH_DEF))
			emit_rdma(qpn, v);
	}
	return 0;
}

/*
 * int ib_post_send(struct ib_qp *qp, const struct ib_send_wr *wr,
 *                  const struct ib_send_wr **bad_wr)
 * mlx5_ib_post_send() has the same signature.
 */
SEC("kprobe/ib_post_send")
int BPF_KPROBE(rdma_post_send, struct ib_qp *qp)
{
	return post_send(qp);
}

SEC("kprobe/mlx5_ib_post_send")
int BPF_KPROBE(rdma_mlx5_post_send, struct ib_qp *qp)
{
	return post_send(qp);
}

/*
 * int ib_poll_cq(struct ib_cq *cq, int num_entries, struct ib_wc *wc)
 * mlx5_ib_poll_cq() has the same signature.
 */
SEC("kprobe/ib_poll_cq")
int BPF_KPROBE(rdma_poll_cq_entry, void *cq, int num_entries, struct ib_wc *wc)
{
	return poll_enter(wc);
}

SEC("kretprobe/ib_poll_cq")
int BPF_KRETPROBE(rdma_poll_cq_exit, int ret)
{
	return poll_exit(ret);
}

SEC("kprobe/mlx5_ib_poll_cq")
int BPF_KPROBE(rdma_mlx5_poll_cq_entry, void *cq, int num_entries, struct ib_wc *wc)
{
	return poll_enter(wc);
}

SEC("kretprobe/mlx5_ib_poll_cq")
int BPF_KRETPROBE(rdma_mlx5_poll_cq_exit, int ret)
{
	return poll_exit(ret);
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
