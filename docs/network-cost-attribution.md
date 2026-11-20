# Network cost attribution

Opt-in pipeline that measures tenant network egress on each node, records it as `GryviaNetworkUsageRecord` objects, and
lets the gateway turn it into an **estimate** next to the GPU lines of an invoice. Everything is **off by default**; with
the defaults the collector, the charts and the gateway behave exactly as before. Costs are estimates from byte counters.
Nothing is invoiced, charged or paid.

Status: unit-tested with crafted pod/node JSON, canned counter samples and a fake API server; the map reader was
also run against real kernel LRU hash maps on Linux 7.0 (IPv4 and IPv6 key/value layouts), and `cost_tracker` itself was
run with `BPF_PROG_TEST_RUN` on crafted IPv4 and IPv6 packets (egress and ingress, both maps checked). **Not run end to end on a real cluster**
(pod churn, CNI behaviour and Netra's live API are unverified).

## What is measured

The only source of bytes is the `traffic_costs` map of the `cost_tracker` tcx program (`ebpf/cost_tracker.c`): cumulative
bytes per `(local IP, remote IP)` pair (IPv4 pairs in `traffic_costs`, IPv6 pairs in `traffic_costs6`, 32-byte key of the
two raw addresses, same value layout), `bytes_sent` counted on egress (local is the packet source) and `bytes_recv`
on ingress (local is the packet destination). It is attached to **one** interface (`-iface`); attach it to the interface
that carries pod traffic (with a routed CNI, the node NIC). Attaching to several interfaces would count a packet once per
interface.

`flow_event.Bytes` from `tcp_trace` is **not** used: the kernel program never increments the counters it reads at
`tcp_close`, so it is 0 today, and adding it to counter deltas would count the same connection twice. Flow events are not
billed, ever.

## Attribution (collector, userspace)

Flags: `-attribute-network` (needs `-iface` so `cost_tracker.o` is loaded, a cluster, `NODE_NAME`) and
`-publish-network-usage` (needs `-attribute-network`). Both default off.

The pod cache (`collector/pkg/netcost`) lists pods, nodes and namespaces from the API server every 15 s (paged) and is
**fail closed**: an address held by two pods, a `hostNetwork` pod, a finished pod, and any lookup when the cache is older
than 2 minutes resolve to "unknown". Counters carry no PID, so this pipeline uses the pod IP, not the PID -> cgroup
resolver of the Flight Recorder (which only knows pods with a `gryvia.io/job` label).

IPv4 and IPv6 are handled the same way. The cache holds every address of a pod (`status.podIPs`, so a dual-stack pod
resolves under both families, and is not ambiguous with itself) and node addresses; IPv6 addresses are compared in
canonical form (`2001:DB8::1` equals `2001:db8:0::1`). A pair whose two ends are not the same family, an IPv4-mapped
IPv6 address or an address with a zone is `invalid` and never billed. For IPv6, `unknown` (never priced) covers
ULA `fc00::/7`, link-local `fe80::/10` and multicast; any other address that is not a known pod or node is `external`
(global unicast).

A tenant is the namespace label `gryvia.io/tenant` (set by the tenant controller), falling back to the `tenant-<name>`
naming convention. The **local** address must be exactly one non-hostNetwork pod of a tenant namespace scheduled **on this
node**; otherwise the bytes are counted as unattributed (metric `gryvia_netcost_unattributed_bytes_total`) and never
billed.

| peerClass | meaning |
| --- | --- |
| `same-tenant` | peer is a pod of the same tenant |
| `other-tenant` | peer is a pod of another tenant |
| `cluster` | peer is a pod outside tenant namespaces (kube-system, ...) or a node address |
| `external` | public unicast address that is not in the cluster |
| `unknown` | private / CGNAT / link-local address that is neither a pod nor a node (other VPC, service VIP, ...) |

| zoneClass | meaning |
| --- | --- |
| `same-zone` / `cross-zone` | both nodes carry `topology.kubernetes.io/zone` and the values are equal / differ |
| `unknown-zone` | a label is missing, or the peer is unresolved: never priced |
| `internet` | every `external` peer |
| `same-node` | peer pod on the same node, only recorded for `other-tenant` |

## Double counting: the rules

1. **Egress only.** A connection between pods on two nodes is seen by both collectors: the sender's node sees egress of
   the source pod, the receiver's node sees ingress of the destination pod. Only the **egress of the source tenant's own
   pod, on the node that hosts it**, is billable, so each byte is billed at most once, to whoever sent it (providers bill
   egress). Ingress is stored in `ingressBytes` for information and never priced. Replies are billed to the replier.
2. **Own pods only.** A collector attributes only local addresses of pods scheduled on its node; traffic it merely
   forwards for a pod of another node is never counted.
3. **One byte source.** Counter deltas are the only input. `tcp_trace` connect/close events add nothing
   (`Meter.NoteFlowEvent` only counts them).
4. **Loopback and same-node pod-to-pod** are excluded, unless the peer belongs to another tenant (kept as
   `other-tenant` / `same-node`, which no rate prices, so it is visible but free unless a provider adds a rate).
5. **Collector and Netra are never summed.** Every record has a `source` (`collector`; `netra` is reserved). The gateway
   uses exactly one source per request (`source=collector` by default). Netra is only used to reconcile.
6. **Idempotent accumulation.** Buckets are keyed `(UTC hour, tenant, peerClass, zoneClass)` and hold both directions.
   Only deltas are added: a re-observed cumulative value adds 0; a counter that went down (kernel LRU eviction or program
   reload) is a reset and its new value is the delta; a pair listed twice in one scan is counted once. Memory is bounded
   (65 536 pairs, 20 000 buckets; overflow is counted, not stored).
7. **Duplicate records** with the same `(node, tenant, hour, peerClass, zoneClass)` are counted once by the gateway
   (larger egress wins); fragments of different nodes are summed.

## Records

`GryviaNetworkUsageRecord` (namespaced, written into `tenant-<tenant>`), one per `(node, tenant, UTC hour, peerClass,
zoneClass)`, named `netusage-<node>-<yyyymmddhh>-<peerClass>-<zoneClass>`, with `egressBytes`, `ingressBytes`,
`source`, `node` and `final`. It is a **new kind** rather than new fields on `GryviaUsageRecord`: that kind is owned and
rewritten by the quota operator per job and aggregated as GPU hours by the gateway, the CLI and the invoice, so mixing
would break them. Prices live in the cluster-scoped `GryviaNetworkRate` (`sameZone`, `crossZone`, `internetEgress` per
GB, 1 GB = 10^9 bytes, `currency`); the gateway uses the first object by name. `GryviaNetworkCost` is untouched.

The publisher writes every 60 s. An hour is closed (`final: true`, never modified again, evicted from memory) 2 minutes
after it ends. **Restart safety:** the first time a process publishes a bucket it reads the stored record and adds its own
bytes on top, so a restart never lowers a value and a final record is never touched. Bytes counted in memory but not yet
written when a collector dies, and traffic during downtime, are lost: records can undercount, never double count.

## Gateway, CLI, invoice

- `GET /api/network/usage?tenant=&from=&to=&groupBy=tenant|peerClass|zoneClass|day&source=collector` bytes (and cost when
  a rate exists). Tenant users see only their own tenant's namespaces regardless of `tenant`; admins see all.
- `GET /api/invoices/...` gains `networkLines` (per peerClass/zoneClass: GB x rate), `networkSubtotal` and `networkNote`
  **only when both records and a rate exist**. `subtotal` stays GPU-only; status stays `estimate`. The CSV appends
  `network:<peer>/<zone>` rows and a `NETWORK-SUBTOTAL` row after the GPU `TOTAL` row (the `gpuHours` column then holds GB).
  Unpriced classes are listed with `priced: false` and amount 0.
- `GET /api/network/reconcile?tenant=&month=&tolerancePct=` compares the collector's egress for the tenant and month with
  Netra's egress records (`GET /api/v1/flows/history`, namespace `tenant-<tenant>`, direction `egress`, `observedAt` in
  the month; needs `GRYVIA_NETRA_URL`). It returns `deltaBytes`, `percent` (relative to the larger figure) and a verdict:
  `within-tolerance`, `investigate`, or `insufficient-data` (Netra unreachable, no records on either side, or Netra's
  history hit its 5 000-record limit). The tolerance is `tolerancePct` or `GRYVIA_NETWORK_RECONCILE_TOLERANCE_PCT`
  (default 10). Tenant users can only reconcile their own tenant (another tenant is a 404). The two figures are not
  expected to match exactly: the collector excludes same-node and unattributed traffic. Netra's per-record byte
  semantics and history retention are taken from its docs, not verified here.
- CLI: `gryvia usage --network [--group-by tenant|peer-class|zone-class|day] [-o table|json|yaml|csv]`.
- Dashboard: the Invoices page shows the network section when present.

## Deploy

```bash
helm upgrade gryvia-ni helm/network-intelligence --set ebpf.enabled=true --set ebpf.interface=eth0 \
  --set ebpf.attributeNetwork=true --set ebpf.publishNetworkUsage=true
kubectl apply -f examples/networkrate.yaml
```

`ebpf.attributeNetwork` adds only the flag (pods, nodes and namespaces are already readable by the collector role).
`ebpf.publishNetworkUsage` additionally adds `create`, `get`, `patch` on `gryvianetworkusagerecords` to the collector
ClusterRole. The gateway chart gets read access to `gryvianetworkusagerecords` and `gryvianetworkrates`.

## What is NOT measured

- **NAT / SNAT:** traffic whose source is rewritten to a node address before the interface (masquerade, egress gateways)
  has a node IP as local address and is unattributed. Service ClusterIPs are translated before the NIC sees the packet,
  so the peer is the backend pod.
- **hostNetwork pods** share the node IP and are never attributed. Traffic between pods on one node does not cross the
  NIC and is not counted.
- **IPv6 extension headers are not walked.** IPv6 bytes are `payload_length + 40`: extension headers are inside
  `payload_length`, so they count as bytes, but the upper-layer protocol is never parsed, and a jumbo packet
  (`payload_length` 0) counts only its 40-byte header. The kernel-side zone classification (`zone_map`, `cost_stats`)
  is IPv4 only; IPv6 adds to the packet total only (tenant attribution is userspace and covers IPv6). NPTv6 or NAT64
  rewrites are like SNAT below: the local address is not a pod and the bytes are unattributed.
- Bytes are IP-level (IPv4 `tot_len`, IPv6 as above): headers included, encrypted payload irrelevant. Retransmissions count.
- Traffic during collector downtime, the first scan gap, and the sub-scan skew at hour boundaries (up to 15 s of bytes
  land in the next hour).
- `unknown-zone` and `unknown` peers are reported but never priced. Provider-side discounts, tiers, NAT gateway or load
  balancer processing charges are not modelled.
