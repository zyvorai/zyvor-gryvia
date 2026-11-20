/* SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause */
/*
 * xdp_chain.h - run several XDP observers on one interface.
 *
 * Linux allows one XDP program per device, so xdp_mux.c is the only program the
 * collector attaches.  It tail-calls the first populated slot of the xdp_features
 * prog array.  A successful bpf_tail_call never returns, so one tail call cannot
 * run more than one program: each feature program therefore ends by tail-calling
 * the next populated slot itself (xdp_chain_next), and the last one returns
 * XDP_PASS.  A feature attached directly, without the mux, behaves as before: its
 * own xdp_features map is empty, every tail call misses and it returns its verdict.
 *
 * The collector must make every feature object use the mux's xdp_features map
 * (cilium/ebpf CollectionOptions.MapReplacements) and put each feature program at
 * its slot.  The kernel allows at most 33 chained tail calls; the longest chain
 * here is XDP_SLOT_MAX (5) plus the mux.
 *
 * Feature programs must keep returning XDP_PASS for packets they only observe: a
 * verdict other than XDP_PASS ends the chain and is the packet's verdict.
 */
#ifndef GRYVIA_XDP_CHAIN_H
#define GRYVIA_XDP_CHAIN_H

#include "gryvia_core.h"

/* Slot map. The collector (collector/pkg/fabric/signal_ext.go) and the mux agree on it. */
#define XDP_SLOT_ROCE_CNP      0
#define XDP_SLOT_PFC_PAUSE     1
#define XDP_SLOT_DNS           2
#define XDP_SLOT_PACKET_FILTER 3
#define XDP_SLOT_ROCE_ECN      4
#define XDP_SLOT_MAX           5

struct {
	__uint(type, BPF_MAP_TYPE_PROG_ARRAY);
	__uint(max_entries, XDP_SLOT_MAX);
	__type(key, __u32);
	__type(value, __u32);
} xdp_features SEC(".maps");

/*
 * Tail-call the first populated slot at or after `first`; returns only if there is none.
 * Written out instead of looped: clang will not unroll a loop around bpf_tail_call, and
 * `first` is a compile-time constant after inlining.  Add a line here when adding a slot.
 */
_Static_assert(XDP_SLOT_MAX == 5, "xdp_chain_from calls every slot: update it with XDP_SLOT_MAX");

static __always_inline void xdp_chain_from(struct xdp_md *ctx, __u32 first)
{
	if (first <= 0)
		bpf_tail_call(ctx, &xdp_features, 0);
	if (first <= 1)
		bpf_tail_call(ctx, &xdp_features, 1);
	if (first <= 2)
		bpf_tail_call(ctx, &xdp_features, 2);
	if (first <= 3)
		bpf_tail_call(ctx, &xdp_features, 3);
	if (first <= 4)
		bpf_tail_call(ctx, &xdp_features, 4);
}

/*
 * End of a feature program: pass the packet on to the next populated slot, or return
 * its verdict.  `verdict` is what the program would have returned on its own.
 */
static __always_inline int xdp_chain_next(struct xdp_md *ctx, __u32 slot, int verdict)
{
	if (verdict != XDP_PASS)
		return verdict;
	xdp_chain_from(ctx, slot + 1);
	return XDP_PASS;
}

#endif /* GRYVIA_XDP_CHAIN_H */
