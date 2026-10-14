# Job Priority and Preemption

Sophisticated job priority system with preemption support for efficient resource utilization.

> **Status: mostly design. Priority classes and preemption are not running behaviour.**
> The one real exception is the opt-in Kueue integration (`--kueue-integration`, off by default): `spec.priority` then maps to a Kueue
> `WorkloadPriorityClass` and preempts within a queue, victims requeued, no checkpointing. It is unit-tested and its kind e2e
> (real Kueue, CPU pods) passes in CI; never run on GPUs. See [docs/kueue-integration.md](../docs/kueue-integration.md). Everything below is the design.
>
> `GryviaPriority` has a controller (with the ML controllers, on by default) that creates a
> Kubernetes PriorityClass of the same name; a job annotated `gryvia.io/priority-class: <name>`
> gets it on its batch Job pods and Kubernetes does the pod preemption. The named classes
> below (`system-critical` ... `spot`) are not shipped, and quota overrides and SLA guarantees
> are not enforced. `GryviaAIJob` has no `priorityClassName` field: its only
> priority field is the integer `spec.priority` (0-100, higher is more important),
> range-checked by the admission webhook. Jobs are not reordered, and no
> job is preempted by priority unless Kueue is enabled or the opt-in native queue gate
> (`--native-queue-scheduler`, [docs/native-scheduler.md](../docs/native-scheduler.md)) is on; with the gate, a higher
> `spec.priority` job evicts lower-priority jobs annotated `gryvia.io/native-preemptible: "true"`. Also not implemented: the `kfctl` CLI (it does not
> exist), `--priority` on `gryvia submit`, the `resources.gpuCount` / `checkpointing`
> job fields used in the examples (use `spec.gpus`, `spec.gpuType`, and a
> `GryviaCheckpointGuard` for checkpointing), the priority Grafana dashboard, and the
> queue/preemption numbers in example outputs (made up). `gryvia queue` exists but lists
> pending, queued and scheduling jobs, not a priority or preemption view.

## Overview

Gryvia provides a priority-based scheduling system that:

- **7 Priority Levels**: From system-critical to best-effort
- **Smart Preemption**: Higher priority jobs can preempt lower priority jobs
- **Quota Overrides**: Critical jobs can exceed team quotas
- **SLA Guarantees**: Maximum queue time commitments
- **Auto-Checkpointing**: Preempted jobs automatically checkpoint and resume

## Priority Classes

### System-Critical (1,000,000)

For system and infrastructure jobs.

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: system-job
spec:
  priorityClassName: system-critical
  # Can preempt any other job
  # Can use 150% of quota
  # Guaranteed <1 minute queue time
```

**Use for:**
- Cluster maintenance jobs
- System monitoring
- Critical infrastructure

### Production (100,000)

For production inference and serving workloads.

```yaml
spec:
  priorityClassName: production
  # Can preempt normal/low/best-effort jobs
  # Can use 125% of quota
  # Guaranteed <5 minute queue time
```

**Use for:**
- Production model serving
- Customer-facing APIs
- Real-time inference

### High (10,000)

For important research and critical experiments.

```yaml
spec:
  priorityClassName: high
  # Can preempt normal/low/best-effort jobs
  # Can use 110% of quota
  # Guaranteed <30 minute queue time
```

**Use for:**
- Critical research deadlines
- Paper deadline experiments
- Important customer demos

### Normal (1,000) - Default

Standard training and development jobs.

```yaml
spec:
  priorityClassName: normal
  # Cannot preempt other jobs
  # Must respect quotas
  # Guaranteed <2 hour queue time
```

**Use for:**
- Regular training jobs
- Development work
- Experiments

### Low (100)

Non-urgent batch jobs.

```yaml
spec:
  priorityClassName: low
  # Cannot preempt
  # Must respect quotas
  # Guaranteed <24 hour queue time
```

**Use for:**
- Batch processing
- Non-urgent experiments
- Background tasks

### Best-Effort (10)

Opportunistic jobs using spare capacity.

```yaml
spec:
  priorityClassName: best-effort
  # Cannot preempt
  # Uses only idle resources
  # Guaranteed <1 week queue time
```

**Use for:**
- Dataset preprocessing
- Model benchmarking
- Exploratory research

### Spot (50)

Jobs using spot instances (70% cheaper).

```yaml
spec:
  priorityClassName: spot
  # Cannot preempt
  # Can be interrupted by cloud provider
  # Guaranteed <48 hour queue time
```

**Use for:**
- Cost-optimized training
- Fault-tolerant workloads
- Checkpointable jobs

## Using Priorities

### Basic Priority Assignment

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: my-job
spec:
  priorityClassName: high  # Set priority

  framework: pytorch
  resources:
    gpuType: A100-80G
    gpuCount: 8

  image: nvcr.io/nvidia/pytorch:24.01-py3
  command: ["python", "train.py"]
```

### With CLI

```bash
kfctl submit job.yaml --priority high

# Or inline
kfctl submit job.yaml --priority high --gpus 8 --gpu-type A100-80G
```

## Preemption

### How Preemption Works

1. **High-priority job submitted** but cluster is full
2. **Scheduler finds lower-priority jobs** to preempt
3. **Preempted jobs checkpoint** their state
4. **Preempted jobs terminate** gracefully (30s grace period)
5. **High-priority job starts** on freed resources
6. **Preempted jobs auto-queue** for restart when capacity available

### Enabling Checkpointing

**Required for jobs that can be preempted:**

```yaml
spec:
  priorityClassName: normal

  # Enable checkpointing for preemption
  checkpointing:
    enabled: true
    frequency: 10m  # Checkpoint every 10 minutes
    path: /checkpoints

  command:
    - python
    - train.py
    - --checkpoint-dir=/checkpoints
    - --auto-resume  # Resume from checkpoint if exists
```

### Training Code Example

```python
# PyTorch training with checkpointing
import torch
import os
import signal

class CheckpointManager:
    def __init__(self, checkpoint_dir):
        self.checkpoint_dir = checkpoint_dir
        self.checkpoint_file = os.path.join(checkpoint_dir, "checkpoint.pt")

        # Register SIGTERM handler for graceful preemption
        signal.signal(signal.SIGTERM, self._sigterm_handler)

    def save(self, epoch, model, optimizer, loss):
        checkpoint = {
            'epoch': epoch,
            'model_state_dict': model.state_dict(),
            'optimizer_state_dict': optimizer.state_dict(),
            'loss': loss,
        }
        torch.save(checkpoint, self.checkpoint_file)
        print(f"Checkpoint saved at epoch {epoch}")

    def load(self, model, optimizer):
        if os.path.exists(self.checkpoint_file):
            checkpoint = torch.load(self.checkpoint_file)
            model.load_state_dict(checkpoint['model_state_dict'])
            optimizer.load_state_dict(checkpoint['optimizer_state_dict'])
            epoch = checkpoint['epoch']
            loss = checkpoint['loss']
            print(f"Resumed from epoch {epoch}, loss {loss}")
            return epoch + 1
        return 0

    def _sigterm_handler(self, signum, frame):
        print("Received SIGTERM, checkpointing before exit...")
        # Checkpoint will be saved by training loop
        # Set flag to exit cleanly after current batch
        global should_exit
        should_exit = True

# Training loop
checkpoint_mgr = CheckpointManager('/checkpoints')
start_epoch = checkpoint_mgr.load(model, optimizer)

for epoch in range(start_epoch, num_epochs):
    for batch in dataloader:
        loss = train_step(batch)

        if should_exit:
            checkpoint_mgr.save(epoch, model, optimizer, loss)
            print("Exiting gracefully after preemption")
            sys.exit(0)

    # Periodic checkpointing
    if epoch % 10 == 0:
        checkpoint_mgr.save(epoch, model, optimizer, loss)
```

## Quota Overrides

High-priority jobs can exceed team quotas:

```yaml
# Priority class configuration
apiVersion: gryvia.io/v1alpha1
kind: GryviaPriority
metadata:
  name: high
spec:
  value: 10000
  quotaOverride:
    enabled: true
    maxOverridePercent: 10  # Can use 110% of quota
```

Example:
- Team quota: 100 GPUs
- Normal jobs: Can use up to 100 GPUs
- High-priority jobs: Can use up to 110 GPUs

## SLA Guarantees

Maximum queue time guarantees:

```yaml
spec:
  value: 10000
  sla:
    maxQueueTimeMinutes: 30
    guaranteedResources: false
```

If job waits >30 minutes, scheduler will:
1. Try preempting lower-priority jobs
2. Alert administrators
3. Optionally burst to cloud for capacity

## Monitoring Priorities

### View Queue Status

```bash
kfctl queue status

# Output:
# Queue: default
#
# Priority Queue:
#   1. llm-serving (production, 100000) - RUNNING
#   2. critical-training (high, 10000) - RUNNING
#   3. normal-job-1 (normal, 1000) - PENDING (15m)
#   4. normal-job-2 (normal, 1000) - PENDING (25m)
#   5. batch-job (low, 100) - PENDING (45m, preempted)
#
# Preemptions Today: 3
# Average Queue Time by Priority:
#   production: 2m
#   high: 12m
#   normal: 45m
#   low: 2h 30m
```

### View Preemption Events

```bash
kubectl get events --field-selector reason=Preempted

# Output:
# LAST SEEN   TYPE      REASON      OBJECT         MESSAGE
# 10m         Warning   Preempted   job/batch-job  Preempted for high-priority job
```

### Grafana Dashboard

```json
{
  "panels": [
    {
      "title": "Jobs by Priority",
      "targets": [{
        "expr": "sum(gryvia_jobs_total) by (priority)"
      }]
    },
    {
      "title": "Preemptions per Hour",
      "targets": [{
        "expr": "rate(gryvia_preemptions_total[1h])"
      }]
    },
    {
      "title": "Queue Time by Priority",
      "targets": [{
        "expr": "histogram_quantile(0.95, gryvia_queue_time_seconds_bucket) by (priority)"
      }]
    }
  ]
}
```

## Best Practices

### 1. Use Appropriate Priorities

```bash
# ✓ Good: Use high priority for critical work
kfctl submit --priority high critical-experiment.yaml

# ✗ Bad: Don't abuse high priority for routine work
kfctl submit --priority high routine-job.yaml  # Don't do this
```

### 2. Always Enable Checkpointing for Preemptible Jobs

```yaml
# Any job with priority <10000 should checkpoint
spec:
  priorityClassName: normal
  checkpointing:
    enabled: true
    frequency: 10m
```

### 3. Set Realistic SLAs

```yaml
# Production serving: Very short queue time
production:
  sla:
    maxQueueTimeMinutes: 5

# Batch jobs: Longer queue time acceptable
low:
  sla:
    maxQueueTimeMinutes: 1440  # 24 hours
```

### 4. Monitor Preemption Rates

```bash
# Check if preemption is too frequent
kfctl metrics preemption-rate

# If >10% of jobs are preempted, consider adding capacity
```

### 5. Use Spot for Cost Savings

```yaml
spec:
  priorityClassName: spot
  checkpointing:
    enabled: true  # Required for spot

  # Spot instances are 70% cheaper
  # But can be interrupted
```

## Advanced Configuration

### Custom Priority Classes

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaPriority
metadata:
  name: paper-deadline
spec:
  value: 50000  # Between production and high
  description: "Paper deadline crunch time"
  preemptionPolicy: PreemptLowerPriority
  quotaOverride:
    enabled: true
    maxOverridePercent: 20
  sla:
    maxQueueTimeMinutes: 15
```

### Preemption Policies

```yaml
# Can preempt lower priority jobs
preemptionPolicy: PreemptLowerPriority

# Never preempt (even if higher priority)
preemptionPolicy: Never
```

### Guaranteed Resources

```yaml
sla:
  guaranteedResources: true  # Reserves resources permanently
```

**Use sparingly** - guaranteed resources can't be used by other jobs.

## Troubleshooting

### Job Stuck in Pending

```bash
# Check priority queue
kfctl queue status

# Check if being preempted repeatedly
kubectl describe gryviaaijob my-job | grep -i preempt

# Temporarily increase priority
kfctl priority set my-job high
```

### Too Many Preemptions

```bash
# Check preemption rate
kfctl metrics preemption-rate

# Solutions:
# 1. Add more capacity
# 2. Reduce high-priority job submissions
# 3. Adjust priority class values
```

### Checkpoint Failures

```bash
# Check checkpoint logs
kubectl logs my-job | grep -i checkpoint

# Verify checkpoint path is writable
kubectl exec my-job -- ls -la /checkpoints

# Test checkpoint manually
kfctl checkpoint test my-job
```

## Examples

See `crds/gryvia.io_gryviapriorities.yaml` for:
- Priority class definitions
- Example jobs with priorities
- Preemption event examples
- Queue status examples

## Support

- Priority Issues: https://github.com/zyvorai/gryvia/issues
- Preemption Discussion: https://github.com/zyvorai/gryvia/discussions
