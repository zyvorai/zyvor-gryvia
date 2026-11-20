## Gryvia Examples

This directory contains example configurations for running AI workloads on Gryvia.

> **Status.** Every manifest is schema-checked against `crds/` by `scripts/check-examples.py`, which only proves the YAML is valid, not that a controller acts on it. 43 of the 49 kinds have a controller (the [README](../README.md) lists them; several are opt-in, for example job hooks, datasets, reservations, GPU sharing, GPU health, the model watch, RAG and agents). The other six, `GryviaGpuSku`, `GryviaNetworkRate`, `GryviaNetworkUsageRecord`, `GryviaNodeFabric`, and the opt-in billing records `GryviaLedgerEntry` and `GryviaInvoice`, are data that controllers or the gateway write and read. Nothing here has been run on real GPU or RDMA hardware except where stated.

### Training Examples

#### Simple Single-GPU Training
```bash
kubectl apply -f training/simple-pytorch-training.yaml
kubectl get gryviaaijob pytorch-simple-training
kubectl logs -f $(kubectl get pod -l gryvia.io/job=pytorch-simple-training -o name)
```

#### Multi-GPU Distributed Training
```bash
# Create PVCs for data and checkpoints first
kubectl apply -f distributed/multi-gpu-training.yaml
kubectl get gryviaaijob distributed-llama-training
```

### Inference Examples

#### LLM Inference with vLLM
```bash
kubectl apply -f inference/llm-inference.yaml
kubectl get gryviaaijob llama-inference

# Port forward to access the API
kubectl port-forward svc/llama-inference-headless 8000:8000

# Test the inference endpoint
curl http://localhost:8000/v1/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama-70b",
    "prompt": "Explain quantum computing in simple terms:",
    "max_tokens": 100
  }'
```

### Model factory

Fine-tune, evaluate and serve every new open model that passes your filters (opt-in: `aiOperator.modelWatch.enabled`).
See [model-factory/README.md](model-factory/README.md); unverified on GPUs.

```bash
kubectl apply -f model-factory/model-watch.yaml
gryvia models watch runs open-llms -n ml-team
```

### GPU Node Configuration

#### Register a GPU Node
```bash
kubectl apply -f demo/demo.yaml           # fake nodes for a GPU-less cluster
kubectl apply -f complete-setup/production-deployment.yaml   # includes a GryviaGpuNode
kubectl get gryviagpunode
# On a cluster with NVIDIA GPU Feature Discovery labels, the gpu-operator can also
# register nodes automatically (--auto-register).
```

### Storage Configuration

#### Setup VAST Storage
```bash
kubectl apply -f storage/vast-storage-example.yaml
kubectl get gryviastorage
```

### Network Configuration

#### Setup RDMA Network
```bash
kubectl apply -f network/rdma-network-example.yaml
kubectl get gryvianetwork
```

### Quotas

#### Set Team GPU Quota
```bash
kubectl apply -f quota/team-ml-quota.yaml
kubectl get gryviaquota
```

## Best Practices

1. **Always specify GPU type** for predictable performance
2. **Use RDMA networking** for multi-GPU training
3. **Mount fast storage** for data-intensive workloads
4. **Set resource limits** to prevent resource exhaustion
5. **Use priorities** to manage job scheduling
6. **Enable retries** for fault tolerance

## Monitoring

Watch job progress:
```bash
kubectl get gryviaaijob -w
```

Check GPU utilization:
```bash
kubectl top node -l gryvia.io/gpu=true
```

View metrics in Grafana:
```bash
kubectl port-forward -n gryvia-system svc/gryvia-observability-grafana 3000:80
# Open http://localhost:3000
```
