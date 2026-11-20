// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
//
// trace_correlator.c - Distributed trace correlation via tc ingress.
//
// Parses incoming TCP packets for HTTP headers containing W3C traceparent
// headers.  Extracts trace-id and span-id and stores them keyed by the
// connection's 4-tuple, enabling correlation of eBPF-observed network
// events with application-level distributed traces.

#include "headers/common.h"

/* Maximum payload bytes to scan for traceparent header */
#define MAX_PAYLOAD_SCAN 512
/* Start offsets tried for the header name (bounded loop for the verifier) */
#define MAX_SCAN_POS 256

/* traceparent header format:
 * traceparent: 00-<32 hex trace-id>-<16 hex span-id>-<2 hex flags>
 * Example: traceparent: 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01
 */

/* ---- structs ---------------------------------------------------------- */

struct trace_context {
    __u64 trace_id_hi;
    __u64 trace_id_lo;
    __u64 span_id;
    __u64 parent_span_id;
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
    __u32 pid;
    __u64 timestamp;
};

/* The collector (collector/pkg/trace) decodes this struct as a 56-byte little-endian record. */
_Static_assert(sizeof(struct trace_context) == 56, "trace_context size");
_Static_assert(__builtin_offsetof(struct trace_context, span_id) == 16, "span_id offset");
_Static_assert(__builtin_offsetof(struct trace_context, src_ip) == 32, "src_ip offset");
_Static_assert(__builtin_offsetof(struct trace_context, dst_ip) == 36, "dst_ip offset");
_Static_assert(__builtin_offsetof(struct trace_context, src_port) == 40, "src_port offset");
_Static_assert(__builtin_offsetof(struct trace_context, dst_port) == 42, "dst_port offset");
_Static_assert(__builtin_offsetof(struct trace_context, timestamp) == 48, "timestamp offset");

/* 4-tuple key for trace context map */
struct trace_key {
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
};

/* ---- BPF maps --------------------------------------------------------- */

/* Trace context map: 4-tuple -> trace context */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, struct trace_key);
    __type(value, struct trace_context);
} trace_ctx_map SEC(".maps");

/* Ring buffer for trace correlation events */
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 128 * 1024);
} trace_events SEC(".maps");

GRYVIA_DECLARE_DROPS();

/* ---- helpers ---------------------------------------------------------- */

/* Convert a single hex character to its numeric value */
static __always_inline int hex_to_val(char c)
{
    if (c >= '0' && c <= '9')
        return c - '0';
    if (c >= 'a' && c <= 'f')
        return c - 'a' + 10;
    if (c >= 'A' && c <= 'F')
        return c - 'A' + 10;
    return -1;
}

/* The payload is copied into a per-CPU scratch buffer with bpf_skb_load_bytes and scanned there with masked
 * indexes. Scanning packet memory directly (variable pointer + bounds check per offset) makes the verifier track a
 * distinct state per offset and exceeds its complexity limit on some kernels (E2BIG on 6.17). */
#define SCAN_BUF 1024 /* power of two: indexes are masked with SCAN_BUF - 1 */

/* Mask an index so the verifier can bound the access. The empty asm comes first so clang cannot prove the value
 * is already small (from earlier checks) and drop the AND; older verifiers cannot see that bound and would reject
 * the access as unbounded. */
static __always_inline __u32 clamp_idx(__u32 i)
{
    asm volatile("" : "+r"(i));
    return i & (SCAN_BUF - 1);
}
#define SB(b, i) ((b)->d[clamp_idx(i)])

struct scan_buf {
    unsigned char d[SCAN_BUF];
};

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct scan_buf);
} scan_scratch SEC(".maps");

/* Parse 16 hex characters at buf[off..off+15] into a __u64. Returns 0 on success, -1 on failure. */
static __always_inline int parse_hex64(struct scan_buf *buf, __u32 off, __u64 *result)
{
    __u64 val = 0;

    #pragma unroll
    for (int i = 0; i < 16; i++) {
        int v = hex_to_val(SB(buf, off + i));
        if (v < 0)
            return -1;
        val = (val << 4) | (__u64)v;
    }

    *result = val;
    return 0;
}

struct find_ctx {
    struct scan_buf *buf;
    __u32 n;
    int found; /* offset just after the colon, or -1 */
};

/* bpf_loop callback: does "traceparent:" (case-insensitive first 't') start at offset i? Returning 1 stops. */
static long find_cb(__u64 idx, void *data)
{
    struct find_ctx *fc = data;
    struct scan_buf *buf = fc->buf;
    __u32 i = (__u32)idx;

    if (i + 12 > fc->n)
        return 1;
    unsigned char c0 = SB(buf, i);
    if (c0 != 't' && c0 != 'T')
        return 0;
    if (SB(buf, i + 1) == 'r' && SB(buf, i + 2) == 'a' && SB(buf, i + 3) == 'c' &&
        SB(buf, i + 4) == 'e' && SB(buf, i + 5) == 'p' && SB(buf, i + 6) == 'a' &&
        SB(buf, i + 7) == 'r' && SB(buf, i + 8) == 'e' && SB(buf, i + 9) == 'n' &&
        SB(buf, i + 10) == 't' && SB(buf, i + 11) == ':') {
        fc->found = (int)(i + 12);
        return 1;
    }
    return 0;
}

/* Find "traceparent:" in the first n bytes of buf. Returns the offset just after the colon, or -1. bpf_loop
 * (Linux 5.17+) verifies the body once instead of once per offset, which keeps the verifier's state count small on
 * every kernel. */
static __always_inline int find_traceparent(struct scan_buf *buf, __u32 n)
{
    struct find_ctx fc = { .buf = buf, .n = n, .found = -1 };

    bpf_loop(MAX_SCAN_POS, find_cb, &fc, 0);
    return fc.found;
}

/* ---- tc classifier ---------------------------------------------------- */

SEC("tcx/ingress")
int trace_correlator_ingress(struct __sk_buff *skb)
{
    void *data     = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;

    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return TC_ACT_OK;
    if (eth->h_proto != bpf_htons(ETH_P_IP))
        return TC_ACT_OK;

    struct iphdr *iph = (void *)(eth + 1);
    if ((void *)(iph + 1) > data_end)
        return TC_ACT_OK;
    if (iph->protocol != IPPROTO_TCP)
        return TC_ACT_OK;

    __u32 ip_hdr_len = iph->ihl * 4;
    if (ip_hdr_len < sizeof(struct iphdr))
        return TC_ACT_OK;

    struct tcphdr *tcph = (void *)iph + ip_hdr_len;
    if ((void *)(tcph + 1) > data_end)
        return TC_ACT_OK;

    __u32 tcp_hdr_len = tcph->doff * 4;
    if (tcp_hdr_len < sizeof(struct tcphdr))
        return TC_ACT_OK;

    /* Payload offset; bpf_skb_load_bytes also reads paged (non-linear) skb data. */
    __u32 pay_off = sizeof(struct ethhdr) + ip_hdr_len + tcp_hdr_len;
    __u32 len = skb->len;
    if (pay_off >= len)
        return TC_ACT_OK;
    __u32 n = len - pay_off;
    if (n > MAX_PAYLOAD_SCAN)
        n = MAX_PAYLOAD_SCAN;
    /* "traceparent:" + optional space + "00-" + 32 + "-" + 16 hex */
    if (n < 12 + 1 + 55)
        return TC_ACT_OK;

    __u32 zero = 0;
    struct scan_buf *buf = bpf_map_lookup_elem(&scan_scratch, &zero);
    if (!buf)
        return TC_ACT_OK;
    if (bpf_skb_load_bytes(skb, pay_off, buf->d, n))
        return TC_ACT_OK;

    int header_offset = find_traceparent(buf, n);
    if (header_offset < 0)
        return TC_ACT_OK;

    __u32 v = (__u32)header_offset;
    if (SB(buf, v) == ' ')
        v++;

    /* Expected format: "00-<32 hex>-<16 hex>-<2 hex>" = 55 chars */
    if (v + 55 > n)
        return TC_ACT_OK;
    if (SB(buf, v) != '0' || SB(buf, v + 1) != '0' || SB(buf, v + 2) != '-')
        return TC_ACT_OK;

    __u64 trace_id_hi = 0, trace_id_lo = 0, span_id = 0;
    if (parse_hex64(buf, v + 3, &trace_id_hi) < 0)
        return TC_ACT_OK;
    if (parse_hex64(buf, v + 19, &trace_id_lo) < 0)
        return TC_ACT_OK;
    if (SB(buf, v + 35) != '-')
        return TC_ACT_OK;
    if (parse_hex64(buf, v + 36, &span_id) < 0)
        return TC_ACT_OK;

    /* Store trace context keyed by 4-tuple */
    struct trace_key key = {};
    key.src_ip   = iph->saddr;
    key.dst_ip   = iph->daddr;
    key.src_port = bpf_ntohs(tcph->source);
    key.dst_port = bpf_ntohs(tcph->dest);

    struct trace_context tctx = {};
    tctx.trace_id_hi = trace_id_hi;
    tctx.trace_id_lo = trace_id_lo;
    tctx.span_id     = span_id;
    tctx.src_ip      = iph->saddr;
    tctx.dst_ip      = iph->daddr;
    tctx.src_port    = key.src_port;
    tctx.dst_port    = key.dst_port;
    tctx.timestamp   = bpf_ktime_get_ns();

    bpf_map_update_elem(&trace_ctx_map, &key, &tctx, BPF_ANY);

    /* Emit event to ring buffer */
    struct trace_context *ev;
    ev = bpf_ringbuf_reserve(&trace_events, sizeof(*ev), 0);
    if (!ev) {
        GRYVIA_COUNT_DROP(GRYVIA_DROP_RINGBUF);
        return TC_ACT_OK;
    }

    *ev = tctx;
    bpf_ringbuf_submit(ev, 0);

    return TC_ACT_OK; /* always pass the packet */
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
