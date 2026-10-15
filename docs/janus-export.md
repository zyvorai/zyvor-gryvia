# Replaying a Gryvia cluster in Janus

[Janus](https://github.com/zyvorai/janus) is a discrete-event simulator for GPU scheduling: it replays a cluster under
FIFO, priority, preemptive, best-fit and other policies with no GPUs. `scripts/export-janus-bundle.py` turns a Gryvia
cluster (or a directory of manifests) into the bundle Janus reads, so the same jobs, nodes and quotas can be compared
across policies.

```bash
# from a live cluster (current kubectl context), or --from-dir DIR with JSON/YAML manifests
./scripts/export-janus-bundle.py --from-kubectl --out janus-bundle --profiles-dir ../janus/configs/profiles \
    --default-model llama-7b

cd ../janus
cargo run -p zyvor-janus-cli --release -- run --zynera-bundle ../gravia/janus-bundle --scheduler bestfit
```

## Mapping

Janus reads Zynera's kinds (`apiVersion: zynera.ai/v1`: `FabricAIJob`, `FabricGpuNode`, `FabricQuota`). Gryvia's
`GryviaGpuNode` and `GryviaQuota` have the same spec fields and are copied; `GryviaTenant.spec.quotas.concurrentGPUs`
becomes a quota for tenants no `GryviaQuota` covers. A `GryviaAIJob` is translated:

| Gryvia | Janus |
|---|---|
| `spec.gpus` (GPUs **per pod**) with `distributed.enabled` | `distributed: {nodes, gpusPerNode: gpus}` (Janus multiplies) |
| `spec.gpus`, not distributed | `gpus` |
| `spec.migProfile` + `gpus` | `mig: {profile, count: gpus}` |
| `distributed.enabled` with `nodes > 1` | gang annotations `zynera.ai/gang-schedule`, `zynera.ai/gang-size` |
| `spec.priority`, `spec.network`, `spec.type` | same |
| `spec.model`, `spec.gpuType` | must have a calibrated profile in Janus' profiles dir |

## What the result means

- Janus takes a job's **runtime and GPU memory from the calibrated model profile**, not from the job. The profiles Janus
  ships are labelled as estimates. The simulation compares policies on placement, waiting and utilisation; it does not
  predict how long your training run takes.
- A job with no model, or a (model, GPU type) without a profile, cannot be simulated and is **skipped with a warning**
  (`--strict` fails instead). CPU-only jobs are skipped.
- A job that needs more GPUs than the cluster has is flagged by the exporter and will never complete in the simulation.
- Arrival times are not exported: every job arrives at t=0.
- It models none of Gryvia's Kueue quotas, fair-share or fabric penalties; quotas enter only as `maxGPUs` per team.
  It says nothing about NCCL, RDMA or real GPU behaviour.

Tests: `python3 -m pytest scripts/tests/test_export_janus_bundle.py`. Set `JANUS_BIN` (the `zyvor-janus` binary) and
`JANUS_REPO` (a Janus checkout) to also replay an exported bundle through the real simulator.
