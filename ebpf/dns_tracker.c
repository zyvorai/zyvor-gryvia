// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// dns_tracker.c - Passive DNS query/response tracking.
//
// Hooks UDP traffic on port 53 via a socket filter (or can be used with
// XDP).  Parses the DNS header to extract query type, response code,
// and measures resolution latency by correlating query/response
// transaction IDs.

#include "headers/common.h"
#include "headers/xdp_chain.h"

/* DNS header (RFC 1035 section 4.1.1) */
struct dns_header {
    __u16 id;
    __u16 flags;
    __u16 qdcount;
    __u16 ancount;
    __u16 nscount;
    __u16 arcount;
} __attribute__((packed));

#define DNS_FLAG_QR    0x8000  // Query(0) / Response(1)
#define DNS_RCODE_MASK 0x000F

/* DNS response codes of interest */
#define DNS_RCODE_NOERROR  0
#define DNS_RCODE_NXDOMAIN 3
#define DNS_RCODE_SERVFAIL 2

/* DNS query tracking entry */
struct dns_query_info {
    __u64 query_ts;     // timestamp when query was sent
    __u32 src_ip;       // who asked
    __u32 pid;
    char  comm[TASK_COMM_LEN];
};

/* DNS event emitted to userspace */
struct dns_event {
    __u64 timestamp;
    __u64 latency_ns;   // 0 for queries, measured for responses
    __u32 src_ip;
    __u32 dst_ip;
    __u16 tx_id;
    __u16 flags;
    __u8  rcode;
    __u8  is_response;
    __u32 pid;
    char  comm[TASK_COMM_LEN];
};

/* ---- BPF maps --------------------------------------------------------- */

// In-flight DNS queries keyed by transaction ID.
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 16384);
    __type(key, __u16);
    __type(value, struct dns_query_info);
} dns_queries SEC(".maps");

// DNS failure counters: rcode -> count.
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 16);
    __type(key, __u32);
    __type(value, __u64);
} dns_rcode_counts SEC(".maps");

// Perf ring buffer for DNS events.
struct {
    __uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
    __uint(key_size, sizeof(__u32));
    __uint(value_size, sizeof(__u32));
} dns_events SEC(".maps");

/* ---- XDP DNS tracker -------------------------------------------------- */

static __always_inline int xdp_dns_tracker_body(struct xdp_md *ctx)
{
    void *data     = (void *)(long)ctx->data;
    void *data_end = (void *)(long)ctx->data_end;

    /* Ethernet */
    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return XDP_PASS;
    if (eth->h_proto != bpf_htons(ETH_P_IP))
        return XDP_PASS;

    /* IPv4 */
    struct iphdr *iph = (void *)(eth + 1);
    if ((void *)(iph + 1) > data_end)
        return XDP_PASS;
    if (iph->protocol != IPPROTO_UDP)
        return XDP_PASS;

    __u32 ip_hdr_len = iph->ihl * 4;
    if (ip_hdr_len < sizeof(struct iphdr))
        return XDP_PASS;

    /* UDP */
    struct udphdr *udph = (void *)iph + ip_hdr_len;
    if ((void *)(udph + 1) > data_end)
        return XDP_PASS;

    __u16 src_port = bpf_ntohs(udph->source);
    __u16 dst_port = bpf_ntohs(udph->dest);

    /* Only DNS traffic */
    if (src_port != 53 && dst_port != 53)
        return XDP_PASS;

    /* DNS header */
    struct dns_header *dns = (void *)(udph + 1);
    if ((void *)(dns + 1) > data_end)
        return XDP_PASS;

    __u16 tx_id = bpf_ntohs(dns->id);
    __u16 flags = bpf_ntohs(dns->flags);
    __u8  is_response = (flags & DNS_FLAG_QR) ? 1 : 0;
    __u8  rcode = flags & DNS_RCODE_MASK;

    __u64 now = bpf_ktime_get_ns();

    if (!is_response) {
        /* Outgoing query: record timestamp keyed by transaction ID. */
        struct dns_query_info qi = {};
        qi.query_ts = now;
        qi.src_ip   = iph->saddr;
        bpf_map_update_elem(&dns_queries, &tx_id, &qi, BPF_ANY);
    } else {
        /* Incoming response: calculate latency, track rcode. */
        __u64 latency = 0;
        struct dns_query_info *qi = bpf_map_lookup_elem(&dns_queries, &tx_id);
        if (qi) {
            latency = now - qi->query_ts;
            bpf_map_delete_elem(&dns_queries, &tx_id);
        }

        /* Increment rcode counter. */
        __u32 rcode_idx = (__u32)rcode;
        if (rcode_idx < 16) {
            __u64 *cnt = bpf_map_lookup_elem(&dns_rcode_counts, &rcode_idx);
            if (cnt)
                __sync_fetch_and_add(cnt, 1);
        }

        /* Emit event for failed lookups or high latency. */
        if (rcode != DNS_RCODE_NOERROR || latency > 100000000ULL /* 100ms */) {
            struct dns_event ev = {};
            ev.timestamp   = now;
            ev.latency_ns  = latency;
            ev.src_ip      = iph->saddr;
            ev.dst_ip      = iph->daddr;
            ev.tx_id       = tx_id;
            ev.flags       = flags;
            ev.rcode       = rcode;
            ev.is_response = 1;

            bpf_perf_event_output(ctx, &dns_events, BPF_F_CURRENT_CPU,
                                  &ev, sizeof(ev));
        }
    }

    return XDP_PASS;
}

/* Entry point.  With xdp_mux this continues the chain to the next populated slot (see
 * headers/xdp_chain.h); attached on its own it simply returns the verdict. */
SEC("xdp")
int xdp_dns_tracker(struct xdp_md *ctx)
{
	return xdp_chain_next(ctx, XDP_SLOT_DNS, xdp_dns_tracker_body(ctx));
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
