# Integration Examples

Gryvia integrations with popular ML tools and platforms.

> **Status.** Examples only. The `GryviaAIJob` manifests match the CRD schema (checked by tooling), but none of the third-party integrations (JupyterHub, Ray, Kubeflow, Dask, RStudio, MLflow) has been deployed and verified end to end here. `GryviaAIJob` has no `ports` field, so port-forward the pod (or create a Service yourself) to reach the web UIs shown. Gryvia provides no Kubeflow, Ray or Dask operator.

## Available Integrations

### Interactive Development
- [JupyterHub](#jupyterhub) - Multi-user Jupyter notebook server with GPU profiles
- [VSCode Server](#vscode-server) - Browser-based VS Code with GPU access
- [RStudio Server](#rstudio-server) - R development environment

### Distributed Computing
- [Ray Cluster](#ray-cluster) - Distributed computing framework
- [Dask Gateway](#dask-gateway) - Parallel computing with Dask

### ML Platforms
- [MLflow](../../services/mlflow/README.md) - Experiment tracking (design sketch, see that README)

## JupyterHub

Multi-user Jupyter notebook server with GPU profile selection.

### Features

- **GPU Profiles**: 5 pre-configured profiles (CPU, T4, A100-40G, A100-80G, Multi-GPU)
- **Persistent Storage**: 100GB VAST Data storage per user
- **MLflow Integration**: Pre-configured MLflow tracking
- **Idle Culling**: Automatic shutdown after 1 hour idle
- **Gryvia Integration**: Quota enforcement and cost tracking

### Installation

```bash
# Deploy JupyterHub
kubectl apply -f examples/integrations/jupyterhub.yaml

# Get external IP
kubectl get svc -n jupyterhub jupyterhub

# Access JupyterHub
open http://<EXTERNAL-IP>
```

### Usage

1. **Login**: Use any username/password (dummy auth - configure real auth in production)
2. **Select Profile**: Choose GPU profile based on workload
3. **Start Server**: Wait for notebook server to spawn
4. **Open Notebook**: Create new notebook or open existing

### Example Notebook

```python
# notebook.ipynb
import torch
import mlflow

# Check GPU
print(f"CUDA available: {torch.cuda.is_available()}")
print(f"GPU count: {torch.cuda.device_count()}")
print(f"GPU name: {torch.cuda.get_device_name(0)}")

# MLflow tracking
mlflow.set_tracking_uri("http://mlflow.gryvia-system.svc.cluster.local:5000")
mlflow.set_experiment("jupyter-experiments")

with mlflow.start_run():
    # Your training code
    mlflow.log_param("notebook", "true")
    mlflow.log_metric("test_metric", 0.95)
```

### Customization

Edit ConfigMap to customize profiles:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: jupyterhub-config
data:
  jupyterhub_config.py: |
    c.KubeSpawner.profile_list = [
      {
        'display_name': 'Custom H100 Profile',
        'kubespawner_override': {
          'cpu_limit': 32,
          'mem_limit': '256G',
          'extra_resource_limits': {'nvidia.com/gpu': '1'},
          'node_selector': {'gryvia.io/gpu-type': 'H100'}
        }
      }
    ]
```

## VSCode Server

Browser-based VS Code with GPU access.

### Deployment

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: vscode-server
spec:
  type: training
  gpus: 1
  gpuType: A100-80G
  resources:
    requests:
      cpu: "16"
      memory: 64Gi
  image: codercom/code-server:4.96.4
  command:
    - code-server
    - --bind-addr=0.0.0.0:8080
    - --auth=password
    - --disable-telemetry
  env:
    - name: PASSWORD
      value: "change-me-in-production"
  volumeMounts:
    - name: workspace
      mountPath: /home/coder/workspace
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: vscode-workspace
```

### Access

```bash
# Port-forward
kubectl port-forward gryviaaijob/vscode-server 8080:8080

# Open browser
open http://localhost:8080
```

## Ray Cluster

Distributed computing with GPU support.

### Deployment

```yaml
apiVersion: ray.io/v1alpha1
kind: RayCluster
metadata:
  name: ray-cluster
spec:
  rayVersion: '2.9.0'
  headGroupSpec:
    rayStartParams:
      dashboard-host: '0.0.0.0'
    template:
      spec:
        containers:
          - name: ray-head
            image: rayproject/ray:2.9.0-py310-gpu
            resources:
              limits:
                cpu: 8
                memory: 32Gi
              requests:
                cpu: 4
                memory: 16Gi

  workerGroupSpecs:
    - replicas: 4
      minReplicas: 1
      maxReplicas: 10
      groupName: gpu-workers
      rayStartParams: {}
      template:
        spec:
          containers:
            - name: ray-worker
              image: rayproject/ray:2.9.0-py310-gpu
              resources:
                limits:
                  cpu: 16
                  memory: 128Gi
                  nvidia.com/gpu: 2
                requests:
                  cpu: 8
                  memory: 64Gi
                  nvidia.com/gpu: 2
          nodeSelector:
            gryvia.io/gpu-type: A100-80G
```

### Usage

```python
import ray
from ray.util.accelerators import NVIDIA_TESLA_A100

# Connect to cluster
ray.init(address="ray://ray-cluster:10001")

# GPU task
@ray.remote(num_gpus=1)
def train_model():
    import torch
    device = torch.device("cuda")
    # Training code
    return "trained"

# Distributed training
futures = [train_model.remote() for _ in range(4)]
results = ray.get(futures)
```

## Kubeflow Integration

### Pipeline Example

```python
from kfp import dsl
from kfp import components

# Gryvia job component
@dsl.component
def gryvia_training_op(
    model: str,
    dataset: str,
    gpu_type: str,
    gpu_count: int
):
    import subprocess

    job_yaml = f"""
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: kfp-training
spec:
  type: training
  gpus: {gpu_count}
  gpuType: {gpu_type}
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command:
    - python
    - train.py
    - --model={model}
    - --dataset={dataset}
"""

    subprocess.run(["kubectl", "apply", "-f", "-"], input=job_yaml.encode())

# Pipeline
@dsl.pipeline(
    name='Gryvia Training Pipeline',
    description='Training pipeline using Gryvia'
)
def training_pipeline():
    train_op = gryvia_training_op(
        model="llama-7b",
        dataset="openwebtext",
        gpu_type="A100-80G",
        gpu_count=8
    )
```

## Dask Gateway

Distributed computing with Dask.

### Configuration

```yaml
gateway:
  backend:
    scheduler:
      cores:
        request: 4
        limit: 8
      memory:
        request: 16G
        limit: 32G

    worker:
      cores:
        request: 8
        limit: 16
      memory:
        request: 32G
        limit: 64G
      extraPodConfig:
        nodeSelector:
          gryvia.io/gpu: "true"
```

### Usage

```python
from dask_gateway import Gateway

gateway = Gateway()
cluster = gateway.new_cluster()
cluster.scale(10)  # 10 GPU workers

client = cluster.get_client()

# Distributed computation
import dask.array as da

x = da.random.random((10000, 10000), chunks=(1000, 1000))
result = x.mean().compute()
```

## RStudio Server

R development environment with GPU support.

### Deployment

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: rstudio-server
spec:
  type: training
  gpus: 1
  gpuType: A100-40G
  resources:
    requests:
      cpu: "16"
      memory: 64Gi
  image: rocker/ml-gpu:4.4.0
  command:
    - /init
  env:
    - name: PASSWORD
      value: "change-me"
```

### R GPU Example

```r
# Install packages
install.packages("tensorflow")
tensorflow::install_tensorflow(version = "gpu")

# Check GPU
library(tensorflow)
tf$config$list_physical_devices("GPU")

# Train model
model <- keras_model_sequential() %>%
  layer_dense(units = 128, activation = "relu", input_shape = c(784)) %>%
  layer_dense(units = 10, activation = "softmax")

model %>% compile(
  optimizer = "adam",
  loss = "categorical_crossentropy",
  metrics = c("accuracy")
)

model %>% fit(x_train, y_train, epochs = 10, batch_size = 32)
```

## Best Practices

### Resource Management

1. **Profile Selection**: Choose smallest GPU that meets requirements
2. **Idle Timeout**: Set reasonable timeouts to avoid waste
3. **Quotas**: Assign team quotas to prevent overuse
4. **Monitoring**: Track usage with Gryvia cost tracking

### Security

1. **Authentication**: Use real auth (OAuth, LDAP) in production
2. **Network Policies**: Restrict network access
3. **RBAC**: Limit user permissions
4. **Secrets**: Store credentials in Kubernetes Secrets

### Cost Optimization

1. **Preemptible Workers**: Use spot instances for Ray/Dask workers
2. **Auto-scaling**: Configure auto-scaling for dynamic workloads
3. **Storage Tiers**: Use appropriate storage class
4. **Idle Culling**: Automatically shutdown idle sessions

## Troubleshooting

### JupyterHub Issues

```bash
# Check hub logs
kubectl logs -n jupyterhub deployment/jupyterhub

# Check user pod
kubectl get pods -n jupyterhub
kubectl logs -n jupyterhub jupyter-<username>

# Reset user
kubectl delete pod -n jupyterhub jupyter-<username>
```

### Ray Cluster Issues

```bash
# Check head node
kubectl logs ray-cluster-head-xxxxx

# Check worker status
kubectl exec -it ray-cluster-head-xxxxx -- ray status

# Check dashboard
kubectl port-forward svc/ray-cluster-head 8265:8265
open http://localhost:8265
```

## Support

- Integration Issues: https://github.com/zyvorai/gryvia/issues
- Documentation: https://github.com/zyvorai/gryvia/docs
