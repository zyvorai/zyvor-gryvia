# MLflow Integration

> **Status: integration sketch, not implemented in this repo.** This directory holds only this README and `deploy.yaml` (example manifests for an MLflow server). They are not part of the Helm chart and have not been verified on a live cluster. The replica counts, storage sizes and the VAST Data backend are example choices, not defaults Gryvia sets up. No Gryvia component logs to MLflow automatically.

MLflow tracking server for experiment management and model registry.

## Features

- **Experiment Tracking**: Log parameters, metrics, and artifacts
- **Model Registry**: Version and manage ML models
- **High Availability**: 2 replicas with PostgreSQL backend
- **Scalable Storage**: VAST Data for artifacts (500GB default)
- **Prometheus Metrics**: Integrated monitoring

## Architecture

```
┌─────────────────┐
│   AI Jobs       │
│  (PyTorch,      │
│   TensorFlow)   │
└────────┬────────┘
         │ Log experiments
         ↓
┌─────────────────────────────────┐
│     MLflow Server (2 replicas)  │
│  ┌───────────┐  ┌──────────┐  │
│  │ Tracking  │  │  Model   │  │
│  │  Server   │  │ Registry │  │
│  └───────────┘  └──────────┘  │
└────┬────────────────────┬──────┘
     │                    │
     ↓                    ↓
┌──────────┐      ┌──────────────┐
│PostgreSQL│      │  Artifacts   │
│ Backend  │      │  (VAST Data) │
└──────────┘      └──────────────┘
```

## Installation

### Deploy MLflow

```bash
# Create namespace
kubectl create namespace gryvia-system

# Deploy MLflow stack
kubectl apply -f services/mlflow/deploy.yaml

# Wait for deployment
kubectl wait --for=condition=available --timeout=300s \
  deployment/mlflow-server -n gryvia-system
```

### Verify Installation

```bash
# Check pods
kubectl get pods -n gryvia-system -l app=mlflow-server

# Check service
kubectl get svc -n gryvia-system mlflow
```

### Access MLflow UI

```bash
# Port-forward
kubectl port-forward -n gryvia-system svc/mlflow 5000:5000

# Open browser
open http://localhost:5000
```

## Usage

### Python Client

```python
import mlflow
import mlflow.pytorch

# Set tracking URI
mlflow.set_tracking_uri("http://mlflow.gryvia-system.svc.cluster.local:5000")

# Create experiment
mlflow.set_experiment("llama-training")

# Start run
with mlflow.start_run(run_name="run-001"):
    # Log parameters
    mlflow.log_param("learning_rate", 3e-4)
    mlflow.log_param("batch_size", 32)
    mlflow.log_param("model", "llama-7b")

    # Train model
    # ...

    # Log metrics
    mlflow.log_metric("train_loss", 0.234, step=100)
    mlflow.log_metric("val_accuracy", 0.876, step=100)

    # Log model
    mlflow.pytorch.log_model(model, "model")

    # Log artifacts
    mlflow.log_artifact("config.yaml")
```

### Using in AI Jobs

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: mlflow-training
spec:
  framework: pytorch
  resources:
    gpuType: A100-80G
    gpuCount: 8
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command:
    - python
    - train.py
  env:
    - name: MLFLOW_TRACKING_URI
      value: "http://mlflow.gryvia-system.svc.cluster.local:5000"
    - name: MLFLOW_EXPERIMENT_NAME
      value: "distributed-training"
```

## Model Registry

### Register Model

```python
import mlflow

# Register model from run
mlflow.set_tracking_uri("http://mlflow:5000")

run_id = "abc123def456"
model_uri = f"runs:/{run_id}/model"

# Register
mlflow.register_model(model_uri, "llama-7b")
```

### Promote Model to Production

```python
from mlflow.tracking import MlflowClient

client = MlflowClient("http://mlflow:5000")

# Transition to production
client.transition_model_version_stage(
    name="llama-7b",
    version=3,
    stage="Production"
)
```

### Load Production Model

```python
import mlflow.pyfunc

model = mlflow.pyfunc.load_model(
    model_uri="models:/llama-7b/Production"
)

predictions = model.predict(data)
```

## Auto-Logging

### PyTorch

```python
import mlflow.pytorch

mlflow.pytorch.autolog()

# Training automatically logged
trainer = Trainer(model, train_loader)
trainer.fit()
```

### TensorFlow

```python
import mlflow.tensorflow

mlflow.tensorflow.autolog()

# Training automatically logged
model.fit(x_train, y_train, epochs=10)
```

## Advanced Features

### Nested Runs

```python
# Parent run for hyperparameter sweep
with mlflow.start_run(run_name="hp-sweep"):
    for lr in [1e-3, 1e-4, 1e-5]:
        # Child run for each trial
        with mlflow.start_run(run_name=f"lr-{lr}", nested=True):
            mlflow.log_param("learning_rate", lr)
            # Train and log metrics
```

### Custom Metrics

```python
# Log custom metrics
mlflow.log_metric("gpu_utilization", 95.2)
mlflow.log_metric("throughput_samples_per_sec", 1250)
mlflow.log_metric("cost_per_epoch", 12.50)
```

### Artifacts Organization

```python
# Log files
mlflow.log_artifact("model.onnx")
mlflow.log_artifact("tokenizer.json")

# Log directory
mlflow.log_artifacts("checkpoints/", artifact_path="checkpoints")
```

## Querying Experiments

### Search Runs

```python
from mlflow.tracking import MlflowClient

client = MlflowClient("http://mlflow:5000")

# Search by metric
runs = client.search_runs(
    experiment_ids=["1"],
    filter_string="metrics.val_accuracy > 0.90",
    order_by=["metrics.val_accuracy DESC"],
    max_results=10
)

for run in runs:
    print(f"Run: {run.info.run_id}, Accuracy: {run.data.metrics['val_accuracy']}")
```

### Compare Runs

```python
import mlflow

# Get experiment
experiment = mlflow.get_experiment_by_name("llama-training")

# Get all runs
runs = mlflow.search_runs(
    experiment_ids=[experiment.experiment_id],
    order_by=["metrics.val_accuracy DESC"]
)

print(runs[['params.learning_rate', 'metrics.val_accuracy', 'metrics.train_loss']])
```

## Monitoring

### Metrics Endpoint

MLflow exposes metrics at `/metrics` for Prometheus scraping.

### Grafana Dashboard

Import dashboard from `monitoring/grafana-dashboards/mlflow-dashboard.json`

**Panels:**
- Active experiments
- Total runs
- Storage usage
- API request rate
- Query latency

## Backup & Restore

### Backup Database

```bash
# Backup PostgreSQL
kubectl exec -n gryvia-system mlflow-postgres-xxx -- \
  pg_dump -U mlflow mlflow > mlflow-backup.sql

# Backup artifacts
kubectl exec -n gryvia-system mlflow-server-xxx -- \
  tar -czf /tmp/artifacts.tar.gz /mlflow/artifacts
```

### Restore Database

```bash
# Restore PostgreSQL
kubectl exec -i -n gryvia-system mlflow-postgres-xxx -- \
  psql -U mlflow mlflow < mlflow-backup.sql
```

## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `MLFLOW_TRACKING_URI` | Tracking server URL | Required |
| `MLFLOW_EXPERIMENT_NAME` | Experiment name | `Default` |
| `MLFLOW_RUN_NAME` | Run name | Auto-generated |
| `MLFLOW_TRACKING_USERNAME` | Auth username | - |
| `MLFLOW_TRACKING_PASSWORD` | Auth password | - |

### Resource Limits

Edit deployment to adjust resources:

```yaml
resources:
  requests:
    memory: 4Gi
    cpu: 2
  limits:
    memory: 8Gi
    cpu: 4
```

## Troubleshooting

### Connection Refused

```bash
# Check service
kubectl get svc -n gryvia-system mlflow

# Test connectivity
kubectl run -it --rm debug --image=curlimages/curl -- \
  curl http://mlflow.gryvia-system.svc.cluster.local:5000/health
```

### Storage Full

```bash
# Check PVC usage
kubectl exec -n gryvia-system mlflow-server-xxx -- df -h /mlflow/artifacts

# Resize PVC
kubectl patch pvc mlflow-artifacts-pvc -n gryvia-system \
  -p '{"spec":{"resources":{"requests":{"storage":"1Ti"}}}}'
```

### Slow Queries

```bash
# Check PostgreSQL logs
kubectl logs -n gryvia-system deployment/mlflow-postgres

# Optimize database
kubectl exec -n gryvia-system mlflow-postgres-xxx -- \
  psql -U mlflow -c "VACUUM ANALYZE;"
```

## Best Practices

1. **Organize Experiments**: Use meaningful experiment names
2. **Tag Runs**: Add tags for filtering (`team`, `project`, `gpu_type`)
3. **Version Models**: Always use model registry for production models
4. **Clean Up**: Archive old experiments regularly
5. **Monitor Costs**: Track experiment costs with Gryvia cost tracking

## Integration with Gryvia

All AI jobs can automatically log to MLflow:

```yaml
# Enable auto-logging
spec:
  mlflow:
    enabled: true
    experimentName: "my-experiment"
    trackingUri: "http://mlflow:5000"
```

## Support

- MLflow Docs: https://mlflow.org/docs/latest/index.html
- Gryvia Issues: https://github.com/zyvorai/gryvia/issues
