// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// cost_tracker.c - Network cost tracking via tc classifiers.
//
// Hooks tc egress and ingress to track per-pod byte counters and
// classify traffic by zone (same-zone, cross-zone, external) for
// cloud cost analysis and optimization.
//
// IPv4 pairs are counted in traffic_costs (8-byte key, unchanged layout) and
// IPv6 pairs in traffic_costs6 (32-byte key). IPv6 bytes are payload_length + 40:
// extension headers are not walked (they are part of payload_length, so they are
// counted as bytes, but the upper-layer protocol is never looked at) and a jumbo
// packet (payload_length 0) counts only its 40-byte header. The zone_map /
// cost_stats zone classification is IPv4 only; IPv6 adds to the packet total only.

#include "headers/common.h"

/* ---- structs ---------------------------------------------------------- */

struct cost_key {
    __u32 src_ip;
    __u32 dst_ip;
};

/* IPv6 pair: raw 16-byte addresses in network order */
struct cost_key6 {
    __u8 src_ip[16];
    __u8 dst_ip[16];
};

struct cost_value {
    __u64 bytes_sent;
    __u64 bytes_recv;
    __u64 packets_sent;
    __u64 packets_recv;
};

/* Zone classification from IP prefix */
struct zone_key {
    __u32 ip_prefix;    /* IP & mask */
    __u32 prefix_len;   /* CIDR prefix length */
};

/* Aggregate cost stats indices */
#define STAT_SAME_ZONE_BYTES    0
#define STAT_CROSS_ZONE_BYTES   1
#define STAT_EXTERNAL_BYTES     2
#define STAT_TOTAL_PACKETS      3
#define NUM_COST_STATS          4

/* ---- BPF maps --------------------------------------------------------- */

/* Per-pair traffic counters */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, struct cost_key);
    __type(value, struct cost_value);
} traffic_costs SEC(".maps");

/* Per-pair traffic counters, IPv6 (same value layout as traffic_costs) */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, struct cost_key6);
    __type(value, struct cost_value);
} traffic_costs6 SEC(".maps");

/* IP prefix to zone ID mapping (populated from userspace) */
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __type(key, __u32);       /* IP prefix (network byte order) */
    __type(value, __u32);     /* zone ID */
} zone_map SEC(".maps");

/* Aggregate cost stats (percpu for lock-free updates) */
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, NUM_COST_STATS);
    __type(key, __u32);
    __type(value, __u64);
} cost_stats SEC(".maps");

/* ---- helpers ---------------------------------------------------------- */

/* Look up zone ID for an IP address by checking common prefix lengths */
static __always_inline __u32 lookup_zone(__u32 ip)
{
    /* Try /24 prefix first (most specific for pod subnets) */
    __u32 prefix24 = ip & bpf_htonl(0xFFFFFF00);
    __u32 *zone = bpf_map_lookup_elem(&zone_map, &prefix24);
    if (zone)
        return *zone;

    /* Try /16 prefix */
    __u32 prefix16 = ip & bpf_htonl(0xFFFF0000);
    zone = bpf_map_lookup_elem(&zone_map, &prefix16);
    if (zone)
        return *zone;

    /* Try /8 prefix */
    __u32 prefix8 = ip & bpf_htonl(0xFF000000);
    zone = bpf_map_lookup_elem(&zone_map, &prefix8);
    if (zone)
        return *zone;

    /* Unknown zone - treat as external */
    return 0;
}

static __always_inline void count_packet(void);

static __always_inline void classify_and_count(__u32 src_ip, __u32 dst_ip,
                                                __u64 bytes)
{
    __u32 src_zone = lookup_zone(src_ip);
    __u32 dst_zone = lookup_zone(dst_ip);

    __u32 stat_idx;
    if (src_zone == 0 || dst_zone == 0)
        stat_idx = STAT_EXTERNAL_BYTES;
    else if (src_zone == dst_zone)
        stat_idx = STAT_SAME_ZONE_BYTES;
    else
        stat_idx = STAT_CROSS_ZONE_BYTES;

    __u64 *cnt = bpf_map_lookup_elem(&cost_stats, &stat_idx);
    if (cnt)
        *cnt += bytes;

    /* Update total packet counter */
    count_packet();
}

/* Add bytes/packets to the pair's entry (creating it), egress or ingress. */
static __always_inline void account(void *map, void *key, __u64 bytes, int ingress)
{
    struct cost_value *val = bpf_map_lookup_elem(map, key);
    if (!val) {
        struct cost_value new_val = {};
        if (ingress) {
            new_val.bytes_recv   = bytes;
            new_val.packets_recv = 1;
        } else {
            new_val.bytes_sent   = bytes;
            new_val.packets_sent = 1;
        }
        if (!bpf_map_update_elem(map, key, &new_val, BPF_NOEXIST))
            return;
        /* lost the insert race: account into the winner's entry */
        val = bpf_map_lookup_elem(map, key);
        if (!val)
            return;
    }
    if (ingress) {
        __sync_fetch_and_add(&val->bytes_recv, bytes);
        __sync_fetch_and_add(&val->packets_recv, 1);
    } else {
        __sync_fetch_and_add(&val->bytes_sent, bytes);
        __sync_fetch_and_add(&val->packets_sent, 1);
    }
}

static __always_inline void count_packet(void)
{
    __u32 pkt_idx = STAT_TOTAL_PACKETS;
    __u64 *pkt_cnt = bpf_map_lookup_elem(&cost_stats, &pkt_idx);
    if (pkt_cnt)
        (*pkt_cnt)++;
}

static __always_inline int track(struct __sk_buff *skb, int ingress)
{
    void *data     = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;

    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return TC_ACT_OK;

    if (eth->h_proto == bpf_htons(ETH_P_IP)) {
        struct iphdr *iph = (void *)(eth + 1);
        if ((void *)(iph + 1) > data_end)
            return TC_ACT_OK;

        __u32 src_ip = iph->saddr;
        __u32 dst_ip = iph->daddr;
        __u64 pkt_bytes = (__u64)bpf_ntohs(iph->tot_len);

        /* Key is (local, remote): local is the source on egress and the
         * destination on ingress. */
        struct cost_key key = {};
        key.src_ip = ingress ? dst_ip : src_ip;
        key.dst_ip = ingress ? src_ip : dst_ip;
        account(&traffic_costs, &key, pkt_bytes, ingress);

        /* Classify by zone and update aggregate stats */
        classify_and_count(src_ip, dst_ip, pkt_bytes);
    } else if (eth->h_proto == bpf_htons(ETH_P_IPV6)) {
        struct ipv6hdr *ip6 = (void *)(eth + 1);
        if ((void *)(ip6 + 1) > data_end)
            return TC_ACT_OK;

        /* Fixed header only: payload_length covers extension headers. */
        __u64 pkt_bytes = (__u64)bpf_ntohs(ip6->payload_len) + sizeof(*ip6);

        struct cost_key6 key = {};
        if (ingress) {
            __builtin_memcpy(key.src_ip, &ip6->daddr, 16);
            __builtin_memcpy(key.dst_ip, &ip6->saddr, 16);
        } else {
            __builtin_memcpy(key.src_ip, &ip6->saddr, 16);
            __builtin_memcpy(key.dst_ip, &ip6->daddr, 16);
        }
        account(&traffic_costs6, &key, pkt_bytes, ingress);
        count_packet();
    }

    return TC_ACT_OK;
}

/* ---- tc classifiers --------------------------------------------------- */

/* Egress: track outbound traffic */
/* tcx (kernel >= 6.6); the collector loader attaches these via tcx. */
SEC("tcx/egress")
int cost_tracker_egress(struct __sk_buff *skb)
{
    return track(skb, 0);
}

/* Ingress: track inbound traffic */
SEC("tcx/ingress")
int cost_tracker_ingress(struct __sk_buff *skb)
{
    return track(skb, 1);
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
