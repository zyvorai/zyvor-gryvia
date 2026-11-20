# Training Time Machine

Navigate training history, fork experiments from any checkpoint, and manage checkpoint lifecycle.

> **Status: partly implemented (ai-operator controller registered).** Forking works at the API level: the
> controller creates a new `GryviaAIJob` from the source job's spec with the requested overrides. The
> checkpoint timeline is derived from the source job's reported `status.metrics` (step/epoch), not by
> reading checkpoint files, and retention (`keepEveryNth`, `keepBest`, `maxStorageGi`) is only
> evaluated and logged: the controller does not delete anything (the code marks file deletion as future
> work). Storage totals and lineage graphs are bookkeeping in `status`. Whether a fork actually resumes
> from the checkpoint depends on your training code honouring the injected step/env; unverified on real
> training runs.

## Overview

GryviaTrainingTimeMachine provides training experiment management that:

- **Checkpoint Timeline**: Indexes all checkpoints with associated metrics
- **Fork from Any Point**: Create new training jobs from any checkpoint with modified hyperparameters
- **Retention Policies**: Automatically manage checkpoint storage with configurable retention rules
- **Lineage Tracking**: Track the full lineage graph of forked experiments
- **Storage Management**: Monitor and control checkpoint storage usage

## How It Works

### Checkpoint Timeline

The time machine watches a source GryviaAIJob and indexes its checkpoints as they are created. For each checkpoint, it records:

- Training step and epoch
- Metrics (loss, accuracy, learning rate, etc.)
- Timestamp
- Storage location

This creates a searchable timeline of the training run.

### Forking

The key feature: create new training jobs from any point in the timeline. A fork:

1. Selects a checkpoint (by step, epoch, or best metric)
2. Creates a new GryviaAIJob that resumes from that checkpoint
3. Applies overrides (environment variables, arguments)
4. Tracks the forked job's progress

This enables rapid experimentation: "What if I had used a lower learning rate at step 5000?"

### Retention Policies

Checkpoints consume significant storage. The retention policy controls what to keep:

- **keepAll**: Keep everything (for critical experiments)
- **keepEveryNth**: Keep every Nth checkpoint (e.g., every 5th)
- **keepBest**: Keep the N best checkpoints by metric
- **keepMilestones**: Always keep specific steps/epochs
- **maxStorageGi**: Hard storage limit

### Lineage Graph

The time machine tracks the relationship between the source job and all forks:

```
llama-70b-finetune (source)
  |-- step 5000 --> fork: lower-lr (loss: 0.42 -> 0.38)
  |-- epoch 10 ---> fork: longer-training (loss: 0.45 -> 0.35)
  |-- best(val_loss) -> fork: augmented (loss: 0.40 -> 0.37)
```

## Usage

### Basic Time Machine

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaTrainingTimeMachine
metadata:
  name: my-experiment-tm
  namespace: ml-team
spec:
  sourceJob: llama-70b-finetune

  timeline:
    enabled: true
    indexCheckpoints: true
    metadataPerCheckpoint:
      - name: loss
        source: metric
      - name: val_loss
        source: metric
      - name: learning_rate
        source: metric

  retention:
    keepEveryNth: 5
    keepBest: 3
    maxStorageGi: 500
```

### Forking from Best Checkpoint

```yaml
spec:
  sourceJob: llama-70b-finetune
  forks:
    - name: lower-lr-experiment
      fromCheckpoint:
        metric:
          name: val_loss
          selector: min    # Fork from the checkpoint with lowest val_loss
      overrides:
        env:
          - name: LEARNING_RATE
            value: "1e-5"
          - name: WARMUP_STEPS
            value: "0"
        args:
          - "--resume-from-checkpoint"
```

### Forking from Specific Step

```yaml
spec:
  forks:
    - name: different-augmentation
      fromCheckpoint:
        step: 50000
      overrides:
        env:
          - name: AUGMENTATION
            value: "heavy"
      newJobName: llama-augmented-v2
```

### Forking from Specific Epoch

```yaml
spec:
  forks:
    - name: extended-training
      fromCheckpoint:
        epoch: 10
      overrides:
        env:
          - name: MAX_EPOCHS
            value: "50"
```

### Retention Policy Examples

Keep everything (for important experiments):

```yaml
spec:
  retention:
    keepAll: true
```

Balanced retention:

```yaml
spec:
  retention:
    keepEveryNth: 10      # Keep every 10th checkpoint
    keepBest: 5           # Plus the 5 best by metric
    keepMilestones:       # Plus specific milestones
      - 100
      - 1000
      - 5000
    maxStorageGi: 200     # Hard limit at 200 GiB
```

Minimal retention:

```yaml
spec:
  retention:
    keepBest: 1           # Only keep the absolute best
    maxStorageGi: 50
```

## Status

The time machine reports comprehensive status:

```yaml
status:
  checkpointTimeline:
    totalCheckpoints: 150
    bestCheckpoint:
      step: 45000
      epoch: 15
      metricName: val_loss
      metricValue: 0.342
    latestCheckpoint:
      step: 50000
      epoch: 17
      timestamp: "2026-04-10T10:00:00Z"

  forks:
    - name: lower-lr-experiment
      sourceStep: 45000
      forkedJob: llama-70b-finetune-fork-lower-lr-experiment
      status: Running
      currentMetric: 0.338
    - name: extended-training
      sourceStep: 30000
      forkedJob: llama-extended-v2
      status: Succeeded
      currentMetric: 0.310

  storageUsed: "45Gi"

  conditions:
    - type: SourceJobFound
      status: "True"
      reason: SourceJobFound
    - type: TimelineIndexed
      status: "True"
      reason: TimelineIndexed
    - type: RetentionEnforced
      status: "True"
      reason: RetentionEnforced
    - type: ForksProcessed
      status: "True"
      reason: ForksProcessed
```

## Fork Environment Variables

Forked jobs automatically receive these environment variables:

| Variable | Description |
|----------|-------------|
| `GRYVIA_RESUME_FROM_CHECKPOINT` | Set to `true` to indicate the job should resume |
| `GRYVIA_CHECKPOINT_STEP` | The training step to resume from |
| `GRYVIA_FORK_NAME` | The name of the fork |

Training code should check these variables to resume correctly:

```python
import os

if os.getenv("GRYVIA_RESUME_FROM_CHECKPOINT") == "true":
    step = int(os.getenv("GRYVIA_CHECKPOINT_STEP", "0"))
    checkpoint_path = f"/checkpoints/step-{step}"
    model.load_state_dict(torch.load(f"{checkpoint_path}/model.pt"))
    optimizer.load_state_dict(torch.load(f"{checkpoint_path}/optimizer.pt"))
    print(f"Resumed from checkpoint at step {step}")
```

## CLI Usage

```bash
# List time machines
kubectl get gryviatrainingtimemachines -n ml-team

# View time machine status
kubectl describe fttm my-experiment-tm -n ml-team

# Check fork status
kubectl get fttm my-experiment-tm -n ml-team -o jsonpath='{.status.forks}'

# View checkpoint timeline
kubectl get fttm my-experiment-tm -n ml-team -o jsonpath='{.status.checkpointTimeline}'
```

## Metadata Sources

Checkpoint metadata can be extracted from three sources:

### Metric Source

Extracts values from the source job's `status.metrics` field:

```yaml
metadataPerCheckpoint:
  - name: loss
    source: metric
  - name: val_loss
    source: metric
```

### Log Source

Extracts values by parsing pod logs (pattern matching):

```yaml
metadataPerCheckpoint:
  - name: perplexity
    source: log
    path: "perplexity: ([0-9.]+)"
```

### File Source

Reads values from files on the checkpoint PVC:

```yaml
metadataPerCheckpoint:
  - name: config
    source: file
    path: "/checkpoints/training_config.json"
```

## Best Practices

### 1. Always Track Key Metrics

Track at minimum `loss` and `val_loss` to enable metric-based forking.

### 2. Set Storage Limits

Always set `maxStorageGi` to prevent unbounded storage growth, especially for large models.

### 3. Use Meaningful Fork Names

Fork names appear in job names and labels. Use descriptive names like `lower-lr-from-best` rather than `fork-1`.

### 4. Integrate Checkpoint Resume in Training Code

Ensure your training code checks `GRYVIA_RESUME_FROM_CHECKPOINT` and handles checkpoint loading.

### 5. Keep Milestone Checkpoints

Use `keepMilestones` to preserve checkpoints at key training phases (warm-up completion, learning rate decay points).

## Architecture

```
GryviaAIJob (source) --> Time Machine Controller
                              |
                    [Checkpoint Indexer]
                       /      |       \
                Timeline  Retention  Fork Handler
                 Index     Policy        |
                   |          |     [Create GryviaAIJob]
                   v          v          |
                Status    Prune       Forked Jobs
                          Files          |
                                    Track Status
```

## Support

- Issues: https://github.com/zyvorai/gryvia/issues
- CRD Reference: `crds/gryvia.io_gryviatrainingtimemachines.yaml`
- Example: `examples/training/timemachine-example.yaml`
