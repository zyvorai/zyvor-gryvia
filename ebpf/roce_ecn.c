// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// roce_ecn.c - sample ECN marks on RoCEv2 (UDP 4791).
//
// CNP count (roce_cnp.c) is the switch's reaction.  This program counts the
// cause: IP ECN bits on RoCE packets (ECT(1) / CE).  Per-CPU counters only,
// always XDP_PASS, no ring buffer.  Attach as XDP slot 4 of xdp_mux, not as
// a second XDP owner.
//
//   0  RoCEv2 packets seen
//   1  CE (congestion experienced, ECN == 3)
//   2  ECT(0) or ECT(1), not CE
//
// IPv4 and IPv6.  IPv6 extension headers are not walked (nexthdr must be UDP).

#include "headers/gryvia_core.h"
#include "headers/xdp_chain.h"

#define ROCE_UDP_PORT 4791
#define MAX_VLAN_TAGS 2

#define ECN_SLOT_ROCE 0
#define ECN_SLOT_CE   1
#define ECN_SLOT_ECT  2
#define ECN_SLOTS     3

struct vlan_hdr {
	__be16 tci;
	__be16 proto;
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, ECN_SLOTS);
	__type(key, __u32);
	__type(value, __u64);
} ecn_count SEC(".maps");

static __always_inline void bump(__u32 slot)
{
	__u64 *v = bpf_map_lookup_elem(&ecn_count, &slot);

	if (v)
		*v += 1;
}

static __always_inline int gryvia_roce_ecn_body(struct xdp_md *ctx)
{
	void *data = (void *)(long)ctx->data;
	void *end = (void *)(long)ctx->data_end;
	struct ethhdr *eth = data;
	struct udphdr *udp;
	void *cur;
	__be16 proto;
	__u8 ecn = 0;
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

		if ((void *)(ip + 1) > end)
			return XDP_PASS;
		if (ip->protocol != IPPROTO_UDP || ip->ihl < 5)
			return XDP_PASS;
		if (ip->frag_off & bpf_htons(0x1FFF))
			return XDP_PASS;
		ecn = ip->tos & 3;
		udp = cur + (ip->ihl * 4);
	} else if (proto == bpf_htons(ETH_P_IPV6)) {
		struct ipv6hdr *ip6 = cur;

		if ((void *)(ip6 + 1) > end)
			return XDP_PASS;
		if (ip6->nexthdr != IPPROTO_UDP)
			return XDP_PASS;
		ecn = ip6->flow_lbl[0] >> 4;
		ecn &= 3;
		udp = (void *)(ip6 + 1);
	} else {
		return XDP_PASS;
	}

	if ((void *)(udp + 1) > end)
		return XDP_PASS;
	if (udp->dest != bpf_htons(ROCE_UDP_PORT) && udp->source != bpf_htons(ROCE_UDP_PORT))
		return XDP_PASS;

	bump(ECN_SLOT_ROCE);
	if (ecn == 3)
		bump(ECN_SLOT_CE);
	else if (ecn)
		bump(ECN_SLOT_ECT);
	return XDP_PASS;
}

/* Entry point.  With xdp_mux this continues the chain to the next populated slot (see
 * headers/xdp_chain.h); attached on its own it simply returns the verdict. */
SEC("xdp")
int gryvia_roce_ecn(struct xdp_md *ctx)
{
	return xdp_chain_next(ctx, XDP_SLOT_ROCE_ECN, gryvia_roce_ecn_body(ctx));
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
