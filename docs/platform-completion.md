# Platform completion bundle

This bundle closes runtime gaps across serving, recovery, scheduling and metering.
The table distinguishes implemented behavior from integrations and retired APIs.

| Area | Delivered behavior | Deployment requirements / limits |
|---|---|---|
| Inference telemetry | Streaming HTTP proxy sidecar, request/error counters, duration histogram, active requests, version/UID/revision/pod labels | Opt in; scrape its metrics. Errors mean HTTP 5xx, upstream failure or cancellation; latency includes completed stream delivery |
| Serving validation | Live proxy + real Prometheus scrape/query + canary rollback integration; real Kubernetes API identity/rollout checks; Gateway load qualification script | GPU model servers and Gateway data plane still need a real cluster run |
| Training recovery | Crash-consistent immutable checkpoint blobs, atomic latest pointer, SHA256 verification, PyTorch model/optimizer/RNG recovery and a cooperative preStop trainer | Single-process reference trainer; durable shared storage required. A file-based all-ranks commit protocol (`coordinated_checkpoint.py`) is tested with multiple CPU processes and used by an elastic `torchrun` trainer (`elastic_train.py`) in the kind e2e; DCP checkpoints (`dcp_checkpoint.py`) reshard model and optimizer state to another world size, tested with CPU processes (DDP and FSDP2, 4 to 2 and 3); nothing has run on GPUs |
| Scheduling | Native PriorityClasses, Kueue fair-sharing configuration, topology annotations/flavor binding, MultiKueue admission-check binding | Kueue owns admission, preemption and remote dispatch; install/configure its manager and worker credentials |
| GPU health | Fresh-data health checks, opt-in quarantine/cordon and PDB-respecting drain, safe handling of missing observations | No automatic uncordon, GPU reset, driver reload or reboot. These need a privileged node-agent implementation and hardware testing |
| Cost controls | Chargeback from frozen metered usage, period/timezone proration, duplicate-UID and currency checks, budget threshold Kubernetes Events | Usage remains wall-clock estimates; no money movement, FX conversion, PDF/email reports or storage/network attribution |
| Legacy APIs | Explicit `Ready=False/UnsupportedAPI` reporting instead of activating dormant simulation controllers | Objects remain readable for migration; unsupported functionality is not declared complete |
| Qualification | Read-only GPU/RDMA inventories, explicit NCCL test argv, fio read qualification, Gateway request latency/error/distribution reports | Missing binaries/hardware produce blocked results, never passing results |

## Telemetry and serving

Set Helm `platformCompletion.inferenceTelemetry=true` to use the AI operator image as
the sidecar image, or set `--inference-telemetry-image` explicitly. Opt in each inference
service with `gryvia.io/inference-telemetry: "true"`. Existing external instrumentation
and services without this annotation keep their existing deployment shape.

The sidecar uses ports 18080 (serving) and 18081 (metrics). Kubernetes Services target
the named `http` port on the proxy; model readiness/liveness probes still hit the model
server directly. Neither GPU nor model dependencies are required by the proxy. The
operator binary runs `inference-proxy` as a subcommand; the same image is reused.

`examples/inference/telemetry-scrape.yaml` discovers only the telemetry container's
metrics port, avoiding duplicate counter collection. Metrics already contain all labels
required by the SLO selector. The proxy exposes `X-Gryvia-Track` and model-version response
headers for request distribution checks. It does not inspect model outputs or token usage.

SIGTERM stops accepting requests and allows up to 120 seconds for active streams to drain;
pod termination grace is 130 seconds. This is not a guarantee for longer streams or a
model container that exits earlier. No write timeout is imposed on streams.

To verify a real Gateway after installing model servers and routing:

```bash
python3 scripts/qualify-inference.py --url https://chat.example.com/v1/completions \
  --body request.json --requests 1000 --max-p95-seconds 30 --canary-weight 10 \
  --tolerance-percent 5 --output serving-results.json
```

Use enough traffic to satisfy the canary's configured minimum request count. Distribution
is statistical. Run the script again after rollback with `--canary-weight 0`. TLS certificate
verification is enabled. Authenticated endpoints need an authenticated load tool; the
qualification script currently does not supply credentials.

## Recovery

Package `checkpoint_store.py`, `pytorch_recovery.py`, `checkpoint_worker.py` and
`train_recoverable.py` in your training image with PyTorch installed. Use the existing
Gryvia annotations:

```yaml
metadata:
  annotations:
    gryvia.io/checkpoint-command: '["python3", "/app/train_recoverable.py", "--request"]'
    gryvia.io/checkpoint-directory: /data/checkpoints
    gryvia.io/checkpoint-grace-seconds: "120"
```

The trainer must run `/app/train_recoverable.py` and `spec.storage` must provide the
persistent /data mount. It saves periodically, acknowledges preStop requests only after
publishing a durable checkpoint, and resumes on restart. Abrupt node loss recovers the
last periodic save, not an unsaved step; with a [checkpoint guard](checkpoint-guard.md) (`restore.autoRestore`)
the operator replaces the pods of a non-elastic job stuck on a lost node instead of waiting for eviction. Corruption and rank/world-size mismatches fail
explicitly. Checkpoint blobs are retained; operators must provide retention/garbage collection.
SHA256 detects corruption, not malicious writers; trust and isolate the checkpoint volume.

## Scheduling and federation

`platformCompletion.kueueFairSharing=true` adds equal weight to managed ClusterQueues.
Enable `fairSharing` in the Kueue manager configuration too. BestEffortFIFO remains the
queueing strategy, allowing Kueue to consider jobs behind a blocked head job; Gryvia does
not implement a second DRF/backfill scheduler.

Create a Kueue Topology first, then set `platformCompletion.kueueTopologyName`. AIJob
annotations `gryvia.io/required-topology` or `gryvia.io/preferred-topology` carry the topology
level label to the PodSet. They are mutually exclusive. No topology is inferred from hardware. Topology-enabled flavors select Linux nodes; label them with all levels in the configured Topology.

`examples/scheduling/advanced-kueue.yaml` contains a Topology and MultiKueue objects.
Prepare worker kubeconfig Secrets in the Kueue manager namespace and configure manager/
worker job integration following the pinned Kueue documentation. Set
`platformCompletion.kueueAdmissionCheck=multikueue` to bind managed queues. This is native
MultiKueue integration. A two-kind-cluster CPU e2e (`e2e-multikueue.yml`) passes in CI; GPUs, data replication and failover across clusters are untested.

GryviaPriority now creates an owned Kubernetes PriorityClass. Set the AIJob annotation
`gryvia.io/priority-class` to its name. Kubernetes handles pod preemption; Kueue's existing
WorkloadPriorityClass handles admission. This controller does not invent a Preempted status
while pods are still running, bypass quotas, or promise an SLA.

Federation probes are enabled only with administrator-configured
`--federation-allowed-servers` and `--federation-credentials-namespace`. Secrets must contain
inline kubeconfigs matching allowlisted HTTPS endpoints. Exec plugins, auth providers,
file-based credentials, proxy URLs and insecure TLS are rejected. Readiness is an actual
three-second `/readyz` probe; declared inventory is not presented as measured utilization.

## Health and cost controls

Enable `platformCompletion.gpuHealth` to register GryviaHealthCheck, and independently
`platformCompletion.gpuRemediation` to permit configured cordon/drain actions. Missing or
stale GPU observations cannot trigger remediation. Drain uses the eviction API, respects
PDB rejection and refuses unmanaged pods or disk-backed EmptyDir data. DaemonSets and
mirror pods are left alone. Memory-backed /dev/shm is allowed. Quarantines have an owner
annotation and a `gryvia.io/gpu-unhealthy` NoSchedule taint. A human must validate recovery
before removing that taint, owner annotation and cordon. Unsupported measurements (e.g.
ECC without a supplied metric) report unknown rather than passing.

Chargeback supports actual-usage attribution from persisted GryviaUsageRecords. Cost-center
team mappings and namespace ownership must be unambiguous. Usage currency must match the
report currency; prices are not retroactively recomputed. Optional platform overhead is
allocated consistently. This controller creates estimates, not payment invoices.

Budget threshold Events are written in namespace `default` unless EventNamespace is set
in an embedding controller. Event names are deterministic and retries are idempotent while
Events exist; normal Kubernetes Event TTL can cause an older retained alert to reappear.
Use a cluster Event exporter for your own notification system.

## Legacy APIs

The legacy kinds AutoScaler, RetryPolicy, DRTest, SLA, Audit, QuotaPolicy, Benchmark and Metric were removed (see the
CHANGELOG for the upgrade steps). Set `platformCompletion.reportUnsupportedAPIs=true` to mark `GryviaJobHook` and
`GryviaDataset` objects `Ready=False` (`UnsupportedAPI`) while their controllers are off (`aiOperator.jobHooks.enabled`,
`storageOperator.datasets.enabled`). `GryviaGPUSharingPolicy` is not on this list: its controller is kept (opt-in with
`gpuOperator.gpuSharing`) because the NVIDIA one-click path depends on it. The other dormant legacy controller files are removed;
the compatibility capability controllers only update a Ready condition. Template validation/usage reporting
and PriorityClass reconciliation are registered by the existing ML-controller capability.

Replacement paths for the removed kinds: Kubernetes/Kueue job retries (`spec.retryLimit`), a cluster autoscaler,
the Kubernetes audit log, workload Jobs for benchmarks, and externally measured SLOs.

## Hardware qualification

```bash
python3 scripts/qualify-platform.py --gpu --rdma --storage-file /mnt/data/existing-file \
  --output hardware-results.json
python3 scripts/qualify-platform.py --output nccl-results.json \
  --nccl-command all_reduce_perf -b 8M -e 1G -f 2 -g 2
```

The storage command is read-only; NCCL is an explicitly supplied command. Reports distinguish
passed, failed and blocked. Inventory passing does not prove NCCL bandwidth or model serving.
No GPU/RDMA/NCCL/storage performance or uptime claim is made without recorded cluster results.
