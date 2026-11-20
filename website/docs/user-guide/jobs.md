# Job Management Guide

Guide to submitting and managing AI workloads with Gryvia.

> **Status.** What the ai-operator actually does with a `GryviaAIJob` is described in the [Scheduling guide](../guides/SCHEDULING.md#what-runs-today): it picks nodes, then creates an Indexed batch Job (training, fine-tuning, evaluation) or a StatefulSet (inference), a headless Service and an optional PVC. Many features that appear in older versions of this page (checkpointing and restart policy fields, spot, preemption, budget alerts) are not fields of the job schema; this page now says so where relevant. Nothing here has been verified on real GPU hardware.

Required spec fields are `type` (`training`, `inference`, `fine-tuning` or `evaluation`), `gpus` and `image`. The full schema is in `crds/gryvia.io_gryviaaijobs.yaml` and the [CRD reference](../reference/crds.md).

## Job Basics

### Creating a Job

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: my-training-job
  namespace: default
  labels:
    team: ml-research
    project: llama-training
spec:
  type: training
  image: nvcr.io/nvidia/pytorch:24.01-py3
  gpus: 8
  gpuType: A100-80G
  resources:
    requests:
      cpu: "64"
      memory: 512Gi
  command:
    - python
    - train.py
  args:
    - --model=llama-7b
    - --data=/data/openwebtext
```

### Submitting Jobs

**Using kubectl:**
```bash
kubectl apply -f job.yaml
```

**Using gryvia:**
```bash
# Submit from file
gryvia submit --file job.yaml

# Submit and wait for completion
gryvia submit --file job.yaml --wait

# Validate a manifest without submitting it
gryvia validate job.yaml
```

## Job Lifecycle

### States

Set by the ai-operator (`status.phase`):

1. **Pending**: new job, or no node currently qualifies (retried every 30 seconds)
2. **Scheduling**: nodes selected, workload created, pods not ready yet (or the Job is suspended)
3. **Running**: at least one replica is ready
4. **Succeeded**: the batch Job completed (every index exited 0)
5. **Failed**: the batch Job failed (retry limit or timeout reached), with the reason in `status.message`

Set by other components: **Queued** and **Rejected** (quota operator; nothing is created for them). With the opt-in Kueue integration **Queued** is also set by the ai-operator while Kueue holds the job for quota (the Job exists, suspended, no pods; `status.message` carries Kueue's reason) and again after Kueue evicts a running job for a higher-priority one ([Kueue integration](https://github.com/zyvorai/gryvia/blob/main/docs/kueue-integration.md)), **Preempted** (priority controller) and **Cancelled** (`gryvia cancel`, or the annotation `gryvia.io/cancel: "true"`). Cancelling or preempting deletes the Job (pods stop) and keeps the PVC. Succeeded, Failed, Cancelled and Rejected are final. Details: [AIJob lifecycle](https://github.com/zyvorai/gryvia/blob/main/docs/aijob-lifecycle.md).

Workload kind: `spec.workloadKind` is `job` (default for training, fine-tuning, evaluation) or `statefulset` (default for inference; pods restart forever and the job never completes). A job that already runs on a StatefulSet keeps it. `spec.suspend: true` creates the Job suspended. `gpus: 0` runs a CPU-only job.

### Monitoring Jobs

```bash
# List all jobs
gryvia list jobs

# Jobs waiting for resources (Pending/Queued/Scheduling)
gryvia queue

# Get job status
gryvia status my-training-job

# Watch job progress
watch gryvia status my-training-job

# Get detailed info
kubectl describe gryviaaijob my-training-job
```

### Viewing Logs

```bash
# Stream logs
gryvia logs my-training-job

# Follow logs
gryvia logs my-training-job -f

# Get logs from specific replica
gryvia logs my-training-job --replica 0

# Last 500 lines
gryvia logs my-training-job --tail 500

# Save logs to file
gryvia logs my-training-job > training.log
```

### Cancelling and Deleting Jobs

```bash
# Mark the job cancelled (asks for confirmation; see the note under States)
gryvia cancel my-training-job

# Delete the job; the StatefulSet, Service and PVC are garbage-collected via owner references
gryvia delete job my-training-job --yes
```

## Distributed Training

### Single node (PyTorch DDP)

```yaml
spec:
  type: training
  image: nvcr.io/nvidia/pytorch:24.01-py3
  gpus: 8
  gpuType: A100-80G
  distributed:
    enabled: true
    framework: pytorch
    backend: nccl
    nodes: 1
    gpusPerNode: 8
  command: ["torchrun", "--nproc_per_node=8", "--nnodes=1", "train.py"]
```

`distributed.framework` accepts pytorch, tensorflow, horovod, deepspeed or megatron; `backend` accepts nccl, gloo or mpi. There is no `strategy` field: DDP, DeepSpeed and so on are chosen by your command line, not by Gryvia.

### Multi-Node Training

```yaml
spec:
  type: training
  image: nvcr.io/nvidia/pytorch:24.01-py3
  gpus: 8
  gpuType: A100-80G
  network: rdma            # optional: needs RDMA-labelled nodes
  distributed:
    enabled: true
    framework: pytorch
    backend: nccl
    nodes: 4               # 4 pods (Job completions / StatefulSet replicas)
    gpusPerNode: 8         # 8 GPUs per pod = 32 total
  command: ["torchrun"]
  args:
    - --nproc_per_node=8
    - --nnodes=4
    - --master_addr=$(MASTER_ADDR)
    - --master_port=$(MASTER_PORT)
    - train.py
```

For distributed jobs the controller sets `MASTER_ADDR` (`<job>-0.<job>-headless.<namespace>.svc.cluster.local` for Jobs), `MASTER_PORT=29500`, `WORLD_SIZE` (`nodes * gpusPerNode`, 32 above), `RANK` and `NODE_RANK` (the pod's index, from the Job completion index), `NNODES`, `NPROC_PER_NODE`, and `NCCL_DEBUG=INFO` (not for `backend: gloo`), plus `NCCL_IB_DISABLE=0` and `NCCL_NET_GDR_LEVEL=5` when `network: rdma`. It also mounts a memory-backed `/dev/shm`. With `gpusPerNode > 1`, `RANK` is the node index, not the per-GPU process rank: pass `--node_rank=$(NODE_RANK)` to torchrun and let it compute the process ranks. The full variable table is in [AIJob lifecycle](https://github.com/zyvorai/gryvia/blob/main/docs/aijob-lifecycle.md#environment-variables). Use `$(VAR)` syntax in `args` for Kubernetes to expand variables; `$VAR` inside a `command` list is not expanded. Total GPUs are capped at 1024 by the admission webhook.

### DeepSpeed

```yaml
spec:
  type: training
  image: nvcr.io/nvidia/pytorch:24.01-py3
  gpus: 8
  gpuType: A100-80G
  distributed:
    enabled: true
    framework: deepspeed
    backend: nccl
    nodes: 2
    gpusPerNode: 8
  command: ["deepspeed"]
  args: ["--num_gpus=8", "train.py", "--deepspeed", "--deepspeed_config=ds_config.json"]
```

Multi-node DeepSpeed usually needs a hostfile or launcher setup that Gryvia does not create.

## Resource Configuration

### GPU Selection

`spec.gpus` is the GPU count and `spec.gpuType` selects nodes by the `gryvia.io/gpu` label (`any` or empty means no constraint). Nodes get that label from GPU node registration; see [GPU nodes](../guides/GPU_NODES.md). There is no `gpuSelector` (memory or compute-capability) field; use `nodeSelector` or `affinity` on your own node labels instead.

```yaml
spec:
  gpus: 8
  gpuType: A100-80G
  nodeSelector:
    topology.kubernetes.io/zone: us-east-1a
```

### Memory and CPU

Standard Kubernetes resources, per pod. The controller adds the `nvidia.com/gpu` limit itself.

```yaml
spec:
  resources:
    requests:
      cpu: "64"
      memory: 512Gi
    limits:
      memory: 512Gi
```

There is no per-GPU CPU or memory field.

### Storage

Set `spec.storage` (and `spec.storageRequest`) to have the controller create a PVC named `<job>-data` mounted at `/data`. You can also mount your own volumes:

```yaml
spec:
  volumeMounts:
    - name: dataset
      mountPath: /datasets
    - name: checkpoints
      mountPath: /checkpoints
  volumes:
    - name: dataset
      persistentVolumeClaim:
        claimName: shared-dataset
    - name: checkpoints
      persistentVolumeClaim:
        claimName: my-checkpoints
```

## Environment Configuration

### Environment Variables

```yaml
spec:
  env:
    - name: MLFLOW_TRACKING_URI
      value: "http://mlflow:5000"
    - name: WANDB_API_KEY
      valueFrom:
        secretKeyRef:
          name: wandb-secret
          key: api-key
```

### Secrets

Use `secretKeyRef` in `env` as above, or a `secret` volume. There is no `envFrom` field on `GryviaAIJob`.

## Job Templates

Reusable job defaults are stored as cluster-scoped `GryviaTemplate` resources. The ai-operator validates each template
(condition `Valid`) and counts the jobs labelled `gryvia.io/template: <name>` in `status.instantiatedJobs`, but nothing
renders a template into a job and the `gryvia` CLI does not manage templates. Read them with `kubectl`:

```bash
# List templates
kubectl get gryviatemplates

# View a template
kubectl get gryviatemplate pytorch-ddp -o yaml
```

Copy the defaults you want from a template into your `GryviaAIJob` manifest, add the label `gryvia.io/template: <name>`
so the template counts it, and submit it with `gryvia submit --file job.yaml`.

## Features that are not job fields

Older versions of this page showed `checkpointing`, `restartPolicy`/`backoffLimit`, `priority: high`, `preemptible` and `spot` blocks on the job. None of those exist in the `GryviaAIJob` schema, so `kubectl apply` rejects them. What exists:

- **Checkpointing**: write checkpoints to a mounted volume from your own training code. With the opt-in `aiOperator.checkpointGuard`, a `GryviaCheckpointGuard` injects the checkpoint directory and cadence, records committed steps and replaces pods stuck on a lost node; see [checkpoint guard](https://github.com/zyvorai/gryvia/blob/main/docs/checkpoint-guard.md).
- **Retries**: for the batch Job workload `spec.retryLimit` is the Job's `backoffLimit` (0 fails the job on the first failed pod; a failed pod is replaced by a new pod with the same index and rank) and `status.retries` counts failed pods. `spec.timeout` (`90m`, `24h`, `7d`) is the Job's `activeDeadlineSeconds`. Neither applies to the StatefulSet workload, whose pods restart in place.
- **Priority**: `spec.priority` is an integer 0 to 100 that the admission webhook range-checks. Only with the opt-in Kueue integration does it matter: it preempts lower-priority jobs in the same queue ([Kueue integration](https://github.com/zyvorai/gryvia/blob/main/docs/kueue-integration.md)). `spec.queueName` (or the annotation `gryvia.io/queue-name`) names the Kueue LocalQueue; it is ignored without the integration. See [Scheduling](../guides/SCHEDULING.md#priority-preemption).
- **Spot**: no spot field or controller. Spot-related services in `services/` are separate and not part of the job API.

## Cost Management

### Cost Tracking

```bash
# Cost for the team that owns the job, with a detailed breakdown
gryvia cost my-team --detailed

# Costs over the last week
gryvia cost my-team --period week
```

### Budgets

Budget alert annotations (`gryvia.io/budget-alert`) are not read by anything. `GryviaBudget` objects are reconciled by the quota-operator: spend from the metered usage records sets
`status.state`, and `gryvia budget` shows it. New jobs are blocked only with the opt-in admission gate
(`aiOperator.admissionGate`). Cost figures from `gryvia cost` are estimates based on the GPU SKU catalog prices; there is no billing or payment integration.

## Performance Optimization

### Profile Jobs

```bash
# Helper script in the repository (needs the python kubernetes package and a kubeconfig)
python3 tools/profiler.py --job my-training-job \
  --gpu-type A100-80G \
  --gpu-count 8

# Check GPU health across the cluster
gryvia health gpu
```

### Common Optimizations

1. **Increase Batch Size**: Improve GPU utilization
2. **Mixed Precision**: Enable AMP (gains depend on model and GPU; measure)
3. **Gradient Accumulation**: Simulate larger batches
4. **Data Loading**: Increase num_workers
5. **Distributed Training**: Scale to multiple GPUs

## Troubleshooting

### Job Stuck in Pending

```bash
# The Scheduled condition and message explain why no node qualified
# (GPU type label, free GPUs, network mode, nodeSelector)
kubectl describe gryviaaijob my-training-job

# Jobs waiting for resources
gryvia queue

# Check GPU availability
gryvia list nodes
gryvia capacity

# Check quota
gryvia quota my-team
```

The admission webhook can also deny a job at create time (quota, GPU type, per-job GPU limit); the error is returned by `kubectl apply` or `gryvia submit`.

### Job Failed

```bash
# Check logs
gryvia logs my-training-job

# Check events
kubectl get events --field-selector involvedObject.name=my-training-job

# Describe job
kubectl describe gryviaaijob my-training-job
```

### Out of Memory

```bash
# Reduce batch size
# Enable gradient checkpointing
# Use gradient accumulation
# Switch to larger GPU type
```

### Low GPU Utilization

```bash
# Profile job
python3 tools/profiler.py --job my-training-job

# Common fixes:
# - Increase batch size
# - Increase num_workers in DataLoader
# - Use prefetching
# - Enable pin_memory
```

## Best Practices

1. **Resource Requests**: Set appropriate resource requests
2. **Labels**: Add team and project labels
3. **MLflow**: Enable experiment tracking
4. **Checkpointing**: Save checkpoints regularly
5. **Monitoring**: Monitor GPU utilization
6. **Cost Tracking**: Track job costs
7. **Documentation**: Document job parameters
8. **Version Control**: Version control job configs

## Examples

See [Job Examples](https://github.com/zyvorai/gryvia/tree/main/examples/jobs/) for complete examples:
- PyTorch distributed training (`pytorch-distributed.yaml`)
- TensorFlow multi-node (`tensorflow-multi-node.yaml`)
- JAX inference (`jax-inference.yaml`)
- Hyperparameter tuning sweep (`hyperparameter-tuning.yaml`)

The examples are schema-checked but have not been run on GPU hardware.

## Support

- Job Issues: https://github.com/zyvorai/gryvia/issues
- Documentation: https://github.com/zyvorai/gryvia/docs
