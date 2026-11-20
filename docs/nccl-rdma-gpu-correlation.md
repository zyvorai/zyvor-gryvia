# NCCL collective identity, RDMA NIC counters and GPU correlation

Three additions to the eBPF collector (`collector/`, `ebpf/`, gateway `services/api-gateway`). All of them are **off by
default or skip themselves** when their inputs are absent; with default flags nothing changes.

> **Hardware status.** The host used for development has no GPU, NCCL, RDMA NIC or DCGM. Everything below was built and
> tested against **faithful fakes**: a stand-in `libnccl`/`libibverbs` shared library exercised with real uprobes on
> Linux 7.0, fake sysfs trees, canned dcgm-exporter text. Nothing has been observed on real NCCL, RDMA or DCGM. Each
> item says what that means in practice.

## A. Collective identity (`ebpf/straggler.c`, `collector/pkg/fabric`, gateway)

Before: `straggler.c` compared the fastest recent `ncclAllReduce` of one payload size with later, unrelated calls, and
the rank was always 0 (it needs a private `ncclComm` struct offset).

Now `straggler.o` (same file name and object) emits one `FABRIC_SIG_COLLECTIVE` (`type 10`) per completed collective:

| field of `fabric_signal` | meaning |
|---|---|
| `rank`, `world_size` | rank in the communicator, communicator size (`0xffffffff` / `0` when never learned) |
| `peer_rank` | communicator **ordinal** in the process (1-based, order of creation or first use) |
| `peer_latency_ns` | per-communicator collective **sequence** (1-based, in call order) |
| `latency_ns` | host-side duration of the API call |
| `bytes`, `nccl_op` | `count * element size`, op type |
| `retry_count` | flags: `1` = communicator first seen at a collective (late), `2` = rank unknown |
| `rnr_count` | low 32 bits of the `ncclComm_t` pointer, **debug only** |

Identity is learned from public API only, no NCCL struct offsets: uprobe+uretprobe on `ncclCommInitRank` /
`ncclCommInitRankConfig` (rank and world size from the arguments, the comm pointer from `*comm` on return),
`ncclCommInitAll`, `ncclCommUserRank`, `ncclCommCount`, and `ncclCommDestroy` / `ncclCommAbort` (forget). Collectives
hooked: `ncclAllReduce`, `ncclBroadcast`, `ncclAllGather`, `ncclReduceScatter`, `ncclAlltoAll`. Not hooked and why:
`ncclReduce` (comm is a stack argument on x86-64), `ncclSend/Recv` (point to point, no op-for-op match), `ncclCommSplit`
and `ncclCommInitRankScalable` (rank stays unknown unless the workload calls `ncclCommUserRank`), `ncclGetUniqueId`
(carries no communicator identity).

`ncclCommInitRank` takes the 128-byte `ncclUniqueId` **by value**: on x86-64 it is on the stack (`myrank` is integer
argument 3); on arm64 (AAPCS64) it is passed by reference in a register (`myrank` is argument 4). Both are coded with
`__TARGET_ARCH_*`; only x86-64 was executed.

### Comparison

Sequence numbers are **per communicator, counted at collective entry**, so rank r's Nth collective on a communicator
lines up with rank s's Nth as long as both issue the same hooked collectives (NCCL requires it). `collective_cfg[0] = N`
emits only calls whose seq is a multiple of N (same on every rank, so sampled records still line up).

* **Per node** (`fabric.Folder`): spans of the job's local ranks are grouped by `(ordinal, seq, op)`; `Status` gets
  `collectivesCompared` and `collectiveMaxSkewMs`, and `stragglerRank/Hits/ncclP99ms` now come from matched groups
  (slowest > 2x the fastest and >= 5 ms slower; the old thresholds) instead of the heuristic. The legacy
  `FABRIC_SIG_STRAGGLER` is still decoded.
* **Across nodes**: per-node collectors are separate processes, so the merge point is the gateway. The collector's
  flight report gains `collectives` (kind `nccl_collective_seq`: `comm_ordinal`, `comm_seq`, `comm_rank`, `comm_world`,
  `comm_late`, `operation`, `bytes`, `duration_ns`) in its own bounded ring (4096 per job), so a burst of collectives
  cannot evict other events. `routers/flight.py::merge` passes them to the pure function
  `routers/collectives.py::compare_collectives`, exposed as `collectiveComparison` in `GET /api/flight/jobs/{job}`:
  matching by `(comm ordinal, seq, op)`, skew = max - min duration, slowest/fastest rank and node, `missingRanks`
  (ranks of `[0, world)` that did not report), optional arrival skew (event time minus duration; clock-sync dependent),
  per-rank counts and a `nccl_collective_slow_rank` finding only after >= 5 straggling groups dominated by one rank.
  Groups whose ranks disagree on bytes or world size, or contain one rank twice, are marked `inconsistent` and **not**
  compared (that pattern usually means misaligned sequences, not a slow rank). Missing ranks are listed, never invented.

### Limits (state them when you use the numbers)

* The `ncclComm_t` pointer is per process and per node. Cross-node identity is **job + communicator ordinal**, which
  assumes every process creates or first uses communicators in the same order.
* Sequence numbers start when the probes attach. If the collector attaches after `ncclCommInitRank`, a communicator is
  created lazily at its first collective, flagged `late`, with unknown rank until `ncclCommUserRank` runs; its ordinal
  and seq are only comparable with ranks attached at the same moment.
* The counter increment is not atomic: two threads issuing on one communicator concurrently can lose a count.
* Durations are **host-side API call durations**. NCCL launches asynchronously, so this is mostly enqueue time. A large
  skew means one rank's call was slow or blocked; it does not identify a GPU or network cause.
* The gateway sees at most the newest 200 spans per node; overlapping windows are needed for matches, otherwise groups
  show as partial.
* Volume: one 80-byte record per collective per rank (`collective_cfg` samples it).

### Verification done (x86-64, Linux 7.0, stand-in `libnccl`)

`ebpf/test/collective_ident/run.sh` builds a fake library exporting the symbols above with the real signatures
(including the by-value 128-byte `ncclUniqueId`), starts fake ranks, attaches the real uprobes with libbpf and reads the
ring. Observed: rank/world/ordinal/seq/bytes/op exactly as issued for two communicators (ordinal 1 and 2, seq per
communicator), rank 1 (delayed 20 ms) showing ~20 ms vs ~2 us for rank 0 on the same `(ordinal, seq)`, the failing call
(`ncclInvalidArgument`) not emitted, `ncclCommUserRank` filling a late communicator's rank, and late attach giving
`flags=3`, `rank=-1`. `make -k && make check` loads all 35 objects; `make ARCH=arm64` builds all 35.
Not verified: real NCCL (symbol names/versions, inlining, `ncclCommInitRankConfig` variants), arm64 execution.

## B. NCCL userspace RDMA visibility

The kernel `ib_post_send` / `ib_poll_cq` kprobes (`rdma_trace.c`, `rdma_health.c`) do not see NCCL's data path: work
requests are posted and doorbells rung from userspace, and `ibv_post_send`, `ibv_post_recv`, `ibv_poll_cq` are `static
inline` wrappers around function pointers in the device context, so **there is no exported symbol to probe**. The
supported visibility is therefore:

1. **NIC hardware counters** (`-nic-counters`, chart `ebpf.nicCounters`; `collector/pkg/nic`). Reads
   `/sys/class/infiniband/<dev>/ports/<n>/{counters,hw_counters}`; every file is optional and non-numeric files are
   skipped. Per-second deltas with wrap handling (narrow classic counters wrap only when the previous value was in the top
   quarter of the range and the new one in the bottom quarter, otherwise a decrease is treated as a reset). Exported as
   `gryvia_nic_counter_rate{device,port,counter}` (a fixed whitelist; `port_*_data` counted in 4-byte units and exported
   as bytes), and folded into `fabric.Status` under job `_node/nic`: `nicRetryRate` (`out_of_sequence`,
   `packet_seq_err`, `local_ack_timeout_err`, `implied_nak_seq_err`, `rnr_nak_retry_err`, `duplicate_request`) and
   `nicErrorRate` (`symbol_error`, `link_downed`, `port_rcv_errors`, `rx_discards_phy`, `port_xmit_discards`,
   `link_error_recovery`). Both informational, they never change `scoreDelta`. Absent `/sys/class/infiniband`: logged once,
   skipped.
   **Reconciliation, no double counting:** when the NIC exports `rp_cnp_handled`, `CNPRate` uses only the NIC value and the
   XDP `roce_cnp` samples are ignored; when it exports a `*pause*` counter (durations/transitions excluded), `PFCRate`
   uses only that. A NIC without those counters leaves the XDP values in charge. (Pause frames are normally consumed by
   the MAC and never reach XDP, so the NIC counter is the better source where it exists.) `rdmaRetryRate` (kernel verbs
   completions) stays separate and is never summed with the NIC rates.
2. **libibverbs control-path uprobes** (`-ibverbs-probes`, chart `ebpf.ibverbsProbes`; `ebpf/ibv_verbs.c`): counters for
   `ibv_create_qp`, `ibv_destroy_qp`, `ibv_reg_mr`/`ibv_reg_mr_iova2` (calls that succeeded, registered bytes), exported as
   `gryvia_ibverbs_*_total`. Useful for connection count and pinned-memory churn; it says nothing about retries or
   bandwidth. `ibv_modify_qp` (retry/timeout attributes) is **not** probed: reading `struct ibv_qp_attr` needs the
   rdma-core header layout, which is not part of this build, and a guessed offset would report wrong values.

Unverified: a real NIC's counter set, units and widths (mlx5 vs others; the 4-byte unit and the widths follow the kernel
sysfs ABI documentation), versioned symbol lookup in a real `libibverbs.so` (the stand-in exports unversioned symbols;
`ibv_verbs.o` is verified only with it, expected `qp_created=2 qp_destroyed=1 mr_registered=2 mr_bytes=1052672`, observed
identical), and the collector container seeing `/sys/class/infiniband` (it is privileged; not checked on a cluster).

## C. GPU execution correlation (DCGM)

CUDA/NCCL API durations cannot show GPU idle. `-dcgm-correlate` (chart `ebpf.dcgm.enabled`; `collector/pkg/dcgm`) scrapes
the node's dcgm-exporter (`-dcgm-url`, default `http://127.0.0.1:9400/metrics`, needs `hostNetwork` for the loopback
default; `-dcgm-interval` 1 s) and keeps a 6-minute window per GPU of `DCGM_FI_DEV_GPU_UTIL`, `DCGM_FI_PROF_SM_ACTIVE`,
`..._PIPE_TENSOR_ACTIVE`, `..._DRAM_ACTIVE`, `..._NVLINK_TX/RX_BYTES`, `..._PCIE_TX/RX_BYTES`.

Attribution: a GPU sample belongs to a job only through the exporter's Kubernetes labels (`namespace`, `pod`) resolved with
the Flight Recorder pod list; unlabelled samples are never guessed onto a job.

Correlation (`dcgm.Correlate`, pure, unit-tested with canned text): the job's communication windows are the union of its
local ranks' NCCL call windows `[end - duration, end]`; each DCGM sample stands for the interval since the previous one
(at most 10 s). New `fabric.Status` and `GryviaFabricSignal.status` fields, all informational (**never** part of
`scoreDelta`):

* `gpuIdleDuringCommRatio`: share of covered communication time with `SM_ACTIVE` (else `GPU_UTIL / 100`) below
  `-dcgm-idle-threshold` (default 0.1). Its complement is the share of communication overlapped with GPU work.
* `smActiveDuringCompute`: mean SM activity outside the windows between the job's first and last collective.
* `gpuCorrelationCoverage`: share of communication time that had any sample. **A coarse exporter interval (default 30 s)
  shows as low coverage, not as a made-up ratio.** Run dcgm-exporter with `--collect-interval 1000` or lower.
  Fields are omitted from the status patch when a node did not measure them (see [fabric-status.md](fabric-status.md)). Prometheus: `gryvia_fabric_gpu_idle_during_comm_ratio`,
  `gryvia_fabric_sm_active_during_compute`, `gryvia_fabric_gpu_correlation_coverage`.

Limits: the windows are host-side call windows, not on-GPU collective time; GPUs are averaged per job (the rank -> GPU
mapping is not known); ingest time stands in for the event time (milliseconds of jitter); the ratio needs identified
collectives from A.

**CUPTI activity tracing is NOT implemented.** It needs a library injected into the workload (`LD_PRELOAD` or a CUPTI
subscriber in the process) to record kernel and memcpy activity; this collector attaches from outside and does not do
that. It remains a future option (an opt-in injected sidecar library reporting to the collector); nothing here fakes it.

Unverified: real dcgm-exporter output (label names follow its documentation; test text is canned), value scales of the
profiling fields, and the behaviour at real scrape intervals.

## Flags and chart values (all default off)

| flag | chart value | effect |
|---|---|---|
| `-nic-counters`, `-nic-sysfs` | `ebpf.nicCounters` | NIC counters (B1) |
| `-ibverbs-probes`, `-ibverbs-lib` | `ebpf.ibverbsProbes` | libibverbs control-path counters (B2) |
| `-dcgm-correlate`, `-dcgm-url`, `-dcgm-interval`, `-dcgm-idle-threshold` | `ebpf.dcgm.*` | DCGM correlation (C) |

No new RBAC: the DCGM path reuses the pod list the Flight Recorder already reads. The collective identity probes ride on
the existing `straggler.o` (attached whenever libnccl is found, as before).
