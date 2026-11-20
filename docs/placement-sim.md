# Placement simulator (no GPUs needed)

`operators/ai-operator/cmd/placement-sim` runs a [Janus](https://github.com/zyvorai/janus) cluster config and workload through the operator's real node-selection code (`scheduler.FindOptimalNodesHeld`) using a fake Kubernetes client.

```bash
cd operators/ai-operator
go run ./cmd/placement-sim -config ../../../janus/configs/clusters/small_h100.yaml
```

It prints makespan, mean wait, GPU utilisation, jobs completed/unschedulable and GPU-memory violations. `-backfill=false` makes the queue head block (Janus' `fifo` backfills, so the default matches it).

## What it models

Whole-GPU jobs, nodes as bins of GPUs, arrival-order queue with optional backfill, multi-node jobs when `gpu_count` exceeds the largest node. It does **not** model NVLink/PCIe topology penalties, MIG, preemption, gang timeouts, inference serving, or GPU memory as a constraint (violations are only counted).

## Agreement with Janus

On the Janus cluster configs without those features (`priority_fifo`, `single_gpu`, `rl_small`, `preemption_priority`) the numbers match Janus exactly. On `small_h100` the makespan is 7800 s here against 8294.67 s in Janus, the difference being exactly Janus' 494.67 s topology inflation. Configs that use preemption, MIG, gang timeouts or topology differ, as expected.

To replay a live Gryvia cluster in Janus under other scheduler policies, see [Replaying a Gryvia cluster in Janus](janus-export.md).

This checks placement quality only. It says nothing about NCCL, RDMA, eBPF or training performance, which still need hardware.

## Finding

The default scoring prefers the node with the most free GPUs, so it spreads single-GPU jobs and a later whole-node job waits for a node to drain (`TestSpreadingScoreFragmentsWholeNodeJobs`: two 1-GPU jobs plus a 4-GPU job on 2x4 GPUs gives makespan 110 s instead of 100 s).

Annotate a job `gryvia.io/placement-strategy: pack` for best-fit scoring (fewer free GPUs that still fit wins); the same workload then finishes at 100 s (`-strategy pack` in the simulator). Default stays spread. Simulated only, not measured on a cluster.
