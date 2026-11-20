# GPU Memory Optimizer

Proactive GPU memory management with OOM prevention, right-sizing recommendations, and inference packing.

> **Status: partly implemented (gpu-operator controller registered); acts as advisor only.**
> The controller reads per-GPU memory from `GryviaGpuNode.status.gpuStatus` (populated by scraping
> the DCGM exporter on each node), projects growth, and writes OOM predictions and right-sizing
> recommendations to the optimizer's status. Not implemented: the `Auto-Mitigate` action only logs
> which mitigations it would apply (the code notes that injecting env vars and signalling the
> framework is not done), `evict` only logs a recommendation, no Kubernetes Events are emitted (so
> the `kubectl get events --field-selector reason=OomPredicted` examples below will return
> nothing), and `inferencePacking` is in the CRD but the controller has no packing logic (no
> co-location, MPS or MIG isolation is performed). Unverified on real GPU nodes.

## Overview

GryviaGpuMemoryOptimizer provides intelligent GPU memory management that:

- **OOM Prevention**: Profiles memory growth and predicts OOM before it happens
- **Right-Sizing**: Recommends GPU type downgrades or count reductions based on actual usage
- **Inference Packing**: Co-locates inference workloads on shared GPUs to maximize utilization
- **Memory Efficiency Tracking**: Reports aggregate memory utilization metrics across the cluster

## How It Works

### OOM Prevention

The optimizer continuously monitors GPU memory usage from GryviaGpuNode status reports. It collects memory usage samples over time and uses configurable projection methods (linear, exponential, or polynomial) to predict when a GPU will run out of memory.

When OOM is predicted, the optimizer can:

1. **Warn**: Emit a Kubernetes event and log a warning
2. **Auto-Mitigate**: Inject environment variables to enable gradient checkpointing, mixed precision, or reduce batch size
3. **Evict**: Recommend evicting the workload before OOM occurs

### Memory Growth Profiling

The profiler collects memory usage data points at each reconciliation cycle. After collecting the configured number of `profileSteps`, it projects future memory usage using the selected method:

- **Linear**: Fits a line through memory samples. Best for steady growth patterns.
- **Exponential**: Fits an exponential curve. Best for accelerating growth.
- **Polynomial**: Fits a quadratic curve. Best for complex patterns with varying growth rates.

Each projection includes a confidence score (R-squared) to indicate how well the model fits the data.

### Right-Sizing Recommendations

The optimizer analyzes memory utilization patterns over a configurable analysis window and generates recommendations:

- **GPU Type Downgrade**: When peak utilization is below 50%, recommend a GPU with less memory (e.g., A100-40G instead of A100-80G)
- **GPU Count Reduction**: When steady-state utilization is below 30%, recommend reducing the number of GPUs

Recommendations respect a minimum headroom percentage to ensure jobs have enough memory for peak usage.

### Inference Packing

For inference workloads, the optimizer can co-locate multiple models on a single GPU:

- Targets a configurable memory utilization percentage
- Checks model compatibility before packing
- Supports GPU isolation via MPS (Multi-Process Service) or MIG (Multi-Instance GPU)

## Scope Types

The optimizer can operate at three levels:

- **cluster**: Monitors all GPU nodes in the cluster
- **namespace**: Monitors GPU nodes associated with a specific namespace
- **job-selector**: Monitors GPU nodes matching specific label selectors

## Usage

### Cluster-Wide Memory Optimizer

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaGpuMemoryOptimizer
metadata:
  name: cluster-memory-optimizer
spec:
  scope:
    type: cluster

  oomPrevention:
    enabled: true
    memoryGrowthProfiling:
      profileSteps: 20
      projectionMethod: linear
    preemptiveAction:
      type: auto-mitigate
      warningThresholdPercent: 85
      autoEnableGradientCheckpointing: true
      autoEnableMixedPrecision: true

  rightSizing:
    enabled: true
    analysisWindow: "24h"
    recommendations:
      gpuTypeDowngrade: true
      gpuCountReduction: true
    minimumGpuMemoryHeadroom: 15
```

### Namespace-Scoped with Inference Packing

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaGpuMemoryOptimizer
metadata:
  name: inference-optimizer
spec:
  scope:
    type: namespace
    namespace: inference-prod

  oomPrevention:
    enabled: true
    preemptiveAction:
      type: warn
      warningThresholdPercent: 90

  inferencePacking:
    enabled: true
    memoryUtilizationTarget: 80
    compatibilityCheck: true
    isolationLevel: mps
```

### Job-Specific Optimizer

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaGpuMemoryOptimizer
metadata:
  name: training-optimizer
spec:
  scope:
    type: job-selector
    jobSelector:
      matchLabels:
        gryvia.io/type: training

  oomPrevention:
    enabled: true
    memoryGrowthProfiling:
      profileSteps: 10
      projectionMethod: exponential
    preemptiveAction:
      type: auto-mitigate
      warningThresholdPercent: 80
      autoReduceBatchSize: true
```

## Status

The optimizer reports the following status fields:

```yaml
status:
  jobsAnalyzed: 42
  oomPrevented: 7
  rightSizingRecommendations: 12
  memoryEfficiency:
    avgPeakUtilization: 72.5
    avgSteadyStateUtilization: 58.3
  lastAnalysisTime: "2026-04-10T10:30:00Z"
  conditions:
    - type: AnalysisRunning
      status: "True"
      reason: AnalysisActive
    - type: OomMonitoringActive
      status: "True"
      reason: OomMonitoringActive
    - type: RightSizingActive
      status: "True"
      reason: RightSizingActive
```

## CLI Usage

```bash
# List memory optimizers
kubectl get gryviagpumemoryoptimizers

# View optimizer status
kubectl describe gryviagpumemoryoptimizer cluster-memory-optimizer

# Check events for OOM warnings
kubectl get events --field-selector reason=OomPredicted

# View right-sizing recommendations
kubectl get events --field-selector reason=RightSizingRecommendation
```

## Projection Methods

### Linear Projection

Best for workloads with constant memory growth:

```
Memory(t) = slope * t + intercept
```

Use when memory grows at a fixed rate (e.g., accumulating gradients, growing buffers).

### Exponential Projection

Best for workloads with accelerating memory growth:

```
Memory(t) = a * e^(b*t)
```

Use when memory growth accelerates over time (e.g., dynamic computation graphs).

### Polynomial Projection

Best for complex memory patterns:

```
Memory(t) = a + b*t + c*t^2
```

Use when memory growth varies over different training phases.

## Best Practices

### 1. Start with Warning Mode

Begin with `preemptiveAction.type: warn` to understand memory patterns before enabling auto-mitigation.

### 2. Tune Profile Steps

Set `profileSteps` based on your reconciliation interval:
- 60s interval with 20 steps = 20 minutes of profiling before prediction
- For fast-growing workloads, use fewer steps (e.g., 10)

### 3. Set Appropriate Headroom

The `minimumGpuMemoryHeadroom` should account for:
- Memory spikes during gradient accumulation
- Framework overhead (CUDA context, cuDNN workspace)
- A typical value of 15-20% works for most training workloads

### 4. Use Inference Packing Carefully

- Always enable `compatibilityCheck` when packing inference models
- Start with MPS isolation (`isolationLevel: mps`) for basic isolation
- Use MIG isolation (`isolationLevel: mig`) for strict performance guarantees

## Architecture

```
GryviaGpuNode (status.gpuStatus) --> Memory Optimizer Controller
                                         |
                                    [Predictor]
                                     /    |    \
                              OOM    Right  Inference
                           Prevention Sizing Packing
                              |        |        |
                           Actions  Recs    Scheduling
```

## Support

- Issues: https://github.com/zyvorai/gryvia/issues
- CRD Reference: `crds/gryvia.io_gryviagpumemoryoptimizers.yaml` (generated from the Go types)
- Example: `examples/training/memory-optimizer-example.yaml`
