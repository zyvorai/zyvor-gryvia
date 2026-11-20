/* SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause */
/*
 * gryvia_core.h - portable CO-RE foundation for all Gryvia eBPF programs.
 *
 * There is deliberately NO generated vmlinux.h.  Instead this header
 *   - pulls in the stable UAPI headers and libbpf helper headers,
 *   - defines the handful of constants the kernel keeps private,
 *   - declares MINIMAL local copies of the kernel structs we read, each
 *     marked preserve_access_index so libbpf relocates every field access
 *     against the running kernel's BTF (/sys/kernel/btf/vmlinux) at load time.
 *
 * Rules for the local struct definitions:
 *   - Only declare fields that a program really reads.  Field ORDER and
 *     OFFSETS here are irrelevant (they are relocated by name); names must
 *     match the kernel's.  Anonymous unions/structs in the kernel are
 *     flattened here: CO-RE finds fields inside anonymous members.
 *   - Read them with BPF_CORE_READ()/BPF_CORE_READ_INTO() (or direct ->
 *     access inside a helper-free context), never with hard-coded offsets.
 *   - If a field might not exist on some kernel, guard it with
 *     bpf_core_field_exists().
 *   - Adding a field is always safe; renaming/removing one is not.
 */
#ifndef __GRYVIA_CORE_H__
#define __GRYVIA_CORE_H__

#include <linux/types.h>
#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/in.h>
#include <linux/in6.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <linux/pkt_cls.h>
#include <linux/socket.h>
#include <stdbool.h>

/*
 * We describe struct pt_regs with the KERNEL's register names below, so tell
 * bpf_tracing.h to use them (it keys this off __VMLINUX_H__ / __KERNEL__).
 */
#ifndef __VMLINUX_H__
#define __VMLINUX_H__
#endif

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>

/* ---- constants the UAPI headers do not provide ------------------------- */

#ifndef NULL
#define NULL ((void *)0)
#endif

#ifndef AF_UNSPEC
#define AF_UNSPEC 0
#endif
#ifndef AF_UNIX
#define AF_UNIX 1
#endif
#ifndef AF_INET
#define AF_INET 2
#endif
#ifndef AF_INET6
#define AF_INET6 10
#endif

#ifndef IPPROTO_ICMP
#define IPPROTO_ICMP 1
#endif
#ifndef IPPROTO_TCP
#define IPPROTO_TCP 6
#endif
#ifndef IPPROTO_UDP
#define IPPROTO_UDP 17
#endif
#ifndef IPPROTO_ICMPV6
#define IPPROTO_ICMPV6 58
#endif

#ifndef ETH_P_IP
#define ETH_P_IP 0x0800
#endif
#ifndef ETH_P_IPV6
#define ETH_P_IPV6 0x86DD
#endif

/* TCP states (include/net/tcp_states.h); BPF_TCP_* in linux/bpf.h is the
 * same numbering. */
#ifndef TCP_ESTABLISHED
#define TCP_ESTABLISHED 1
#define TCP_SYN_SENT    2
#define TCP_SYN_RECV    3
#define TCP_FIN_WAIT1   4
#define TCP_FIN_WAIT2   5
#define TCP_TIME_WAIT   6
#define TCP_CLOSE       7
#define TCP_CLOSE_WAIT  8
#define TCP_LAST_ACK    9
#define TCP_LISTEN      10
#define TCP_CLOSING     11
#endif

/* struct file f_mode / f_flags bits (include/linux/fs.h, uapi fcntl). */
#ifndef O_ACCMODE
#define O_ACCMODE 00000003
#define O_WRONLY  00000001
#define O_RDWR    00000002
#define O_CREAT   00000100
#define O_TRUNC   00001000
#define O_APPEND  00002000
#endif
#ifndef FMODE_READ
#define FMODE_READ  0x1
#define FMODE_WRITE 0x2
#endif

/* Capability bit numbers (uapi/linux/capability.h). */
#ifndef CAP_SYS_ADMIN
#define CAP_NET_ADMIN 12
#define CAP_SYS_MODULE 16
#define CAP_SYS_PTRACE 19
#define CAP_SYS_ADMIN 21
#endif

#define TASK_COMM_LEN 16

/*
 * Syscall numbers.  These differ between x86_64 and arm64, so they are
 * selected on the target architecture Makefile passes as
 * -D__TARGET_ARCH_{x86,arm64}.  Programs must use GRYVIA_NR_*.
 */
#if defined(__TARGET_ARCH_x86)
#define GRYVIA_NR_connect      42
#define GRYVIA_NR_sendto       44
#define GRYVIA_NR_recvfrom     45
#define GRYVIA_NR_setuid       105
#define GRYVIA_NR_setgid       106
#define GRYVIA_NR_setreuid     113
#define GRYVIA_NR_setregid     114
#define GRYVIA_NR_setresuid    117
#define GRYVIA_NR_setresgid    119
#define GRYVIA_NR_capset       126
#define GRYVIA_NR_ptrace       101
#define GRYVIA_NR_mount        165
#define GRYVIA_NR_umount2      166
#define GRYVIA_NR_setns        308
#define GRYVIA_NR_unshare      272
#define GRYVIA_NR_init_module  175
#define GRYVIA_NR_delete_module 176
#define GRYVIA_NR_finit_module 313
#define GRYVIA_NR_open_by_handle_at 304
#define GRYVIA_NR_pivot_root   155
#define GRYVIA_NR_chroot       161
#elif defined(__TARGET_ARCH_arm64)
#define GRYVIA_NR_connect      203
#define GRYVIA_NR_sendto       206
#define GRYVIA_NR_recvfrom     207
#define GRYVIA_NR_setuid       146
#define GRYVIA_NR_setgid       144
#define GRYVIA_NR_setreuid     145
#define GRYVIA_NR_setregid     143
#define GRYVIA_NR_setresuid    147
#define GRYVIA_NR_setresgid    149
#define GRYVIA_NR_capset       91
#define GRYVIA_NR_ptrace       117
#define GRYVIA_NR_mount        40
#define GRYVIA_NR_umount2      39
#define GRYVIA_NR_setns        268
#define GRYVIA_NR_unshare      97
#define GRYVIA_NR_init_module  105
#define GRYVIA_NR_delete_module 106
#define GRYVIA_NR_finit_module 273
#define GRYVIA_NR_open_by_handle_at 265
#define GRYVIA_NR_pivot_root   41
#define GRYVIA_NR_chroot       51
#else
#error "gryvia_core.h: unsupported target arch (need __TARGET_ARCH_x86 or __TARGET_ARCH_arm64)"
#endif

/* ---- register files for PT_REGS_* / BPF_KPROBE -------------------------
 *
 * bpf_tracing.h expects `struct pt_regs` (x86) or `struct user_pt_regs`
 * (arm64) to be complete.  With no vmlinux.h we declare the pieces here;
 * they are CO-RE relocated like everything else.
 */
#if defined(__TARGET_ARCH_x86)
struct pt_regs {
	unsigned long r15, r14, r13, r12, bp, bx, r11, r10, r9, r8;
	unsigned long ax, cx, dx, si, di, orig_ax, ip, cs, flags, sp, ss;
} __attribute__((preserve_access_index));
#elif defined(__TARGET_ARCH_arm64)
struct user_pt_regs {
	__u64 regs[31];
	__u64 sp;
	__u64 pc;
	__u64 pstate;
} __attribute__((preserve_access_index));
#endif

/* ---- minimal kernel structs (all CO-RE relocated) ---------------------- */

#define __core __attribute__((preserve_access_index))

typedef __u32 gryvia_uid_t;

typedef struct __core { gryvia_uid_t val; } kuid_t;
typedef struct __core { gryvia_uid_t val; } kgid_t;
/* kernel_cap_t is {u64 val} on >=6.3, {u32 cap[2]} before: both 8 bytes.
 * Read it as a whole with BPF_CORE_READ_INTO(&caps, cred, cap_effective). */
typedef struct __core { __u64 val; } kernel_cap_t;

struct sock_common {
	__be32 skc_daddr;
	__be32 skc_rcv_saddr;
	__be16 skc_dport;
	__u16  skc_num;
	unsigned short skc_family;
	unsigned char  skc_state;
	int    skc_bound_dev_if;
	struct in6_addr skc_v6_daddr;
	struct in6_addr skc_v6_rcv_saddr;
} __core;

struct sock {
	struct sock_common __sk_common;
	__u32 sk_rcvbuf;
	__u32 sk_sndbuf;
	int   sk_err;
} __core;

struct qstr {
	__u32 hash;
	__u32 len;
	const unsigned char *name;
} __core;

struct dentry {
	struct qstr d_name;
	struct dentry *d_parent;
	struct inode *d_inode;
} __core;

struct inode {
	unsigned short i_mode;
	unsigned long  i_ino;
	kuid_t i_uid;
	kgid_t i_gid;
} __core;

struct vfsmount {
	struct dentry *mnt_root;
} __core;

struct path {
	struct vfsmount *mnt;
	struct dentry   *dentry;
} __core;

struct file {
	struct path f_path;
	struct inode *f_inode;
	unsigned int f_flags;
	unsigned int f_mode; /* fmode_t */
} __core;

struct cred {
	kuid_t uid;
	kgid_t gid;
	kuid_t suid;
	kgid_t sgid;
	kuid_t euid;
	kgid_t egid;
	kuid_t fsuid;
	kgid_t fsgid;
	kernel_cap_t cap_effective;
	kernel_cap_t cap_permitted;
	kernel_cap_t cap_inheritable;
} __core;

struct ns_common {
	unsigned int inum;
} __core;

struct mnt_namespace { struct ns_common ns; } __core;
struct net           { struct ns_common ns; } __core;
struct uts_namespace { struct ns_common ns; } __core;
struct ipc_namespace { struct ns_common ns; } __core;
struct cgroup_namespace { struct ns_common ns; } __core;
struct pid_namespace { struct ns_common ns; unsigned int level; } __core;

struct nsproxy {
	struct uts_namespace *uts_ns;
	struct ipc_namespace *ipc_ns;
	struct mnt_namespace *mnt_ns;
	struct pid_namespace *pid_ns_for_children;
	struct net           *net_ns;
	struct cgroup_namespace *cgroup_ns;
} __core;

struct task_struct {
	int pid;
	int tgid;
	unsigned int flags;
	char comm[TASK_COMM_LEN];
	struct task_struct *real_parent;
	struct task_struct *group_leader;
	const struct cred *real_cred;
	const struct cred *cred;
	struct nsproxy *nsproxy;
} __core;

struct sk_buff {
	unsigned int len;
	__u32 mark;
	__u16 protocol;
	struct sock *sk;
} __core;

struct net_device {
	char name[16];
	int  ifindex;
} __core;

/* sockaddr_in / sockaddr / in6_addr come from <linux/in.h>, <linux/socket.h>
 * and <linux/in6.h> (UAPI, stable layout - no CO-RE needed). */

/* ---- helpers ----------------------------------------------------------- */

/* Read a kuid_t/kgid_t out of a cred field in one shot, e.g.
 * GRYVIA_CRED_UID(cred, euid). */
#define GRYVIA_CRED_UID(cred, field) \
	((__u32)BPF_CORE_READ((cred), field.val))

/*
 * Read the IPv4 4-tuple of a struct sock.  Addresses and ports
 * follow the flow_event convention: IPs stay in network byte order, ports
 * are host byte order.  Any output pointer may be NULL.  Fields that cannot
 * be read are left at 0.
 */
static __always_inline void gryvia_sock_v4_tuple(const struct sock *sk,
						 __u32 *saddr, __u32 *daddr,
						 __u16 *sport, __u16 *dport)
{
	__u32 s = 0, d = 0;
	__u16 sp = 0, dp = 0;

	if (sk) {
		BPF_CORE_READ_INTO(&s, sk, __sk_common.skc_rcv_saddr);
		BPF_CORE_READ_INTO(&d, sk, __sk_common.skc_daddr);
		BPF_CORE_READ_INTO(&sp, sk, __sk_common.skc_num);
		BPF_CORE_READ_INTO(&dp, sk, __sk_common.skc_dport);
	}
	if (saddr)
		*saddr = s;
	if (daddr)
		*daddr = d;
	if (sport)
		*sport = sp;
	if (dport)
		*dport = bpf_ntohs(dp);
}

/* ---- security additions ---- */
#ifndef GRYVIA_NR_execve
#if defined(__TARGET_ARCH_x86)
#define GRYVIA_NR_execve 59
#elif defined(__TARGET_ARCH_arm64)
#define GRYVIA_NR_execve 221
#endif
#endif
/* ---- end security additions ---- */

/* ---- gpu/rdma additions ---- */
struct request { unsigned int cmd_flags; } __attribute__((preserve_access_index));
struct ib_qp { __u32 qp_num; } __attribute__((preserve_access_index));
struct ib_sge { __u64 addr; __u32 length; __u32 lkey; } __attribute__((preserve_access_index));
struct ib_send_wr {
	struct ib_send_wr *next;
	struct ib_sge *sg_list;
	int num_sge;
} __attribute__((preserve_access_index));
struct ib_recv_wr {
	struct ib_recv_wr *next;
	struct ib_sge *sg_list;
	int num_sge;
} __attribute__((preserve_access_index));
/* ---- end gpu/rdma additions ---- */

/* ---- misc (connect/tuning) additions ---- */
/*
 * At tcp_v4_connect() entry the socket's skc_daddr/skc_dport are NOT set yet
 * (the function fills them in from uaddr), so kprobes must read the
 * destination from the sockaddr argument.  uaddr is a kernel copy.
 * IP stays in network byte order, port is returned in host byte order.
 */
static __always_inline int gryvia_uaddr_v4(const struct sockaddr *uaddr,
					   __u32 *ip, __u16 *port)
{
	struct sockaddr_in sin = {};

	if (!uaddr || bpf_probe_read_kernel(&sin, sizeof(sin), uaddr))
		return -1;
	if (sin.sin_family != AF_INET)
		return -1;
	*ip = sin.sin_addr.s_addr;
	*port = bpf_ntohs(sin.sin_port);
	return 0;
}
/* ---- end misc (connect/tuning) additions ---- */

/* ---- silent-drop accounting -------------------------------------------
 * bpf_ringbuf_reserve() fails when the ring is full and bpf_perf_event_output()
 * fails when the per-CPU perf buffer is full; without a counter those events
 * vanish and a report built from the survivors looks healthier than it was.
 * A program that declares the map with GRYVIA_DECLARE_DROPS() and calls
 * GRYVIA_COUNT_DROP(slot) on the failure path exposes a per-CPU counter map
 * named `drops`; the collector sums it (loader.Manager.ReadCounter) and reports
 * it as measurement incompleteness (docs/flight-diagnosis.md).
 */
#define GRYVIA_DROP_RINGBUF 0 /* bpf_ringbuf_reserve() returned NULL */
#define GRYVIA_DROP_PERF    1 /* bpf_perf_event_output() returned an error */
#define GRYVIA_DROP_SLOTS   2

#define GRYVIA_DECLARE_DROPS()                              \
struct {                                                    \
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);            \
	__uint(max_entries, GRYVIA_DROP_SLOTS);             \
	__type(key, __u32);                                 \
	__type(value, __u64);                               \
} drops SEC(".maps")

#define GRYVIA_COUNT_DROP(slot_)                            \
	do {                                                \
		__u32 drop_slot_ = (slot_);                 \
		__u64 *drop_v_ = bpf_map_lookup_elem(&drops, &drop_slot_); \
		if (drop_v_)                                \
			*drop_v_ += 1;                      \
	} while (0)
/* ---- end silent-drop accounting ---- */

#endif /* __GRYVIA_CORE_H__ */
