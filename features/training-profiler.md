# GryviaTrainingProfiler - Automatic GPU Efficiency Analysis

Automatic GPU efficiency analysis that profiles training jobs and produces actionable recommendations to improve throughput, reduce cost, and maximize GPU utilization.

> **Status: controller and analyzer implemented (ai-operator); the metric producer is not in this repo.**
> The profiler reads its inputs from `gryvia.io/gpu-*` annotations on running job pods (see
> "Metrics Collection"). Nothing in this repository writes those annotations: there is no
> "GPU monitoring sidecar", and the GPU operator does not set them. Until you supply your own
> annotator, all metrics read as 0 and recommendations will not reflect real utilization. The
> MFU baselines, thresholds and "expected impact" percentages are heuristics in the analyzer, not
> measured results, and the Prometheus metrics mentioned are not exported (see
> `monitoring/README.md`). Unverified on real GPU workloads.

## Overview

GryviaTrainingProfiler is a Kubernetes-native profiling system that:

- **Automatically profiles** running GryviaAIJob training workloads
- **Calculates MFU** (Model FLOPS Utilization) to measure actual vs theoretical GPU performance
- **Generates actionable recommendations** across batch size, data loading, mixed precision, compilation, distributed strategy, and GPU type selection
- **Compares against baselines** for H100, A100, V100, and T4 GPUs
- **Emits Kubernetes events** so teams get notified about optimization opportunities
- **Exposes Prometheus metrics** for cluster-wide GPU efficiency dashboards

## Quick Start

### 1. Deploy the CRD

```bash
kubectl apply -f crds/gryvia.io_gryviatrainingprofilers.yaml
```

### 2. Create a Profiler

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaTrainingProfiler
metadata:
  name: my-profiler
  namespace: default
spec:
  target:
    type: auto
    jobSelector:
      gryvia.io/type: training
    autoProfile:
      warmupSteps: 100
      profileSteps: 50
      cooldownMinutes: 30

  metrics:
    gpuCompute:
      smUtilization: true
      tensorCoreUtilization: true
      flopsEfficiency: true
    gpuMemory:
      memoryBandwidthUtilization: true
      peakMemoryUsage: true
    dataPipeline:
      ioWaitRatio: true
      dataloaderThroughput: true

  analysis:
    compareBaseline: true
    baselineSource: builtin
    recommendations:
      batchSize: true
      dataLoading: true
      mixedPrecision: true
      compilationOptimization: true
      distributedStrategy: true
      gpuTypeRecommendation: true
    severityThreshold: warning

  output:
    storeInJobStatus: true
    emitEvents: true
    prometheusMetrics: true
```

```bash
kubectl apply -f profiler.yaml
```

### 3. Check Results

```bash
# View profiler status
kubectl get gryviatrainingprofilers

# Output:
# NAME          JOBSPROFILED   AVGMFU   EFFICIENCYSCORE   AGE
# my-profiler   12             34.5     67.8              2h

# View detailed recommendations
kubectl describe gryviatrainingprofiler my-profiler

# View recommendation events
kubectl get events --field-selector reason=ProfilerBatchSize
kubectl get events --field-selector reason=ProfilerMixedPrecision
kubectl get events --field-selector reason=ProfilingComplete
```

## How MFU is Calculated

Model FLOPS Utilization (MFU) measures what fraction of a GPU's theoretical peak compute is actually used by a training workload:

```
MFU = (Achieved TFLOPS / Peak TFLOPS) * 100
```

### Peak TFLOPS by GPU Type (FP16 Tensor Core)

| GPU  | Peak TFLOPS | Good MFU | Great MFU |
|------|-------------|----------|-----------|
| H100 | 989         | >30%     | >45%      |
| A100 | 312         | >25%     | >40%      |
| V100 | 125         | >20%     | >35%      |
| T4   | 65          | >15%     | >30%      |

### What Affects MFU

- **Batch size**: Larger batches amortize kernel launch overhead
- **Mixed precision**: FP16/BF16 enables tensor core utilization
- **Data pipeline**: GPU idle time waiting for data directly reduces MFU
- **Communication overhead**: AllReduce in distributed training consumes GPU cycles
- **Model architecture**: Some operations (e.g., normalization) do not map well to tensor cores

### Interpreting MFU Values

- **>50%**: Excellent. Workload is well-optimized.
- **30-50%**: Good. Typical for well-tuned LLM training.
- **15-30%**: Fair. Common for vision models and smaller workloads. Room for improvement.
- **<15%**: Poor. Significant optimization opportunities exist.

## Efficiency Score

The overall efficiency score (0-100) is a weighted composite of:

| Component              | Weight | What it Measures                          |
|------------------------|--------|-------------------------------------------|
| SM Utilization         | 25%    | Streaming multiprocessor activity         |
| Tensor Core Util       | 25%    | Tensor core activity (FP16/BF16 ops)      |
| Memory Bandwidth       | 15%    | HBM bandwidth utilization                 |
| Data Pipeline          | 15%    | How well the data pipeline feeds the GPU  |
| Communication          | 10%    | Distributed training efficiency           |
| Memory Usage           | 10%    | GPU memory utilization (60-90% is optimal)|

## Recommendation Categories

### Batch Size

Triggered when GPU memory is under-utilized alongside low SM utilization, indicating the batch size can be increased to improve throughput.

**Critical example**: "GPU memory only 35% utilized (52GB free of 80GB) with SM utilization at 42%. Increase batch size to improve throughput."

### Data Loading

Triggered when the GPU spends significant time idle waiting for data from the CPU/storage pipeline.

**Critical example**: "GPU idle 45% of the time waiting for data. Data loading is the bottleneck."

**Fixes**:
- Increase `num_workers` in DataLoader
- Enable `pin_memory=True`
- Use `persistent_workers=True`
- Switch to a faster storage backend (e.g., VAST Data NFS)
- Pre-process and cache data on local NVMe

### Mixed Precision

Triggered when tensor cores are not being utilized because the workload runs in FP32.

**Critical example**: "Tensor cores are barely used (3.2% utilization). Enable mixed precision training (AMP) to leverage FP16/BF16 tensor core acceleration on A100."

### Compilation Optimization

Triggered when torch.compile or XLA compilation could improve kernel fusion and reduce launch overhead.

**Info example**: "SM utilization at 65% without graph compilation. torch.compile() can fuse operations and reduce kernel launch overhead."

### Distributed Strategy

Covers NCCL bandwidth, AllReduce overhead, and compute-communication overlap for multi-GPU/multi-node training.

**Critical example**: "AllReduce takes 55% of step time. Communication is dominating training. Consider gradient compression or pipeline parallel strategy."

### GPU Type Recommendation

Suggests alternative GPU types when the current GPU is significantly under-utilized relative to its cost.

**Warning example**: "MFU is only 12% on H100. If the workload is memory-bandwidth-bound, A100 may deliver similar performance at lower cost."

## Profiling Modes

### Auto (Default)

Continuously profiles all matching jobs based on the job selector. Respects warmup steps and cooldown intervals.

```yaml
spec:
  target:
    type: auto
    jobSelector:
      gryvia.io/type: training
```

### Job Reference

Targets a single specific job by name.

```yaml
spec:
  target:
    type: job-ref
    jobRef: my-training-job
```

### On-Demand

Profiles matching jobs only when the GryviaTrainingProfiler CR is created or updated. Useful for one-shot debugging sessions.

```yaml
spec:
  target:
    type: on-demand
    jobSelector:
      team: ml-research
```

## Metrics Collection

GPU metrics are collected from pod annotations that must be set by your own metrics agent (nothing in this repo sets them). The following annotations are read:

| Annotation                               | Description                          |
|------------------------------------------|--------------------------------------|
| `gryvia.io/gpu-sm-utilization`       | SM utilization (0-100)               |
| `gryvia.io/gpu-tensor-utilization`   | Tensor core utilization (0-100)      |
| `gryvia.io/gpu-tflops`              | Achieved TFLOPS                      |
| `gryvia.io/gpu-memory-bw-utilization`| Memory bandwidth utilization (0-100) |
| `gryvia.io/gpu-peak-memory-gb`       | Peak GPU memory usage (GB)           |
| `gryvia.io/gpu-total-memory-gb`      | Total GPU memory (GB)                |
| `gryvia.io/gpu-io-wait-ratio`        | IO wait ratio (0-1)                  |
| `gryvia.io/dataloader-throughput`    | Dataloader throughput (samples/sec)  |
| `gryvia.io/nccl-bandwidth-gbps`      | NCCL bandwidth (GB/s)               |
| `gryvia.io/allreduce-time-fraction`  | AllReduce time fraction (0-1)        |
| `gryvia.io/compute-comm-overlap`     | Compute-comm overlap (0-1)           |
| `gryvia.io/mixed-precision`          | "true" if AMP is enabled             |
| `gryvia.io/torch-compile`            | "true" if torch.compile is used      |

## Severity Levels

- **critical**: Major performance issue. Action strongly recommended. Expected impact >30%.
- **warning**: Significant optimization opportunity. Expected impact 10-30%.
- **info**: Minor improvement possible. Expected impact <10%.

The `severityThreshold` in the spec controls the minimum severity shown:

```yaml
spec:
  analysis:
    severityThreshold: warning  # Only show warning + critical
```

## Output Options

### Store in Job Status

When `storeInJobStatus: true`, the profiler updates the `gpuUtilization` field in the GryviaAIJob's `.status.metrics` with the efficiency score.

### Emit Events

When `emitEvents: true`, Kubernetes events are created for each recommendation:

```
Events:
  Type     Reason                  Message
  ----     ------                  -------
  Warning  ProfilerBatchSize       Job resnet-train: [critical] GPU memory only 35% utilized...
  Warning  ProfilerMixedPrecision  Job resnet-train: [critical] Tensor cores barely used...
  Normal   ProfilingComplete       Job resnet-train profiled: MFU=22.3%, Efficiency=45.2, 3 recs
```

### Prometheus Metrics

When `prometheusMetrics: true`, the following metrics are intended to be exposed (planned: the operator does not currently register these series):

```
gryvia_profiler_mfu{job="resnet-train", gpu_type="A100"} 22.3
gryvia_profiler_efficiency{job="resnet-train"} 45.2
gryvia_profiler_recommendations_total{severity="critical"} 2
gryvia_profiler_recommendations_total{severity="warning"} 5
gryvia_profiler_jobs_profiled_total 12
```

## Examples

See `examples/training/profiler-example.yaml` for complete examples including:

- Automatic cluster-wide profiling
- Single-job profiling
- On-demand debugging profiler
- Training job with profiling annotations

## Troubleshooting

### No Jobs Being Profiled

```bash
# Check that jobs match the selector
kubectl get gryviaaijobs -l gryvia.io/type=training

# Check profiler status
kubectl describe gryviatrainingprofiler my-profiler

# Verify jobs are in Running phase
kubectl get gryviaaijobs -o custom-columns=NAME:.metadata.name,PHASE:.status.phase
```

### Missing Metrics

If metrics show as 0, verify that pod annotations are being set:

```bash
kubectl get pods -l gryvia.io/job=my-job -o jsonpath='{.items[0].metadata.annotations}'
```

Your own metrics agent must set the `gryvia.io/gpu-*` annotations on training pods; none ships with Gryvia.

### Recommendations Not Appearing

Check the severity threshold. If set to `critical`, only critical issues will appear:

```yaml
spec:
  analysis:
    severityThreshold: info  # Show all recommendations
```

## Support

- Issues: https://github.com/zyvorai/gryvia/issues
- Discussions: https://github.com/zyvorai/gryvia/discussions
