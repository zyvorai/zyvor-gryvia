# GryviaStragglerPersists

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`gryvia_fabric_straggler_rank > 0` for 15 minutes.

## What it means

The same job keeps having an NCCL rank flagged as the slowest participant. Rank 0 means 'unknown' as well, so a rank 0 straggler is never alerted.

## How to check

```bash
gryvia_fabric_nccl_p99_seconds{job=...} and gryvia_fabric_collective_max_skew_seconds{job=...}   # how slow, and how uneven
gryvia_fabric_collectives_compared{job=...}   # low counts mean weak evidence
kubectl -n gryvia-network get pods -l app.kubernetes.io/component=collector -o wide   # which nodes host the ranks
# Per-rank NCCL durations from the collector:  GET /api/v1/gpu/nccl   (signed; see the README)
# DCGM view of the same GPU (requires dcgm-exporter): DCGM_FI_DEV_GPU_UTIL, DCGM_FI_DEV_POWER_USAGE, DCGM_FI_DEV_GPU_TEMP for the node hosting the rank
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Thermal or power throttling of one GPU.
- A degraded NIC port or cable on the straggler's node (see GryviaNicErrors).
- Uneven data loading or CPU contention on one node (a compute straggler, not a network one).
- A noisy neighbour on the same node.

## Mitigation

- Find the node hosting the straggler rank and inspect it (throttling, NIC counters, competing pods).
- Restart the job onto different nodes if the node stays bad; use `kubectl cordon` to keep it from being reused.
- Do not treat the alert as proof of a network fault: host-side call durations include waiting for peers.

## Limits (honest)

`gryvia_fabric_collective_max_skew_seconds` is host-side call duration, not GPU time. Detection only sees ranks the local collector can attribute to the job.
