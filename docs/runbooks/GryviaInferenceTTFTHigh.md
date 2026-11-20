# GryviaInferenceTTFTHigh

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`gryvia_inference_latency_seconds{metric="ttft_p99"} > 2` for 10 minutes.

## What it means

Time to first token p99 (engine-reported) is above 2 s. Threshold is generic: set it from your model and SLO.

## How to check

```bash
gryvia_inference_latency_seconds{metric=~"queue_time_p99|prefill_p99|ttft_p99"}   # is the time in the queue or in prefill?
gryvia_inference_kv_cache_usage_ratio
gryvia_fabric_infer_wait_p99_seconds   # network wait before the first recv: rules the network in or out
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Queueing (GryviaInferenceQueueHigh).
- Long prompts: prefill dominates.
- Cold start or model reload.

## Mitigation

- If queue time dominates, scale out. If prefill dominates, shorten prompts or use prefix caching/chunked prefill in the engine.
- If `gryvia_fabric_infer_wait_p99_seconds` is high, investigate the network path.

## Limits (honest)

TTFT is only as accurate as the engine's histogram buckets; different engines define it slightly differently.
