# GryviaInferenceQueueHigh

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`gryvia_inference_latency_seconds{metric="queue_time_p99"} > 5` for 10 minutes.

## What it means

The serving engine (vLLM, Triton or TGI) reports that requests wait longer than 5 s in its own queue at p99. The value is read from the engine's /metrics, so it is the engine's view.

## How to check

```bash
gryvia_inference_requests{state=~"running|waiting"}   # waiting vs running requests
gryvia_inference_kv_cache_usage_ratio                    # KV cache saturation drives queueing
kubectl -n <ns> get pods -l gryvia.io/job=<job>; kubectl -n <ns> logs <pod> | tail
Dashboard 'Gryvia / Inference serving'
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Offered load above capacity (too few replicas or GPUs).
- KV cache full (GryviaKVCacheSaturated), forcing preemption.
- Very long prompts or generation lengths.

## Mitigation

- Scale replicas (`kubectl scale` or the GryviaInferenceService replica field) or add GPUs.
- Reduce max batch/sequence length or enable admission limits in the engine.

## Limits (honest)

Only available with the collector's opt-in `-infer-metrics` / `-infer-metrics-discover`. Absent series do not mean healthy. The p99 is computed over a 5 minute window of histogram deltas.
