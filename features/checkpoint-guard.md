# GryviaCheckpointGuard

Intelligent checkpoint and restore system for AI training jobs running on Gryvia.

> **Status: partly implemented (ai-operator controller registered); the checkpoint I/O is not.**
> The controller matches jobs, evaluates emergency triggers (pod health, `GryviaGpuNode` phase and
> per-GPU health/temperature, spot-preemption pod signals, NVLink state, loss divergence) and the
> periodic schedule, and keeps checkpoint bookkeeping in `status` (counts, last checkpoint, retention
> index). It does not itself make a training process write a checkpoint: it derives a
> `/checkpoints/<job>/<name>` path and runs the validator (checksum, shape, load test) against that path
> on the operator's own filesystem, so validation is only meaningful when that path is reachable
> there. Automatic restore on job restart and replication to S3/GCS/Azure/NFS/PVC are described in
> the CRD but no code in the controller performs them. The `gryvia_checkpoint_*` Prometheus metrics
> listed below are not exported by the operator. Unverified end to end on a real GPU training job.

## Overview

GryviaCheckpointGuard provides automated, policy-driven checkpointing for long-running AI training jobs. It continuously monitors GPU health, node conditions, and training metrics to determine when checkpoints should be taken -- including emergency checkpoints triggered by hardware degradation or spot preemption signals.

Key capabilities:

- **Periodic checkpointing** at configurable intervals
- **Emergency checkpointing** triggered by GPU health degradation, spot preemption, memory pressure, loss divergence, or NVLink issues
- **Checkpoint validation** with checksum verification, tensor shape checks, and load testing
- **Automatic restore** from the latest valid checkpoint on job restart
- **Retention management** to control storage usage
- **Replication** to durable backends (S3, GCS, Azure Blob, NFS, PVC)
- **Prometheus metrics** and alerting for checkpoint operations

## Quick Start

Create a checkpoint guard that monitors all training jobs labeled `team: ml-research`:

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaCheckpointGuard
metadata:
  name: training-guard
  namespace: default
spec:
  jobSelector:
    matchLabels:
      gryvia.io/team: ml-research

  checkpointPolicy:
    intervalMinutes: 15
    emergencyCheckpoint:
      triggers:
        - GpuHealthDegraded
        - SpotPreemptionSignal
        - MemoryPressure
    replication:
      backend: pvc
      copies: 2
      asyncUpload: true

  validation:
    checksumVerify: true
    retainValidOnly: true
    retentionCount: 5

  restore:
    autoRestore: true
    injectEnvVars:
      CHECKPOINT_DIR: "/checkpoints"

  monitoring:
    exportMetrics: true
    alertOnFailure: true
```

Apply it:

```bash
kubectl apply -f checkpoint-guard.yaml
```

## How It Works

```
                                 +---------------------+
                                 | GryviaCheckpointGuard|
                                 | Controller          |
                                 +----------+----------+
                                            |
                    +-----------+-----------+-----------+-----------+
                    |           |           |           |           |
              Match Jobs   Check Pods  Check GPUs  Check Loss  Check Spot
                    |           |           |           |           |
              +-----+    +-----+     +-----+    +-----+     +-----+
              |GryviaAI|  |StatefulSet| |GryviaGpu| |Metrics |  |Node   |
              |Job     |  |Pods      | |Node     | |        |  |Taints |
              +--------+  +----------+ +---------+ +--------+  +-------+
                    |
           +-------+-------+
           |               |
     Periodic Ckpt   Emergency Ckpt
           |               |
           +-------+-------+
                   |
           +-------+-------+
           |               |
       Validate        Replicate
           |               |
           +-------+-------+
                   |
            Retention Mgmt
                   |
            Update Status
```

1. The controller watches GryviaCheckpointGuard resources.
2. For each guard, it finds matching GryviaAIJob resources using the jobSelector.
3. It inspects each matched job's StatefulSet pods for health issues.
4. It queries GryviaGpuNode resources for GPU health and NVLink status.
5. It checks for spot preemption signals on nodes running the job.
6. It evaluates job metrics for loss divergence.
7. If any emergency trigger fires, an immediate checkpoint is taken.
8. Periodic checkpoints are taken at the configured interval.
9. Each checkpoint is validated, and only valid checkpoints are retained (if configured).
10. Status is updated with checkpoint counts, timing, and storage usage.

## Spec Reference

### jobSelector

Selects which GryviaAIJob resources this guard applies to.

| Field | Type | Description |
|-------|------|-------------|
| `matchLabels` | `map[string]string` | Key-value pairs matched against job labels |

### checkpointPolicy

Controls when and how checkpoints are created.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `intervalMinutes` | `integer` | `30` | Minutes between periodic checkpoints |
| `emergencyCheckpoint.triggers` | `[]string` | `[]` | Conditions that trigger immediate checkpoint |
| `replication.backend` | `string` | `pvc` | Storage backend: s3, gcs, azure-blob, nfs, pvc |
| `replication.copies` | `integer` | `2` | Number of checkpoint copies |
| `replication.asyncUpload` | `boolean` | `true` | Upload asynchronously |

**Emergency triggers:**

| Trigger | Fires when |
|---------|------------|
| `GpuHealthDegraded` | GryviaGpuNode reports Degraded/Failed phase or unhealthy GPU |
| `SpotPreemptionSignal` | Node has spot termination taint or pod has preemption annotation |
| `MemoryPressure` | Pod is OOMKilled or in CrashLoopBackOff |
| `LossDivergence` | Training loss is NaN, Inf, or exceeds 1e6 |
| `NvlinkDegraded` | NVLink interconnect is unhealthy on allocated nodes |

### validation

Controls how checkpoints are validated after creation.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `checksumVerify` | `boolean` | `true` | Compute and verify SHA-256 checksum |
| `tensorShapeVerify` | `boolean` | `false` | Verify file header matches known checkpoint formats |
| `loadTest` | `boolean` | `false` | Read entire file to detect corruption |
| `retainValidOnly` | `boolean` | `true` | Discard checkpoints that fail validation |
| `retentionCount` | `integer` | `5` | Number of valid checkpoints to keep |

### restore

Controls how checkpoints are used when a job restarts.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `autoRestore` | `boolean` | `true` | Automatically restore from latest valid checkpoint |
| `preferNearStorage` | `boolean` | `true` | Prefer scheduling near checkpoint storage |
| `injectEnvVars` | `map[string]string` | `{}` | Env vars injected into restored pods |

### monitoring

Controls observability for checkpoint operations.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `exportMetrics` | `boolean` | `true` | Export Prometheus metrics |
| `alertOnFailure` | `boolean` | `true` | Alert when checkpoints fail |

## Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `phase` | `string` | Current phase: Idle, Monitoring, Checkpointing, Restoring, Error |
| `totalCheckpoints` | `integer` | Total checkpoints taken |
| `validCheckpoints` | `integer` | Checkpoints that passed validation |
| `lastCheckpointTime` | `datetime` | When the last checkpoint was taken |
| `lastValidCheckpoint` | `string` | Name of the last valid checkpoint |
| `emergencyCheckpointsTaken` | `integer` | Number of emergency checkpoints |
| `storageUsed` | `string` | Storage consumed (e.g., 42Gi) |
| `avgCheckpointDuration` | `string` | Average checkpoint time (e.g., 2m30s) |
| `matchedJobs` | `integer` | Jobs currently matched by selector |

## CLI Usage

```bash
# List checkpoint guards
kubectl get gryviacheckpointguards
# or with short name
kubectl get fcg

# Example output:
# NAME               VALID   LASTCHECKPOINT              STORAGEUSED   AGE
# training-guard     12      2026-04-10T14:30:00Z        42Gi          5d
# llm-guard          8       2026-04-10T14:15:00Z        128Gi         12d

# Describe a checkpoint guard
kubectl describe fcg training-guard

# View checkpoint guard status
kubectl get fcg training-guard -o jsonpath='{.status}'

# Check emergency checkpoint count
kubectl get fcg training-guard -o jsonpath='{.status.emergencyCheckpointsTaken}'
```

## Examples

### Minimal Guard

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaCheckpointGuard
metadata:
  name: simple-guard
spec:
  jobSelector:
    matchLabels:
      gryvia.io/type: training
  checkpointPolicy:
    intervalMinutes: 30
```

### Full-Featured Guard for Production LLM Training

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaCheckpointGuard
metadata:
  name: llm-production-guard
  namespace: ml-production
spec:
  jobSelector:
    matchLabels:
      gryvia.io/team: llm-research
      gryvia.io/type: training

  checkpointPolicy:
    intervalMinutes: 10
    emergencyCheckpoint:
      triggers:
        - GpuHealthDegraded
        - SpotPreemptionSignal
        - MemoryPressure
        - LossDivergence
        - NvlinkDegraded
    replication:
      backend: s3
      copies: 3
      asyncUpload: true

  validation:
    checksumVerify: true
    tensorShapeVerify: true
    loadTest: true
    retainValidOnly: true
    retentionCount: 20

  restore:
    autoRestore: true
    preferNearStorage: true
    injectEnvVars:
      CHECKPOINT_DIR: "/checkpoints"
      RESUME_FROM_CHECKPOINT: "true"
      CHECKPOINT_BACKEND: "s3"

  monitoring:
    exportMetrics: true
    alertOnFailure: true
```

### Guard for Spot Instance Training

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaCheckpointGuard
metadata:
  name: spot-training-guard
spec:
  jobSelector:
    matchLabels:
      gryvia.io/priority: spot

  checkpointPolicy:
    intervalMinutes: 5
    emergencyCheckpoint:
      triggers:
        - SpotPreemptionSignal
    replication:
      backend: s3
      copies: 2
      asyncUpload: false  # Synchronous to ensure checkpoint is saved before termination

  validation:
    checksumVerify: true
    retainValidOnly: true
    retentionCount: 3

  restore:
    autoRestore: true
    preferNearStorage: false  # Spot instances may run anywhere
```

## Prometheus Metrics

When `monitoring.exportMetrics` is true, the following metrics are exported:

| Metric | Type | Description |
|--------|------|-------------|
| `gryvia_checkpoint_total` | Counter | Total checkpoints taken |
| `gryvia_checkpoint_valid_total` | Counter | Valid checkpoints |
| `gryvia_checkpoint_emergency_total` | Counter | Emergency checkpoints triggered |
| `gryvia_checkpoint_duration_seconds` | Histogram | Checkpoint duration |
| `gryvia_checkpoint_storage_bytes` | Gauge | Total storage used |
| `gryvia_checkpoint_validation_failures` | Counter | Validation failures |

## Troubleshooting

### No jobs matched

```bash
# Verify labels on your GryviaAIJob
kubectl get gryviaaijobs --show-labels

# Ensure the jobSelector matches
kubectl get fcg training-guard -o jsonpath='{.spec.jobSelector}'
```

### Checkpoint validation failing

```bash
# Check the guard conditions
kubectl describe fcg training-guard

# Look for CheckpointValid condition with status False
# Common causes:
# - Checkpoint path not writable
# - Disk full
# - Checkpoint file corrupted during write
```

### Emergency checkpoints not firing

```bash
# Verify triggers are configured
kubectl get fcg training-guard -o jsonpath='{.spec.checkpointPolicy.emergencyCheckpoint.triggers}'

# Check GPU node health
kubectl get gryviagpunodes

# Check for spot preemption annotations
kubectl get pods -l gryvia.io/job=my-job -o jsonpath='{.items[*].metadata.annotations}'
```

## Support

- Issues: https://github.com/zyvorai/gryvia/issues
- Discussions: https://github.com/zyvorai/gryvia/discussions
