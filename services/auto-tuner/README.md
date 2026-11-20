# Auto-Tuning Framework

> **Status: design sketch, not implemented in this repo.** This directory holds only this README and a Dockerfile that references a `requirements.txt` that does not exist; there is no source code. `GryviaAutoTuner` itself is run by the ai-operator (each trial is a child `GryviaAIJob`; see [ML controllers](../../docs/ml-controllers.md)); this directory's separate service is not part of that. The `gryvia` `AutoTuner` Python import shown below does not exist in the SDK. Feature lists and any speed-up or cost numbers are design targets, not measurements.

Automatically optimize hyperparameters, resource allocation, and training configurations.

## Features

- **Hyperparameter Optimization**: Bayesian optimization, grid search, random search
- **Resource Auto-Tuning**: Optimize batch size, learning rate, GPU count
- **Configuration Search**: Find optimal NCCL, distributed training settings
- **Cost-Performance Trade-offs**: Balance cost vs training speed
- **Automatic Recommendations**: ML-driven suggestions

## Architecture

```
┌──────────────────────────────────────────────┐
│         Auto-Tuner Controller                 │
│  ┌────────────┐  ┌────────────┐  ┌─────────┐│
│  │   Optuna   │  │   Ray      │  │ Custom  ││
│  │   Backend  │  │   Tune     │  │ Tuner   ││
│  └────────────┘  └────────────┘  └─────────┘│
└────────────┬─────────────────────────────────┘
             │
      ┌──────┴──────┐
      │             │
┌─────▼────┐  ┌────▼─────┐
│ Training │  │ Training │
│ Trial 1  │  │ Trial 2  │
└──────────┘  └──────────┘
```

## Quick Start

### 1. Enable Auto-Tuning

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAutoTuner
metadata:
  name: llama-tuning
  namespace: default
spec:
  objective: minimize
  metric: validation_loss

  # Hyperparameter search space
  hyperparameters:
    - name: learning_rate
      type: float
      range: [1e-5, 1e-3]
      scale: log

    - name: batch_size
      type: int
      values: [16, 32, 64, 128]

    - name: weight_decay
      type: float
      range: [0.0, 0.1]

    - name: warmup_steps
      type: int
      range: [100, 1000]

  # Resource optimization
  resources:
    gpuTypes: [A100-80G, A100-40G, T4]
    gpuCounts: [1, 2, 4, 8]

  # Search configuration
  search:
    algorithm: bayesian  # bayesian, random, grid
    maxTrials: 50
    maxParallelTrials: 4
    earlyStoppingRounds: 5

  # Training job template
  jobTemplate:
    framework: pytorch
    image: nvcr.io/nvidia/pytorch:24.01-py3
    command:
      - python
      - train.py
    volumeMounts:
      - name: data
        mountPath: /data
```

### 2. Submit Tuning Job

```bash
# Submit auto-tuning job
kubectl apply -f auto-tuner.yaml

# Monitor progress
kfctl tune status llama-tuning

# View trials
kfctl tune trials llama-tuning

# Get best configuration
kfctl tune best llama-tuning
```

## Search Algorithms

### Bayesian Optimization (Recommended)

Uses Gaussian Processes to intelligently explore search space.

```yaml
search:
  algorithm: bayesian
  acquisitionFunction: expected_improvement
  maxTrials: 50
  explorationRatio: 0.2  # 20% exploration, 80% exploitation
```

**Pros:** Sample efficient, finds good configs quickly
**Cons:** Slower for high-dimensional spaces

### Random Search

Randomly samples from search space.

```yaml
search:
  algorithm: random
  maxTrials: 100
```

**Pros:** Simple, parallelizable, good baseline
**Cons:** Less efficient than Bayesian

### Grid Search

Exhaustively searches all combinations.

```yaml
search:
  algorithm: grid
  # Automatically calculates all combinations
```

**Pros:** Comprehensive, guaranteed to find best
**Cons:** Expensive for large spaces

### Hyperband

Multi-armed bandit approach with early stopping.

```yaml
search:
  algorithm: hyperband
  maxEpochsPerTrial: 100
  reductionFactor: 3
  minResourcePercent: 0.1
```

**Pros:** Efficient, good for neural architecture search
**Cons:** Requires progressive training

## Hyperparameter Types

### Continuous (Float)

```yaml
- name: learning_rate
  type: float
  range: [1e-5, 1e-3]
  scale: log  # or linear
```

### Discrete (Int)

```yaml
- name: num_layers
  type: int
  range: [6, 24]
  step: 2  # Only even numbers
```

### Categorical

```yaml
- name: optimizer
  type: categorical
  values: [adam, adamw, sgd, adagrad]
```

### Conditional

```yaml
- name: optimizer
  type: categorical
  values: [adam, sgd]

- name: momentum
  type: float
  range: [0.9, 0.99]
  condition: optimizer == 'sgd'
```

## Resource Optimization

### Auto-Batch Sizing

Automatically find optimal batch size:

```yaml
resources:
  autoBatchSize:
    enabled: true
    startBatchSize: 32
    maxBatchSize: 512
    memoryUtilizationTarget: 0.90  # Use 90% GPU memory

  gpuType: A100-80G
  gpuCount: 8
```

**Process:**
1. Start with initial batch size
2. Gradually increase until GPU memory ~90%
3. Measure throughput at each size
4. Select size with best throughput

### GPU Count Optimization

Find optimal number of GPUs:

```yaml
resources:
  gpuCountSearch:
    enabled: true
    counts: [1, 2, 4, 8, 16]
    costPerformanceWeight: 0.5  # 0=pure performance, 1=pure cost
```

**Metrics:**
- Training speed (samples/sec)
- Cost ($/hour)
- Scaling efficiency
- Time to convergence

### GPU Type Selection

Compare different GPU types:

```yaml
resources:
  gpuTypeComparison:
    enabled: true
    types: [H100, A100-80G, A100-40G]
    budget: 1000  # Max budget per trial
```

## Advanced Features

### Multi-Objective Optimization

Optimize multiple objectives simultaneously:

```yaml
objectives:
  - name: validation_loss
    direction: minimize
    weight: 0.6

  - name: training_time
    direction: minimize
    weight: 0.2

  - name: cost
    direction: minimize
    weight: 0.2
```

**Pareto Front:** Find configurations on Pareto frontier.

### Transfer Learning

Use results from previous tuning runs:

```yaml
search:
  transferLearning:
    enabled: true
    sourceTuners:
      - llama-7b-tuning
      - gpt2-tuning
    warmStartTrials: 10
```

### Distributed Tuning

Scale tuning across multiple clusters:

```yaml
distribution:
  enabled: true
  clusters: [cluster-a, cluster-b, cluster-c]
  trialsPerCluster: auto
```

### Cost-Aware Tuning

Optimize within budget constraints:

```yaml
budget:
  maxTotalCost: 5000  # Max $5000 for entire tuning
  maxCostPerTrial: 100
  stopIfBudgetExceeded: true

  # Prefer cheaper trials early
  costAwareScheduling: true
```

## Integration with Training Code

### Training Script

```python
# train.py
import argparse
import torch
from gryvia import AutoTuner

def train(config):
    # Parse hyperparameters
    lr = config['learning_rate']
    batch_size = config['batch_size']

    # Setup model and optimizer
    model = MyModel()
    optimizer = torch.optim.AdamW(model.parameters(), lr=lr)

    # Training loop
    for epoch in range(num_epochs):
        for batch in dataloader:
            loss = train_step(model, batch, optimizer)

        # Evaluate
        val_loss = evaluate(model, val_loader)

        # Report metrics to tuner
        AutoTuner.report(
            epoch=epoch,
            metrics={
                'validation_loss': val_loss,
                'training_loss': loss,
            }
        )

        # Check if should stop (early stopping)
        if AutoTuner.should_stop():
            break

    # Return final metrics
    return {'validation_loss': val_loss}

if __name__ == '__main__':
    # Get hyperparameters from auto-tuner
    config = AutoTuner.get_config()

    # Train
    result = train(config)

    # Report final result
    AutoTuner.complete(result)
```

### Checkpoint-Based Tuning

For long training runs, use checkpointing:

```python
def train_with_checkpoints(config, checkpoint_dir):
    model = MyModel()

    # Resume from checkpoint if exists
    start_epoch = 0
    if checkpoint_dir and os.path.exists(checkpoint_dir):
        checkpoint = torch.load(checkpoint_dir + '/checkpoint.pt')
        model.load_state_dict(checkpoint['model'])
        start_epoch = checkpoint['epoch']

    # Train
    for epoch in range(start_epoch, num_epochs):
        train_epoch(model)
        val_loss = evaluate(model)

        # Save checkpoint
        if checkpoint_dir:
            torch.save({
                'epoch': epoch,
                'model': model.state_dict(),
                'val_loss': val_loss
            }, checkpoint_dir + '/checkpoint.pt')

        # Report
        AutoTuner.report(epoch=epoch, metrics={'val_loss': val_loss})

        if AutoTuner.should_stop():
            break

    return {'validation_loss': val_loss}
```

## Monitoring

### Live Dashboard

```bash
# Open auto-tuning dashboard
kubectl port-forward -n gryvia-system svc/auto-tuner-ui 8080:80

# View at http://localhost:8080
```

**Dashboard shows:**
- Trial progress (running, completed, failed)
- Hyperparameter importance
- Optimization history
- Best configurations
- Cost breakdown

### Command Line

```bash
# List all tuning jobs
kfctl tune list

# Get status
kfctl tune status llama-tuning

# View trial results
kfctl tune trials llama-tuning --sort-by validation_loss

# Get best config
kfctl tune best llama-tuning

# Export results
kfctl tune export llama-tuning --format json > results.json
```

### Metrics

```prometheus
# Total trials
gryvia_autotuner_trials_total{job="llama-tuning"} 50

# Best score so far
gryvia_autotuner_best_score{job="llama-tuning"} 0.234

# Cost spent
gryvia_autotuner_cost_total{job="llama-tuning"} 2341.50

# Trials per second
gryvia_autotuner_trials_per_second{job="llama-tuning"} 0.15
```

## Example Results

### LLaMA-7B Training Optimization

**Search Space:**
- Learning rate: [1e-5, 1e-3]
- Batch size: [16, 32, 64, 128]
- Weight decay: [0.0, 0.1]
- Warmup steps: [100, 1000]
- GPU count: [1, 2, 4, 8]

**Results (50 trials, 12 hours):**

```
Best Configuration:
  learning_rate: 3.2e-4
  batch_size: 64
  weight_decay: 0.05
  warmup_steps: 500
  gpu_count: 8

Performance:
  Validation Loss: 2.14 (vs 2.47 baseline, 13% improvement)
  Training Time: 18.5 hours (vs 24 hours baseline, 23% faster)
  Cost: $3,552 (vs $4,608 baseline, 23% cheaper)

Top 5 Trials:
  #42: loss=2.14, lr=3.2e-4, bs=64, gpus=8
  #38: loss=2.16, lr=2.8e-4, bs=64, gpus=8
  #31: loss=2.18, lr=3.5e-4, bs=64, gpus=8
  #47: loss=2.19, lr=3.0e-4, bs=128, gpus=8
  #29: loss=2.21, lr=4.0e-4, bs=64, gpus=8
```

### Hyperparameter Importance

```
Feature Importance:
  learning_rate:  45.2%
  batch_size:     28.7%
  warmup_steps:   16.3%
  weight_decay:    9.8%
```

## Best Practices

1. **Start Small**: Begin with 10-20 trials
2. **Use Bayesian**: More efficient than random/grid
3. **Enable Early Stopping**: Save compute on poor configs
4. **Monitor Costs**: Set budget limits
5. **Checkpoint Frequently**: Enable resumption
6. **Use Transfer Learning**: Leverage previous runs
7. **Validate Results**: Re-run best config 3x to verify

## Troubleshooting

### Trials Failing

```bash
# Check trial logs
kubectl logs auto-tuner-trial-42

# Common issues:
# - OOM: Reduce max batch size
# - Divergence: Widen LR search range
# - Timeout: Increase trial timeout
```

### No Improvement

```bash
# Check if search space is appropriate
kfctl tune analyze llama-tuning

# Suggestions:
# - Widen search ranges
# - Add more hyperparameters
# - Increase trial count
# - Check if metric is being reported correctly
```

### High Costs

```bash
# Enable cost-aware tuning
kubectl patch gryviaautotuner llama-tuning -p '{
  "spec": {
    "budget": {
      "maxTotalCost": 1000,
      "costAwareScheduling": true
    }
  }
}'

# Use cheaper GPUs for trials
# Use early stopping aggressively
```

## Advanced Examples

See `examples/auto-tuning/` for:
- Multi-objective optimization
- Neural architecture search
- Distributed hyperparameter tuning
- Cost-performance optimization
- Transfer learning examples

## API Reference

### Python SDK

```python
from gryvia.autotuner import AutoTuner

# Initialize
tuner = AutoTuner(
    name='my-tuning',
    objective='minimize',
    metric='val_loss'
)

# Define search space
tuner.add_hyperparameter('lr', 'float', [1e-5, 1e-3], scale='log')
tuner.add_hyperparameter('batch_size', 'int', [16, 32, 64, 128])

# Run tuning
result = tuner.run(
    train_fn=train,
    max_trials=50,
    max_parallel=4
)

# Get best config
best = tuner.get_best_trial()
print(f"Best config: {best.config}")
print(f"Best score: {best.score}")
```

## Support

- Auto-Tuning Issues: https://github.com/zyvorai/gryvia/issues
- Optimization Help: https://github.com/zyvorai/gryvia/discussions
