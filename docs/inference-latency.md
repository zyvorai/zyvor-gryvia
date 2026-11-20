# Inference latency and request tracing

Four different numbers get called "inference latency". They answer different questions and come from different
places, so Gryvia reports them separately and never adds or substitutes one for another.

| Number | What it is | Where it comes from | Field |
|--------|------------|---------------------|-------|
| Network accept wait | Time from the kernel accepting a TCP connection on an inference port to the first `tcp_recvmsg` on it. It includes the time the client takes to send its first bytes. It is **not** queue time and not model latency | eBPF `infer_latency.c` (always on when the collector runs, ports from `-infer-ports`) | `inferWaitP99ms`, `gryvia_fabric_infer_wait_p99_seconds` |
| Engine queue time | Time a request waited inside the serving engine before it was scheduled | The engine's own metrics | `queueTimeP99ms` |
| Time to first token (TTFT) | Time from request arrival to the first generated token | The engine's own metrics (vLLM) | `ttftP99ms` |
| Inter-token latency (ITL) | Time per generated token after the first | The engine's own metrics (vLLM; TGI reports a per-request mean) | `itlP99ms` |

Kernel probes cannot see tokens, so TTFT, ITL and engine queue time can only be taken from the engine. That is what
the opt-in scraper below does. Everything on this page is **off by default**; an install that sets none of the flags
behaves exactly as before.

## Reading the engines' metrics

The collector scrapes the engines' Prometheus endpoints with read-only HTTP GETs (no redirects followed, 5 s timeout,
8 MiB limit) every 15 s and folds the result into the job's fabric status.

```text
-infer-metrics 'name=vllm,url=http://10.0.0.5:8000/metrics'                 # repeatable, or ';'-separated
-infer-metrics 'name=trt,url=http://10.0.0.6:8002/metrics,engine=triton,namespace=ml,job=serve'
-infer-metrics-discover -infer-metrics-ports 8000,8002,8080               # pods on THIS node
-infer-metrics-interval 15s                                                # minimum 5s
```

A target has a `name` (unique), a `url` (`http` or `https`, no credentials), an optional `engine` (`vllm`, `triton`,
`tgi`; default: detected from the metric names) and optionally `namespace` and `job` together, which attribute the
figures to a job (the `GryviaFabricSignal` whose `spec.jobRef` is `job`). A target without them is exported only on the
collector's `/metrics` under namespace `_unattributed`.

Discovery (`-infer-metrics-discover`) lists the pods of this node (the pod-list permission the collector already has;
no new RBAC) and scrapes `http://<podIP>:<port>/metrics` for every Running pod that carries a `gryvia.io/job` label and
declares one of the ports as a `containerPort`. It only works in a cluster with `NODE_NAME` set; pods on the host
network and pods without the label are skipped, at most 64 targets are scraped, and a failing pod list keeps the last
known set. Helm: `ebpf.inferMetrics.targets`, `ebpf.inferMetrics.discover`, `.ports`, `.interval` (network-intelligence
chart). Writing the figures into the `GryviaFabricSignal` status additionally needs `ebpf.publishFabricStatus`
(existing flag and RBAC); without it the figures are available on `/metrics` and on the collector's HMAC-protected
`GET /api/v1/inference` (per-target status, which metrics were available or missing, last error).

### What is read from each engine

| Engine | TTFT | ITL | Queue time | End-to-end | Other |
|--------|------|-----|------------|------------|-------|
| vLLM | `vllm:time_to_first_token_seconds` | `vllm:inter_token_latency_seconds`, or the older `vllm:time_per_output_token_seconds` | `vllm:request_queue_time_seconds` | `vllm:e2e_request_latency_seconds` | prefill/decode `vllm:request_prefill_time_seconds`, `vllm:request_decode_time_seconds` when present; `vllm:num_requests_running`, `vllm:num_requests_waiting`; KV cache from `vllm:kv_cache_usage_perc` or the older `vllm:gpu_cache_usage_perc` (a 0..1 fraction despite the name) |
| NVIDIA Triton | not exported | not exported | `nv_inference_queue_duration_us` / `nv_inference_request_success` (mean), or `nv_inference_queue_summary_us{quantile="0.99"}` | `nv_inference_request_duration_us` / `nv_inference_request_success` (mean), or `nv_inference_request_summary_us{quantile="0.99"}` | compute mean per execution from `nv_inference_compute_infer_duration_us` / `nv_inference_exec_count`; `nv_inference_pending_request_count` as requests waiting |
| HuggingFace TGI | not exported | `tgi_request_mean_time_per_token_duration` (the p99 of per-request means, not of single tokens) | `tgi_request_queue_duration` | `tgi_request_duration` | `tgi_queue_size` (waiting), `tgi_batch_current_size` (running) |

Triton's latency counters are cumulative microseconds, so without summary latencies (`--metrics-config
summary_latencies=true` on the server) only a **mean** over the window can be derived. Means are reported in separate
fields (`queueTimeMeanMs`, `e2eMeanMs`) and are never placed in a `p99` field. With summaries, the 0.99 quantile is used
(the worst model when a server hosts several).

A metric an engine version does not export is reported as **not measured** (the field is absent, `null` in a status
patch, no Prometheus series), never as zero. `GET /api/v1/inference` lists, per target, which logical metrics were
available and which were missing.

### How the quantiles are computed

Histograms are cumulative since the engine started, so the scraper subtracts consecutive scrapes. The first scrape of
a target is only a baseline. The per-interval bucket deltas of the last five minutes (the collector window) are summed
and the p99 is found by linear interpolation inside the bucket that holds the rank (the same method as PromQL
`histogram_quantile`; a rank in the `+Inf` bucket returns the highest finite bound). Series that differ only by labels
(`model_name`, `engine`) are added together first. A counter that goes backwards (engine restart) or a changed bucket
layout re-baselines: the window is emptied and that interval is dropped rather than guessed. Idle intervals keep the
earlier deltas until they age out. Bucket boundaries limit accuracy: a p99 is an interpolated estimate, not an exact
percentile.

Several scraped replicas of one job on a node are combined pessimistically: latencies and KV cache take the worst
replica, request counts add up. That is "the slowest replica's p99", not the p99 of all requests. With several
nodes the last collector to write the status wins, as for every other fabric field.

### Where the figures appear

* Prometheus (collector `/metrics`): `gryvia_inference_latency_seconds{namespace,job,engine,metric}` (`ttft_p99`,
  `itl_p99`, `queue_time_p99`, `e2e_p99`, `prefill_p99`, `decode_p99`, and `queue_time_mean`, `e2e_mean`,
  `compute_mean` for counter-only engines), `gryvia_inference_requests{...,state="running|waiting"}`,
  `gryvia_inference_kv_cache_usage_ratio`. The older `gryvia_fabric_infer_wait_p99_seconds` keeps its meaning (network
  accept wait).
* `GryviaFabricSignal.status`: `engine`, `ttftP99ms`, `itlP99ms`, `queueTimeP99ms`, `e2eP99ms`, `queueTimeMeanMs`,
  `e2eMeanMs`, `requestsWaiting`, `kvCacheUsage` (CRD reference: `website/docs/reference/crds.md`). None of them
  feeds `scoreDelta`.
* Gateway: `GET /api/flight/inference/{job}?namespace=` reads that status (same namespace rules as the Flight Recorder
  route); `available` is `false` when the scraper is off.
* Dashboard: an **Inference latency** card on the job details page. It renders only when `available` is true, names
  the engine, shows "not measured" for what the engine does not export, and lists the network accept wait as a
  separate row.
* CLI: `gryvia-flight -inference` adds an inference section to `-o text`, or wraps the output as
  `{"flight": ..., "inference": ...}` with `-o json`.

## Request tracing (W3C trace context)

`ebpf/trace_correlator.c` (a TCX ingress program attached to `-iface`) reads a `traceparent` header out of ingress TCP
payloads and emits `{trace id, span id, 4-tuple}` on its `trace_events` ring. With `-trace-correlate` the collector
consumes that ring, and:

* keeps a bounded index (16 384 connections, 8 requests per connection, 10 minute TTL) of the requests addressed to a
  pod on this node that carries a `gryvia.io/job` label (resolved by destination pod IP through the Flight Recorder's
  pod list; anything else is dropped, so unrelated traffic leaves no trace id behind);
* attaches `traceId` (and `traceMatch`) to Flight Recorder events at report time: flow and retransmit events of the same
  TCP 4-tuple within 30 s (`traceMatch: "tuple"`), and, when tracing is on, the inference accept-wait events (recorded
  as `inference_accept_wait`), which carry no 4-tuple and are joined by pod, local port and time within 2 s only when
  exactly one trace matches (`traceMatch: "port_time"`);
* serves `GET /api/v1/flight/trace?traceId=<32 hex>` behind the same HMAC as `/api/v1/flight/diagnose`.

`-trace-correlate` needs `-iface` and `-flight-token-file`; Helm: `ebpf.traceCorrelation` (needs `ebpf.interface` and
`ebpf.flightTokenSecret`). The gateway route is `GET /api/flight/trace/{traceId}[?namespace=]`. It fans out to every
collector, and every observation and event carries the identity of the pod that received the request. Tenant users only
receive items in their own namespaces; the trace of another tenant answers 404, exactly like a trace that does not exist,
and `coverage.reporting` counts only nodes that contributed something the caller may see. The provider admin sees every
namespace (or one, with `?namespace=`).

**Limits** (kernel-level, not oversights):

* plaintext HTTP/1.x only. TLS-terminated traffic, HTTP/2 and gRPC (HPACK) carry no readable `traceparent`;
* the header name must start within the first 256 bytes of a packet's payload (the probe reads at most 512), and only
  the first `traceparent` in a packet is used; IPv4 only;
* the trace is keyed by 4-tuple, so on a keep-alive connection the newest request wins and a slow earlier request on
  the same connection can be attributed to the newer trace (`tuple` is a connection match, not a proof of causality);
* the observation is taken at the interface `-iface` is attached to. Behind kube-proxy DNAT (a NodePort seen on the
  node's physical interface) the destination is the node address, not the pod, and the request is not indexed. Attach
  to the pod-facing interface (or use pod-network traffic) for it to be attributed;
* node-local: only nodes with a running collector answer, and a trace that crosses nodes appears once per node.

The trace id is user data. The collector and the gateway never log it, never echo it in error messages, and redact it in
the gateway's access-log lines (`/api/flight/trace/<redacted>`). It is part of the URL path and of the signed request URI
between the gateway and the collectors, so avoid putting the gateway behind an HTTP proxy or WAF that logs full URLs
unless that is acceptable for your traces.

## What was verified, and what was not

* The Prometheus text parser, histogram delta and quantile math, the three engine parsers, the scraper (HTTP behaviour,
  discovery, error handling), the fabric fold and status patch, the exporter, trace decoding/correlation and the gateway
  scoping have unit tests.
* **The engine fixtures under `collector/pkg/inference/testdata/` are hand-written**, generated by a script from the
  metric names, label sets and bucket layouts documented for vLLM, Triton and TGI. They were not captured from a live
  engine, so a real engine version that differs in names, labels or bucket layout is not covered; the scraper reports
  such a target as "endpoint is reachable but exports none of the metrics this collector reads" or lists the missing
  metrics rather than guessing. Nothing was scraped from a running vLLM, Triton or TGI.
* The `trace_events` record layout was checked on a Linux 7.0 host: `trace_correlator.o` was loaded, attached with TCX
  to the loopback interface, a plaintext HTTP request with a `traceparent` header was sent, and the 56-byte ring record
  decoded to the exact trace id, span id and endpoints. The full collector path (pod-IP resolution, gateway fan-out) needs
  a cluster and was tested only against fakes.
