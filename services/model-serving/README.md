# Model Serving with Gryvia

> **Status: design/integration sketch, not implemented in this repo.** This directory holds only this README, `kserve-integration.yaml` (example manifests) and a Dockerfile that references a `requirements.txt` that does not exist; there is no source code. Gryvia does not deploy or manage KServe. Model serving itself is `GryviaInferenceService`, run by the ai-operator (vLLM, Triton, TensorRT-LLM or TorchServe Deployments with a canary; see [ML controllers](../../docs/ml-controllers.md)); this directory is not part of it. Latency and throughput figures below are design targets, not measurements.

Deploy and serve models trained with Gryvia using KServe, vLLM, TensorRT-LLM, and Triton.

## Overview

```
┌──────────────────┐
│  Training Jobs   │
│  (Gryvia)    │
└────────┬─────────┘
         │
         ↓
┌──────────────────┐
│     MLflow       │
│  Model Registry  │
└────────┬─────────┘
         │
         ↓
┌──────────────────┐
│  Model Export    │
│   Job            │
└────────┬─────────┘
         │
         ↓
┌──────────────────┐
│  InferenceService│
│   (KServe)       │
└──────────────────┘
```

## Serving Frameworks

### vLLM (Recommended for LLMs)

High-throughput serving for large language models.

**Features:**
- PagedAttention for efficient memory usage
- Continuous batching
- Tensor parallelism
- OpenAI-compatible API

**Deployment:**

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: llama-7b-vllm
spec:
  predictor:
    containers:
      - name: kserve-container
        image: vllm/vllm-openai:v0.6.6
        args:
          - --model=/mnt/models/llama-7b
          - --tensor-parallel-size=2
          - --max-model-len=4096
        resources:
          limits:
            nvidia.com/gpu: "2"
    nodeSelector:
      gryvia.io/gpu-type: A100-80G
```

**Usage:**

```bash
# Deploy
kubectl apply -f vllm-inference.yaml

# Get service URL
kubectl get inferenceservice llama-7b-vllm

# Test inference
curl http://llama-7b-vllm.default.svc.cluster.local/v1/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama-7b",
    "prompt": "Once upon a time",
    "max_tokens": 100
  }'
```

### TensorRT-LLM

Optimized inference with NVIDIA TensorRT.

**Features:**
- INT8/FP16 quantization
- Multi-GPU support
- Custom CUDA kernels
- Best latency for NVIDIA GPUs

**Build TensorRT Engine:**

```bash
# Submit build job
kubectl apply -f - <<EOF
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: build-trt-llm
spec:
  framework: pytorch
  resources:
    gpuType: A100-80G
    gpuCount: 1
  image: nvcr.io/nvidia/tensorrt-llm:24.01
  command:
    - python
    - /app/tensorrt_llm/examples/llama/build.py
    - --model_dir=/mnt/models/llama-7b
    - --output_dir=/mnt/trt-engines/llama-7b
    - --dtype=float16
    - --use_gpt_attention_plugin=float16
    - --use_gemm_plugin=float16
    - --max_batch_size=128
  volumeMounts:
    - name: models
      mountPath: /mnt/models
    - name: engines
      mountPath: /mnt/trt-engines
EOF
```

**Deploy:**

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: llama-7b-trt
spec:
  predictor:
    containers:
      - name: kserve-container
        image: nvcr.io/nvidia/tensorrt-llm:24.01
        args:
          - --model=/mnt/trt-engines/llama-7b
          - --max-batch-size=128
        resources:
          limits:
            nvidia.com/gpu: "1"
```

### Triton Inference Server

Multi-framework serving platform.

**Features:**
- Support for TensorFlow, PyTorch, ONNX, TensorRT
- Dynamic batching
- Model ensemble
- HTTP/gRPC endpoints

**Model Repository Structure:**

```
/mnt/models/
├── model_a/
│   ├── config.pbtxt
│   └── 1/
│       └── model.pt
├── model_b/
│   ├── config.pbtxt
│   └── 1/
│       └── model.onnx
└── ensemble/
    └── config.pbtxt
```

**Config Example:**

```protobuf
# config.pbtxt
name: "llama-7b"
platform: "pytorch_libtorch"
max_batch_size: 128
input [
  {
    name: "input_ids"
    data_type: TYPE_INT64
    dims: [ -1 ]
  }
]
output [
  {
    name: "output"
    data_type: TYPE_FP32
    dims: [ -1, 50257 ]
  }
]
instance_group [
  {
    count: 1
    kind: KIND_GPU
  }
]
dynamic_batching {
  preferred_batch_size: [ 8, 16, 32 ]
  max_queue_delay_microseconds: 100
}
```

## Model Export Pipeline

### From MLflow to Serving

**1. Export Model from MLflow:**

```python
# export_model.py
import mlflow
import torch
import os

mlflow.set_tracking_uri("http://mlflow.gryvia-system.svc.cluster.local:5000")

# Load production model
model_uri = "models:/llama-7b/Production"
model = mlflow.pytorch.load_model(model_uri)

# Save in serving format
output_dir = "/mnt/serving-models/llama-7b"
os.makedirs(output_dir, exist_ok=True)

# Save model
torch.save(model.state_dict(), f"{output_dir}/model.pt")

# Save config
config = {
    "model_type": "llama",
    "hidden_size": 4096,
    "num_layers": 32,
    # ... other config
}

with open(f"{output_dir}/config.json", "w") as f:
    json.dump(config, f)

print(f"Model exported to {output_dir}")
```

**2. Run Export Job:**

```bash
kubectl apply -f - <<EOF
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: export-llama-7b
spec:
  framework: pytorch
  resources:
    gpuType: T4
    gpuCount: 1
  image: python:3.11
  command:
    - python
    - export_model.py
  volumeMounts:
    - name: serving-models
      mountPath: /mnt/serving-models
EOF
```

**3. Deploy InferenceService:**

```bash
kubectl apply -f kserve-integration.yaml
```

## Deployment Strategies

### Canary Deployment

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: llama-7b
spec:
  predictor:
    canaryTrafficPercent: 20  # 20% to canary
    containers:
      - name: kserve-container
        image: vllm/vllm-openai:v0.3.0  # New version
  transformer:
    containers:
      - name: kserve-container
        image: vllm/vllm-openai:v0.2.7  # Stable version
```

### Blue-Green Deployment

```bash
# Deploy green (new version)
kubectl apply -f llama-7b-green.yaml

# Test green
curl http://llama-7b-green.default/v1/completions ...

# Switch traffic
kubectl patch inferenceservice llama-7b \
  -p '{"spec":{"predictor":{"serviceURL":"llama-7b-green"}}}'

# Remove blue
kubectl delete inferenceservice llama-7b-blue
```

### A/B Testing

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: llama-comparison
spec:
  predictor:
    containers:
      - name: model-a
        image: vllm/vllm-openai:v0.6.6
        env:
          - name: MODEL_VARIANT
            value: "optimized"
  transformer:
    containers:
      - name: model-b
        image: vllm/vllm-openai:v0.6.6
        env:
          - name: MODEL_VARIANT
            value: "standard"
  # Traffic split via Istio/Knative
```

## Auto-scaling

### GPU-based Autoscaling

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: llama-7b-hpa
spec:
  scaleTargetRef:
    apiVersion: serving.kserve.io/v1beta1
    kind: InferenceService
    name: llama-7b
  minReplicas: 1
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: nvidia.com/gpu
        target:
          type: Utilization
          averageUtilization: 80
    - type: Pods
      pods:
        metric:
          name: inference_requests_per_second
        target:
          type: AverageValue
          averageValue: "100"
```

### Scale-to-Zero

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: kserve-autoscaler-config
  namespace: knative-serving
data:
  scale-to-zero-grace-period: "30s"
  scale-to-zero-pod-retention-period: "5m"
  enable-scale-to-zero: "true"
```

## Monitoring

### Metrics

KServe exports Prometheus metrics:

```
# Inference requests
kserve_inference_requests_total{model="llama-7b",status="success"} 1245

# Latency
kserve_inference_latency_seconds{model="llama-7b",quantile="0.95"} 0.234

# GPU utilization
kserve_gpu_utilization_percent{model="llama-7b",gpu="0"} 85.5
```

### Grafana Dashboard

```bash
# Import dashboard
kubectl apply -f monitoring/grafana-dashboards/kserve-inference.json
```

**Panels:**
- Requests per second
- P50/P95/P99 latency
- GPU utilization
- Throughput (tokens/sec)
- Error rate
- Cost per 1M tokens

## Performance Optimization

### Batching

```yaml
# Dynamic batching with Triton
dynamic_batching {
  preferred_batch_size: [ 8, 16, 32 ]
  max_queue_delay_microseconds: 100
}
```

### Quantization

```bash
# INT8 quantization with TensorRT
python build.py \
  --model_dir=/mnt/models/llama-7b \
  --dtype=int8 \
  --use_weight_only \
  --weight_only_precision=int8
```

### Tensor Parallelism

```yaml
args:
  - --tensor-parallel-size=4  # Split across 4 GPUs
```

### KV Cache Optimization

```yaml
args:
  - --max-model-len=4096
  - --gpu-memory-utilization=0.90
  - --swap-space=4  # GB of CPU swap space
```

## Cost Optimization

### GPU Selection

```yaml
# Development/testing - T4
nodeSelector:
  gryvia.io/gpu-type: T4

# Production - A100
nodeSelector:
  gryvia.io/gpu-type: A100-40G

# High throughput - H100
nodeSelector:
  gryvia.io/gpu-type: H100
```

### Cost Tracking

```bash
# Track inference costs
python3 tools/cost-calculator.py \
  --namespace default \
  --filter serving.kserve.io/inferenceservice=llama-7b

# Estimate cost per 1M tokens
python3 tools/cost-calculator.py \
  --estimate-tokens \
  --throughput 100 \
  --gpu-type A100-80G
```

## Security

### Authentication

```yaml
apiVersion: security.istio.io/v1beta1
kind: RequestAuthentication
metadata:
  name: kserve-jwt
spec:
  selector:
    matchLabels:
      serving.kserve.io/inferenceservice: llama-7b
  jwtRules:
    - issuer: "https://auth.example.com"
      jwksUri: "https://auth.example.com/.well-known/jwks.json"
```

### Authorization

```yaml
apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: kserve-authz
spec:
  selector:
    matchLabels:
      serving.kserve.io/inferenceservice: llama-7b
  rules:
    - from:
        - source:
            principals: ["cluster.local/ns/default/sa/api-gateway"]
```

## Troubleshooting

### High Latency

```bash
# Check GPU utilization
kubectl exec -it <pod> -- nvidia-smi

# Check batch size
kubectl logs <pod> | grep "batch"

# Enable profiling
kubectl set env deployment/<name> CUDA_LAUNCH_BLOCKING=1
```

### OOM Errors

```bash
# Reduce max_model_len
--max-model-len=2048

# Reduce batch size
--max-batch-size=64

# Enable CPU swap
--swap-space=8
```

### Slow Cold Start

```yaml
# Keep warm instances
spec:
  predictor:
    minReplicas: 2  # Always keep 2 running
```

## Examples

See `examples/model-serving/` for:
- vLLM deployment
- TensorRT-LLM optimization
- Triton ensemble models
- A/B testing setup
- Cost optimization configs

## Support

- KServe Docs: https://kserve.github.io/website/
- Gryvia Issues: https://github.com/zyvorai/gryvia/issues
