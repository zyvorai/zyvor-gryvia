# Gryvia Job Examples

Example jobs for various AI/ML workloads.

> **Status.** The YAML files in this directory are schema-checked against the `GryviaAIJob` CRD. They have not been run on real GPU hardware; image tags, GPU counts and any throughput or cost figures are illustrative. Job phases are set by the ai-operator; see the operator README for what it actually does.

## Examples

### 1. PyTorch Distributed Training
**File**: `pytorch-distributed.yaml`

Large-scale distributed training using PyTorch DDP on 8x H100 GPUs.

```bash
kubectl apply -f pytorch-distributed.yaml
```

Features:
- Distributed Data Parallel (DDP)
- NCCL optimizations
- RDMA networking
- Gradient checkpointing
- Mixed precision (FP16)

Use cases:
- Large language model training (LLaMA, GPT)
- Vision transformers
- Multi-modal models

### 2. TensorFlow Multi-Node Training
**File**: `tensorflow-multi-node.yaml`

Multi-node TensorFlow training with parameter server strategy.

```bash
kubectl apply -f tensorflow-multi-node.yaml
```

Features:
- Parameter server strategy
- 4 nodes × 4 GPUs = 16 GPUs total
- COCO dataset training
- EfficientDet model

Use cases:
- Object detection
- Image classification
- Computer vision research

### 3. JAX Inference Server
**File**: `jax-inference.yaml`

High-throughput inference server using JAX.

```bash
kubectl apply -f jax-inference.yaml
```

Features:
- Stable Diffusion XL inference
- LoadBalancer service
- Concurrent request handling
- Model caching

Use cases:
- Image generation
- Real-time inference
- Production serving

### 4. Hyperparameter Tuning
**File**: `hyperparameter-tuning.yaml`

Automated hyperparameter search with Optuna and MLflow.

```bash
kubectl apply -f hyperparameter-tuning.yaml
```

Features:
- Bayesian optimization
- 8 parallel trials
- MLflow experiment tracking
- PostgreSQL backend

Use cases:
- Model optimization
- Architecture search
- AutoML pipelines

## Job Management

### Submit a job
```bash
kubectl apply -f <job-file>.yaml
```

### Check job status
```bash
kubectl get gryviaaijobs
kubectl describe gryviaaijob <job-name>
```

### View job logs
```bash
kubectl logs -l gryvia.io/job=<job-name>
```

### Delete a job
```bash
kubectl delete gryviaaijob <job-name>
```

### Monitor GPU usage
```bash
kubectl exec -it <pod-name> -- nvidia-smi
```

## Advanced Usage

### Using the CLI
```bash
# Submit job
gryvia submit -f pytorch-distributed.yaml

# Check status
gryvia status pytorch-distributed-training

# Stream logs
gryvia logs pytorch-distributed-training --follow

# Cancel job
gryvia cancel pytorch-distributed-training
```

### Job Dependencies

Job dependencies are not supported: `GryviaAIJob` has no `dependsOn` field. Use a workflow tool such as Argo or Airflow (see `services/airflow/`) or submit jobs in sequence.

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: inference-job
spec:
  dependsOn:
    - training-job  # Wait for training to complete
  # ... rest of spec
```

### Priority Classes

Set a job priority with the integer `spec.priority`:

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: urgent-job
spec:
  type: training
  gpus: 8
  image: nvcr.io/nvidia/pytorch:24.01-py3
  priority: 100
```

### Resource Preemption

There is no `preemptionPolicy` field on `GryviaAIJob`. Preemption comes from Kubernetes or Kueue: a `GryviaPriority` makes the ai-operator create a Kubernetes PriorityClass of the same name, which a job uses through the annotation `gryvia.io/priority-class: <name>` (batch Job workloads only), and with the opt-in Kueue integration `spec.priority` preempts within a queue.

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: preemptible-job
spec:
  preemptionPolicy: PreemptLowerPriority
  # ... rest of spec
```

## Best Practices

### 1. Resource Requests

Always specify resource requests and limits:
```yaml
resources:
  gpuCount: 8
  memory: 512Gi  # Be generous
  cpu: 64        # 8 CPUs per GPU is common
```

### 2. Volume Mounts

Use persistent volumes for data and checkpoints:
```yaml
volumeMounts:
  - name: data
    mountPath: /data
    readOnly: true
  - name: checkpoints
    mountPath: /checkpoints
```

### 3. Environment Variables

Set framework-specific optimizations:
```yaml
env:
  - name: NCCL_DEBUG
    value: INFO
  - name: PYTORCH_CUDA_ALLOC_CONF
    value: max_split_size_mb:512
```

### 4. Node Selection

Use node selectors for specific hardware:
```yaml
nodeSelector:
  gryvia.io/gpu-type: H100
  gryvia.io/rdma-enabled: "true"
```

### 5. Checkpointing

Implement regular checkpointing:
```bash
--save-steps=1000
--checkpoint-dir=/checkpoints
```

### 6. Monitoring

Add labels for tracking:
```yaml
labels:
  team: ml-research
  project: llama-training
  cost-center: research
```

## Troubleshooting

### Job stuck in Pending

Check quota limits:
```bash
kubectl get gryviaquotas
kubectl describe gryviaquota <quota-name>
```

Check GPU availability:
```bash
kubectl get gryviagpunodes
```

### Out of Memory errors

Reduce batch size or enable gradient checkpointing:
```yaml
command:
  - --batch-size=2  # Reduce from 4
  - --gradient-checkpointing  # Enable
```

### NCCL errors

Check network connectivity:
```bash
kubectl exec -it <pod> -- ping <other-pod-ip>
```

Enable NCCL debug:
```yaml
env:
  - name: NCCL_DEBUG
    value: INFO
```

### Slow training

Check GPU utilization:
```bash
kubectl exec -it <pod> -- nvidia-smi dmon
```

Enable profiling:
```yaml
command:
  - --profile
  - --profile-steps=10,20
```

## Performance Optimization

### Mixed Precision Training
```yaml
command:
  - --fp16  # or --bf16 for better numerical stability
```

### Gradient Accumulation
```yaml
command:
  - --gradient-accumulation-steps=8  # Effective batch size = batch_size * 8
```

### Data Loading
```yaml
env:
  - name: OMP_NUM_THREADS
    value: "8"  # Parallel data loading
```

### NCCL Tuning
```yaml
env:
  - name: NCCL_IB_DISABLE
    value: "0"  # Enable InfiniBand
  - name: NCCL_NET_GDR_LEVEL
    value: "5"  # Enable GPU Direct RDMA
```

## Cost Optimization

Track costs with labels:
```yaml
labels:
  team: ml-research
  cost-center: "12345"
  budget-category: training
```

Use spot instances:
```yaml
nodeSelector:
  gryvia.io/spot: "true"
tolerations:
  - key: gryvia.io/spot
    operator: Exists
```

Set time limits:
```yaml
activeDeadlineSeconds: 3600  # 1 hour max
```

## Support

- Documentation: https://github.com/zyvorai/gryvia/docs
- Issues: https://github.com/zyvorai/gryvia/issues
- Examples: https://github.com/zyvorai/gryvia/tree/main/examples
