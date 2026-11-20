// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// xdp_mux.c - one XDP owner for the RDMA-facing interface.
//
// packet_filter, dns_tracker, roce_cnp, pfc_pause and roce_ecn cannot share an
// interface on their own: Linux allows one XDP program per device.  This is the
// only program the collector attaches.  It tail-calls the first populated slot of
// xdp_features; that feature runs and tail-calls the next populated slot, and so on
// (headers/xdp_chain.h), so every loaded feature sees every packet, in slot order.
// An empty xdp_features map, or a chain that ends, returns XDP_PASS.
//
// Slot map (the collector must agree; see headers/xdp_chain.h):
//   0  roce_cnp
//   1  pfc_pause
//   2  dns_tracker
//   3  packet_filter   (observe-only build; a drop rule still belongs in Cilium)
//   4  roce_ecn
//
// A feature that returns a verdict other than XDP_PASS ends the chain and decides
// the packet.  Every feature here only observes and returns XDP_PASS, so the mux
// passes every packet through unchanged.  The packet is never rewritten.
//
// The collector must replace each feature object's xdp_features map with the
// mux's own map when loading (cilium/ebpf MapReplacements) and put each feature
// program in its slot; a feature loaded without that still works when attached on
// its own.
//
// xdp_mux_stats: 0 = packets seen, 1 = no feature was populated (tail calls missed).

#include "headers/xdp_chain.h"

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 2);
	__type(key, __u32);
	__type(value, __u64);
} xdp_mux_stats SEC(".maps");

static __always_inline void bump(__u32 slot)
{
	__u64 *v = bpf_map_lookup_elem(&xdp_mux_stats, &slot);

	if (v)
		*v += 1;
}

SEC("xdp")
int gryvia_xdp_mux(struct xdp_md *ctx)
{
	bump(0);
	/* Does not return when a slot is populated: that feature continues the chain. */
	xdp_chain_from(ctx, 0);
	bump(1);
	return XDP_PASS;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
