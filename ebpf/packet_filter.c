// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// packet_filter.c - XDP-based packet filter for ultra-fast inline filtering.
//
// Parses Ethernet -> IPv4 -> TCP/UDP headers and matches against a
// dynamically-updatable blocklist stored in BPF maps.  Each rule hit
// increments a per-rule counter that the collector scrapes for metrics.

#include "headers/common.h"
#include "headers/xdp_chain.h"

/*
 * Maximum number of rules.  Every rule is checked by a bounded loop that the
 * verifier must fully explore, so this is limited by verifier complexity
 * (1024 exceeds the 8192-jump-sequence limit).  Keep it in sync with the
 * filter_rules/rule_stats_map sizes below.
 */
#define MAX_RULES 256

/* ---- filter rule ------------------------------------------------------ */

struct filter_rule {
    __u32 src_ip;       // 0 = wildcard
    __u32 dst_ip;       // 0 = wildcard
    __u16 dst_port;     // 0 = wildcard
    __u8  protocol;     // 0 = wildcard
    __u8  action;       // 0 = XDP_DROP, 1 = XDP_TX
};

struct rule_stats {
    __u64 packets;
    __u64 bytes;
};

/* ---- BPF maps --------------------------------------------------------- */

// Blocklist: index -> rule.
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, MAX_RULES);
    __type(key, __u32);
    __type(value, struct filter_rule);
} filter_rules SEC(".maps");

// Number of active rules (single-element array).
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u32);
} rule_count SEC(".maps");

// Per-rule hit counters.
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, MAX_RULES);
    __type(key, __u32);
    __type(value, struct rule_stats);
} rule_stats_map SEC(".maps");

// Global pass/drop counters (index 0 = pass, 1 = drop, 2 = tx).
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 3);
    __type(key, __u32);
    __type(value, __u64);
} global_stats SEC(".maps");

/* ---- XDP program ------------------------------------------------------ */

static __always_inline int xdp_packet_filter_body(struct xdp_md *ctx)
{
    void *data     = (void *)(long)ctx->data;
    void *data_end = (void *)(long)ctx->data_end;

    /* --- Ethernet header ------------------------------------------------ */
    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return XDP_PASS;

    if (eth->h_proto != bpf_htons(ETH_P_IP))
        return XDP_PASS;

    /* --- IPv4 header ---------------------------------------------------- */
    struct iphdr *iph = (void *)(eth + 1);
    if ((void *)(iph + 1) > data_end)
        return XDP_PASS;

    __u32 src_ip   = iph->saddr;
    __u32 dst_ip   = iph->daddr;
    __u8  protocol = iph->protocol;
    __u16 dst_port = 0;

    /* --- L4 header ------------------------------------------------------ */
    __u32 ip_hdr_len = iph->ihl * 4;
    if (ip_hdr_len < sizeof(struct iphdr))
        return XDP_PASS;

    void *l4 = (void *)iph + ip_hdr_len;

    if (protocol == IPPROTO_TCP) {
        struct tcphdr *tcph = l4;
        if ((void *)(tcph + 1) > data_end)
            return XDP_PASS;
        dst_port = bpf_ntohs(tcph->dest);
    } else if (protocol == IPPROTO_UDP) {
        struct udphdr *udph = l4;
        if ((void *)(udph + 1) > data_end)
            return XDP_PASS;
        dst_port = bpf_ntohs(udph->dest);
    }

    /* --- Rule matching -------------------------------------------------- */
    __u32 zero = 0;
    __u32 *count = bpf_map_lookup_elem(&rule_count, &zero);
    __u32 n = count ? *count : 0;

    // Cap iteration to avoid verifier rejection.
    if (n > MAX_RULES)
        n = MAX_RULES;

    for (__u32 i = 0; i < MAX_RULES; i++) {
        if (i >= n)
            break;

        struct filter_rule *rule = bpf_map_lookup_elem(&filter_rules, &i);
        if (!rule)
            continue;

        // Match fields: zero in rule means wildcard.
        if (rule->src_ip != 0 && rule->src_ip != src_ip)
            continue;
        if (rule->dst_ip != 0 && rule->dst_ip != dst_ip)
            continue;
        if (rule->dst_port != 0 && rule->dst_port != dst_port)
            continue;
        if (rule->protocol != 0 && rule->protocol != protocol)
            continue;

        // Rule matched -- update stats.
        struct rule_stats *rs = bpf_map_lookup_elem(&rule_stats_map, &i);
        if (rs) {
            __sync_fetch_and_add(&rs->packets, 1);
            __sync_fetch_and_add(&rs->bytes,
                                 (__u64)(data_end - data));
        }

        int action = (rule->action == 1) ? XDP_TX : XDP_DROP;
        __u32 idx = (action == XDP_TX) ? 2 : 1;
        __u64 *ctr = bpf_map_lookup_elem(&global_stats, &idx);
        if (ctr)
            __sync_fetch_and_add(ctr, 1);

        return action;
    }

    /* No rule matched -- pass the packet. */
    __u64 *pass_ctr = bpf_map_lookup_elem(&global_stats, &zero);
    if (pass_ctr)
        __sync_fetch_and_add(pass_ctr, 1);

    return XDP_PASS;
}

/* Entry point.  With xdp_mux this continues the chain to the next populated slot (see
 * headers/xdp_chain.h); attached on its own it simply returns the verdict. */
SEC("xdp")
int xdp_packet_filter(struct xdp_md *ctx)
{
	return xdp_chain_next(ctx, XDP_SLOT_PACKET_FILTER, xdp_packet_filter_body(ctx));
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
