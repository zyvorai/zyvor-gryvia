# Network Intelligence Guide

Guide to Gryvia's eBPF-based network intelligence: the kernel programs, the per-node collector, the
`network-intelligence` operator and its CRDs. Read the status box first: the programs and the operator are real code and
the operator now reads its sources, but none of it has run on a real cluster with the real collector.

:::caution Status: what works and what does not
**Programs.** The 47 eBPF programs in `ebpf/` are CO-RE (no per-kernel builds; they need a node kernel with BTF, and the
`tcx` programs Linux 6.6+). All 30 compile and pass the kernel verifier on Linux 7.0 x86_64; on that host the collector
attached 36 of the 83 hooks (the kprobes and tracepoints) and decoded real TCP flows. **Not verified:** arm64 (compiles,
never loaded), the XDP/TCX/sockops programs (attach is config-gated: `ebpf.interface`, `ebpf.cgroupPath`), and
everything GPU-related (NCCL/CUDA uprobes, RDMA, GDS), which needs GPU or RDMA hardware. Attach results per node are at
`GET :9090/api/v1/ebpf/status`. See [ebpf/README.md](https://github.com/zyvorai/gryvia/blob/main/ebpf/README.md).

**Collector.** A privileged, `hostNetwork`, `hostPID` DaemonSet in the `helm/network-intelligence` chart,
disabled by default (`ebpf.enabled=false`). Its image (`gryvia-ebpf-collector`) is built in CI and is in the signed multi-arch release
workflow, but no tagged release has published it yet. Its HTTP listener on `:9090` (`/metrics`, `/healthz`, `/api/v1/{graph,anomalies,gpu/nccl,gpu/memory,fabric,security/alerts,ai/training,ai/pipeline,tuning/tcp,ebpf/status}`)
is unauthenticated by default (HMAC, TLS and mTLS are opt-in: `docs/collector-security.md`); `/api/v1/flight/*` and
`/api/v1/inference` always need the flight token.

**Operator.** The ten controllers read real sources; full table in
[`docs/network-intelligence-sources.md`](https://github.com/zyvorai/gryvia/blob/main/docs/network-intelligence-sources.md).
Verified with unit tests (fake Kubernetes client, fake sources, `httptest` collector) and a kind e2e workflow that uses a
FAKE collector (operator side only); **not** run against the real collector at scale, real Hubble or Cilium.

- `ServiceGraph` edges, `NetworkAnomaly`, `TrafficInsight` top talkers and `SecurityPolicy` alerts come from the merged
  `/api/v1/{graph,anomalies,security/alerts}` of every collector pod (discovered by label, requests HMAC-signed with a
  mounted token, optional TLS/mTLS). `NetworkCost` is built from `GryviaNetworkUsageRecord` and `GryviaNetworkRate`
  objects (no HTTP), `TrainingInsight`/`InferenceInsight` from `GryviaFabricSignal.status`, `TraceSession` and
  `FlowPolicy.status.matchedFlows` from Netra, and `AutoPolicy` writes `GryviaFlowPolicy` suggestions from the learned graph.
- Every status has a `SourceAvailable` condition: `False` with a reason (`NoCollectors`, `Unreachable`, `NotConfigured`,
  `NoSignal`, `NoData`, ...) and a message when the source is missing, so an empty status is never mistaken for "nothing
  happened". Fields with no source (p99 latency and drop counts of `TrafficInsight`, per-rank statistics, the inference
  phase breakdown) stay unset.
- `GryviaFlowPolicy`, `GryviaAutoPolicy` suggestions once approved, and the auto-mitigation of `GryviaNetworkAnomaly`
  create **CiliumNetworkPolicy** objects, so those features need Cilium as the CNI. Nothing was verified on a Cilium cluster.
- The Hubble gRPC and Prometheus (PromQL) paths do not exist any more: nothing here uses Hubble or Prometheus.
- Enable the wiring in the chart with `operator.sources.enabled=true` (RBAC, token and TLS Secret mounts).

**Flow sources.** Real flows can also come from **[Netra](https://github.com/zyvorai/netra)**, the separate standalone eBPF
network observability product: set `apiGateway.netra.url` (an address the gateway pod can reach; a `*.svc` name only works
when Netra runs in the same cluster) and `apiGateway.netra.tokenSecret` in the `gryvia` chart, and
`GET /api/network/flows` returns Netra flows. Netra has its own license; Gryvia only calls its HTTP API. With neither
source the network graph, flows and security pages stay empty.
:::

## Table of Contents

1. [Architecture](#architecture)
2. [eBPF Programs](#ebpf-programs)
3. [CRDs](#crds)
4. [CLI Commands](#cli-commands)
5. [Deployment](#deployment)
6. [Best Practices](#best-practices)

---

## Architecture

```
+--------------------+     +------------------+     +-------------------+     +--------+
|   eBPF Programs    | --> |    Collector     | --> |     Operator      | --> |  CRDs  |
| (kernel-attached)  |     | (per-node agent) |     | (control plane)   |     | status |
+--------------------+     +------------------+     +-------------------+     +--------+
        |                         |                         |                     |
  Kernel hooks              Reads maps, folds         Reconciles CRDs,      User-facing
  (kprobe, tracepoint,      events, serves HTTP       creates Cilium        resources
   uprobe, XDP, tcx,        on :9090                  policies
   sockops)
```

**eBPF programs** run in the kernel on every node and capture socket, syscall, packet and GPU-library events. Their
overhead was measured once, on one shared x86 server with loopback traffic only (no NIC, GPU, RDMA or NCCL uprobes):
roughly 3x on a no-op syscall (145 to about 500 ns for `getpid`), about 8-22 us more on 64-byte request/response
latency, no cost distinguishable from noise for bulk TCP with the default program set, and about 41% of one core for
the collector process while the load generator triggered millions of program runs per second (2% idle). Method, noise floor and caveats:
[eBPF overhead](https://github.com/zyvorai/gryvia/blob/main/docs/ebpf-overhead.md). Real NIC, GPU and multi-node
overhead is not measured.

**Collector** (`collector/`) is a per-node DaemonSet that loads the compiled objects with cilium/ebpf, attaches them by
section name, reads the maps and ring buffers, and serves metrics and JSON on `:9090`.

**Operator** (`operators/network-intelligence`, chart `helm/network-intelligence`, not part of `helm/gryvia`) runs as a
Deployment and reconciles the ten CRDs below.

**CRDs** are the user-facing interface. Their status fields are only as good as the data path described in the status box.

---

## eBPF Programs

Gryvia ships 47 eBPF programs (24 original programs in six categories, plus eleven fabric-signal programs and twelve newer ones, from `xdp_mux` to `ucx_complete` in `ebpf/README.md`, ten of which are opt-in with `-enable-programs` because nothing in the collector consumes their signals yet). The collector
loads them and attaches each by section name. Hook types below come from the `SEC()` annotations in `ebpf/*.c`.

### GPU Programs

| Program | Hook Type | Description |
|---------|-----------|-------------|
| `nccl_trace` | uprobe (`ncclAllReduce`, `ncclAllGather`, `ncclBroadcast`, `ncclReduce`, `ncclReduceScatter`, `ncclSend`, `ncclRecv`, group calls) | NCCL collective timing with per-operation histograms. |
| `gpu_mem_trace` | uprobe (`cudaMemcpy`, `cudaMemcpyAsync`, `cudaMalloc`, `cudaFree`, `cudaLaunchKernel`, `cudaDeviceSynchronize`) | Direction-aware GPU memory transfer byte counters and allocation tracking in the CUDA runtime library. |
| `rdma_trace` | kprobe (`ib_post_send`, `ib_post_recv`, `ib_poll_cq`), tracepoint (`rdma/rdma_create_qp`, `rdma/rdma_destroy_qp`) | Per-QP RDMA statistics and completion tracking. |
### Fabric Signal Programs

Ten programs are listed here. Nine feed the scheduler-facing fabric signals and are observe-only (nothing is dropped or modified); `quota_pace` is the one exception, is off by default and is described last. Those that use a ring buffer
emit `struct fabric_signal` on their own `fabric_events` ring buffer instead of extending the frozen 72-byte `gpu_event`.
The collector folds the signals per job over a 5-minute window and serves them, with a `[0,1]` score penalty, at
`GET :9090/api/v1/fabric` and as `gryvia_fabric_*` Prometheus gauges. The `GryviaFabricSignal` CRD (short name `gfs`)
carries the same fields in its status. With `-publish-fabric-status` (off by default) the collector patches the status of an existing `GryviaFabricSignal` whose `spec.jobRef` names the job; with `-fabric-status-per-node` and the ai-operator's `--merge-fabric-signals`, per-node entries are folded in. The scheduler does not read this per-job score; opt-in fabric-aware scheduling reads the per-node `GryviaNodeFabric` instead (see [Scheduling](SCHEDULING.md)).

| Program | Hook Type | Description |
|---------|-----------|-------------|
| `straggler` | uprobe (`ncclAllReduce`) | Flags a rank whose collective takes more than 2x the fastest recent span of the same payload size (and more than 5 ms) on the same node. |
| `rdma_health` | kprobe (`ib_post_send`, `mlx5_ib_post_send`, `ib_poll_cq`, `mlx5_ib_poll_cq`) | Counts sends per QP and retry-exceeded / RNR-exceeded work completions; emits a signal above thresholds set in the `rdma_thresh` map. Every probe is optional. |
| `gds_trace` | uprobe (`cuFileRead`, `cuFileWrite`), kprobe (`nvidia_fs_read`) | Counts bytes and times cuFile calls; a call is "direct" only when the nvidia-fs kernel hook is also seen, otherwise it counts as bounce/unknown. Only calls slower than 2 ms are emitted. |
| `overlap` | uprobe/uretprobe (`ncclAllReduce`, `cudaDeviceSynchronize`) | Reports a `cudaDeviceSynchronize` of at least 1 ms that runs nested inside an in-flight `ncclAllReduce` on the same thread (GPU idle while communicating). Folded as `overlapIdleRatio`, the share of the window spent in such syncs. |
| `roce_cnp` | XDP | Counts RoCEv2 congestion notification packets (UDP 4791, BTH opcode 0x81) in per-CPU counters; always returns `XDP_PASS`. Attached only with `-iface`; the collector polls the counters and folds them as `cnpRate` (packets per second, job `_node/cnp`) and `gryvia_roce_cnp_packets_total`. |
| `infer_latency` | kretprobe (`inet_csk_accept`), kprobe (`tcp_recvmsg`) | Time from accept to the first read on connections to the inference ports (`-infer-ports`, default `8000,8001`: vLLM and Triton HTTP; skipped when the list is empty). Folded as `inferWaitP99ms`, informational only. This is a **network accept wait**, not engine queue time, time to first token or inter-token latency; those come from the engines' own metrics (opt-in `-infer-metrics`, see [inference-latency](https://github.com/zyvorai/gryvia/blob/main/docs/inference-latency.md)). |
| `ucx_gloo` | uprobe/uretprobe (`ucp_tag_send_nb`, `ucp_tag_send_nbx`) | UCX tag-send calls that blocked for at least 5 ms (the span of the posting call, not of the transfer). Skipped when `libucp.so` is not found (`-ucx-lib`, `-uprobe-pid`). Gloo is **not** probed: its allreduce symbols are C++ mangled, vary by version and are normally linked statically into `libtorch_cpu.so`, so a fixed probe could never attach. Folded as `ucxSlowP99ms`, informational only. |
| `pfc_pause` | XDP | Counts 802.1Qbb priority-flow-control pause frames (EtherType 0x8808, opcode 0x0101) in per-CPU counters, with a count per paused priority, plus 802.3x pause frames; always `XDP_PASS`. Attached only with `-iface`; folded as `pfcRate` (frames per second, job `_node/pfc`) and `gryvia_pfc_*_total` counters. Only one XDP program can own an interface, so it conflicts with `roce_cnp`, `packet_filter` and `dns_tracker`: the collector attaches the first one and skips the others with a logged reason instead of replacing it. Many NICs handle pause frames in the MAC and never pass them to XDP, in which case the counters stay 0. |
| `weight_exfil` | kprobe (`vfs_read`, `tcp_v4_connect`) | Observe only. A read of at least 8 MiB from a model-weight file (by name: `.safetensors`, `.gguf`, `.ckpt`, `.onnx`, `.pt`, `.pth`, `.bin`, `.h5`), followed within 30 s by a `connect` from the same process to a destination outside loopback, RFC1918 and link-local ranges, produces one signal (also logged as a warning). A read alone never does. It is a heuristic (IPv4 only, `read()` only, name-based) and a model server that legitimately calls an external API after loading weights will trip it. Folded as `exfilEvents`; it never changes `scoreDelta`. |
| `quota_pace` | sockops (cgroup v2) | **The only program that changes anything, and it is off by default.** When a process in a cgroup that has an entry in the `pace_rate` map opens an outbound TCP connection, the socket's `SO_MAX_PACING_RATE` is lowered to that rate (never raised, and never below 1 Mbit/s). No entry, no change. Attached only with `-quota-pace` **and** `-cgroup-path`. Entries are lease-gated (`Pacer` in `collector/pkg/fabric`): expiry deletes the entry and the collector deletes every entry it wrote when it shuts down. Sockets that are already paced keep their rate until they close; only new connections are unpaced. Alone the flag only attaches the program and paces nothing; the opt-in `-quota-pace-sync` reconcile of `GryviaQuota` `spec.network.maxEgressMbps` is the only thing that grants leases (see [Fabric status and quota pacing](#fabric-status-and-quota-pacing)). |
| `xdp_mux` | XDP | The one XDP program to attach to an interface (`-xdp-mux`, off by default, needs `-iface`). It tail-calls the first populated slot of `xdp_features`, and `roce_cnp`, `pfc_pause`, `dns_tracker`, `packet_filter` and `roce_ecn` each chain to the next, so all loaded features see every packet; `XDP_PASS` when none is loaded. Without the flag only one XDP program attaches per interface. |
| `roce_ecn` | XDP | RoCEv2 packets (UDP 4791, IPv4 and IPv6) counted per ECN codepoint (seen, CE, ECT) in per-CPU counters; always `XDP_PASS`. Slot 4 of `xdp_mux`. The collector does not read the counters yet. |
| `nccl_transport` | uprobe/uretprobe (`ncclCommInitRank`, `ncclCommInitRankConfig`, `ncclGetUniqueId`) | One signal per successful communicator init: rank, world size, init time and a transport hint that the collector does not fill yet (`fabric.TransportHint` classifies `NCCL_*` variables but is not called). |
| `p2p_fallback` | uprobe/uretprobe (`cudaDeviceEnablePeerAccess`, `cudaMemcpyAsync`) | A failed peer-access enable (not "already enabled") followed within 30 s by a device-to-device copy of at least 8 MiB from the same process: a hint of a host bounce buffer, not proof. |
| `capture_gate` | kprobe (`tcp_sendmsg`) | A per-cgroup arm for the Flight Recorder (`capture_lease`); fails open. Nothing writes the lease yet, so it never arms and only counts consultations. |
| `gpu_oom` | uprobe/uretprobe (`cudaMalloc`, `cudaMallocAsync`, `cudaMallocFromPoolAsync`) | A `cudaErrorMemoryAllocation` return. Routine in PyTorch's caching allocator (it retries), so a burst means memory pressure and one is not an incident. |
| `graph_stall` | uprobe/uretprobe (`cudaStreamBeginCapture`, `cudaStreamEndCapture`, `cudaDeviceSynchronize`) | A device-wide sync while a CUDA graph capture is open on the thread, which normally invalidates the capture. |
| `gdr_fail` | kretprobe (`nvidia_p2p_get_pages`) | GPUDirect RDMA page pinning returning non-zero; attaches only while the nvidia module is loaded. |
| `infer_ttft` | kretprobe (`inet_csk_accept`), kprobe (`tcp_sendmsg`) | On the `-infer-ports`: accept to first send, and a send gap of at least 50 ms once per connection. Not tokenizer TTFT; the gap cannot tell a mid-response stall from an idle keep-alive connection. |
| `weight_mmap` | kprobe (`security_mmap_file`, `tcp_v4_connect`) | An mmap of a model-weight file (same name list as `weight_exfil`) followed within 30 s by a connect to a public IPv4. |
| `gpu_dev` | kprobe (`security_file_open`) | An open of a `nvidia*` or `renderD*` character device from a cgroup not in `allowed_cg`, only when `gpu_dev_cfg[0]` enforces; nothing writes either map yet, so it only counts opens. |
| `ucx_complete` | uprobe/uretprobe (`ucp_tag_send_nbx`, `ucp_worker_progress`) | `ucp_worker_progress` ran at least 5 ms after a send that returned a request. Completion is not observed, so this is a hint. |

The fabric score penalty (`scoreDelta`, capped at 1) adds 0.15 when `overlapIdleRatio` exceeds 0.3, 0.15 when
`cnpRate` exceeds 100 packets per second and 0.10 when `pfcRate` exceeds 1000 pause frames per second, on top of the
straggler, RDMA and GDS terms. `exfilEvents`, `ucxSlowP99ms` and `inferWaitP99ms` are informational. The thresholds are heuristics
that have not been calibrated on real fabrics. `FabricPenalty` in the ai-operator scheduler package turns `scoreDelta`
into up to 25 points to subtract. With `--fabric-aware-scheduling` (or the per-job annotation `gryvia.io/fabric-aware`) the job controller reads the fresh per-node `GryviaNodeFabric` objects the collector publishes (`-publish-node-fabric`) and applies the penalty before choosing nodes; stale signals are ignored and a lookup failure leaves the ranking unchanged. Off by default; unit-tested with fake signals, never run on a real fabric.

:::caution Unverified on hardware
These programs compile and pass the kernel verifier (Linux 7.0 x86_64; arm64 compiles only). `roce_cnp` and
`pfc_pause` were run on crafted packets with `BPF_PROG_TEST_RUN`, `overlap` and `ucx_gloo` on stand-in libraries,
`weight_exfil` briefly on the live hooks of a test process, and `quota_pace` on a throwaway cgroup on that host; none of it has run on real UCX,
PFC or model-serving workloads or on arm64 hardware. The GPU, RDMA
and GDS paths, real RoCE traffic and `infer_latency` attached to a live server have not been exercised on GPU, RDMA or GPUDirect Storage hardware. On the test host none of
`ib_post_send`, `ib_poll_cq`, `mlx5_ib_*` or `nvidia_fs_read` exists, and the collector skips those hooks with a log
line (they show as `skipped: symbol not found` in `/api/v1/ebpf/status`). `ib_post_send` and `ib_poll_cq` are inline
wrappers, so kernel-side hooks only see in-kernel RDMA users; user-space verbs (NCCL) bypass them. The straggler span
is the host-side duration of the NCCL call, which is the enqueue time for asynchronous collectives. Inside pods the
uprobes need the library resolved through `/proc/<pid>/root` (`-uprobe-pid`); the per-container mount-namespace
resolver is not built yet. Signals are attributed to a job by pid through the Flight Recorder's resolver (host PID, then
cgroup pod UID, then the node's pod list and its `gryvia.io/job` label); a pid that cannot be resolved (not in a pod, pod
not on this node, no `gryvia.io/job` label, stale pod list) is never guessed and stays grouped under `_unattributed` by
process name, and node-level counters stay under `_node/*`. Neither is ever published to a `GryviaFabricSignal`.

### Fabric status and quota pacing

Both are opt-in collector features; see [`docs/fabric-status.md`](https://github.com/zyvorai/gryvia/blob/main/docs/fabric-status.md)
for the flags, RBAC and safety rules.

| Feature | Flag (chart value) | Default | Changes anything? |
|---------|--------------------|---------|-------------------|
| Publish fabric status into `GryviaFabricSignal.status` | `-publish-fabric-status` (`ebpf.publishFabricStatus`) | off | Writes the status subresource of existing objects every 30 s; needs extra RBAC |
| Quota pacing from `GryviaQuota` | `-quota-pace` + `-cgroup-path` + `-quota-pace-sync` (`ebpf.quotaPace.enabled`, `.sync`) | off | **Yes**: lowers the pacing rate of new outbound TCP connections of the quota's pods on the node |
| Dry run of the above | `-quota-pace-dry-run` (`ebpf.quotaPace.dryRun`) | off | No: logs what would be granted or revoked |

`spec.network.maxEgressMbps` (1 to 32000) on a `GryviaQuota` is the per-connection egress cap for pods in the quota's
namespaces (`SO_MAX_PACING_RATE`, not an aggregate limit, TCP only). `kube-system`, `gryvia-system`, `kube-public`,
`kube-node-lease` and the collector's own namespace are never paced.
:::

### Security Programs

| Program | Hook Type | Description |
|---------|-----------|-------------|
| `container_escape` | raw_tracepoint (`sys_enter`), kprobe (`security_file_open`) | Flags suspicious syscalls (unshare, setns and similar) and sensitive file access from containers. |
| `crypto_detect` | kprobe (`tcp_v4_connect`), tracepoint (`sched/sched_process_exec`) | Flags outbound connections to mining-pool ports and known miner executables. |
| `exfil_detect` | kprobe (`tcp_sendmsg`, `tcp_v4_connect`) | Tracks outbound bytes per (PID, destination) over a window and flags volumes above a threshold. |
| `privesc_monitor` | raw_tracepoint (`sys_enter`), kprobe (`commit_creds`) | Flags UID/GID transitions to root, credential changes and capability acquisition. |
| `driver_fim` | kprobe (`security_file_open`), raw_tracepoint (`sys_enter`) | Watches write access to NVIDIA driver files and CUDA libraries, and kernel module loading. |

### Performance Programs

| Program | Hook Type | Description |
|---------|-----------|-------------|
| `tcp_tuning` | tracepoint (`tcp/tcp_probe`) | Collects per-connection cwnd, RTT and receive-window samples for **recommendations** (`GET /api/v1/tuning/tcp`). It does not change any TCP parameter. |
| `connpool_analyze` | kprobe / kretprobe (`tcp_v4_connect`), kprobe (`tcp_close`) | Tracks connection lifecycles to spot short-lived connections and connection storms. |
| `numa_path` | tracepoint (`net/netif_receive_skb`), kprobe (`__napi_poll`) | Correlates the CPU handling each packet with NUMA topology to detect cross-NUMA packet processing. |
| `sockops_optimize` | sockops, sk_msg | Detects same-node connections and redirects their traffic with `bpf_msg_redirect_hash`, bypassing the TCP stack. Attached only with `-cgroup-path`. |

### Observability Programs

| Program | Hook Type | Description |
|---------|-----------|-------------|
| `trace_correlator` | tcx/ingress | Extracts W3C `traceparent` trace and span IDs from incoming HTTP headers. Needs Linux 6.6+ and `-iface`. |
| `latency_breakdown` | kprobe / kretprobe (`udp_sendmsg`, `tcp_v4_connect`, `inet_stream_connect`, `tls_sw_sendmsg`, `tcp_sendmsg`, `tcp_recvmsg`) | Splits request latency into DNS, TCP handshake, TLS handshake and application phases. |
| `cost_tracker` | tcx/egress, tcx/ingress | Per-pod byte counters classified as same-zone, cross-zone or external. Needs Linux 6.6+ and `-iface`. |
| `fingerprint` | raw_tracepoint (`sys_enter`), kprobe (`tcp_v4_connect`) | Builds per-process behavioural feature vectors (syscall frequency, connection patterns) as an anomaly baseline. |

### AI-Specific Programs

| Program | Hook Type | Description |
|---------|-----------|-------------|
| `training_pattern` | uprobe (`ncclAllReduce`), kprobe (`tcp_sendmsg`, `tcp_recvmsg`) | Builds a rank communication matrix and compute/communication cycle view for training jobs. |
| `datapipe_bottleneck` | tracepoint (`block/block_rq_complete`), kprobe (`tcp_recvmsg`), uprobe (`cudaLaunchKernel`, `cudaDeviceSynchronize`) | Correlates storage I/O, network ingestion and GPU busy/idle phases. |
| `gradient_compress` | uprobe (`ncclAllReduce`) | Compares expected and actual bytes per collective to estimate a compression ratio. |

### Core Programs

| Program | Hook Type | Description |
|---------|-----------|-------------|
| `tcp_trace` | kprobe (`tcp_v4_connect`, `inet_csk_accept`, `tcp_close`, `tcp_retransmit_skb`) | TCP connection lifecycle: establishment latency, bytes, retransmits. This is the program behind the decoded real TCP flows. |
| `packet_filter` | XDP | Filters against a dynamically updatable blocklist in BPF maps, with per-rule hit counters. Attached only with `-iface`. Nothing in the operator programs the blocklist yet. |
| `latency_probe` | kprobe (`tcp_sendmsg`, `tcp_recvmsg`) | Per-connection latency with histogram buckets. |
| `syscall_monitor` | raw_tracepoint (`sys_enter`) | Tracks which processes call `connect`, `sendto`, `recvfrom`; flags first-time network activity. |
| `dns_tracker` | XDP | Passive DNS query/response correlation, resolution latency, NXDOMAIN/SERVFAIL counts. Attached only with `-iface`. |

---

## CRDs

All ten kinds are `gryvia.io/v1alpha1` and have a registered controller in the `network-intelligence` operator. The
manifests below are validated against `crds/`; the fields shown are the complete useful surface of each spec. See
`examples/network-intelligence/` for more and [reference/crds.md](../reference/crds.md) for the generated schema reference.

### GryviaFlowPolicy

Intent-based flow policy. The controller translates it into a CiliumNetworkPolicy named `ffp-<name>` (needs Cilium).
`intent` is one of `low-latency`, `high-throughput`, `secure`, `default`.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaFlowPolicy
metadata:
  name: payment-to-db
  namespace: production
spec:
  source:
    service: payment-service
    namespace: production
    labels:
      app: payment
  destination:
    service: postgres-primary
    namespace: production
    port: 5432
    labels:
      app: postgres
  protocol: tcp
  action: allow
  intent: low-latency
  priority: 100
```

### GryviaTrafficInsight

Declares a service and a rolling window to analyse, with the metrics of interest (`latency`, `throughput`, `drops`,
`retransmits`). The controller requeues every `window` and fills, from the collector graph, the top talkers (inbound
edges of the service by cumulative bytes), a smoothed p50 latency (moving average of the inbound edges, not a percentile),
the throughput (growth of those byte counters between two reconciles) and the collector's anomalies of that service.
`p99Latency` and `dropCount` have no source and stay unset.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaTrafficInsight
metadata:
  name: payment-traffic-analysis
  namespace: production
spec:
  service: payment-service
  namespace: production
  window: "5m"
  metrics:
    - latency
    - throughput
    - drops
    - retransmits
```

### GryviaAutoPolicy

Learns Service-to-Service edges from the collector graph (kept in the ConfigMap `autopolicy-<name>-learned`) and, in
mode `suggest` after `learningWindow`, writes one `GryviaFlowPolicy` per learned edge in the AutoPolicy's namespace:
deterministic name, labelled `gryvia.io/suggested=true` and `gryvia.io/auto-policy=<name>`, owned by the AutoPolicy,
action `allow`. **They are never applied automatically**: the flow-policy controller ignores a suggested policy until a
human removes the label (`gryvia network policy apply NAME`). Mode `enforce` behaves like `suggest` (condition
`EnforceNotAutomatic`). The confidence is a heuristic from the number of observed flows. `gryvia network policy suggest`
lists the suggestions.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAutoPolicy
metadata:
  name: production-auto-firewall
  namespace: gryvia-system
spec:
  mode: learn
  learningWindow: "10m"
  targetNamespaces:
    - production
    - staging
  excludeServices:
    - kube-dns
    - metrics-server
  approvalRequired: true
```

### GryviaTraceSession

Time-limited debugging session. The controller creates a results ConfigMap, marks the session `active` and completes it
when `duration` expires. While active it reads Netra's flow history for the window, keeps the flows of the service's pods
(and `filters.port/protocol/dstIP`) in the ConfigMap (at most 2000, replaced on every poll) and sets `flowsCaptured`.
Without Netra (`GRYVIA_NETRA_URL` on the operator) nothing is captured and the `SourceAvailable` condition says so.
`level`, `captureHeaders` and `filters.srcIP` are not applied.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaTraceSession
metadata:
  name: debug-payment-latency
  namespace: production
spec:
  service: payment-service
  namespace: production
  duration: "2m"
  level: l7
  captureHeaders: true
  filters:
    port: 8080
    protocol: tcp
```

### GryviaServiceGraph

Service dependency graph over a set of namespaces, refreshed every `refreshInterval`. Nodes are the Services of the namespaces; edges are the merged collector graph
(bytes, flow counts and a smoothed p50 latency; no verdicts, so node health stays `unknown`; `depth` is not used).
Edges to bare IPs are kept only with `includeExternal`. While the collector is unavailable the last edges are kept and
the condition says why. The gateway's `GET /api/network/flows` falls back to these edges when Netra is not configured.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaServiceGraph
metadata:
  name: production-graph
  namespace: production
spec:
  namespaces:
    - production
    - production-data
  refreshInterval: "30s"
  includeExternal: true
  depth: 5
```

### GryviaNetworkAnomaly

Anomalies of a service, taken from the collector's own statistical detector (`/api/v1/anomalies`: latency spikes, traffic
bursts, new connections, DNS failures). `detectionRules` only select which types to keep (`latency` -> latency_spike,
`throughput` -> traffic_burst, `connections` -> new_connection); their thresholds are not evaluated. Also real: the
webhook call and, with `autoMitigate`, a temporary deny CiliumNetworkPolicy (`app=<targetService>` ingress deny) for new
critical/high anomalies that expires after 15 minutes. That mitigation now fires on real collector data: enable it only
after reading the collector's detections.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaNetworkAnomaly
metadata:
  name: payment-anomaly-detector
  namespace: production
spec:
  targetService: payment-service
  detectionRules:
    - metric: latency
      operator: gt
      threshold: 100
      window: "5m"
    - metric: drops
      operator: gt
      threshold: 100
      window: "1m"
  alertWebhook: "https://alerts.example.com/network-anomaly"
  autoMitigate: true
```

### GryviaSecurityPolicy

Selects which detections (`type`: `escape`, `mining`, `exfiltration`, `privesc`, `driver_fim`; `sensitivity`: `low`,
`medium`, `high`) apply to which namespaces. The controller polls the collector's `/api/v1/security/alerts`, counts alerts
per type into `status.detectionCounts` (which `gryvia security alerts` reads), sends the webhook, and with `autoBlock`
creates temporary CiliumNetworkPolicy blocks. It is subject to the collector URL mismatch described in the status box.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaSecurityPolicy
metadata:
  name: gpu-cluster-security
  namespace: gryvia-system
spec:
  targetNamespaces:
    - ml-research
  detectionRules:
    - type: mining
      enabled: true
      sensitivity: high
    - type: escape
      enabled: true
      sensitivity: high
    - type: exfiltration
      enabled: true
      sensitivity: medium
    - type: privesc
      enabled: true
    - type: driver_fim
      enabled: true
  autoBlock: false
  alertWebhook: "https://alerts.example.com/security"
```

### GryviaNetworkCost

Per-namespace network cost reports from same-zone, cross-zone and external byte counters, priced with `costPerGB` and
attributed with `costCenters`. The byte counters are the `GryviaNetworkUsageRecord` objects the collector writes into the
`tenant-<name>` namespaces (`ebpf.attributeNetwork` + `ebpf.publishNetworkUsage`), read from Kubernetes, no HTTP. One
report per namespace and UTC day is (re)computed every `reportingInterval`; only egress is priced, only the zone classes
same-zone, cross-zone and internet have a price (1 GB = 10^9 bytes, same rules as the gateway). Prices: `costPerGB`, or
when it is all zero the cluster's `GryviaNetworkRate`. Estimates, not invoices; the rates below are placeholders.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaNetworkCost
metadata:
  name: monthly-network-costs
  namespace: gryvia-system
spec:
  targetNamespaces:
    - ml-research
  reportingInterval: "1h"
  costPerGB:
    sameZone: 0.0
    crossZone: 0.01
    internetEgress: 0.09
  costCenters:
    - namespace: ml-research
      team: ml-research
      costCenter: cc-1001
```

### GryviaTrainingInsight

NCCL analysis for one `GryviaAIJob`. The controller reads the `status` of the job's `GryviaFabricSignal`
(`spec.jobRef == targetJob`, same namespace; the collector fills it with `ebpf.publishFabricStatus`): NCCL p99, the
collective skew, the overlap-idle ratio, the RDMA retry ratio and the fabric score. That status has per-job aggregates
only, so there are no per-rank statistics, communication pattern or comm/compute ratio (they stay unset); a straggler is
reported only when the collector flagged one, and the bottleneck is `communication` only with that evidence, else
`unknown`. `phase` is `AwaitingData` until a signal with data exists; a signal older than 5 minutes is reported `Stale`.
Nothing here has run against a real NCCL job.

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaTrainingInsight
metadata:
  name: llm-training-insight
  namespace: ml-research
spec:
  targetJob: llm-distributed-training
  analysisWindow: "5m"
  metrics:
    - collective_timing
    - straggler_detection
    - communication_ratio
    - pattern_analysis
```

### GryviaInferenceInsight

Serving-engine latency for an inference service, read from the `GryviaFabricSignal` with `spec.jobRef == targetService`
(collector flags `-infer-metrics` and `-publish-fabric-status`): time to first token, inter-token latency, engine queue time
and end-to-end latency (p99, as the engine reports them; a field the engine does not export stays unset), queued requests,
KV-cache usage and the eBPF accept-to-read wait. `gpuQueueNs` is the engine queue p99 and `totalNs`/`p99TotalNs` the engine
end-to-end p99; the per-phase breakdown (DNS, TCP connect, TLS, GPU execution, postprocess) and p50/p95 have no source and
stay unset. `bottleneck` is `queue`, `network_wait`, `engine` or `unknown` (a heuristic: p99s are not additive).

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaInferenceInsight
metadata:
  name: llm-serving-insight
  namespace: ml-production
spec:
  targetService: llama-serving
  analysisWindow: "5m"
```

### GryviaFabricSignal

The CRD holds per-job fabric signals. The collector fills its `status` (`ebpf.publishFabricStatus`); the
network-intelligence operator only reads it (Training/InferenceInsight) and the ai-operator's fabric-aware scheduling
uses it. It is not one of the operator's ten kinds.

---

## CLI Commands

Most commands read Kubernetes objects with your kubeconfig. The commands whose data lives in the collectors or behind the
gateway (`network flows`, `network graph`, `gpu memory`, `security alerts`) use the gateway when `GRYVIA_GATEWAY_URL` (or
`--gateway`) and `GRYVIA_API_KEY` are set:

| Command | Reads | Note |
|---------|-------|------|
| `gryvia network flows`, `graph` | gateway `/api/network/flows`, `/api/network/graph`; without a gateway the `GryviaServiceGraph` status | The gateway shows Netra flows when it has Netra, else the graph edges. Edges have no verdict. |
| `gryvia network trace` | creates a `GryviaTraceSession`, prints the flows from its result ConfigMap | Flows need Netra on the operator (see the trace caveats above). |
| `gryvia network policy list/suggest/apply` | `GryviaFlowPolicy` | `suggest` lists the policies a `GryviaAutoPolicy` labelled `gryvia.io/suggested=true`; `apply` removes the label so the operator enforces it. |
| `gryvia network anomalies`, `status` | `GryviaNetworkAnomaly` status, `GryviaServiceGraph` edges, policies, traces | Empty until the operator has produced them. |
| `gryvia security alerts/status/policy` | gateway `/api/security/alerts` when it has per-event alerts, else `GryviaSecurityPolicy` (`status.detectionCounts`) | |
| `gryvia gpu nccl`, `training` | `GryviaTrainingInsight` | Needs an insight object for the job. |
| `gryvia gpu memory` | gateway `/api/gpu/memory` | Cumulative counters summed over all collectors; without a gateway it only says what it needs. |
| `gryvia gpu rdma` | `GryviaFabricSignal` status (and `GryviaNodeFabric` with `--node`) | Rates that were not measured show `-`. |

For real flows in a browser, use the dashboard with Netra configured (`GET /api/network/flows`). See the
[CLI guide](./CLI_GUIDE.md) for every flag.

### Network Tracing

```bash
gryvia network trace training-worker --duration 5m --trace-namespace ml-research
gryvia network trace training-worker --level l4 --duration 2m --trace-namespace ml-research
gryvia network trace training-worker --follow --trace-namespace ml-research

kubectl get gryviatracesessions -n ml-research
```

### Flow Analysis and Service Graph

```bash
gryvia network flows --flow-namespace ml-research
gryvia network flows --service training-worker --flow-namespace ml-research --last 1h
gryvia network flows --flow-namespace ml-research --output json

gryvia network graph --graph-namespace ml-research
gryvia network graph --graph-namespace ml-research --format json > graph.json
```

### Policy Management

```bash
gryvia network policy list --policy-namespace ml-research
gryvia network policy suggest --policy-namespace ml-research
gryvia network policy apply suggestion-name --policy-namespace ml-research

# Flow policies and security policies are ordinary manifests
kubectl apply -f flow-policy.yaml
```

To evaluate a policy without enforcing it, set a GryviaAutoPolicy's `mode` to `learn` or `suggest` instead of `enforce`.

### Anomalies and Security

```bash
gryvia network anomalies --anomaly-namespace ml-research
gryvia network anomalies --severity critical --anomaly-namespace ml-research
gryvia network anomalies --service training-worker --anomaly-namespace ml-research

gryvia security alerts --security-namespace ml-research
gryvia security alerts --severity critical --security-namespace ml-research
gryvia security alerts --alert-type mining --security-namespace ml-research
gryvia security status
gryvia security policy list --security-namespace ml-research
```

### GPU Network Analysis

```bash
gryvia gpu nccl --job llm-distributed-training
gryvia gpu memory --node gpu-node-01
gryvia gpu rdma --node gpu-node-01
gryvia gpu training --job llm-distributed-training

kubectl get gryviatraininginsights -n ml-research
```

---

## Deployment

### Prerequisites

- A node kernel with BTF (`/sys/kernel/btf/vmlinux`, `CONFIG_DEBUG_INFO_BTF=y`; standard on Ubuntu 22.04+, RHEL 9, Debian 12+); Linux 6.6+ for the `tcx` programs.
- A privileged DaemonSet is acceptable in your cluster (the collector runs as root, `hostNetwork`, `hostPID`).
- Cilium as the CNI, for the CiliumNetworkPolicy-based features (FlowPolicy, AutoPolicy enforce, anomaly and security blocking).
- NVIDIA drivers and the NCCL/CUDA libraries visible to the collector for the GPU uprobes (`ebpf.ncclLib`, `ebpf.cudaLib`).

### Install the Operator and Collector

The chart is `helm/network-intelligence`; it is separate from `helm/gryvia`. The operator is always installed; the
collector DaemonSet only with `ebpf.enabled=true`:

```bash
helm install network-intelligence ./helm/network-intelligence \
  --set ebpf.enabled=true \
  --set ebpf.interface=eth0
```

Relevant values (see `helm/network-intelligence/values.yaml`): `operator.*`, `collector.*`, `ebpf.enabled`,
`ebpf.interface` (XDP/tcx attach), `ebpf.cgroupPath` (sockops), `ebpf.ncclLib` / `ebpf.cudaLib`,
`ebpf.flightTokenSecret` (Flight Recorder token), `prometheus.serviceMonitor.*`, `namespace.name` (default
`gryvia-network`). The collector image is in the release workflow, but no tagged release has published it yet; until then build it
yourself (`collector/Dockerfile`) and set `collector.image.repository` and `collector.image.tag`.

### Verify

```bash
kubectl get pods -n gryvia-network
kubectl logs -n gryvia-network -l app.kubernetes.io/component=collector --tail 100

# per-node attach results (from a pod that can reach the node, or kubectl port-forward)
curl -s http://<node-ip>:9090/api/v1/ebpf/status
```

---

## Best Practices

- Start with the core programs (`tcp_trace`, `latency_probe`, `dns_tracker`); the GPU programs need the NCCL/CUDA library paths and hardware.
- Keep `ebpf.enabled=false` on clusters where a privileged, hostPID DaemonSet is not acceptable, and use Netra for flows instead.
- Keep `autoBlock` and `autoMitigate` off until you have confirmed on a test cluster that the resulting CiliumNetworkPolicy objects match your intent.
- Use `learn` or `suggest` modes for GryviaAutoPolicy and review suggestions before accepting them, especially in multi-tenant clusters.
- Use GryviaTraceSession for targeted debugging rather than cluster-wide capture (once flow capture is implemented).
- Treat all thresholds in the programs (2x straggler ratio, 0.3 overlap idle ratio, 100 CNP per second) as uncalibrated heuristics.

---

## Support

- **Issues**: https://github.com/zyvorai/gryvia/issues
- **Discussions**: https://github.com/zyvorai/gryvia/discussions
