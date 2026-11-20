// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// roce_cnp.c - count RoCEv2 congestion notification packets (CNP).
//
// XDP on the RDMA-facing interface.  A RoCEv2 packet is UDP to port 4791 and
// starts with the 12-byte InfiniBand Base Transport Header; a CNP has BTH
// opcode 0x81.  Parsed: Ethernet, up to two VLAN tags (802.1Q / 802.1ad),
// then IPv4 (non-fragment, any IHL) or IPv6 (UDP as the next header).
//
// Counting only: two per-CPU counters (CNP, all RoCEv2) in cnp_count, read
// and summed by the collector.  No ring buffer, so a CNP storm cannot flood
// userspace.  Every path returns XDP_PASS; packets are never modified,
// dropped or redirected.  The collector attaches it only when -iface is set
// (it is skipped on a NIC that carries no RoCE traffic, where it only counts
// zero).  Only one XDP program can own an interface: it conflicts with
// packet_filter.c and dns_tracker.c on the same interface.

#include "headers/fabric_signal.h"
#include "headers/xdp_chain.h"

#define ROCE_UDP_PORT  4791
#define BTH_OPCODE_CNP 0x81
#define MAX_VLAN_TAGS  2

struct vlan_hdr {
	__be16 tci;
	__be16 proto;
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, CNP_SLOTS);
	__type(key, __u32);
	__type(value, __u64);
} cnp_count SEC(".maps");

static __always_inline void bump(__u32 slot)
{
	__u64 *v = bpf_map_lookup_elem(&cnp_count, &slot);

	if (v)
		*v += 1; /* per-CPU element, XDP runs with preemption off */
}

static __always_inline int gryvia_roce_cnp_body(struct xdp_md *ctx)
{
	void *data = (void *)(long)ctx->data;
	void *end = (void *)(long)ctx->data_end;
	struct ethhdr *eth = data;
	struct udphdr *udp;
	__u8 *bth;
	void *cur;
	__be16 proto;
	int i;

	if ((void *)(eth + 1) > end)
		return XDP_PASS;
	proto = eth->h_proto;
	cur = eth + 1;

#pragma unroll
	for (i = 0; i < MAX_VLAN_TAGS; i++) {
		struct vlan_hdr *vh;

		if (proto != bpf_htons(ETH_P_8021Q) && proto != bpf_htons(ETH_P_8021AD))
			break;
		vh = cur;
		if ((void *)(vh + 1) > end)
			return XDP_PASS;
		proto = vh->proto;
		cur = vh + 1;
	}

	if (proto == bpf_htons(ETH_P_IP)) {
		struct iphdr *ip = cur;
		__u32 hlen;

		if ((void *)(ip + 1) > end)
			return XDP_PASS;
		if (ip->protocol != IPPROTO_UDP || ip->ihl < 5)
			return XDP_PASS;
		if (ip->frag_off & bpf_htons(0x1FFF)) /* non-first fragment: no UDP header */
			return XDP_PASS;
		hlen = ip->ihl * 4;
		udp = cur + hlen;
	} else if (proto == bpf_htons(ETH_P_IPV6)) {
		struct ipv6hdr *ip6 = cur;

		if ((void *)(ip6 + 1) > end)
			return XDP_PASS;
		if (ip6->nexthdr != IPPROTO_UDP)
			return XDP_PASS;
		udp = (void *)(ip6 + 1);
	} else {
		return XDP_PASS;
	}

	if ((void *)(udp + 1) > end)
		return XDP_PASS;
	if (udp->dest != bpf_htons(ROCE_UDP_PORT))
		return XDP_PASS;
	bth = (__u8 *)(udp + 1);
	if ((void *)(bth + 1) > end)
		return XDP_PASS;

	bump(CNP_SLOT_ROCE);
	if (*bth == BTH_OPCODE_CNP)
		bump(CNP_SLOT_CNP);
	return XDP_PASS;
}

/* Entry point.  With xdp_mux this continues the chain to the next populated slot (see
 * headers/xdp_chain.h); attached on its own it simply returns the verdict. */
SEC("xdp")
int gryvia_roce_cnp(struct xdp_md *ctx)
{
	return xdp_chain_next(ctx, XDP_SLOT_ROCE_CNP, gryvia_roce_cnp_body(ctx));
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
