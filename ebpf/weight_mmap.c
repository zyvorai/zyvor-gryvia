// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// weight_mmap.c - mmap of a model-weight file, then a connect to a public IPv4.
//
// weight_exfil.c only sees vfs_read; loaders mmap safetensors, so a weight file
// that is mapped never enters it.  security_mmap_file on a file whose NAME ends in
// a weight extension (the same list as weight_exfil.c) marks the process; a
// tcp_v4_connect from the same process within 30 s to a destination that is not
// RFC1918, loopback or link-local emits one FABRIC_SIG_WEIGHT_MMAP
// (latency_ns = mmap -> connect, rank = remote port, peer_rank = remote IPv4 host
// order).  A mmap alone never fires, and a mark fires at most once.
//
// Same limits as weight_exfil.c: name-based (an unusual extension is missed),
// IPv4 only, and a model server that legitimately maps weights and calls a cloud
// API trips it.  State is a bounded LRU keyed by tgid.  Observe only.

#include "headers/fabric_signal.h"

#define MMAP_WINDOW_NS  30000000000ULL /* 30 s */
#define MMAP_MAX_PROCS  4096
#define WEIGHT_TAIL     12             /* strlen(".safetensors") */

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MMAP_MAX_PROCS);
	__type(key, __u32);   /* tgid */
	__type(value, __u64); /* time of the latest weight-file mmap, ns */
} mmap_mark SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

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

// int security_mmap_file(struct file *file, unsigned long prot, unsigned long flags)
// file is NULL for anonymous mappings.
SEC("kprobe/security_mmap_file")
int BPF_KPROBE(wm_mmap_file, struct file *file)
{
	__u32 tgid;
	__u64 now;

	if (!file || !is_weight_file(file))
		return 0;
	tgid = bpf_get_current_pid_tgid() >> 32;
	now = bpf_ktime_get_ns();
	bpf_map_update_elem(&mmap_mark, &tgid, &now, BPF_ANY);
	return 0;
}

SEC("kprobe/tcp_v4_connect")
int BPF_KPROBE(wm_connect, struct sock *sk, struct sockaddr *uaddr)
{
	__u32 tgid = bpf_get_current_pid_tgid() >> 32;
	struct fabric_signal *ev;
	__u64 *mark = bpf_map_lookup_elem(&mmap_mark, &tgid);
	__u64 now = bpf_ktime_get_ns();
	__u64 gap;
	__u32 ip;
	__u16 port;

	if (!mark)
		return 0;
	if (now - *mark > MMAP_WINDOW_NS) {
		bpf_map_delete_elem(&mmap_mark, &tgid); /* stale mark */
		return 0;
	}
	if (gryvia_uaddr_v4(uaddr, &ip, &port))
		return 0;
	ip = bpf_ntohl(ip);
	if (ipv4_internal(ip))
		return 0;

	gap = now - *mark;
	bpf_map_delete_elem(&mmap_mark, &tgid);

	ev = bpf_ringbuf_reserve(&fabric_events, sizeof(*ev), 0);
	if (!ev)
		return 0;
	__builtin_memset(ev, 0, sizeof(*ev));
	ev->timestamp_ns = now;
	ev->pid = tgid;
	ev->cgroup_id_lo = (__u32)bpf_get_current_cgroup_id();
	ev->signal_type = FABRIC_SIG_WEIGHT_MMAP;
	ev->latency_ns = gap;
	ev->rank = port;
	ev->peer_rank = ip;
	bpf_get_current_comm(ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
