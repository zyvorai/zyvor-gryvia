// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// pfc_pause.c - count 802.1Qbb priority flow control (PFC) pause frames.
//
// XDP on the RDMA-facing interface.  A PFC frame is a MAC control frame:
// EtherType 0x8808, opcode 0x0101, then a 2-byte priority enable vector
// (bits 0-7 = priority 0-7) and eight 2-byte pause quanta.  Priority i is
// paused by the frame when its enable bit is set and its quanta is non-zero
// (quanta 0 is a resume).  Link-level 802.3x pause (opcode 0x0001) is counted
// separately.  Untagged frames only: MAC control frames are never VLAN-tagged.
//
// Counting only: per-CPU counters in pause_count (slots PFC_SLOT_* in
// headers/fabric_signal.h: PFC frames, legacy pause frames, and one slot per
// paused priority), read and summed by the collector.  No ring buffer, so a
// pause storm cannot flood userspace.  Every path returns XDP_PASS; packets
// are never modified, dropped or redirected.
//
// Attach: only with -iface.  Only ONE XDP program can own an interface: this
// program conflicts with roce_cnp.c, packet_filter.c and dns_tracker.c on the
// same interface.  The collector attaches the first XDP program it loads for
// -iface and skips every later one with a logged reason (it never replaces a
// running program), so run pfc_pause on its own interface or instead of
// roce_cnp when both are wanted.  Many NICs consume pause frames in the MAC
// and never hand them to XDP; there the counters stay 0 and ethtool -S
// (rx_prio*_pause) is the reliable source.

#include "headers/fabric_signal.h"
#include "headers/xdp_chain.h"

#define ETH_P_PAUSE     0x8808          /* MAC control */
#define PAUSE_OP_LEGACY 0x0001          /* IEEE 802.3x PAUSE */
#define PAUSE_OP_PFC    0x0101          /* IEEE 802.1Qbb PFC */

struct pfc_hdr {
	__be16 opcode;
	__be16 enable_vec;      /* high byte reserved, low byte = priority bits */
	__be16 quanta[PFC_PRIORITIES];
} __attribute__((packed));

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, PFC_SLOTS);
	__type(key, __u32);
	__type(value, __u64);
} pause_count SEC(".maps");

static __always_inline void bump(__u32 slot)
{
	__u64 *v = bpf_map_lookup_elem(&pause_count, &slot);

	if (v)
		*v += 1; /* per-CPU element, XDP runs with preemption off */
}

static __always_inline int gryvia_pfc_pause_body(struct xdp_md *ctx)
{
	void *data = (void *)(long)ctx->data;
	void *end = (void *)(long)ctx->data_end;
	struct ethhdr *eth = data;
	struct pfc_hdr *p;
	__u16 vec;
	int i;

	if ((void *)(eth + 1) > end)
		return XDP_PASS;
	if (eth->h_proto != bpf_htons(ETH_P_PAUSE))
		return XDP_PASS;
	p = (void *)(eth + 1);
	if ((void *)(p + 1) > end) {
		/* Too short for a PFC body; a legacy pause needs only the opcode. */
		__be16 *op = (void *)(eth + 1);

		if ((void *)(op + 1) <= end && *op == bpf_htons(PAUSE_OP_LEGACY))
			bump(PFC_SLOT_LEGACY);
		return XDP_PASS;
	}

	if (p->opcode == bpf_htons(PAUSE_OP_LEGACY)) {
		bump(PFC_SLOT_LEGACY);
		return XDP_PASS;
	}
	if (p->opcode != bpf_htons(PAUSE_OP_PFC))
		return XDP_PASS;

	bump(PFC_SLOT_FRAMES);
	vec = bpf_ntohs(p->enable_vec);
#pragma unroll
	for (i = 0; i < PFC_PRIORITIES; i++) {
		if ((vec & (1U << i)) && p->quanta[i] != 0)
			bump(PFC_SLOT_PRIO0 + i);
	}
	return XDP_PASS;
}

/* Entry point.  With xdp_mux this continues the chain to the next populated slot (see
 * headers/xdp_chain.h); attached on its own it simply returns the verdict. */
SEC("xdp")
int gryvia_pfc_pause(struct xdp_md *ctx)
{
	return xdp_chain_next(ctx, XDP_SLOT_PFC_PAUSE, gryvia_pfc_pause_body(ctx));
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
