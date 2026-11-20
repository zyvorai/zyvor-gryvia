// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// ucx_gloo.c - blocked UCX tag sends (non-NCCL collective transports).
//
// uprobes/uretprobes on the exported C symbols ucp_tag_send_nb and
// ucp_tag_send_nbx of libucp.so (UCX; used by OpenMPI, UCC and the NCCL UCX
// plugin).  Both are "non-blocking" calls, so the measured span is how long
// the POSTING call itself took (memory registration, rendezvous set-up, a
// stuck progress lock), not the completion of the transfer.  A span of at
// least UCX_SLOW_NS emits one FABRIC_SIG_UCX_SLOW fabric_signal
// (latency_ns = call duration, bytes = payload when known, comm = "ucx").
//
// Entry and exit pair on the same thread (pid_tgid) and the map element is
// deleted on every exit, so nothing accumulates; the map is an LRU that
// recycles entries orphaned by a missed uretprobe.
//
// Gloo is deliberately NOT probed.  Gloo is C++: its allreduce entry points
// only exist as mangled symbols (for example
// _ZN5gloo9allreduceERKNS_16AllreduceOptionsE), the mangling changes with the
// compiler and the Gloo version, and PyTorch links Gloo statically into
// libtorch_cpu.so.  A probe with a symbol baked into the section name would
// silently never attach on most systems.  Supporting it needs a configurable
// mangled symbol on the collector side; until then this object only covers UCX.
//
// Attached only when libucp.so is found (-ucx-lib, -uprobe-pid or the
// standard library directories); a missing symbol is a skip.  Observe-only.

#include "headers/fabric_signal.h"

#define UCX_SLOW_NS   5000000ULL        /* 5 ms */
#define MAX_THREADS   65536

#define UCX_SEND_NB   1
#define UCX_SEND_NBX  2

// UCP_DATATYPE_CONTIG is class 0 and the element size of ucp_dt_make_contig(n) is n << 3.
#define UCP_DATATYPE_CLASS_MASK 0x7ULL
#define UCP_DATATYPE_SHIFT      3

struct ucx_key {
	__u64 id;       /* pid_tgid */
	__u32 which;    /* UCX_SEND_*: nb and nbx can nest, keep them apart */
	__u32 _pad;
};
_Static_assert(sizeof(struct ucx_key) == 16, "ucx_key size (no implicit padding)");

struct ucx_call {
	__u64 start_ns;
	__u64 bytes;
};
_Static_assert(sizeof(struct ucx_call) == 16, "ucx_call size");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_THREADS);
	__type(key, struct ucx_key);
	__type(value, struct ucx_call);
} ucx_inflight SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

GRYVIA_DECLARE_DROPS();

static __always_inline void call_begin(__u32 which, __u64 bytes)
{
	struct ucx_key key = {};
	struct ucx_call call = {};

	key.id = bpf_get_current_pid_tgid();
	key.which = which;
	call.start_ns = bpf_ktime_get_ns();
	call.bytes = bytes;
	bpf_map_update_elem(&ucx_inflight, &key, &call, BPF_ANY);
}

static __always_inline void call_end(__u32 which)
{
	struct ucx_key key = {};
	struct ucx_call *call;
	struct fabric_signal *ev;
	__u64 now, lat, bytes;

	key.id = bpf_get_current_pid_tgid();
	key.which = which;
	call = bpf_map_lookup_elem(&ucx_inflight, &key);
	if (!call)
		return;
	now = bpf_ktime_get_ns();
	lat = now > call->start_ns ? now - call->start_ns : 0;
	bytes = call->bytes;
	bpf_map_delete_elem(&ucx_inflight, &key);
	if (lat < UCX_SLOW_NS)
		return;

	ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
	if (!ev) {
		GRYVIA_COUNT_DROP(GRYVIA_DROP_RINGBUF);
		return;
	}
	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = now;
	ev->pid = key.id >> 32;
	ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
	ev->signal_type = FABRIC_SIG_UCX_SLOW;
	ev->latency_ns = lat;
	ev->bytes = bytes;
	__builtin_memcpy(ev->comm, "ucx", 4);
	bpf_ringbuf_submit(ev, 0);
}

// ucs_status_ptr_t ucp_tag_send_nb(ucp_ep_h ep, const void *buffer, size_t count,
//                                  ucp_datatype_t datatype, ucp_tag_t tag, cb)
SEC("uprobe/ucp_tag_send_nb")
int BPF_UPROBE(ucx_send_nb_entry, void *ep, const void *buf, __u64 count, __u64 datatype)
{
	__u64 bytes = 0;

	// Contiguous datatype: count elements of (datatype >> 3) bytes; anything
	// else (iov, generic) has no cheap size, report 0.
	if ((datatype & UCP_DATATYPE_CLASS_MASK) == 0)
		bytes = count * (datatype >> UCP_DATATYPE_SHIFT);
	call_begin(UCX_SEND_NB, bytes);
	return 0;
}

// Runs for every return so the entry is always consumed.
SEC("uretprobe/ucp_tag_send_nb")
int BPF_URETPROBE(ucx_send_nb_exit, long ret)
{
	call_end(UCX_SEND_NB);
	return 0;
}

// ucs_status_ptr_t ucp_tag_send_nbx(ucp_ep_h ep, const void *buffer, size_t count,
//                                   ucp_tag_t tag, const ucp_request_param_t *param)
SEC("uprobe/ucp_tag_send_nbx")
int BPF_UPROBE(ucx_send_nbx_entry, void *ep, const void *buf, __u64 count)
{
	call_begin(UCX_SEND_NBX, count); /* bytes unless param carries its own datatype */
	return 0;
}

SEC("uretprobe/ucp_tag_send_nbx")
int BPF_URETPROBE(ucx_send_nbx_exit, long ret)
{
	call_end(UCX_SEND_NBX);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
