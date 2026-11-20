// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// gds_trace.c - GPU Direct Storage vs bounce-buffer reads.
//
// uprobes on cuFileRead/cuFileWrite (libcufile.so) time each call and count
// the bytes actually transferred.  A call is classified as direct (confirmed
// GDS) only when the optional nvidia-fs kernel hook is also seen inside the
// call on the same thread; otherwise it counts as bounce/unknown, because
// cuFile silently falls back to POSIX compat mode when GDS is unavailable.
// Only slow calls (> 2 ms) are emitted on fabric_events.
//
// The nvidia_fs_read kprobe is optional: the collector skips it, with a log
// line, when the kernel (or nvidia-fs module) does not export that symbol.

#include "headers/fabric_signal.h"

#define MAX_INFLIGHT   65536
#define GDS_SLOW_NS    2000000ULL   /* 2 ms */

// In-flight cuFile calls keyed by pid_tgid.  LRU so a missed uretprobe cannot
// fill the map.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, MAX_INFLIGHT);
	__type(key, __u64);
	__type(value, struct gds_inflight);
} gds_inflight_map SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 2); /* 0 direct (confirmed), 1 bounce/unknown */
	__type(key, __u32);
	__type(value, __u64);
} gds_bytes SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, FABRIC_RINGBUF_SIZE);
} fabric_events SEC(".maps");

GRYVIA_DECLARE_DROPS();

static __always_inline void add_bytes(__u32 slot, __u64 n)
{
	__u64 *v = bpf_map_lookup_elem(&gds_bytes, &slot);

	if (v)
		__sync_fetch_and_add(v, n);
}

static __always_inline int gds_enter(__u64 size)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct gds_inflight inf = {};

	inf.start_ns = bpf_ktime_get_ns();
	inf.size = size;
	bpf_map_update_elem(&gds_inflight_map, &id, &inf, BPF_ANY);
	return 0;
}

// ret is the ssize_t result of cuFileRead/cuFileWrite: bytes transferred, or
// a negative value on error.
static __always_inline int gds_exit(long ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct gds_inflight *inf;
	struct fabric_signal *ev;
	__u64 now, lat, bytes;
	__u32 flags;

	inf = bpf_map_lookup_elem(&gds_inflight_map, &id);
	if (!inf)
		return 0;
	if (ret < 0) {
		bpf_map_delete_elem(&gds_inflight_map, &id);
		return 0;
	}
	now = bpf_ktime_get_ns();
	lat = now - inf->start_ns;
	bytes = ret;
	flags = inf->flags;
	bpf_map_delete_elem(&gds_inflight_map, &id);

	add_bytes(flags & GDS_FLAG_NVFS_SEEN ? 0 : 1, bytes);
	/* only emit slow calls to keep the ring small */
	if (lat < GDS_SLOW_NS)
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
	ev->signal_type = FABRIC_SIG_GDS;
	ev->latency_ns = lat;
	ev->bytes = bytes;
	ev->retry_count = flags & GDS_FLAG_NVFS_SEEN ? 1 : 0;
	bpf_get_current_comm(&ev->comm, sizeof(ev->comm));
	bpf_ringbuf_submit(ev, 0);
	return 0;
}

// ssize_t cuFileRead(CUfileHandle_t fh, void *devPtr_base, size_t size,
//                    off_t file_offset, off_t devPtr_offset)
SEC("uprobe/cuFileRead")
int BPF_UPROBE(cufile_read_entry, void *fh, void *buf, __u64 size)
{
	return gds_enter(size);
}

SEC("uretprobe/cuFileRead")
int BPF_URETPROBE(cufile_read_exit, long ret)
{
	return gds_exit(ret);
}

// ssize_t cuFileWrite(CUfileHandle_t fh, const void *devPtr_base, size_t size,
//                     off_t file_offset, off_t devPtr_offset)
SEC("uprobe/cuFileWrite")
int BPF_UPROBE(cufile_write_entry, void *fh, void *buf, __u64 size)
{
	return gds_enter(size);
}

SEC("uretprobe/cuFileWrite")
int BPF_URETPROBE(cufile_write_exit, long ret)
{
	return gds_exit(ret);
}

// nvidia-fs kernel path (optional).  It does not open an in-flight entry: it
// marks the cuFile call already in flight on this thread as confirmed direct.
SEC("kprobe/nvidia_fs_read")
int BPF_KPROBE(nvidia_fs_read_entry)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct gds_inflight *inf;

	inf = bpf_map_lookup_elem(&gds_inflight_map, &id);
	if (inf)
		inf->flags |= GDS_FLAG_NVFS_SEEN;
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
