# GPU Benchmarking Suite

Standard ML and GPU benchmarks for validating Gryvia cluster performance.

> **Status: benchmark job templates; no results are checked in.** `suite.yaml` defines `GryviaAIJob` and
> plain `Job` templates (the `GryviaAIJob`s were corrected to the real schema: `spec.gpus`,
> `spec.gpuType`, `distributed.*`) that have not been run on GPU hardware for this repo. Every
> "Expected" figure and the whole "Baseline Results" section (A100/H100 throughput, NCCL bandwidth,
> scaling efficiency) are reference/design targets, not measurements from Gryvia, and are not the official
> MLPerf results. The MLPerf names refer to the model/workload only; nothing here is an MLPerf submission.
> Files referenced but not in this directory: `run-all.sh`, `scaling-benchmark.yaml`,
> `nccl-benchmark.yaml`, `memory-benchmark.yaml`, `scripts/check-regression.py`,
> `results/baseline.json`; the `kfctl` commands do not exist. Benchmark jobs and scripts inside
> `suite.yaml` are unverified.

## Available Benchmarks

### ML Training Benchmarks

#### 1. MLPerf ResNet50
- **Dataset**: ImageNet
- **Model**: ResNet-50
- **Configurations**: 1, 2, 4, 8 GPUs
- **Expected**: 9,000 images/sec (8x A100-80G)

```bash
# Run benchmark
kubectl apply -f benchmarks/suite.yaml

# Check results
kubectl logs bench-resnet50-8gpu
```

#### 2. MLPerf BERT
- **Dataset**: Wikipedia
- **Model**: BERT-Large
- **Expected**: 1,100 samples/sec (8x A100-80G)

#### 3. GPT-2 Training
- **Model**: GPT-2 (1.5B parameters)
- **Expected**: 500 tokens/sec (8x A100-80G)

### GPU Performance Benchmarks

#### 1. NCCL Collective Operations
Tests communication performance for distributed training.

**Operations:**
- All-Reduce
- All-Gather
- Broadcast
- Reduce-Scatter

**Expected Bandwidth:**
- NVLink: 300 GB/s
- InfiniBand: 180 GB/s
- Ethernet: 90 GB/s

```bash
# Run NCCL benchmark
kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: nccl-allreduce
spec:
  template:
    spec:
      containers:
      - name: nccl-test
        image: nvcr.io/nvidia/pytorch:24.01-py3
        command: ["/usr/local/bin/all_reduce_perf", "-b", "8M", "-e", "2G", "-g", "8"]
        resources:
          limits:
            nvidia.com/gpu: "8"
      nodeSelector:
        gryvia.io/gpu-type: A100-80G
      restartPolicy: Never
EOF
```

#### 2. GPU Memory Bandwidth
Tests PCIe and GPU memory bandwidth.

**Expected Bandwidth:**
- H100: 3.35 TB/s
- A100-80G: 2.0 TB/s
- A100-40G: 1.6 TB/s
- V100: 900 GB/s
- T4: 320 GB/s

#### 3. GPU Compute (GEMM)
Matrix multiplication performance.

**Expected TFLOPS:**
- H100 (FP16): 1,979 TFLOPS
- A100-80G (FP16): 312 TFLOPS
- A100-40G (FP16): 312 TFLOPS

### Scaling Benchmarks

#### Multi-Node Scaling
Test scaling efficiency across multiple nodes.

**Configurations:**
- 1 node (8 GPUs)
- 2 nodes (16 GPUs)
- 4 nodes (32 GPUs)
- 8 nodes (64 GPUs)

**Expected Scaling Efficiency:**
- 2 nodes: 95%
- 4 nodes: 90%
- 8 nodes: 85%

```bash
# Run scaling benchmark
kubectl apply -f benchmarks/scaling-benchmark.yaml
```

## Running Benchmarks

### Quick Start

```bash
# Run all benchmarks
./benchmarks/run-all.sh

# Run specific benchmark
kubectl apply -f benchmarks/suite.yaml

# View results
kubectl logs -f bench-resnet50-8gpu

# Export results
kubectl logs bench-resnet50-8gpu > results.txt
```

### Benchmark Script

```bash
#!/bin/bash
# benchmarks/run-all.sh

echo "Running Gryvia Benchmark Suite..."

# ResNet50
echo "1. ResNet50 (8 GPU)"
kubectl apply -f suite.yaml
kubectl wait --for=condition=complete job/bench-resnet50-8gpu --timeout=3600s
kubectl logs bench-resnet50-8gpu

# NCCL
echo "2. NCCL All-Reduce"
kubectl apply -f nccl-benchmark.yaml
kubectl wait --for=condition=complete job/bench-nccl-allreduce --timeout=600s
kubectl logs bench-nccl-allreduce

# Memory Bandwidth
echo "3. Memory Bandwidth"
kubectl apply -f memory-benchmark.yaml
kubectl wait --for=condition=complete job/bench-memory-bandwidth --timeout=300s
kubectl logs bench-memory-bandwidth

# Scaling
echo "4. Multi-Node Scaling"
for nodes in 1 2 4; do
    echo "  Testing $nodes nodes..."
    kubectl apply -f scaling-benchmark-${nodes}node.yaml
    kubectl wait --for=condition=complete job/bench-scaling-${nodes}node --timeout=3600s
    kubectl logs bench-scaling-${nodes}node
done

echo "Benchmarks complete!"
```

## Interpreting Results

### Performance Thresholds

**Excellent (≥95% of expected):**
- System is optimally configured
- No action needed

**Good (85-95%):**
- Minor optimizations possible
- Review NCCL settings
- Check network configuration

**Poor (<85%):**
- Investigate bottlenecks
- Check driver versions
- Review GPU topology
- Verify network bandwidth

### Common Issues

#### Low Training Throughput

**Possible Causes:**
- Small batch size
- Slow data loading
- CPU bottleneck
- Network issues

**Solutions:**
```bash
# Increase batch size
--batch-size=2048

# More data workers
--num-workers=16

# Enable pinned memory
--pin-memory

# Faster data format
# Convert to LMDB or TFRecord
```

#### Low NCCL Bandwidth

**Possible Causes:**
- InfiniBand not enabled
- Wrong network interface
- NCCL misconfigured

**Solutions:**
```bash
# Verify InfiniBand
ibstat

# Set NCCL interface
export NCCL_IB_HCA=mlx5

# Enable NCCL debugging
export NCCL_DEBUG=INFO
```

#### Poor Scaling

**Possible Causes:**
- Network bottleneck
- Imbalanced data
- Synchronization overhead

**Solutions:**
```bash
# Gradient accumulation
--gradient-accumulation-steps=4

# Reduce sync frequency
--log-interval=100

# Use async updates (if applicable)
```

## Baseline Results

### A100-80G (8 GPUs, NVLink)

```
ResNet50:
  Batch Size: 2048
  Throughput: 9,200 images/sec
  Time/Iteration: 22.3 ms

BERT-Large:
  Batch Size: 192
  Throughput: 1,150 samples/sec
  Time/Iteration: 167 ms

NCCL All-Reduce:
  8MB: 12.5 GB/s
  128MB: 285 GB/s
  2GB: 295 GB/s

Memory Bandwidth:
  Host→Device: 25 GB/s
  Device→Host: 26 GB/s
  Device→Device: 1.95 TB/s

Scaling (GPT-2):
  1 node (8 GPU): 500 tokens/sec (100%)
  2 nodes (16 GPU): 950 tokens/sec (95%)
  4 nodes (32 GPU): 1,800 tokens/sec (90%)
```

### H100 (8 GPUs, NVSwitch)

```
ResNet50:
  Throughput: 14,500 images/sec (+58% vs A100)

BERT-Large:
  Throughput: 1,850 samples/sec (+61% vs A100)

NCCL All-Reduce:
  2GB: 450 GB/s (+53% vs A100)

Memory Bandwidth:
  Device→Device: 3.3 TB/s (+69% vs A100)
```

## Continuous Benchmarking

### Daily Benchmarks

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: daily-benchmarks
  namespace: gryvia-system
spec:
  schedule: "0 2 * * *"  # Daily at 2 AM
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: benchmark
            image: gryvia/benchmark-suite:1.0.0
            command: ["/benchmarks/run-all.sh"]
          restartPolicy: OnFailure
```

### Regression Detection

Monitor performance over time:

```python
# scripts/check-regression.py
import json
from datetime import datetime, timedelta

def check_regression(current, baseline, threshold=0.05):
    """Check if performance regressed by >5%"""
    regression = (baseline - current) / baseline

    if regression > threshold:
        print(f"⚠️  REGRESSION DETECTED: {regression:.1%}")
        print(f"Baseline: {baseline:.2f}")
        print(f"Current: {current:.2f}")
        return False

    print(f"✓ Performance OK: {current:.2f} ({regression:+.1%})")
    return True

# Load results
with open('results/latest.json') as f:
    current = json.load(f)

with open('results/baseline.json') as f:
    baseline = json.load(f)

# Check
check_regression(
    current['resnet50_throughput'],
    baseline['resnet50_throughput']
)
```

## Best Practices

1. **Baseline First**: Establish baseline before changes
2. **Consistent Config**: Use same settings for comparisons
3. **Warmup**: Always warmup before measuring
4. **Multiple Runs**: Average over 3-5 runs
5. **Isolated Testing**: Run on idle cluster
6. **Document Changes**: Track config changes
7. **Regular Cadence**: Benchmark weekly/monthly

## Troubleshooting

### Benchmark Fails to Start

```bash
# Check GPU availability
kfctl cluster nodes

# Check pod events
kubectl describe job bench-resnet50-8gpu

# Check logs
kubectl logs bench-resnet50-8gpu
```

### Inconsistent Results

```bash
# Check GPU clocks
nvidia-smi -q -d CLOCK

# Enable persistence mode
nvidia-smi -pm 1

# Set max clocks
nvidia-smi -lgc 1410
```

### Low Utilization

```bash
# Check batch size
# Increase if GPU util < 80%

# Check data loading
# Use nvprof/nsys to profile

# Check CUDA streams
# Ensure async operations
```

## Custom Benchmarks

### Add Custom Benchmark

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: custom-benchmark
spec:
  type: training
  gpus: 8
  gpuType: A100-80G
  image: my-org/my-benchmark:latest
  command:
    - python
    - benchmark.py
    - --model=custom
    - --measure-performance
```

## References

- MLPerf Training: https://mlcommons.org/en/training-normal-21/
- NCCL Tests: https://github.com/NVIDIA/nccl-tests
- CUDA Samples: https://github.com/NVIDIA/cuda-samples

## Support

- Benchmark Issues: https://github.com/zyvorai/gryvia/issues
- Performance Discussions: https://github.com/zyvorai/gryvia/discussions
