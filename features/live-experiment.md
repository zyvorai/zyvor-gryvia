# Live Experiment Comparison

Real-time multi-experiment comparison system with automatic early stopping for GPU training jobs.

> **Status: implemented for the log-scraping path (ai-operator controller registered), unverified on real training runs.**
> Only `metricSource.type: log-pattern` is implemented; the `prometheus` source described below has no
> code behind it. Early stopping applies a median stopping rule; the `bayesian` and `percentile`
> methods listed below are accepted by the CRD but not implemented as distinct algorithms. `costSaved` is
> `gpuHoursSaved` multiplied by a hard-coded $3.50 per GPU-hour, not real pricing.

## Overview

GryviaLiveExperiment enables running multiple training jobs simultaneously and comparing them in real time. The system:

- **Live Leaderboard**: Continuously ranks jobs by configurable metrics
- **Early Stopping**: Automatically terminates underperforming jobs to save GPU hours
- **Anomaly Detection**: Detects loss plateau, divergence, and gradient explosion
- **Cost Tracking**: Reports GPU hours and dollars saved through early termination

## How It Works

1. Define a `GryviaLiveExperiment` CR referencing two or more `GryviaAIJob` resources
2. Configure the primary metric (e.g., `loss`) and optimization direction (`minimize`/`maximize`)
3. The controller scrapes pod logs at 30-second intervals, extracting metric values via regex
4. Jobs are ranked by a composite score (primary metric + weighted secondary metrics)
5. When early termination is enabled, underperforming jobs are stopped automatically
6. The experiment status tracks a live leaderboard, GPU hours saved, and anomalies

## Creating an Experiment

### Basic Example

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaLiveExperiment
metadata:
  name: lr-sweep
spec:
  description: "Learning rate sweep for ResNet-50"
  jobs:
    - name: lr-0.001
      jobRef: resnet-lr-001
    - name: lr-0.01
      jobRef: resnet-lr-01
    - name: lr-0.1
      jobRef: resnet-lr-1
  comparison:
    primaryMetric: loss
    direction: minimize
    metricSource:
      type: log-pattern
      patterns:
        loss: 'loss[=:]\s*([\d.]+)'
        accuracy: 'accuracy[=:]\s*([\d.]+)'
```

### With Early Stopping

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaLiveExperiment
metadata:
  name: architecture-search
spec:
  description: "Architecture search with early stopping"
  jobs:
    - name: transformer-small
      jobRef: arch-transformer-small
    - name: transformer-large
      jobRef: arch-transformer-large
    - name: lstm-baseline
      jobRef: arch-lstm-baseline
  comparison:
    primaryMetric: loss
    direction: minimize
    secondaryMetrics:
      - name: accuracy
        weight: 0.3
    metricSource:
      type: log-pattern
      patterns:
        loss: 'loss[=:]\s*([\d.]+)'
        accuracy: 'accuracy[=:]\s*([\d.]+)'
  strategy:
    earlyTermination:
      enabled: true
      method: median
      minRunFraction: 0.25
      confidenceLevel: 0.95
      graceEpochs: 3
    anomalyDetection:
      lossPlateauDetection: true
      lossDivergenceDetection: true
      gradientExplosionDetection: true
    intervention:
      enabled: true
      actions:
        - terminate
        - notify
  notifications:
    realTimeLeaderboard: true
    onNewLeader: true
    onTermination: true
    channel: "slack://#ml-experiments"
```

## Metric Collection

### Log Pattern Source

The default metric source scrapes pod logs using regex patterns. Each pattern must contain exactly one capture group matching a numeric value.

Common patterns:

| Metric | Pattern | Matches |
|--------|---------|---------|
| loss | `loss[=:]\s*([\d.]+)` | `loss: 0.342`, `loss=0.342` |
| accuracy | `accuracy[=:]\s*([\d.]+)` | `accuracy: 92.5` |
| perplexity | `ppl[=:]\s*([\d.]+)` | `ppl: 15.3` |
| learning_rate | `lr[=:]\s*([\d.eE+-]+)` | `lr: 1e-4` |

If no patterns are configured, the controller uses sensible defaults for `loss` and `accuracy`.

### Prometheus Source

**Not implemented.** Setting `metricSource.type: prometheus` is intended to query a Prometheus server for metrics instead of scraping logs. The `patterns` map is used to map metric names to PromQL queries.

## Early Termination

### Methods

- **median**: Terminates jobs performing worse than the median of all running jobs. Good default for hyperparameter sweeps.
- **bayesian**: Uses Bayesian optimization to predict which jobs are unlikely to improve. Best for expensive experiments.
- **percentile**: Terminates jobs below a configurable percentile. Aggressive early stopping.

### Parameters

| Parameter | Default | Description |
|-----------|---------|-------------|
| `minRunFraction` | 0.25 | Minimum fraction of the run before termination is allowed |
| `confidenceLevel` | 0.95 | Statistical confidence required before terminating |
| `graceEpochs` | 3 | Number of epochs to wait before considering termination |

## Anomaly Detection

The controller monitors training metrics for three types of anomalies:

- **Loss Plateau**: Loss changes less than 0.1% over the last 10 data points
- **Loss Divergence**: Loss monotonically increases for 5 consecutive data points
- **Gradient Explosion**: NaN or Inf values detected in loss

Detected anomalies increment the `anomaliesDetected` counter in the experiment status.

## Status and Leaderboard

View the live leaderboard:

```bash
kubectl get gryvialiveexperiment lr-sweep

# Output:
# NAME       PHASE     LEADER    GPUHOURSSAVED   COSTSAVED   AGE
# lr-sweep   Running   lr-0.01   12.5            43.75       2h
```

Detailed status:

```bash
kubectl get gryvialiveexperiment lr-sweep -o yaml
```

```yaml
status:
  phase: Running
  startTime: "2024-01-21T10:00:00Z"
  leaderboard:
    - rank: 1
      job: lr-0.01
      primaryMetricValue: 0.152
      status: Running
    - rank: 2
      job: lr-0.001
      primaryMetricValue: 0.287
      status: Running
    - rank: 3
      job: lr-0.1
      primaryMetricValue: 1.543
      status: TerminatedEarly
      reason: "loss value 1.5430 exceeds median threshold 0.2200"
  gpuHoursSaved: 12.5
  costSaved: 43.75
  anomaliesDetected: 1
```

## Cost Savings

The controller estimates GPU hours saved by calculating the remaining run time that terminated jobs would have consumed. Cost savings are computed using a configurable per-GPU-hour rate (default: $3.50/hour for H100-class GPUs).

## Best Practices

### 1. Use Meaningful Job Names

```yaml
jobs:
  - name: adamw-lr1e-3      # Descriptive
    jobRef: exp-job-001
  - name: sgd-lr1e-2         # Descriptive
    jobRef: exp-job-002
```

### 2. Set Appropriate Grace Periods

For large models that take time to converge, increase `graceEpochs` and `minRunFraction`:

```yaml
strategy:
  earlyTermination:
    enabled: true
    graceEpochs: 10
    minRunFraction: 0.4
```

### 3. Enable Anomaly Detection for Long Runs

```yaml
strategy:
  anomalyDetection:
    lossPlateauDetection: true
    lossDivergenceDetection: true
    gradientExplosionDetection: true
```

### 4. Use Secondary Metrics for Nuanced Ranking

```yaml
comparison:
  primaryMetric: loss
  direction: minimize
  secondaryMetrics:
    - name: accuracy
      weight: 0.3
    - name: f1_score
      weight: 0.2
```

## Troubleshooting

### No Metrics Appearing

1. Verify your log patterns match the training output format
2. Check that pods are in Running state: `kubectl get pods -l gryvia.io/job=<jobRef>`
3. Inspect pod logs manually: `kubectl logs <pod-name>`

### Early Termination Too Aggressive

- Increase `minRunFraction` to require more training before termination
- Increase `graceEpochs` to give jobs more time to converge
- Increase `confidenceLevel` to require stronger evidence

### Early Termination Not Triggering

- Verify `earlyTermination.enabled: true`
- Check that at least 2 jobs have metric data
- Ensure jobs have run past `minRunFraction` of the total

## Examples

See `examples/training/live-experiment-example.yaml` for a complete working example.

## Support

- Issues: https://github.com/zyvorai/gryvia/issues
- Discussions: https://github.com/zyvorai/gryvia/discussions
