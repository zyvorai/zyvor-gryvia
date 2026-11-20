// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// weight_exfil.c - large read of a model-weight file, then a connect to an
// external IPv4 address from the same process.  Observe only.
//
// kprobe vfs_read: a read request of at least EXFIL_MIN_READ (8 MiB) from a
// file whose NAME ends in a model-weight extension (.safetensors .gguf .ckpt
// .onnx .pt .pth .bin .h5) marks the process (tgid) with the time and adds the
// requested bytes.  kprobe tcp_v4_connect: if the same tgid has a mark newer
// than EXFIL_WINDOW_NS and the destination is not internal, one
// FABRIC_SIG_EXFIL fabric_signal is emitted and the mark is dropped (one
// event per read burst).  A read alone never produces a signal, and a connect
// to loopback / RFC1918 / link-local (or 0.0.0.0/8) neither fires nor spends
// the mark.
//
// The destination comes from the connect ARGUMENT (uaddr): the socket's
// skc_daddr is only filled in later inside tcp_v4_connect.
//
// Scope and limits: a heuristic, not proof of theft.  Only read()-style access
// is seen (mmap'd weights never enter vfs_read); the file is matched by name,
// so an unusual extension is missed; IPv6 (tcp_v6_connect) is not covered; a
// model server that legitimately pulls weights and calls a cloud API will
// trip it.  State is a bounded LRU keyed by tgid.  Userspace decides what to
// do with the signal; this program changes nothing.

#include "headers/fabric_signal.h"

#define EXFIL_MIN_READ   (8ULL * 1024 * 1024)
#define EXFIL_WINDOW_NS  30000000000ULL         /* 30 s */
#define EXFIL_MAX_PROCS  4096
#define WEIGHT_TAIL      12                     /* strlen(".safetensors") */

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, EXFIL_MAX_PROCS);
	__type(key, __u32);                     /* tgid */
	__type(value, struct exfil_mark);
} big_read SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

GRYVIA_DECLARE_DROPS();

// True when the last `elen` bytes of the `n`-byte tail equal the constant ext.
#define TAIL_EQ(tail, n, ext, elen) ({					\
	int __ok = ((n) >= (elen));					\
	int __i;							\
	_Pragma("unroll")						\
	for (__i = 0; __i < (elen); __i++) {				\
		if (__ok && (tail)[((n) - (elen) + __i) & 15] != (ext)[__i]) \
			__ok = 0;					\
	}								\
	__ok;								\
})

static __always_inline int is_weight_file(struct file *file)
{
	struct dentry *d = BPF_CORE_READ(file, f_path.dentry);
	const unsigned char *name;
	char tail[16] = {};
	__u32 len, n;

	if (!d)
		return 0;
	len = BPF_CORE_READ(d, d_name.len);
	name = BPF_CORE_READ(d, d_name.name);
	if (!name || len < 3)
		return 0;
	n = len < WEIGHT_TAIL ? len : WEIGHT_TAIL;
	if (bpf_probe_read_kernel(tail, n & 15, name + len - n))
		return 0;

	return TAIL_EQ(tail, n, ".safetensors", 12) || TAIL_EQ(tail, n, ".gguf", 5) ||
	       TAIL_EQ(tail, n, ".ckpt", 5) || TAIL_EQ(tail, n, ".onnx", 5) ||
	       TAIL_EQ(tail, n, ".pth", 4) || TAIL_EQ(tail, n, ".bin", 4) ||
	       TAIL_EQ(tail, n, ".pt", 3) || TAIL_EQ(tail, n, ".h5", 3);
}

// RFC1918, loopback, link-local and "this network" (0.0.0.0/8); ip is host order.
static __always_inline int ipv4_internal(__u32 ip)
{
	return (ip >> 24) == 10 ||                      /* 10.0.0.0/8 */
	       (ip & 0xFFF00000) == 0xAC100000 ||       /* 172.16.0.0/12 */
	       (ip & 0xFFFF0000) == 0xC0A80000 ||       /* 192.168.0.0/16 */
	       (ip >> 24) == 127 ||                     /* 127.0.0.0/8 */
	       (ip & 0xFFFF0000) == 0xA9FE0000 ||       /* 169.254.0.0/16 */
	       (ip >> 24) == 0;                         /* 0.0.0.0/8 */
}

SEC("kprobe/vfs_read")
int BPF_KPROBE(exfil_vfs_read, struct file *file, void *buf, __u64 count)
{
	__u32 tgid;
	__u64 now;
	struct exfil_mark *m;

	if (count < EXFIL_MIN_READ)
		return 0;
	if (!file || !is_weight_file(file))
		return 0;

	tgid = bpf_get_current_pid_tgid() >> 32;
	now = bpf_ktime_get_ns();
	m = bpf_map_lookup_elem(&big_read, &tgid);
	if (m && now - m->last_read_ns <= EXFIL_WINDOW_NS) {
		m->last_read_ns = now;
		__sync_fetch_and_add(&m->bytes, count);
	} else {
		struct exfil_mark fresh = {};

		fresh.last_read_ns = now;
		fresh.bytes = count;
		bpf_map_update_elem(&big_read, &tgid, &fresh, BPF_ANY);
	}
	return 0;
}

SEC("kprobe/tcp_v4_connect")
int BPF_KPROBE(exfil_connect, struct sock *sk, struct sockaddr *uaddr)
{
	__u32 tgid = bpf_get_current_pid_tgid() >> 32;
	struct exfil_mark *m = bpf_map_lookup_elem(&big_read, &tgid);
	struct fabric_signal *ev;
	__u64 now = bpf_ktime_get_ns();
	__u64 gap, bytes;
	__u32 ip;
	__u16 port;

	if (!m)
		return 0;
	if (now - m->last_read_ns > EXFIL_WINDOW_NS) {
		bpf_map_delete_elem(&big_read, &tgid); /* stale mark */
		return 0;
	}
	if (gryvia_uaddr_v4(uaddr, &ip, &port))
		return 0;
	ip = bpf_ntohl(ip);
	if (ipv4_internal(ip))
		return 0;

	gap = now - m->last_read_ns;
	bytes = m->bytes;
	bpf_map_delete_elem(&big_read, &tgid);

	ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
	if (!ev) {
		GRYVIA_COUNT_DROP(GRYVIA_DROP_RINGBUF);
		return 0;
	}
	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = now;
	ev->pid = tgid;
	ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
	ev->signal_type = FABRIC_SIG_EXFIL;
	ev->latency_ns = gap;
	ev->bytes = bytes;
	ev->rank = port;
	ev->peer_rank = ip;
	bpf_get_current_comm(&ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
