# GryviaKVCacheSaturated

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`gryvia_inference_kv_cache_usage_ratio > 0.95` for 10 minutes.

## What it means

The worst replica of the serving job has its KV cache almost full; the engine will queue requests or preempt running ones.

## How to check

```bash
gryvia_inference_requests{state="waiting"}
gryvia_inference_latency_seconds{metric="queue_time_p99"}
DCGM_FI_DEV_FB_USED / (DCGM_FI_DEV_FB_USED + DCGM_FI_DEV_FB_FREE)   # requires dcgm-exporter
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Too many concurrent long sequences for the GPU memory reserved.
- Memory fraction too small for the model (`--gpu-memory-utilization` in vLLM).

## Mitigation

- Add replicas, lower max concurrent sequences, use a smaller context length, or a larger-memory GPU.
- Enable KV cache quantisation if the engine supports it.

## Limits (honest)

Reported by the engine (worst replica); engines that do not export it produce no series.
