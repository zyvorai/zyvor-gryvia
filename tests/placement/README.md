# Placement benchmark

Janus-format cluster and workload fixtures (copied from [Janus](https://github.com/zyvorai/janus), Zyvor Production Licence v2.0)
run through the operator's real node-selection code by `operators/ai-operator/cmd/placement-sim`.
`scripts/placement-benchmark.sh` compares the result with `baseline.json` and fails when makespan, mean wait or
GPU utilisation gets worse by more than the tolerance (default 1%). It measures placement quality on a fixed
set of small workloads only. It says nothing about NCCL, RDMA, eBPF, training throughput or storage, which still
need hardware, and the numbers are a regression tripwire, not a performance claim.

```bash
scripts/placement-benchmark.sh            # compare with baseline.json
scripts/placement-benchmark.sh --update   # accept new numbers (review the diff, explain it in the PR)
```

An improvement (lower makespan or wait, higher utilisation) passes but is reported, so the baseline can be tightened.
