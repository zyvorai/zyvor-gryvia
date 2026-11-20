// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// sockops_optimize.c - Socket optimization via sockops and sk_msg redirect.
//
// Intercepts socket operations to detect local (same-node) connections and
// redirects traffic between them via bpf_msg_redirect_hash, bypassing the
// kernel TCP/IP stack for improved latency and throughput on intra-node
// communication.

#include "headers/common.h"
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

/* ---- structs ---------------------------------------------------------- */

struct sock_key {
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
};

/* Trusted service pair for selective bypass */
struct trusted_pair {
    __u32 ip_a;
    __u32 ip_b;
    __u16 port;
    __u16 _pad;
};

/* Bypass statistics indices */
#define STAT_REDIRECTED_BYTES   0
#define STAT_REDIRECTED_PKTS    1
#define STAT_ESTABLISHED_LOCAL  2
#define STAT_TOTAL_ESTABLISHED  3
#define NUM_BYPASS_STATS        4

/* ---- BPF maps --------------------------------------------------------- */

/* Socket hash map for message redirection */
struct {
    __uint(type, BPF_MAP_TYPE_SOCKHASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, struct sock_key);
    __type(value, int);
} sock_hash SEC(".maps");

/* Shadow set of the keys currently in sock_hash.  bpf_msg_redirect_hash()
 * drops the message when the peer is missing (and sockhash lookups cannot be
 * released from sk_msg), so sk_msg checks this map first and passes traffic
 * to non-local peers through the normal stack. */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, struct sock_key);
    __type(value, __u8);
} local_socks SEC(".maps");

/* Bypass statistics (percpu for lock-free updates) */
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, NUM_BYPASS_STATS);
    __type(key, __u32);
    __type(value, __u64);
} bypass_stats SEC(".maps");

/* Trusted service pairs (populated from userspace) */
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, struct trusted_pair);
    __type(value, __u8);
} trusted_pairs SEC(".maps");

/* ---- helpers ---------------------------------------------------------- */

static __always_inline void update_stat(__u32 idx, __u64 delta)
{
    __u64 *cnt = bpf_map_lookup_elem(&bypass_stats, &idx);
    if (cnt)
        *cnt += delta;
}

static __always_inline void extract_key_from_ops(struct bpf_sock_ops *skops,
                                                  struct sock_key *key)
{
    key->src_ip   = skops->local_ip4;
    key->dst_ip   = skops->remote_ip4;
    key->src_port = skops->local_port;
    /* local_port is host order; remote_port is network order held in a u32
     * (the kernel shifts it into the upper half) - bpf_ntohl() yields the
     * host-order port. */
    key->dst_port = (__u16)bpf_ntohl(skops->remote_port);
}

/* ---- sockops program -------------------------------------------------- */

SEC("sockops")
int bpf_sockops(struct bpf_sock_ops *skops)
{
    /* Reply for value-returning ops (TIMEOUT_INIT, RWND_INIT, ...): -1 means
     * "leave the TCP default"; 0 or positive would change the parameter. */
    int rv = -1;

    switch (skops->op) {
    case BPF_SOCK_OPS_ACTIVE_ESTABLISHED_CB:
    case BPF_SOCK_OPS_PASSIVE_ESTABLISHED_CB: {
        rv = 1;

        /* Only handle IPv4 */
        if (skops->family != AF_INET)
            break;

        update_stat(STAT_TOTAL_ESTABLISHED, 1);

        /* Heuristic for "probably same node": both endpoints in the same
         * /16 or loopback.  Addresses are in network byte order, so build
         * the masks with htonl().  A wrong guess is harmless: sk_msg only
         * redirects when the peer socket is itself in sock_hash. */
        __u32 local_ip  = skops->local_ip4;
        __u32 remote_ip = skops->remote_ip4;
        __u32 lo_mask   = bpf_htonl(0xFF000000);
        __u32 lo_net    = bpf_htonl(0x7F000000);
        bool loopback = (local_ip & lo_mask) == lo_net ||
                        (remote_ip & lo_mask) == lo_net;

        if (!loopback &&
            (local_ip & bpf_htonl(0xFFFF0000)) != (remote_ip & bpf_htonl(0xFFFF0000)))
            break;

        /* Register THIS socket under its own key (local -> remote).  The
         * peer registers itself under the mirrored key and sk_msg looks the
         * peer up by that mirrored key, so no reverse entry is added here
         * (it would overwrite the peer's own registration). */
        struct sock_key key = {};
        extract_key_from_ops(skops, &key);

        if (bpf_sock_hash_update(skops, &sock_hash, &key, BPF_ANY))
            break;

        __u8 one = 1;
        bpf_map_update_elem(&local_socks, &key, &one, BPF_ANY);

        update_stat(STAT_ESTABLISHED_LOCAL, 1);

        /* Enable state-change callbacks so we can drop the entry on close */
        bpf_sock_ops_cb_flags_set(skops,
            skops->bpf_sock_ops_cb_flags | BPF_SOCK_OPS_STATE_CB_FLAG);

        break;
    }

    case BPF_SOCK_OPS_STATE_CB: {
        /* args[1] is the new state: any transition out of ESTABLISHED (FIN,
         * close, ...) stops redirection immediately.  Sockets are also removed
         * from sock_hash automatically on destroy. */
        if (skops->args[1] != BPF_TCP_ESTABLISHED) {
            struct sock_key key = {};
            extract_key_from_ops(skops, &key);
            bpf_map_delete_elem(&sock_hash, &key);
            bpf_map_delete_elem(&local_socks, &key);
        }
        rv = 1;
        break;
    }

    default:
        break;
    }

    /* The verdict for value-returning ops travels in skops->reply; the
     * program's own return value must be 0 or 1. */
    skops->reply = rv;
    return 1;
}

/* ---- sk_msg program --------------------------------------------------- */

SEC("sk_msg")
int bpf_skmsg_redirect(struct sk_msg_md *msg)
{
    /* The peer registered itself under ITS view of the connection, i.e. the
     * mirror of ours: (our remote -> our local). */
    struct sock_key key = {};
    key.src_ip   = msg->remote_ip4;
    key.dst_ip   = msg->local_ip4;
    key.src_port = (__u16)bpf_ntohl(msg->remote_port);
    key.dst_port = (__u16)msg->local_port;

    /* bpf_msg_redirect_hash() DROPS the message when the peer is not in the
     * map (e.g. a remote peer), so only redirect if the peer is registered. */
    if (!bpf_map_lookup_elem(&local_socks, &key))
        return SK_PASS;

    update_stat(STAT_REDIRECTED_BYTES, msg->size);
    update_stat(STAT_REDIRECTED_PKTS, 1);

    /* Deliver straight to the peer's ingress queue, bypassing TCP/IP */
    return bpf_msg_redirect_hash(msg, &sock_hash, &key, BPF_F_INGRESS);
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
