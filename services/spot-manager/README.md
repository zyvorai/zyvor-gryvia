# Spot Instance Management

> **Status: design sketch, not implemented in this repo.** This directory holds only this README and a Dockerfile that references a `requirements.txt` that does not exist; there is no source code. No spot manager, price monitor, bidding or multi-cloud migration logic exists. Checkpoint-related CRDs (for example `GryviaCheckpointGuard`) are handled by the ai-operator, but nothing here integrates with cloud spot APIs. Savings percentages are illustrative, not measured.

Optimize costs with spot/preemptible GPU instances while maintaining reliability.

## Features

- **Auto-Checkpointing**: Automatic checkpoints before preemption
- **Graceful Migration**: Seamless migration to on-demand instances
- **Cost Tracking**: Real-time cost savings monitoring
- **Bidding Strategy**: Smart bidding for spot capacity
- **Multi-Cloud**: Support for AWS, GCP, Azure spot instances

## Architecture

```
┌────────────────────────────────────────────┐
│         Spot Instance Manager               │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐ │
│  │  Price   │  │Migration │  │Checkpoint│ │
│  │ Monitor  │  │ Manager  │  │ Manager  │ │
│  └──────────┘  └──────────┘  └──────────┘ │
└─────────────┬──────────────────────────────┘
              │
       ┌──────┴───────┐
       │              │
┌──────▼────┐  ┌─────▼──────┐
│   Spot    │  │ On-Demand  │
│ Instances │  │ Instances  │
└───────────┘  └────────────┘
```

## Configuration

### Enable Spot Instances

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: training-spot
  annotations:
    gryvia.io/spot-enabled: "true"
    gryvia.io/spot-max-price: "15.00"  # Max $/hour per GPU
    gryvia.io/spot-fallback: "on-demand"
spec:
  framework: pytorch
  resources:
    gpuType: A100-80G
    gpuCount: 8
  checkpointing:
    enabled: true
    interval: 600  # Checkpoint every 10 min
    path: /checkpoints
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command: ["python", "train.py"]
```

### Spot Configuration

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: spot-config
  namespace: gryvia-system
data:
  config.yaml: |
    # Pricing
    maxPricePerGPU:
      H100: 20.00      # vs $30.00 on-demand (33% savings)
      A100-80G: 15.00  # vs $24.00 on-demand (38% savings)
      A100-40G: 7.50   # vs $12.00 on-demand (38% savings)
      T4: 2.00         # vs $3.00 on-demand (33% savings)

    # Interruption handling
    preemptionWarning: 120  # 2 minutes warning
    gracePeriod: 300        # 5 minutes to checkpoint
    autoMigrate: true       # Migrate to on-demand on preemption

    # Bidding strategy
    biddingStrategy: balanced  # aggressive, balanced, conservative
    fallbackToOnDemand: true
    maxSpotAttempts: 3

    # Checkpointing
    autoCheckpoint: true
    checkpointInterval: 600
    checkpointVerification: true
```

## Usage Examples

### Cost-Optimized Training

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: llama-training-spot
  annotations:
    gryvia.io/spot-enabled: "true"
    gryvia.io/spot-max-price: "15.00"
spec:
  framework: pytorch
  distributed:
    enabled: true
    strategy: ddp
    nodes: 1
    gpusPerNode: 8
  resources:
    gpuType: A100-80G
    gpuCount: 8
  # Checkpointing is critical for spot
  checkpointing:
    enabled: true
    interval: 600
    path: /checkpoints
    strategy: incremental
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command:
    - torchrun
    - --nproc_per_node=8
    - train.py
    - --checkpoint-dir=/checkpoints
    - --resume-from-checkpoint=latest
```

### Hybrid Strategy

Mix spot and on-demand for optimal cost/reliability:

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: hybrid-training
spec:
  resources:
    gpuCount: 16
  # Use spot for 12 GPUs
  spotAllocation:
    enabled: true
    count: 12
    maxPrice: 15.00
  # Keep 4 GPUs on-demand for reliability
  onDemandAllocation:
    count: 4
```

## Cost Savings

### Pricing Comparison

| GPU Type | On-Demand | Spot (Avg) | Savings |
|----------|-----------|------------|---------|
| H100 | $30.00/hr | $18.00/hr | 40% |
| A100-80G | $24.00/hr | $14.40/hr | 40% |
| A100-40G | $12.00/hr | $7.20/hr | 40% |
| V100 | $8.00/hr | $4.80/hr | 40% |
| T4 | $3.00/hr | $1.80/hr | 40% |

### Real-World Savings

**Example: 7-day training on 8x A100-80G**

```
On-Demand:
  8 GPUs × $24.00/hr × 168 hours = $32,256

Spot (with 3 interruptions):
  8 GPUs × $14.40/hr × 168 hours = $19,354
  Migration overhead: ~$200
  Total: $19,554

Savings: $12,702 (39.4%)
```

## Interruption Handling

### Preemption Warning

Spot instances receive 2-minute warning before termination:

```python
# training_script.py
import signal
import sys

def preemption_handler(signum, frame):
    print("Preemption warning received. Checkpointing...")

    # Save checkpoint
    torch.save({
        'epoch': epoch,
        'model_state_dict': model.state_dict(),
        'optimizer_state_dict': optimizer.state_dict(),
        'loss': loss,
    }, '/checkpoints/preemption.pt')

    print("Checkpoint saved. Exiting gracefully.")
    sys.exit(0)

# Register handler
signal.signal(signal.SIGTERM, preemption_handler)

# Training loop
for epoch in range(num_epochs):
    # Regular checkpoints
    if epoch % checkpoint_interval == 0:
        save_checkpoint(epoch)

    train_epoch()
```

### Auto-Migration

On preemption, job automatically migrates to on-demand:

```yaml
status:
  phase: Running
  spotInstance: true
  preemptions: 2
  migrations:
    - timestamp: "2024-01-15T10:30:00Z"
      reason: "Spot capacity unavailable"
      from: spot-a100-node-1
      to: ondemand-a100-node-3
      checkpoint: checkpoint-epoch-42.pt
      downtime: 127s
```

## Monitoring

### Cost Dashboard

Track real-time savings:

```bash
# View spot usage
kfctl spot status

# Output:
# Spot Usage:
#   Active Jobs: 12
#   Total GPUs: 96 (spot: 72, on-demand: 24)
#   Current Cost: $1,036.80/hr
#   On-Demand Cost: $1,728.00/hr
#   Savings: $691.20/hr (40.0%)
#
# Interruptions (last 7 days):
#   Total: 8
#   Avg per job: 0.67
#   Avg downtime: 94 seconds
```

### Grafana Dashboard

Metrics tracked:
- Spot vs on-demand ratio
- Interruptions per day
- Migration success rate
- Cost savings over time
- Spot price trends

## Best Practices

1. **Always Enable Checkpointing**
   ```yaml
   checkpointing:
     enabled: true
     interval: 600  # 10 minutes
   ```

2. **Test Recovery**
   ```bash
   # Simulate preemption
   kubectl delete pod spot-job-worker-0

   # Verify auto-recovery
   kubectl logs spot-job-worker-0-new | grep "Resumed from checkpoint"
   ```

3. **Monitor Interruption Rate**
   ```bash
   # Check interruption history
   kfctl spot interruptions --days 30

   # If interruptions > 5/day, consider:
   # - Higher max bid price
   # - Different GPU type
   # - Different region
   ```

4. **Gradual Adoption**
   ```
   Week 1: 10% spot (low-priority jobs)
   Week 2: 25% spot (development)
   Week 3: 50% spot (most training)
   Week 4: 75% spot (production-ready)
   ```

5. **Hybrid for Critical Jobs**
   ```yaml
   # Critical jobs: 50% spot, 50% on-demand
   spotAllocation:
     count: 4
   onDemandAllocation:
     count: 4
   ```

## Troubleshooting

### High Interruption Rate

```bash
# Check spot price history
kfctl spot prices --gpu-type A100-80G --days 7

# Adjust max price
kubectl annotate gryviaaijob my-job \
  gryvia.io/spot-max-price=18.00 --overwrite

# Or switch to different GPU type
# T4 often has lower interruption rate
```

### Failed Migrations

```bash
# Check migration logs
kubectl logs -n gryvia-system deployment/spot-manager

# Verify checkpoint integrity
kfctl spot verify-checkpoint my-job

# Manual migration
kfctl spot migrate my-job --to on-demand
```

### Checkpoint Corruption

```bash
# Enable checkpoint verification
kubectl patch gryviaaijob my-job -p '{
  "spec": {
    "checkpointing": {
      "verification": true,
      "redundancy": 2
    }
  }
}'
```

## Advanced Features

### Dynamic Bidding

Automatically adjust bid price based on market:

```yaml
biddingStrategy:
  type: dynamic
  targetSavings: 35  # Target 35% savings
  maxPrice: 18.00
  minPrice: 12.00
  adjustmentInterval: 300  # Adjust every 5 min
```

### Multi-Region Failover

Automatically failover to different region:

```yaml
spotConfig:
  regions:
    - name: us-west-2
      priority: 1
    - name: us-east-1
      priority: 2
    - name: eu-west-1
      priority: 3
  autoFailover: true
```

### Predictive Interruption

ML model predicts interruptions:

```yaml
predictiveMode:
  enabled: true
  checkpointBeforePredictedInterruption: true
  predictionThreshold: 0.7  # 70% probability
```

## Cost Calculator

Estimate spot savings:

```bash
# Estimate cost for job
kfctl spot estimate \
  --gpu-type A100-80G \
  --gpu-count 8 \
  --duration 7d \
  --interruption-rate 2/day

# Output:
# On-Demand Cost: $32,256
# Spot Cost (estimated): $19,750
# Savings: $12,506 (38.8%)
# Risk: Low (2 interruptions/day)
```

## Support

- Spot Issues: https://github.com/zyvorai/gryvia/issues
- Cost Optimization: https://github.com/zyvorai/gryvia/discussions
