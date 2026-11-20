# GPU Advanced Features

> **Status: guidance built on NVIDIA tooling; Gryvia only labels nodes, NVIDIA's components partition the GPUs.**
> MIG partitioning is done by NVIDIA's GPU Operator / mig-manager (the optional `nvidia.enabled` sub-chart
> in `helm/gryvia`, or your own install). `GryviaGPUSharingPolicy` (the real name of the CRD shown as
> `GPUSharingPolicy` below) has an opt-in controller in the gpu-operator (`gpuOperator.gpuSharing`, flag
> `--enable-gpu-sharing`) that labels matching GPU nodes for NVIDIA's device plugin and mig-manager
> (`nvidia.com/device-plugin.config`, `nvidia.com/mig.config`; see
> [NVIDIA one-click](../website/docs/guides/NVIDIA_ONE_CLICK.md)). `GryviaAIJob` has no `mig`
> field (only `spec.gpus` and `spec.gpuType`). Design-sketch snippets are marked as such. The MIG profile
> table follows NVIDIA's A100/H100 80GB documentation. All dollar figures and savings percentages (for
> example "$3.43/hour", "86%") are made-up worked examples, not Gryvia pricing or measurements. Unverified
> on real MIG-capable hardware.

## Multi-Instance GPU (MIG) Support

Split A100/H100 GPUs into smaller instances for better utilization and cost savings.

### What is MIG?

MIG allows a single A100 or H100 GPU to be partitioned into up to 7 separate instances, each with dedicated:
- GPU compute (SM slices)
- Memory
- Cache
- Memory bandwidth

**Benefits:**
- **Higher Utilization**: Use GPU for multiple small workloads
- **Cost Savings**: Pay for what you need (85% cheaper for small instances)
- **Quality of Service**: Isolated instances, no interference
- **Multi-Tenancy**: Safe sharing of expensive GPUs

### MIG Profiles

#### A100-80GB / H100-80GB

| Profile | Compute | Memory | Instances | Use Cases |
|---------|---------|--------|-----------|-----------|
| 1g.10gb | 1/7 GPU | 10GB | 7 | Inference, dev, small models |
| 2g.20gb | 2/7 GPU | 20GB | 3 | Medium models, fine-tuning |
| 3g.40gb | 3/7 GPU | 40GB | 2 | Large models, training |
| 4g.40gb | 4/7 GPU | 40GB | 1 | Very large models |
| 7g.80gb | Full GPU | 80GB | 1 | Largest models |

### Setup MIG

#### 1. Enable MIG on Nodes

```bash
# Label MIG-capable nodes
kubectl label nodes gpu-node-1 gryvia.io/mig-capable=true

# Deploy MIG manager
kubectl apply -f gpu-features/mig-support.yaml
```

#### 2. Configure MIG Profile

```bash
# Option 1: All 1g.10gb instances (7 instances per GPU)
kubectl label nodes gpu-node-1 nvidia.com/mig.config=all-1g.10gb

# Option 2: Mixed profile
kubectl label nodes gpu-node-1 nvidia.com/mig.config=mixed-3g-1g

# Option 3: Balanced
kubectl label nodes gpu-node-1 nvidia.com/mig.config=balanced
```

#### 3. Verify MIG Instances

```bash
# Check available MIG instances
kubectl describe node gpu-node-1 | grep nvidia.com/mig

# Output:
# nvidia.com/mig-1g.10gb: 56  # 8 GPUs × 7 instances
# nvidia.com/mig-2g.20gb: 0
# nvidia.com/mig-3g.40gb: 0
```

### Using MIG in Jobs

#### Request MIG Instance

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: small-inference
spec:
  framework: pytorch
  resources:
    mig:
      profile: 1g.10gb
      count: 1
    memory: 8Gi
    cpu: 2
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command: ["python", "inference.py"]
```

#### Using kubectl Directly

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: mig-pod
spec:
  containers:
    - name: app
      image: nvcr.io/nvidia/pytorch:24.01-py3
      resources:
        limits:
          nvidia.com/mig-1g.10gb: 1
```

### Cost Savings with MIG

#### Example: Inference Workload

**Without MIG:**
- Full A100-80G: $24/hour
- Utilization: 15% (wasted capacity)
- Cost for 100 inference instances: $2,400/hour

**With MIG (1g.10gb):**
- MIG 1g.10gb: $3.43/hour per instance
- 100 instances: $343/hour
- **Savings: $2,057/hour (86%)**

#### Real-World Scenarios

**Scenario 1: Development Team (10 developers)**
- Without MIG: 10 × $24/hour = $240/hour
- With MIG (1g.10gb): 10 × $3.43/hour = $34.30/hour
- **Savings: $205.70/hour (86%)**

**Scenario 2: Inference Farm (50 models)**
- Without MIG: 50 × $24/hour = $1,200/hour
- With MIG (mixed profiles):
  - 40 × 1g.10gb = $137.20/hour
  - 10 × 2g.20gb = $68.60/hour
  - Total: $205.80/hour
- **Savings: $994.20/hour (83%)**

### MIG Strategies

#### 1. All Small Instances (all-1g.10gb)

**Use when:**
- Many small workloads
- Development/testing
- Inference serving
- Cost optimization priority

**Configuration:**
```yaml
mig-devices:
  "1g.10gb": 7  # Per GPU
```

**Result:** 56 instances from 8 GPUs

#### 2. Mixed Profile (mixed-3g-1g)

**Use when:**
- Mix of workload sizes
- Some larger training jobs
- Flexibility needed

**Configuration:**
```yaml
mig-devices:
  "3g.40gb": 2
  "1g.10gb": 1
```

**Result:** 16 large + 8 small instances

#### 3. Balanced (balanced)

**Use when:**
- Medium-sized workloads
- Fine-tuning common
- Balance cost and capability

**Configuration:**
```yaml
mig-devices:
  "2g.20gb": 3
  "1g.10gb": 1
```

**Result:** 24 medium + 8 small instances

### Dynamic MIG Reconfiguration

Change MIG profile on the fly:

```bash
# Drain node
kubectl drain gpu-node-1 --ignore-daemonsets

# Change MIG profile
kubectl label nodes gpu-node-1 \
  nvidia.com/mig.config=all-1g.10gb --overwrite

# Wait for reconfiguration
kubectl wait --for=condition=mig-configured node/gpu-node-1

# Uncordon node
kubectl uncordon gpu-node-1
```

### Monitoring MIG

#### GPU Metrics per MIG Instance

```prometheus
# MIG instance utilization
nvidia_mig_gpu_utilization{instance_id="0", profile="1g.10gb"} 85.2

# MIG memory usage
nvidia_mig_memory_used_bytes{instance_id="0", profile="1g.10gb"} 8.5e9

# MIG power consumption
nvidia_mig_power_usage_watts{instance_id="0"} 45.2
```

#### Grafana Dashboard

```json
{
  "panels": [
    {
      "title": "MIG Instance Utilization",
      "targets": [{
        "expr": "avg(nvidia_mig_gpu_utilization) by (profile)"
      }]
    },
    {
      "title": "MIG Instances Available",
      "targets": [{
        "expr": "sum(kube_node_status_capacity{resource=~\"nvidia.com/mig-.*\"}) by (resource)"
      }]
    }
  ]
}
```

### Auto-Scaling with MIG

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: mig-inference-hpa
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: inference-service
  minReplicas: 2
  maxReplicas: 50  # Can scale to 50 MIG instances
  metrics:
    - type: Resource
      resource:
        name: nvidia.com/mig-1g.10gb
        target:
          type: Utilization
          averageUtilization: 80
```

### Best Practices

1. **Right-Size Workloads**
   ```bash
   # Profile your workload first
   nvidia-smi dmon -s um

   # Choose smallest MIG profile that fits
   ```

2. **Use MIG for Development**
   - Developers get isolated GPU access
   - Much cheaper than full GPUs
   - No interference between users

3. **Inference Serving**
   - MIG is perfect for inference
   - Low latency, QoS guaranteed
   - Huge cost savings

4. **Mix Profiles**
   - Don't use all-1g.10gb for everything
   - Match profile to workload
   - Reconfigure based on demand

5. **Monitor Utilization**
   - Track per-instance metrics
   - Identify underutilized instances
   - Adjust profiles accordingly

### Limitations

**MIG Cannot:**
- ❌ Span multiple physical GPUs
- ❌ Use NVLink between instances
- ❌ Share memory between instances
- ❌ Be used with GPU Direct RDMA

**When NOT to use MIG:**
- Large-scale distributed training (use full GPUs)
- NVLink-dependent workloads
- Workloads needing >40GB memory per instance
- Maximum performance requirements

### Troubleshooting

#### MIG Instances Not Available

```bash
# Check MIG is enabled
nvidia-smi -i 0 --query-gpu=mig.mode.current --format=csv

# Should show: Enabled

# If not, enable MIG
sudo nvidia-smi -i 0 -mig 1

# Reboot node
sudo reboot
```

#### Job Not Scheduling

```bash
# Check available MIG instances
kubectl describe node gpu-node-1 | grep mig

# Check requested profile
kubectl describe gryviaaijob my-job | grep mig

# Verify profile exists
nvidia-smi mig -lgi
```

#### Poor Performance

```bash
# Check MIG instance utilization
nvidia-smi -i 0 --query-compute-apps=pid,used_memory --format=csv

# Ensure not over-subscribed
# Each MIG instance should have ≤1 workload
```

## GPU Sharing (Without MIG)

For GPUs that don't support MIG (V100, T4), use time-sharing:

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GPUSharingPolicy
metadata:
  name: gpu-sharing
spec:
  # Enable GPU sharing
  enabled: true

  # Max pods per GPU
  maxPodsPerGPU: 4

  # Scheduling strategy
  strategy: time-sharing

  # Resource allocation
  memoryLimit: 25%  # Each pod gets 25% memory

  # Supported GPUs
  gpuTypes:
    - V100
    - T4
```

**Note:** Time-sharing has more overhead than MIG and no QoS guarantees.

## Support

- MIG Issues: https://github.com/zyvorai/gryvia/issues
- NVIDIA MIG Docs: https://docs.nvidia.com/datacenter/tesla/mig-user-guide/
