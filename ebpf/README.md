# Gryvia eBPF Programs

Kernel-space eBPF programs that form the data-plane of the Gryvia
network intelligence layer.  They run inside the Linux kernel and feed
structured events to the userspace flow collector (`../collector/`).

**Status: experimental.** There are 47 programs (`ls *.c`). The first 35 build and load through the kernel verifier on
Linux 7.0 x86_64. The last twelve (from `xdp_mux` to `ucx_complete` in the table below) build for x86_64 and arm64
with `-Wall -Werror` and load through the verifier in CI (`collector/cmd/verifyobj`), but have not run on hardware. The collector's loader attaches every object in its directory whose hooks
resolve (libraries and symbols present, `-iface` for XDP), except the ten of them that nothing in the collector consumes
yet, which are opt-in: `nccl_transport`, `p2p_fallback`, `capture_gate`, `gpu_oom`, `graph_stall`, `gdr_fail`, `infer_ttft`,
`weight_mmap`, `gpu_dev` and `ucx_complete` attach only when named in `-enable-programs` (chart `ebpf.enablePrograms`;
`all` enables every one). Without it they are not loaded into the kernel, and `/api/v1/ebpf/status` lists them as skipped
with `notRequested: true` and no `gryvia_ebpf_program_attached` series (so they do not trigger `GryviaEbpfProgramNotAttached`;
the same goes for any program the configuration did not ask for, such as XDP without `-iface` or quota pacing while off).
Enabled, their signals are counted per job by the fabric folder (`gryvia_fabric_signal_events{signal=...}`, plus
`gryvia_fabric_infer_ttft_p99_seconds`; `weight_mmap` and `gpu_dev` also log a warning). They are informational: none
feeds the placement score, none is published to `GryviaFabricSignal` status, and nothing acts on them. The maps that steer `nccl_transport`, `capture_gate` and `gpu_dev` (`transport_hint`,
`capture_lease`, `allowed_cg`, `gpu_dev_cfg`) are never written. `xdp_mux` and `roce_ecn` are governed by `-xdp-mux`
(off by default). Earlier results: the collector attached the kprobe/tracepoint subset there and decoded real TCP flows. GPU, NCCL,
RDMA and GPUDirect Storage behaviour, arm64 attachment (arm64 loading is verified in CI) and the gated XDP/TCX/sockops attachments have not been verified on
hardware. Nothing in the rest of the platform depends on them, and the collector is off by default.

## Programs

| Program | Hook Type | Purpose |
|---------|-----------|---------|
| `tcp_trace.c` | kprobe (`tcp_v4_connect`, `tcp_close`, `tcp_retransmit_skb`), kretprobe (`inet_csk_accept`) | TCP connection lifecycle tracking -- establishment latency, bytes transferred, retransmits |
| `packet_filter.c` | XDP | Ultra-fast inline packet filtering with dynamic rule maps and per-rule hit counters |
| `latency_probe.c` | kprobe (`tcp_sendmsg`, `tcp_recvmsg`) | Per-connection round-trip latency measurement with histogram buckets |
| `syscall_monitor.c` | raw_tracepoint (`sys_enter`) | Tracks which processes make `connect`, `sendto`, `recvfrom` syscalls; alerts on first-time network activity |
| `dns_tracker.c` | XDP | Passive DNS query/response correlation, resolution latency, NXDOMAIN/SERVFAIL tracking |
| `nccl_trace.c` | uprobe (`ncclAllReduce`, `ncclAllGather`, `ncclBroadcast`, `ncclReduce`, `ncclReduceScatter`, `ncclSend`, `ncclRecv`, `ncclGroupStart`, `ncclGroupEnd`) | NCCL collective communication tracing with per-op histograms and latency distribution |
| `gpu_mem_trace.c` | uprobe (`cudaMemcpy`, `cudaMemcpyAsync`, `cudaMalloc`, `cudaFree`, `cudaLaunchKernel`, `cudaDeviceSynchronize`) | GPU memory transfer monitoring with direction-aware byte counters and allocation tracking |
| `rdma_trace.c` | kprobe (`ib_post_send`, `ib_post_recv`, `ib_poll_cq`, `ib_destroy_qp_user`), kretprobe (`ib_poll_cq`, `ib_create_qp_kernel`) | RDMA/InfiniBand traffic analysis with per-QP statistics and completion tracking |
| `training_pattern.c` | uprobe/uretprobe (`ncclAllReduce`), kretprobe (`tcp_sendmsg`, `tcp_recvmsg`) | AI training communication pattern detection -- rank communication matrix and compute/comm cycle analysis |
| `datapipe_bottleneck.c` | tp_btf (`block_rq_complete`), kretprobe (`tcp_recvmsg`), uprobe (`cudaLaunchKernel`), uretprobe (`cudaDeviceSynchronize`) | Data pipeline bottleneck detection -- correlates storage I/O, network ingestion, and GPU busy/idle phases |
| `gradient_compress.c` | uprobe (`ncclAllReduce`) | Gradient compression analysis -- compares expected vs actual bytes per collective to detect compression ratios |
| `straggler.c` | uprobe (`ncclAllReduce`) | Per-rank NCCL skew -- emits a `fabric_signal` when a collective takes >2x the fastest recent span of the same size (and >5 ms) |
| `rdma_health.c` | kprobe (`ib_post_send`, `mlx5_ib_post_send`, `ib_poll_cq`, `mlx5_ib_poll_cq`) | Per-QP send counts and retry-exceeded / RNR-exceeded completions; emits a `fabric_signal` above operator-set thresholds (all four probes optional) |
| `gds_trace.c` | uprobe (`cuFileRead`, `cuFileWrite`), kprobe (`nvidia_fs_read`, optional) | GPU Direct Storage vs bounce-buffer reads -- byte counters, and a `fabric_signal` for slow (>2 ms) calls |
| `overlap.c` | uprobe/uretprobe (`ncclAllReduce`, `cudaDeviceSynchronize`) | GPU sync while a collective is in flight -- emits `FABRIC_SIG_OVERLAP` when a `cudaDeviceSynchronize` (>= 1 ms) runs nested inside an `ncclAllReduce` on the same thread; per-thread nesting counters, state deleted on every exit path |
| `roce_cnp.c` | XDP | RoCEv2 congestion notification packets (UDP 4791, BTH opcode 0x81; Ethernet, up to 2 VLAN tags, IPv4 or IPv6). Per-CPU counters only (`cnp_count`), always `XDP_PASS`; attached only with `-iface` |
| `connpool_analyze.c` | kprobe (`tcp_v4_connect`, `tcp_close`), kretprobe (`tcp_v4_connect`) | TCP connection lifecycle for connection-pool analysis: short-lived connections and churn |
| `latency_breakdown.c` | kprobe (`tcp_v4_connect`, `tcp_sendmsg`, `tcp_recvmsg`, `udp_sendmsg`, `tls_sw_sendmsg`), kretprobe (`inet_stream_connect`, `tcp_recvmsg`) | Per-request latency phases (connect, TLS, request/response) |
| `tcp_tuning.c` | tracepoint (`tcp/tcp_probe`) | Per-connection cwnd, RTT and window histograms for TCP tuning advice |
| `numa_path.c` | tracepoint (`net/netif_receive_skb`), kprobe (`__napi_poll`) | Cross-NUMA packet processing detection |
| `cost_tracker.c` | TCX ingress and egress | Per-pod byte counters classified by zone (same-zone, cross-zone, external); needs `-iface` and Linux 6.6+ |
| `trace_correlator.c` | TCX ingress | Extracts W3C `traceparent` trace and span ids from HTTP requests and keys them by 4-tuple; needs `-iface` and Linux 6.6+ |
| `sockops_optimize.c` | sockops, sk_msg | Redirects same-node TCP connections past the TCP/IP stack; needs `-cgroup-path` |
| `container_escape.c` | raw_tracepoint (`sys_enter`), kprobe (`security_file_open`) | Suspicious syscalls (`unshare`, `setns`, `mount`, `pivot_root`, `ptrace`) and access to sensitive host paths from containers |
| `privesc_monitor.c` | raw_tracepoint (`sys_enter`), kprobe (`commit_creds`) | UID/GID transitions to root, credential changes, capability acquisition |
| `crypto_detect.c` | kprobe (`tcp_v4_connect`), tracepoint (`sched/sched_process_exec`) | Connections to known mining-pool ports and known miner binaries |
| `exfil_detect.c` | kprobe (`tcp_sendmsg`, `tcp_v4_connect`) | Outbound byte volume per PID and external destination against a threshold |
| `fingerprint.c` | kprobe (`tcp_v4_connect`), raw_tracepoint (`sys_enter`) | Per-process syscall and connection behaviour compared with baselines |
| `driver_fim.c` | raw_tracepoint (`sys_enter`), kprobe (`security_file_open`) | File-integrity monitoring of NVIDIA driver files and CUDA libraries, module loading |
| `infer_latency.c` | kretprobe (`inet_csk_accept`), kprobe (`tcp_recvmsg`) | Accept -> first recv wait of connections to the ports in `infer_ports` (collector `-infer-ports`, default 8000 vLLM and 8001 Triton) as `FABRIC_SIG_INFER_WAIT`; other ports are never recorded. It is a network accept wait, not queue time, TTFT or ITL (see [docs/inference-latency.md](../docs/inference-latency.md)) |
| `ucx_gloo.c` | uprobe/uretprobe (`ucp_tag_send_nb`, `ucp_tag_send_nbx` in `libucp.so`) | UCX tag-send calls that blocked for >= 5 ms as `FABRIC_SIG_UCX_SLOW` (span of the posting call, not of the transfer). Skipped when `libucp.so` is not found (`-ucx-lib`, `-uprobe-pid`, standard dirs). **Gloo is not probed**: its C++ entry points are mangled, version-specific and usually linked statically into `libtorch_cpu.so`, so a fixed-symbol probe could never attach |
| `pfc_pause.c` | XDP | 802.1Qbb PFC pause frames (EtherType 0x8808, opcode 0x0101; per-priority counts from the enable vector and quanta) and 802.3x pause frames in per-CPU counters (`pause_count`); always `XDP_PASS`, attached only with `-iface`. **One XDP program per interface**: conflicts with `roce_cnp`, `packet_filter` and `dns_tracker`; the collector attaches the first and skips the rest with a logged reason. Many NICs consume pause frames in the MAC and never show them to XDP |
| `weight_exfil.c` | kprobe (`vfs_read`, `tcp_v4_connect`) | Observe only. A read request >= 8 MiB from a file named `*.safetensors/.gguf/.ckpt/.onnx/.pt/.pth/.bin/.h5`, then a `tcp_v4_connect` by the same process within 30 s to a destination that is not loopback, RFC1918, link-local or 0.0.0.0/8, emits one `FABRIC_SIG_EXFIL`. A read alone never fires. IPv4 only, `read()` only (not mmap), name-based |
| `quota_pace.c` | sockops (cgroup v2) | **The only mutating program. Off by default.** Lowers `SO_MAX_PACING_RATE` of an outbound TCP connection (`TCP_CONNECT_CB`) when `pace_rate[<full 64-bit cgroup id>]` has an entry; no entry means the socket is untouched. Rate clamped up to 1 Mbit/s, never raised, fail open. Attached only with `-quota-pace` **and** `-cgroup-path`; entries are lease-gated by `collector/pkg/fabric` (`Pacer`); the only writer is the opt-in `-quota-pace-sync` reconcile of `GryviaQuota` `spec.network.maxEgressMbps` (see `docs/fabric-status.md`) |
| `xdp_mux.c` | XDP | The one XDP program to attach to an interface. Tail-calls the first populated slot of `xdp_features` (0 roce_cnp, 1 pfc_pause, 2 dns_tracker, 3 packet_filter, 4 roce_ecn); each of those five ends by tail-calling the next populated slot (`headers/xdp_chain.h`), so every loaded feature sees every packet, in slot order, and the chain returns `XDP_PASS` at the end or when nothing is loaded. A feature returning anything else ends the chain and decides the packet (none does today). The collector does this with `-xdp-mux` (chart `ebpf.xdpMux`, off by default, needs `-iface`): it attaches the mux, gives every feature object the mux's `xdp_features` map (cilium/ebpf `MapReplacements`) and puts each program in its slot instead of attaching it. Without the flag it attaches one XDP program per interface, as before. CI runs the chain with `BPF_PROG_TEST_RUN` and through the collector on the loopback interface; it has not run on a NIC. A feature attached on its own behaves as before |
| `nccl_transport.c` | uprobe/uretprobe (`ncclCommInitRank`, `ncclCommInitRankConfig`, `ncclGetUniqueId`) | `FABRIC_SIG_NCCL_XPORT` (11) per successful communicator init (rank, world size, init duration; `retry_count` = transport hint read from `transport_hint[pid]`, which the collector would fill from `NCCL_P2P_DISABLE` / `NCCL_SHM_DISABLE` / `NCCL_NET`; 0 = unknown). Opt-in (`-enable-programs=nccl_transport`); once enabled it attaches when `libnccl` is found, but the collector never writes `transport_hint` (the hint stays 0) and does not interpret the signal yet |
| `p2p_fallback.c` | uprobe/uretprobe (`cudaDeviceEnablePeerAccess`, `cudaMemcpyAsync`) | `FABRIC_SIG_P2P_FALLBACK` (12): a failed peer-access enable (not "already enabled") followed within 30 s by a device-to-device copy of >= 8 MiB from the same process. A heuristic, not proof of a host bounce buffer. Opt-in (`-enable-programs=p2p_fallback`); once enabled it attaches when `libcudart` is found, and the signal is not interpreted yet |
| `capture_gate.c` | kprobe (`tcp_sendmsg`) | Observe only. `capture_lease[cgroup_id]` holds an expiry; a missing or expired lease means "not armed" (fail open). Counts consultations in `gate_hits`; captures nothing itself. Opt-in (`-enable-programs=capture_gate`): nothing writes `capture_lease`, so it never arms, and enabled it costs a map lookup per TCP send |
| `roce_ecn.c` | XDP | RoCEv2 (UDP 4791) packets over IPv4 and IPv6 by ECN codepoint: per-CPU counters `ecn_count` (0 seen, 1 CE, 2 ECT), always `XDP_PASS`, no ring buffer. IPv6 extension headers are not walked. Slot 4 of `xdp_mux`. Needs `-iface`; it attaches only as slot 4 of `xdp_mux` (`-xdp-mux`), otherwise it competes for the single XDP slot. The collector does not read its counters yet |
| `gpu_oom.c` | uprobe/uretprobe (`cudaMalloc`, `cudaMallocAsync`, `cudaMallocFromPoolAsync`) | `FABRIC_SIG_GPU_OOM` (15) when the call returns `cudaErrorMemoryAllocation`; `bytes` = request. Routine in PyTorch\'s caching allocator, so a burst means memory pressure, one is not an incident Opt-in (`-enable-programs=gpu_oom`); its signal is not interpreted yet |
| `graph_stall.c` | uprobe/uretprobe (`cudaStreamBeginCapture`, `cudaStreamEndCapture`, `cudaDeviceSynchronize`) | `FABRIC_SIG_GRAPH_STALL` (16): a device-wide sync while a CUDA graph capture is open on the thread (normally invalidates the capture); `latency_ns` = time since the capture began Opt-in (`-enable-programs=graph_stall`); its signal is not interpreted yet |
| `gdr_fail.c` | kretprobe (`nvidia_p2p_get_pages`) | `FABRIC_SIG_GDR_FAIL` (17) when GPUDirect RDMA page pinning returns non-zero; attaches only while the nvidia module is loaded. There is no kernel `ib_reg_mr`, so memory-registration failures are not covered Opt-in (`-enable-programs=gdr_fail`); its signal is not interpreted yet |
| `infer_ttft.c` | kretprobe (`inet_csk_accept`), kprobe (`tcp_sendmsg`) | On the ports in `infer_ports` (same contract as `infer_latency`): `FABRIC_SIG_INFER_TTFT` (18) accept to first send, and `FABRIC_SIG_INFER_GAP` (19) once per connection for a send gap >= 50 ms. Not tokenizer TTFT; the gap cannot tell a mid-response stall from an idle keep-alive connection Opt-in (`-enable-programs=infer_ttft`); enabled, it uses the same `infer_ports` as `infer_latency` (default `8000,8001`), which the collector fills, and its signals are not interpreted yet |
| `weight_mmap.c` | kprobe (`security_mmap_file`, `tcp_v4_connect`) | `FABRIC_SIG_WEIGHT_MMAP` (20): mmap of a model-weight file (same name list as `weight_exfil`), then a connect to a public IPv4 within 30 s. Name-based, IPv4 only Opt-in (`-enable-programs=weight_mmap`); enabled, it probes every mmap and IPv4 connect on the node and its signal is not interpreted yet |
| `gpu_dev.c` | kprobe (`security_file_open`) | `FABRIC_SIG_GPU_DEV` (21): open of a character device named `nvidia*` or `renderD*` from a cgroup not in `allowed_cg`, only while `gpu_dev_cfg[0]` enforces (fails open by default) Opt-in (`-enable-programs=gpu_dev`): the collector never writes `gpu_dev_cfg` or `allowed_cg`, so enabled it only counts GPU device opens in `gpu_open_count` while probing every file open |
| `ucx_complete.c` | uprobe/uretprobe (`ucp_tag_send_nbx`, `ucp_worker_progress` in `libucp.so`) | `FABRIC_SIG_UCX_WAIT` (22): `ucp_worker_progress` ran >= 5 ms after a send that returned a request. Completion is not observed, so this is a hint, not a stalled-transfer proof Opt-in (`-enable-programs=ucx_complete`); its signal is not interpreted yet |

## Overhead

Measured on one Linux host (loopback only): [`docs/ebpf-overhead.md`](../docs/ebpf-overhead.md) has the method, the
numbers, the noise floor and what was not measured. Re-run it with `scripts/bench-ebpf-overhead.sh`.

## Portability (CO-RE)

The programs are **CO-RE** (compile once, run everywhere): they are built without a generated `vmlinux.h`.
`headers/gryvia_core.h` declares the few kernel structs the programs read as minimal local definitions marked
`preserve_access_index`, and every field read goes through `BPF_CORE_READ`. The loader (cilium/ebpf) relocates
those accesses against the running kernel's BTF (`/sys/kernel/btf/vmlinux`) at load time, so one object file works
across kernel versions, and the same sources build for `x86_64` and `arm64` (`make ARCH=arm64`). Per-architecture
syscall numbers are `GRYVIA_NR_*` in the same header.

Requirements on the node: a kernel with BTF (`CONFIG_DEBUG_INFO_BTF=y`, standard on Ubuntu 22.04+, RHEL 9, Debian 12+);
`tcx/*` programs (`cost_tracker`, `trace_correlator`) need Linux 6.6+.

Verified: all 47 programs compile with `-Wall -Werror` for x86_64 and, cross-compiled, for arm64 (clang 18 on Ubuntu 24.04
in CI; clang 21 earlier) and load through the kernel verifier in CI (`collector/cmd/verifyobj` on a GitHub Ubuntu 24.04
runner); the first 35 also passed the verifier on Linux 7.0 x86_64 locally. The arm64 objects are also built natively and
loaded through the verifier on a GitHub arm64 runner (kernel 6.17 in the last run: 47 objects, 0 failed); they have not been attached on arm64. Runtime behaviour of the GPU/NCCL/RDMA/GDS programs (including `straggler`, `rdma_health` and
`gds_trace`, `overlap`, `infer_latency`) has not been exercised on GPU, RDMA or GPUDirect Storage hardware; the kprobe symbols they use
(`ib_post_send`, `ib_poll_cq`, `mlx5_ib_*`, `nvidia_fs_read`) are absent on the test host and are skipped by the
collector when missing.

`roce_cnp` was exercised with `BPF_PROG_TEST_RUN` on crafted packets (CNP counted for IPv4, IPv4 with options, VLAN
and IPv6; non-CNP RoCE, other UDP ports, TCP, fragments and truncated frames left alone; always `XDP_PASS`, packet
bytes unchanged; 20000 random frames all `XDP_PASS`), and `overlap` on a private stand-in library with the real
uprobe/uretprobe attach path (nested sync reported, sync alone and sub-millisecond sync silent, no state left behind).
`infer_latency` is load-verified only: attaching it needs the live `inet_csk_accept` / `tcp_recvmsg` hooks.

`pfc_pause` was exercised with `BPF_PROG_TEST_RUN` (PFC frames with per-priority counts, XON quanta, legacy pause, other
ethertypes, truncated frames, 20000 random frames: all `XDP_PASS`, bytes unchanged). `ucx_gloo` ran on a private
stand-in `libucp.so` with the real uprobe/uretprobe attach path. `weight_exfil` was attached for a few seconds to the
live `vfs_read` / `tcp_v4_connect` hooks in a test process (read alone silent, internal destinations silent, external
destination fires once, name and threshold edges) and detached again. `quota_pace` was run on a throwaway cgroup tree
(never the root cgroup): no entry leaves the socket unchanged, an entry clamps it, a low rate is raised to the floor, 0 and
out-of-range values change nothing, an already lower limit is kept, a cgroup without an entry is untouched next to one
with an entry, an entry for the parent does not cover its children, and a deleted entry leaves the next connection
unchanged. None of this has run on GPU, RDMA or RoCE hardware; whether pause frames reach XDP depends on the NIC.

`quota_pace` is the only program that changes a socket. Its map (`pace_rate`) is empty until userspace inserts a key, so
loading it alone changes nothing; the loader skips it unless `-quota-pace` and `-cgroup-path` are both set. Limits: only
outbound connections are paced (the ESTABLISHED callbacks run in softirq where the current task is unrelated, and
sockops programs cannot ask for a socket's cgroup); the key is the exact cgroup id (the directory's inode number), not
its descendants; the kernel takes the rate as 32-bit bytes per second (max about 4.29 GB/s); a socket that is already
paced keeps its rate until it closes, deleting the entry only unpaces new connections.

The optional `-quota-pace-sync` reconcile (off by default, needs `-quota-pace`, `-cgroup-path`, a cluster and `NODE_NAME`)
fills that map from `GryviaQuota` objects: pods on the node in the quota's namespaces get their pod and container
cgroup ids granted a 2-minute lease every 30 s. It was functionally tested on the Linux 7.0 host against the real
`pace_rate` map and real sockops on a throwaway cgroup tree: only the granted cgroup's new connections were clamped,
a sibling stayed unlimited, and revoke, an API failure (nothing changed), lease expiry and shutdown behaved as designed
(the Kubernetes calls were fakes; there is no API server there). Not tested: real quotas on real workloads. The cap is
per connection (`SO_MAX_PACING_RATE`), not an aggregate limit.

## Prerequisites

- **clang** >= 12 (with BPF target support), **llvm**, **libbpf-dev**, **linux-libc-dev** (for the `asm/*.h` headers)
- **bpftool** only for `make check`

On Ubuntu/Debian:

```bash
sudo apt install clang llvm libbpf-dev linux-libc-dev make bpftool
```

## Building

```bash
make            # compile all programs to .o ELF objects
make check      # load every object through the verifier with bpftool (needs root; cleans up after itself)
make ARCH=arm64 # cross-compile for arm64
make clean      # remove compiled objects
```

The Makefile auto-detects the host architecture (`x86` or `arm64`).

## Shared Headers

`headers/common.h` defines structures shared between kernel and userspace for
network tracing:

- `struct flow_event` -- the primary event structure emitted over perf ring buffers
- `struct conn_info` -- per-connection state stored in BPF hash maps

`headers/gpu_common.h` defines structures shared between kernel and userspace
for GPU/AI workload tracing:

- `struct gpu_event` -- GPU event structure emitted via ring buffers (NCCL ops, memory transfers, RDMA, CUDA launches)
- `struct nccl_inflight` -- in-flight NCCL operation tracking for entry/exit latency correlation
- `struct cuda_mem_inflight` -- in-flight CUDA memory operation tracking
- `struct rdma_qp_info` -- per-RDMA queue pair statistics (bytes sent/received, retransmits, completions)
- Enums: `gpu_event_type`, `mem_direction`, `nccl_op_type`

`headers/fabric_signal.h` defines the side channel used by `straggler.c`, `rdma_health.c`, `gds_trace.c`, `overlap.c`,
`infer_latency.c`, `ucx_gloo.c` and `weight_exfil.c` (`roce_cnp.c` and `pfc_pause.c` only keep counters)
(`gpu_event` is frozen at 72 bytes and is not extended):

- `struct fabric_signal` -- 80-byte scheduler-facing signal emitted on each program's own `fabric_events` ring buffer
  (straggler, RDMA retry/RNR, GDS, overlap, inference wait). Mirrored by `collector/pkg/fabric/signal.go`; both sides are checked against the same
  offsets (`_Static_assert` in C, a unit test in Go)
- `struct rank_span`, `struct rdma_health_val`, `struct gds_inflight`, `struct overlap_state` -- per-program map values
- `CNP_SLOT_*` -- slots of `roce_cnp`'s per-CPU `cnp_count` array (`FABRIC_SIG_CNP` is synthesised by the collector from it and never appears on a ring)
- `PFC_SLOT_*` -- slots of `pfc_pause`'s per-CPU `pause_count` array (frames, legacy pause, one per paused priority; `FABRIC_SIG_PFC` is synthesised by the collector and never appears on a ring)
- `struct exfil_mark` -- `weight_exfil`'s per-process record of recent large model-file reads
- Enum: `fabric_signal_type`

## How It Works

1. The userspace collector (see `../collector/`) loads the compiled `.o` files
   using the cilium/ebpf Go library.
2. Programs are attached to their respective hooks (kprobes, tracepoints, XDP).
3. Events flow from kernel to userspace via `BPF_MAP_TYPE_PERF_EVENT_ARRAY` maps
   (network programs) or `BPF_MAP_TYPE_RINGBUF` maps (GPU/AI programs).
4. BPF hash/array maps provide shared state that the collector can read for
   metrics (histograms, counters, connection tables).

## GPU/AI Program Architecture

The GPU and AI training eBPF programs use **uprobes** to hook into userspace
libraries (NCCL, CUDA runtime) rather than kernel functions.  This requires
special handling in containerized environments:

### Uprobe Resolution for Container Libraries

- NCCL libraries (`libnccl.so`) and CUDA runtime (`libcudart.so`) live inside
  container filesystems, not on the host.
- The userspace collector must resolve the library path within the container's
  mount namespace (e.g., `/proc/<pid>/root/usr/lib/x86_64-linux-gnu/libnccl.so.2`).
- Symbol offsets are determined by reading the ELF symbol table of the target
  library using `bpftool` or the cilium/ebpf library.
- When containers are recreated, uprobe attachments must be refreshed since PIDs
  and library paths change.

### Event Flow

```
  NCCL/CUDA call (userspace)
        |
        v
  uprobe entry --> record start time + params in BPF hash map
        |
  function executes
        |
        v
  uretprobe exit --> compute latency, emit gpu_event via ring buffer
        |
        v
  userspace collector --> Prometheus metrics / alerts
```

### Ring Buffer vs Perf Buffer

GPU/AI programs use `BPF_MAP_TYPE_RINGBUF` instead of `BPF_MAP_TYPE_PERF_EVENT_ARRAY`
for event output.  Ring buffers provide:

- Single shared buffer across all CPUs (simpler userspace consumption)
- `bpf_ringbuf_reserve`/`bpf_ringbuf_submit` for zero-copy event emission
- Better performance for high-frequency GPU events (no per-CPU wakeups)

### Program Interactions

Several GPU programs hook the same functions for different purposes:

- `nccl_trace.c` and `training_pattern.c` both hook `ncclAllReduce`, but
  `nccl_trace` focuses on per-op latency while `training_pattern` analyzes
  rank communication matrices and compute/comm cycles.
- `gpu_mem_trace.c` and `datapipe_bottleneck.c` both track CUDA kernel launches,
  but `datapipe_bottleneck` correlates them with storage I/O and network
  ingestion to detect pipeline stalls.
- `gradient_compress.c` hooks `ncclAllReduce` specifically to compare expected
  vs actual data volumes for compression ratio analysis.
