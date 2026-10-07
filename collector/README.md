# Gryvia Flow Collector

A Go userspace agent that loads the eBPF programs from `../ebpf/` into the kernel, attaches them by section name,
decodes their events, and serves the result as Prometheus metrics and JSON endpoints (service graph, anomalies, GPU/NCCL,
fabric signals, security alerts, training analysis, TCP tuning advice and the Flight Recorder).

**Status: experimental.** It is off by default (`ebpf.enabled=false` in `helm/network-intelligence`), and its image is
built in CI and is in the signed release workflow, but no tagged release has published it yet, so for now you build it
yourself (`docker build -f collector/Dockerfile .`).
It runs privileged with `hostNetwork` and `hostPID`. On a Linux 7.0 x86_64 host it attached 36 of 83 hooks and decoded
real TCP flows; GPU, NCCL, RDMA and GPUDirect Storage behaviour, arm64 loading and XDP/TCX/sockops attachment have not
been verified on real hardware. Most HTTP endpoints below are **unauthenticated**; see [SECURITY.md](../SECURITY.md).

## Architecture

```
   kernel eBPF programs (../ebpf, 47 CO-RE objects)
        |  perf event arrays (flow)   ring buffers (GPU, security, fabric)
        v
   +--------------------------- collector ---------------------------+
   | loader   attach by section name, tolerate per-program failures  |
   | decoder  flow, GPU and security events; K8s metadata from /proc |
   | aggregator, graph, anomaly, ai, security, tuning, fabric, flight |
   | exporter Prometheus metrics                                      |
   +---------------------------------+-------------------------------+
                                     |  :9090  /metrics  /api/v1/...
```

### Packages

| Package | Purpose |
|---------|---------|
| `pkg/loader` | Loads compiled `.o` files (cilium/ebpf), attaches kprobe, kretprobe, uprobe, tracepoint, raw_tracepoint, tp_btf, XDP, TCX, sockops and sk_msg programs by section name, resolves NCCL/CUDA/cuFile library paths, and records per-program attach results |
| `pkg/decoder` | Decodes `flow_event` (network), `gpu_event` (NCCL, CUDA memory, RDMA) and security events |
| `pkg/aggregator` | Sliding-window flow aggregation (p50/p95/p99 latency, bytes/s, connections/s per service pair), plus NCCL and GPU-memory aggregation |
| `pkg/graph` | In-memory service dependency graph |
| `pkg/anomaly` | Statistical anomaly detection (EMA and standard deviation) |
| `pkg/ai` | Training communication and data-pipeline bottleneck analysis |
| `pkg/security` | Aggregates events from the security programs |
| `pkg/tuning` | TCP tuning recommendations from observed RTT and congestion window |
| `pkg/fabric` | Decodes the `fabric_signal` ring buffers (straggler, RDMA health, GDS, overlap, RoCE CNP, inference wait) and folds them per job with a `[0,1]` score penalty |
| `pkg/flight` | Flight Recorder: bounded, node-local, job-attributed event timeline (see [docs/flight-recorder.md](../docs/flight-recorder.md)) |
| `pkg/exporter` | Prometheus metrics |
| `cmd/verifyobj` | Loads every `.o` through the kernel verifier without attaching (used by CI) |

## HTTP endpoints (listener `-metrics-addr`, default `:9090`)

| Endpoint | Auth | Description |
|----------|------|-------------|
| `/metrics` | none | Prometheus metrics |
| `/healthz` | none | Liveness |
| `/api/v1/ebpf/status` | none | Which programs and hooks attached on this node, and why others did not |
| `/api/v1/graph` | none | Service dependency graph |
| `/api/v1/anomalies` | none | Recent anomalies |
| `/api/v1/gpu/nccl`, `/api/v1/gpu/memory` | none | NCCL per-operation stats and stragglers; host/device transfer counters |
| `/api/v1/fabric` | none | Per-job fabric signals and score penalty |
| `/api/v1/security/alerts` | none | Security detections |
| `/api/v1/ai/training`, `/api/v1/ai/pipeline` | none | Training communication and pipeline analysis |
| `/api/v1/tuning/tcp` | none | TCP tuning advice |
| `/api/v1/flight/diagnose?namespace=&job=` | HMAC token | Flight Recorder timeline; answers 503 until `-flight-token-file` is set |
| `/api/v1/flight/diagnosis?namespace=&job=` | HMAC token | Evidence-backed bottleneck diagnosis with unavailable telemetry and `measurementCompleteness` ([docs/flight-diagnosis.md](../docs/flight-diagnosis.md)) |
| `/api/v1/flight/incidents`, `/incidents/export`, `/compare` | HMAC token | Persistent incident history, export and before/after comparison; `{"enabled":false}` unless `-flight-store-dir` is set |

Metric families include `gryvia_network_*` (flow bytes, latency, active connections, drops, DNS latency, cost bytes),
`gryvia_nccl_*`, `gryvia_gpu_memcpy_*`, `gryvia_rdma_*`, `gryvia_roce_*`, `gryvia_fabric_*`, `gryvia_training_*`,
`gryvia_pipeline_bottleneck`, `gryvia_tcp_*` and `gryvia_security_*`; `pkg/exporter/prometheus.go` is the authority.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-metrics-addr` | `:9090` | HTTP listen address |
| `-ebpf-dir` | `/opt/gryvia/ebpf` | Directory with the compiled `.o` files |
| `-iface` | empty | Interface for XDP/TCX programs (`packet_filter`, `dns_tracker`, `roce_cnp`, `cost_tracker`, `trace_correlator`); empty skips them |
| `-attribute-network` | off | Attribute `cost_tracker` byte counters to tenants and peer/zone classes (needs `-iface`, a cluster, `NODE_NAME`); see [`docs/network-cost-attribution.md`](../docs/network-cost-attribution.md) |
| `-publish-network-usage` | off | Write per-tenant egress as `GryviaNetworkUsageRecord` objects every 60 s (needs `-attribute-network`) |
| `-cgroup-path` | empty | cgroup v2 path for `sockops`/`sk_msg`; empty skips them |
| `-nccl-lib`, `-cuda-lib`, `-cufile-lib` | empty | Library paths for the NCCL, CUDA runtime and cuFile uprobes; empty means auto-discover |
| `-uprobe-pid` | `0` | Find those libraries through `/proc/<pid>/maps` of this process |
| `-infer-ports` | `8000,8001` | Local TCP ports of inference servers for `infer_latency` (at most 8; empty disables it). Measures the network accept wait only |
| `-infer-metrics` | empty | Opt in: scrape a serving engine's own Prometheus endpoint (vLLM, Triton, TGI) for TTFT, ITL, queue time; repeatable, `name=..,url=..[,engine=..][,namespace=..,job=..]`. See [docs/inference-latency.md](../docs/inference-latency.md) |
| `-infer-metrics-discover`, `-infer-metrics-ports`, `-infer-metrics-interval` | off, `8000,8002,8080`, `15s` | Also scrape labelled job pods of this node that declare one of the ports |
| `-trace-correlate` | off | Opt in: attach W3C trace ids to the Flight Recorder timeline and serve `GET /api/v1/flight/trace` (HMAC-protected); needs `-iface` and `-flight-token-file` |
| `-flight-token-file` | empty | File with the Flight Recorder token (at least 32 characters); empty disables the endpoint |
| `-flight-diagnosis-cgroup` | empty | cgroup v2 root the diagnosis reads (read-only) for CPU throttling, memory events and PSI; empty reports those signals as unavailable |
| `-flight-diagnosis-thresholds` | empty | JSON file overriding the diagnosis thresholds |
| `-flight-store-dir` | empty | Persistent incident history directory; empty disables it. Also `-flight-retention` (24h), `-flight-store-max-bytes` (64 MiB), `-flight-store-fsync` (interval), `-flight-incident-min-duration` (60s) |
| `-window` | `300` | Aggregation window in seconds |
| `-nats-url` | empty | Accepted but not implemented: no NATS connection is made |

## Overhead

CPU of the collector and the kernel-side cost of its programs, measured on one host: [`docs/ebpf-overhead.md`](../docs/ebpf-overhead.md).

## Building and deploying

```bash
cd collector && go build -o gryvia-collector .          # binary only
docker build -f collector/Dockerfile -t gryvia-collector .   # compiles ../ebpf too; run from the repo root
```

Deploy with the `helm/network-intelligence` chart (`--set ebpf.enabled=true`, and point `collector.image.repository`
at your build) rather than by hand; `collector/deploy/daemonset.yaml` is a standalone example of the same DaemonSet.

Node prerequisites: a kernel with BTF (`/sys/kernel/btf/vmlinux`), BPF filesystem at `/sys/fs/bpf`, debugfs at
`/sys/kernel/debug`; the `tcx/*` programs need Linux 6.6 or newer. Programs whose kernel symbols or libraries are absent
(for example `mlx5_ib_*`, `nvidia_fs_read`) are skipped with a log line; check `/api/v1/ebpf/status`.

## Tests

`cd collector && go vet ./... && go test ./...`. CI (`.github/workflows/ebpf.yml`) also builds every program and loads
it through the kernel verifier of the runner.
