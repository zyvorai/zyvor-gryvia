# Flight diagnosis, incident history and measurement completeness

This extends the [Flight Recorder](flight-recorder.md) with three things:

1. a **unified diagnosis** per job: ranked findings, each backed by named evidence, from transparent threshold rules;
2. an optional **persistent incident history** with export and before/after comparison;
3. an explicit **measurement completeness** block in every report saying what was *not* measured.

Everything is additive or opt-in. A default install serves the same routes as before, and the new
routes only exist behind the same HMAC token as the flight route.

The one rule that shapes the design: **unavailable is not healthy.** A signal that could not be
collected (probe not attached, no cgroup access, PSI disabled, too little history) is listed under
`unavailable` and never counted as a clean reading. A job with no findings and full coverage says
`no bottleneck detected in measured signals` followed by the list of what was measured; a job with
no findings and missing telemetry says so instead.

## Status of verification

| Part | How it was verified | Not verified |
|------|--------------------|--------------|
| Rules, cgroup readers, completeness | Table-driven unit tests over fake cgroup trees, fake probe status and crafted flight events | Real kubelet cgroup trees on a busy node (both cgroup drivers are covered by the existing host-cgroup layout tests) |
| Incident store | Unit tests: crash recovery (torn/garbage/over-long lines), retention by age and size, open/close, reload, compare math, CSV escaping | Behaviour on a full disk, on network file systems |
| `drops` counters in eBPF | `make -k`, `make check` (kernel verifier loads every object) and `make ARCH=arm64` on the Linux 7.0 test host; the `drops` per-CPU map is present in the loaded `tcp_trace.o` | **The increment path was never exercised**: no ring was driven full. Treat a `0` as "no drop was counted", and the counter code as verifier-checked only |
| GPU/NCCL/RDMA/GDS-derived signals | Crafted `flight.Event` / `fabric.Status` values | **Unverified on hardware**: no GPU, RDMA, NCCL or DCGM was available; the probes those signals come from have not run on real hardware either |

## The diagnosis

`GET /api/v1/flight/diagnosis?namespace=&job=` on a collector (HMAC-signed exactly like
`/api/v1/flight/diagnose`), `GET /api/flight/jobs/{job}/diagnosis?namespace=` on the gateway (merged
across nodes), `gryvia-flight -diagnosis` on the command line, and the "Bottleneck diagnosis" card
on the job page.

```json
{
  "namespace": "ml", "job": "train", "node": "gpu-1",
  "summary": "1 finding(s); most significant: cpu (critical, high confidence)",
  "findings": [{
    "kind": "cpu", "severity": "critical", "confidence": "high",
    "evidence": [{"source": "cgroup", "metric": "cpu_throttle_ratio", "value": 0.75, "window": "5m0s"}],
    "summary": "CFS quota throttled 75% of periods in the worst container ...",
    "whatWasNotMeasured": ["host-wide CPU contention outside the job's cgroups is not read"]
  }],
  "unavailable": [{"signal": "cgroup.io.pressure", "reason": "file not present (controller not enabled ..., or PSI disabled)"}],
  "measured": ["cgroup.cpu.stat", "flight.tcp_retransmit"],
  "metrics": {"cpu_throttle_ratio": 0.75, "tcp_retransmits": 0},
  "measurementCompleteness": { "...": "see below" }
}
```

Finding `kind` is one of `network`, `storage`, `cpu`, `memory`, `gpu-comm`, `straggler`, `rdma`, `exfil`;
`severity` is `info`, `warning` or `critical`; `confidence` is `low`, `medium` or `high`. Findings are
ranked by severity, then confidence, then kind. A finding is a statement about what was measured, not a
proven root cause; `whatWasNotMeasured` always lists the relevant unavailable signals plus the standing
caveats of that kind.

### Signals

| Signal | Source | Needs |
|--------|--------|-------|
| `flight.tcp_retransmit` | Flight events (retransmits on closed connections) | `tcp_trace.o` attached |
| `flight.pipeline_stall` | Flight events (GPU idle gap over 1 ms before a CUDA launch) | `datapipe_bottleneck.o` and the CUDA library |
| `flight.nccl_collective` | Flight events (informational p99, no rule) | `nccl_trace.o` |
| `fabric.straggler` | Fabric status: straggler hits, NCCL span p99 | `straggler.o` |
| `fabric.overlap` | Fabric status: host blocked in sync inside an allreduce | `overlap.o` |
| `fabric.rdma_retry` | Fabric status: QP retry/RNR rate | `rdma_health.o` |
| `fabric.gds` | Fabric status: direct vs bounce ratio | `gds_trace.o` and the nvidia-fs hook |
| `fabric.cnp`, `fabric.pfc` | Fabric status (node-level, not per job) | `roce_cnp.o`, `pfc_pause.o` |
| `fabric.weight_exfil` | Fabric status | `weight_exfil.o` |
| `cgroup.cpu.stat` | `nr_periods`, `nr_throttled`, `throttled_usec` deltas | cgroup access, two samples at least `minSampleSpanSeconds` apart, at least `minCpuPeriods` CFS periods (a pod without a CPU limit has none) |
| `cgroup.cpu.pressure` | `cpu.pressure` `some avg10` (peak in the window) | cgroup access, PSI enabled in the kernel |
| `cgroup.memory.events` | `oom_kill`, `high`, `max` deltas | cgroup access, two samples |
| `cgroup.memory.current+max` | `memory.current / memory.max`, worst container | a container with a finite `memory.max` |
| `cgroup.memory.pressure`, `cgroup.io.pressure` | PSI `some avg10` (peak) | cgroup access, PSI |

The cgroup signals are **userspace only**: the collector reads files under the cgroup root you give it
with `-flight-diagnosis-cgroup`, using the existing `HostCgroups` resolver (pod UID + QoS class from the
Kubernetes pod list, both kubelet cgroup drivers, symlinks never followed, nothing outside the root
touched, nothing written). A sampler polls the job's pods every 10 s and keeps at most 90 samples per job
and 128 jobs. Counters that go backwards (a container restarted) are treated as reset. A pod that
appeared inside the window has no baseline and contributes no delta.

**Storage latency is not measured per job.** `datapipe_bottleneck.c` keeps block-layer throughput in
node-wide per-CPU counters (its `total_latency_ns` field is never filled) and emits no per-job block
latency. What exists per job is `pipeline_stall` (the GPU sat idle before a kernel launch, cause not
established) and the cgroup `io.pressure`. The storage finding says so.

An attached probe that observed nothing yields a measured zero: the report cannot distinguish "the
workload did not do that" from "the hook did not fire" (for example a library the uprobe resolver missed).

### Rules and thresholds

A value strictly greater than `warn` is a warning, greater than `crit` is critical. Defaults
(`diagnosis.DefaultConfig`), all overridable with `-flight-diagnosis-thresholds` (JSON, unknown keys
rejected, validated):

| Rule (kind) | Metric | warn | crit | Base confidence |
|-------------|--------|------|------|-----------------|
| network | `tcp_retransmits` in the window (`tcpRetransmits`) | 10 | 100 | medium |
| network | `cnp_rate` per second (`cnpRate`) | 100 | 1000 | medium |
| network | `pfc_rate` per second (`pfcRate`) | 1000 | 10000 | medium |
| storage | `pipeline_stall_ratio`, worst pod, needs `pipelineStallMin` (5) events (`pipelineStallRatio`) | 0.10 | 0.30 | **low** (cause not established) |
| storage | `io_psi_some_avg10` % (`ioPressure`) | 10 | 30 | medium |
| storage | `gds_direct_ratio` below `gdsHitRatioLow` (0.5) | warning | - | medium |
| cpu | `cpu_throttle_ratio` (`cpuThrottleRatio`) | 0.25 | 0.50 | high (kernel counter) |
| cpu | `cpu_psi_some_avg10` % (`cpuPressure`) | 20 | 50 | medium |
| memory | any `oom_kill` increment | critical | - | high |
| memory | `mem_high_max_events` (`memHighMaxEvts`) | 0 | 100 | high |
| memory | `mem_usage_ratio` (`memUsageRatio`) | 0.90 | 0.97 | medium |
| memory | `mem_psi_some_avg10` % (`memPressure`) | 10 | 30 | medium |
| straggler | `straggler_hits` (`stragglerHits`) | 10 | 50 | medium |
| gpu-comm | `nccl_p99_ms` straggler-span p99 (`ncclP99ms`) | 50 | 200 | medium |
| gpu-comm | `overlap_idle_ratio` (`overlapIdleRatio`) | 0.30 | 0.60 | medium |
| rdma | `rdma_retry_rate` (`rdmaRetryRate`) | 0.02 | 0.10 | medium |
| exfil | `exfil_events` (`exfilEvents`) | 0 | 2 | medium |

Other tunables: `windowSeconds` (300), `minSampleSpanSeconds` (20), `minCpuPeriods` (20).

Several rules of one kind merge into one finding: severity is the maximum, evidence is the union.
Confidence starts at the highest base of the rules that fired, then

* +1 level when two or more *different* signals of that kind fired (for example CPU throttling and CPU
  pressure): independent readings agree;
* -1 level when every rule that fired is in the lowest quarter of its warn..crit band (barely over the
  threshold).

There is no learning and no hidden state; `Evaluate` is a pure function of its input.

## Measurement completeness

Every diagnosis carries `measurementCompleteness`:

| Field | Meaning |
|-------|---------|
| `probesAttached`, `probesSkipped[]` | From the loader status (`/api/v1/ebpf/status`): attached count, and every program that was not attached with the loader's reason |
| `droppedEvents` | Sum and per-source counts of events lost before analysis: producer side, the per-CPU `drops` map of each key eBPF object (ring reserve or perf output failures); consumer side, the userspace decoder channels and perf lost samples. `notCounted` names attached objects that expose no `drops` map (an older build) |
| `sampling` | `ratio: 1` and a note. **No probe samples events.** Some probes drop events under a fixed threshold (`overlap` under 1 ms, `datapipe_bottleneck` idle gaps under 1 ms), listed in `filters`: not sampling, but also not the absence of the event |
| `nodesExpected`, `nodesReporting`, `missingNodes[]` | Node coverage. At the collector `nodesExpected` is `"unknown"` (a node sees only itself); the gateway fills it |
| `complete`, `reasons[]` | `complete` is true only when `reasons` is empty |

`reasons` is filled for: a diagnosis probe not attached or status unknown; dropped events; attached
objects without drop counters; flight ring eviction inside the window; any unavailable signal; expected
nodes unknown or missing.

A node-local report is therefore always `complete: false` (it cannot know the cluster). The gateway
computes the real answer.

### Gateway coverage

The gateway lists the pods with label `gryvia.io/job=<job>` in the job's namespace (phases Running or
Pending) and takes their `spec.nodeName` as the **expected nodes**. It fans out to every discovered
collector and compares:

* `nodesExpected`: the count, or the string `"unknown"` if the pod list failed (RBAC, API error);
  it is never `complete` when unknown;
* `nodesReporting`, `missingNodes`: expected nodes that did not return a diagnosis are named;
* pods not yet on a node, unreachable collectors, and each node's own reasons (prefixed with the node)
  are added to `reasons`;
* `complete` requires expected nodes known and non-empty, every one reporting, and no node reason.

The merged report also has `partial: true` in every other case, and the summary then reads
`no bottleneck detected in measured signals on 1 of 3 node(s); this is not a statement about the whole job`
instead of implying the job is healthy. Findings of one kind are combined across nodes: maximum
severity, evidence kept per node (`node` on every evidence row), and a kind seen on two or more nodes
raises confidence one level. Per-node summaries are in `nodeSummaries`, unavailable telemetry per node in
`unavailable`.

Limits: pods that have not yet been scheduled, or nodes without a collector, are only visible as missing
nodes (the gateway cannot tell a node with no collector from an unreachable one). A job's nodes are known
only while its pods exist.

## Persistent incident history (opt-in)

Off by default. Enable it with `-flight-store-dir` (chart: `ebpf.flightStore.enabled`). The in-memory
timeline is unchanged; the store adds, per job on this node:

* **incidents**: a finding of severity `warning` or worse that persists for `-flight-incident-min-duration`
  (default 60 s) opens an incident (its `start` is the first observation). It is updated as the peak grows
  (rewritten at most every 30 s), and closes when the finding has been absent for 60 s (`end` = last
  observation, `closeReason: "resolved"`). Each incident keeps the peak of every evidence metric, the
  evidence and summary at the peak severity, and the unavailable signals at the last update (an incident is a
  lower bound);
* **samples**: the measured `metrics` of the job about once a minute, used for comparison.

An incident left open by a crash or restart is closed at start-up with `closeReason: "collector-restarted"`
and `end` equal to the last persisted update: the time between is not observed.

### Format

A directory of segment files `flight-<20-digit unix nanoseconds>.jsonl`, mode 0600, directory 0700.
Each line is one JSON record:

```json
{"v":1,"type":"incident","at":"...","incident":{"id":"inc-...","namespace":"ml","job":"train","kind":"cpu","severity":"critical","confidence":"high","start":"...","end":"...","open":false,"closeReason":"resolved","summary":"...","peak":{"cpu_throttle_ratio":0.8},"evidence":[...],"unavailable":["cgroup.io.pressure"],"updatedAt":"..."}}
{"v":1,"type":"sample","at":"...","sample":{"namespace":"ml","job":"train","at":"...","metrics":{"cpu_throttle_ratio":0.3}}}
```

An `incident` record carries the whole incident: the last record per `id` wins. Records over 64 KiB are
refused; lines over 1 MiB are skipped on read.

### Crash safety, retention, bounds

* A restart never appends to an existing segment; it starts a new one. A crash can therefore tear only the
  last line of the newest old segment. Any line that does not parse (torn, garbage, over-long, unknown
  version) is skipped and counted (`corruptLines` in the incidents response); the intact records before it
  are kept.
* `-flight-store-fsync`: `always` fsyncs every record; `interval` (default) fsyncs incident records at once
  and samples every 10 s; `none` leaves it to the OS (a crash can lose recent records).
* `-flight-retention` (default 24h) removes segments whose last write is older, and drops old closed
  incidents from memory. `-flight-store-max-bytes` (default 64 MiB) removes the oldest segments until the
  total fits; the newest segment is never removed, so the total can exceed the bound by one segment
  (`min(8 MiB, max(64 KiB, maxBytes/8))`). Open incidents are rewritten into each new segment so pruning
  cannot lose them. Only files matching the segment name are read or deleted.
* Memory is bounded: at most 2000 incidents are indexed (the oldest closed are evicted from the index but stay
  on disk until retention); samples are not held in memory (comparison scans the segments).

Storage: the chart mounts an `emptyDir` (survives collector restarts, not pod/node replacement) or a
`hostPath` (survives pod replacement on the node). With the store off nothing is written.

### API

Collector (HMAC, all answer `200 {"enabled":false}` when the store is off, so a node without history is
reachable, not an error):

* `GET /api/v1/flight/incidents?namespace=&job=&since=&limit=`: `since` is an RFC3339 time or a duration (`6h`);
* `GET /api/v1/flight/incidents/export?format=json|csv`: same filters;
* `GET /api/v1/flight/compare?namespace=&job=&a=&b=[&window=5m]`: `a`/`b` is an incident id of this job, an
  RFC3339 time (window = the `window` before it) or `now`. Returns per-metric `a`, `b`, `delta` (b - a) and
  `percentChange` (null when `a` is 0). A window's value is the mean of the stored samples in it; with no
  samples an incident window falls back to the incident's peaks (`source: "incident-peak"`), otherwise
  `source: "none"`. A metric measured in only one window is listed with the other side `null`, never as 0.

Gateway (same auth and tenant scoping as `/api/flight/jobs/{job}`: a tenant only sees their namespaces; the
namespace is always sent to the collectors, and incidents whose namespace/job do not match are dropped
gateway-side):

* `GET /api/flight/incidents?namespace=&job=&since=&limit=`
* `GET /api/flight/incidents/export?namespace=&job=&since=&format=json|csv`
* `GET /api/flight/compare?namespace=&job=&a=&b=&window=`

The gateway result carries `history.enabledNodes` / `disabledNodes` and `coverage`: an empty list is not
proof that nothing happened on nodes without history. Comparison results are **per node** (metrics are
node-local and are not averaged; an incident id resolves only on the node that recorded it).

### CSV safety

The CSV export (collector and gateway) neutralises spreadsheet formula injection: any text cell starting with
`=`, `+`, `-`, `@`, tab or CR gets a leading `'`, control characters become spaces, and the standard CSV
writer quotes commas, quotes and newlines. Numeric columns are never prefixed.

## eBPF drop counters (audit)

`bpf_ringbuf_reserve` returning NULL, or `bpf_perf_event_output` failing, used to lose the event silently.
`ebpf/headers/gryvia_core.h` now provides `GRYVIA_DECLARE_DROPS()` (a `PERCPU_ARRAY` named `drops`, slot 0 =
ring reserve failures, slot 1 = perf output failures) and `GRYVIA_COUNT_DROP(slot)`. It is wired into:
`tcp_trace` (perf), `nccl_trace` (both emit sites), `gpu_mem_trace`, `datapipe_bottleneck`, `straggler`,
`overlap`, `infer_latency`, `weight_exfil`, `rdma_health`, `gds_trace` and `ucx_gloo`. The collector sums
each map over CPUs (`Manager.ReadCounter`), exposes them in `droppedEvents` and as the Prometheus gauge
`gryvia_measurement_dropped_events{source}` (monotonic since start).

Still uncounted (they use `bpf_ringbuf_reserve` or perf output without a counter): `container_escape`,
`connpool_analyze`, `driver_fim`, `crypto_detect`, `exfil_detect`, `fingerprint`, `latency_breakdown`,
`gradient_compress`, `numa_path`, `tcp_tuning`, `privesc_monitor`, `rdma_trace`, `training_pattern`,
`trace_correlator`, `dns_tracker`, `latency_probe`, `syscall_monitor`. Losses there are invisible to the
diagnosis. `roce_cnp` and `pfc_pause` are counters only (no ring). Userspace losses counted: the GPU and
fabric decoder channels (`Dropped()`) and flow perf lost samples (`Lost()`); the security decoder is not.

## Limits

* Findings are heuristics over a 5-minute window. They rank suspicion; they do not prove a cause. In
  particular GPU-side behaviour (utilisation, HBM, NCCL internals, NIC hardware counters) is not read.
* Node-level signals (`cnp_rate`, `pfc_rate`, drop counters) are not attributable to one job.
* The flight ring keeps 2048 events per job and 128 jobs; a full ring inside the window is reported as
  truncation and event counts are then lower bounds.
* Counter deltas need two cgroup samples at least 20 s apart; right after start-up (or for a new pod) the
  counter signals are unavailable and say why.
* Cgroup files are read with bounded sizes; at most 64 container cgroups per pod are read.
* The store is per node and local: no replication, no cross-node history in the collector.
