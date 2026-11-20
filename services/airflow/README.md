# Apache Airflow Integration

> **Status: design/integration sketch, not implemented in this repo.** This directory holds only this README and `integration.yaml` (example manifests). No Airflow operator, provider package or DAG code ships here, and the manifests are not part of the Helm chart. Nothing below has been tested against a real Airflow deployment. The pattern it describes (an Airflow task creating a `GryviaAIJob` with the Kubernetes API) is plausible with the CRD that does exist, but treat all code as illustrative.

Orchestrate ML data pipelines with Apache Airflow and Gryvia.

## Overview

```
┌──────────────────────────────────────────────────────────┐
│                   Airflow DAG                             │
│  ┌─────────┐  ┌──────────┐  ┌─────────┐  ┌──────────┐  │
│  │Validate │→ │Preprocess│→ │ Train   │→ │Evaluate  │  │
│  │  Data   │  │   (GPU)  │  │(8xGPU)  │  │  Model   │  │
│  └─────────┘  └──────────┘  └─────────┘  └──────────┘  │
└──────────────────────────────────────────────────────────┘
         │              │              │
         ↓              ↓              ↓
┌────────────────────────────────────────────────────────┐
│              Gryvia Cluster                        │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐            │
│  │   CPU    │  │  1x GPU  │  │  8x GPU  │            │
│  │   Pod    │  │   Job    │  │   Job    │            │
│  └──────────┘  └──────────┘  └──────────┘            │
└────────────────────────────────────────────────────────┘
```

## Features

- **GPU-Aware Scheduling**: Schedule tasks to GPU nodes
- **Gryvia Integration**: Submit jobs via GryviaAIJob CRD
- **Data Validation**: Validate datasets before training
- **Cost Tracking**: Track pipeline costs
- **MLflow Integration**: Auto-register models
- **Failure Handling**: Retry failed tasks

## Installation

```bash
# Deploy Airflow
kubectl apply -f services/airflow/integration.yaml

# Wait for deployment
kubectl wait --for=condition=available --timeout=300s \
  deployment/airflow-webserver -n airflow

# Get service URL
kubectl get svc -n airflow airflow-webserver
```

## Access Airflow UI

```bash
# Port-forward
kubectl port-forward -n airflow svc/airflow-webserver 8080:8080

# Open browser
open http://localhost:8080

# Login: admin / admin
```

## Example DAGs

### 1. ML Training Pipeline

```python
# dags/ml_training_pipeline.py
from datetime import datetime, timedelta
from airflow import DAG
from airflow.providers.cncf.kubernetes.operators.kubernetes_pod import KubernetesPodOperator

default_args = {
    'owner': 'ml-team',
    'retries': 2,
    'retry_delay': timedelta(minutes=5),
}

dag = DAG(
    'ml_training',
    default_args=default_args,
    schedule_interval='@daily',
    start_date=datetime(2024, 1, 1),
)

# GPU preprocessing
preprocess = KubernetesPodOperator(
    task_id='preprocess',
    name='preprocess',
    namespace='default',
    image='nvcr.io/nvidia/pytorch:24.01-py3',
    cmds=['python', 'preprocess.py'],
    resources={
        'limit_nvidia.com/gpu': '1'
    },
    node_selector={'gryvia.io/gpu-type': 'T4'},
    dag=dag,
)

# Multi-GPU training
train = KubernetesPodOperator(
    task_id='train',
    name='train',
    namespace='default',
    image='nvcr.io/nvidia/pytorch:24.01-py3',
    cmds=['torchrun', '--nproc_per_node=8', 'train.py'],
    resources={
        'limit_nvidia.com/gpu': '8'
    },
    node_selector={'gryvia.io/gpu-type': 'A100-80G'},
    dag=dag,
)

preprocess >> train
```

### 2. Data Pipeline with Validation

```python
# dags/data_pipeline.py
from airflow import DAG
from airflow.operators.python import PythonOperator
from airflow.providers.cncf.kubernetes.operators.kubernetes_pod import KubernetesPodOperator

def validate_schema():
    import pandas as pd
    df = pd.read_parquet('/data/dataset.parquet')
    assert 'features' in df.columns
    assert 'labels' in df.columns
    print("Schema validated")

dag = DAG('data_pipeline', schedule_interval='@hourly')

validate = PythonOperator(
    task_id='validate',
    python_callable=validate_schema,
    dag=dag,
)

transform = KubernetesPodOperator(
    task_id='transform',
    name='transform',
    image='python:3.11',
    cmds=['python', 'transform.py'],
    dag=dag,
)

validate >> transform
```

### 3. Hyperparameter Sweep

```python
# dags/hp_sweep.py
from airflow import DAG
from airflow.operators.python import PythonOperator
from airflow.providers.cncf.kubernetes.operators.kubernetes_pod import KubernetesPodOperator

dag = DAG('hp_sweep', schedule_interval=None)

def generate_configs():
    import json
    configs = []
    for lr in [1e-3, 1e-4, 1e-5]:
        for bs in [16, 32, 64]:
            configs.append({'lr': lr, 'batch_size': bs})
    return configs

generate = PythonOperator(
    task_id='generate',
    python_callable=generate_configs,
    dag=dag,
)

# Dynamic task generation
configs = generate_configs()
for i, config in enumerate(configs):
    train_task = KubernetesPodOperator(
        task_id=f'train_{i}',
        name=f'train-{i}',
        image='nvcr.io/nvidia/pytorch:24.01-py3',
        cmds=['python', 'train.py'],
        arguments=[
            f'--lr={config["lr"]}',
            f'--batch-size={config["batch_size"]}'
        ],
        resources={'limit_nvidia.com/gpu': '1'},
        dag=dag,
    )
    generate >> train_task
```

## Gryvia Integration

### Submit GryviaAIJob from Airflow

```python
from kubernetes import client, config

def submit_gryvia_job(**context):
    config.load_incluster_config()

    job_spec = {
        'apiVersion': 'gryvia.io/v1alpha1',
        'kind': 'GryviaAIJob',
        'metadata': {
            'name': 'airflow-training',
            'namespace': 'default'
        },
        'spec': {
            'framework': 'pytorch',
            'resources': {
                'gpuType': 'A100-80G',
                'gpuCount': 8
            },
            'image': 'nvcr.io/nvidia/pytorch:24.01-py3',
            'command': ['python', 'train.py']
        }
    }

    api = client.CustomObjectsApi()
    api.create_namespaced_custom_object(
        group='gryvia.io',
        version='v1alpha1',
        namespace='default',
        plural='gryviaaijobs',
        body=job_spec
    )

    return 'airflow-training'

submit_job = PythonOperator(
    task_id='submit_job',
    python_callable=submit_gryvia_job,
    dag=dag,
)
```

### Wait for Job Completion

```python
from airflow.sensors.python import PythonSensor

def check_job_status(**context):
    config.load_incluster_config()
    api = client.CustomObjectsApi()

    job = api.get_namespaced_custom_object(
        group='gryvia.io',
        version='v1alpha1',
        namespace='default',
        plural='gryviaaijobs',
        name='airflow-training'
    )

    status = job.get('status', {}).get('phase')
    return status == 'Succeeded'

wait_job = PythonSensor(
    task_id='wait_job',
    python_callable=check_job_status,
    poke_interval=30,
    timeout=3600,
    dag=dag,
)

submit_job >> wait_job
```

## Cost Tracking

### Track Pipeline Costs

```python
def calculate_cost(**context):
    from datetime import datetime

    # Get job metrics
    api = client.CustomObjectsApi()
    job = api.get_namespaced_custom_object(
        group='gryvia.io',
        version='v1alpha1',
        namespace='default',
        plural='gryviaaijobs',
        name='airflow-training'
    )

    metrics = job.get('status', {}).get('metrics', {})
    duration_hours = metrics.get('runningTime', 0) / 3600
    gpu_count = job['spec']['resources']['gpuCount']
    gpu_type = job['spec']['resources']['gpuType']

    # Calculate cost
    pricing = {
        'H100': 30.0,
        'A100-80G': 24.0,
        'A100-40G': 12.0,
        'T4': 3.0
    }

    hourly_rate = pricing.get(gpu_type, 12.0)
    total_cost = hourly_rate * gpu_count * duration_hours

    print(f"Pipeline cost: ${total_cost:.2f}")

    # Store in XCom
    context['task_instance'].xcom_push(key='cost', value=total_cost)

    return total_cost

cost_tracker = PythonOperator(
    task_id='track_cost',
    python_callable=calculate_cost,
    dag=dag,
)
```

## Data Validation

### Great Expectations Integration

```python
from airflow.operators.python import PythonOperator

def validate_with_ge():
    import great_expectations as ge

    # Load dataset
    df = ge.read_csv('/data/dataset.csv')

    # Expectations
    df.expect_column_values_to_not_be_null('features')
    df.expect_column_values_to_be_between('labels', 0, 9)

    # Validate
    result = df.validate()

    if not result['success']:
        raise ValueError("Data validation failed")

    return result

validate = PythonOperator(
    task_id='validate',
    python_callable=validate_with_ge,
    dag=dag,
)
```

## Monitoring

### Send Alerts on Failure

```python
from airflow.operators.email import EmailOperator

alert = EmailOperator(
    task_id='alert',
    to='team@example.com',
    subject='Training Pipeline Failed',
    html_content='''
        <h3>Training Pipeline Failed</h3>
        <p>DAG: {{ dag.dag_id }}</p>
        <p>Task: {{ task.task_id }}</p>
        <p>Execution Time: {{ execution_date }}</p>
    ''',
    trigger_rule='one_failed',
    dag=dag,
)
```

### Metrics to Prometheus

```python
from prometheus_client import Counter, Gauge

pipeline_runs = Counter('airflow_pipeline_runs_total', 'Total pipeline runs')
pipeline_duration = Gauge('airflow_pipeline_duration_seconds', 'Pipeline duration')

def report_metrics(**context):
    pipeline_runs.inc()
    duration = context['task_instance'].duration
    pipeline_duration.set(duration)
```

## Best Practices

1. **Resource Limits**: Always set GPU limits
2. **Cost Tracking**: Monitor pipeline costs
3. **Data Validation**: Validate before training
4. **Retries**: Configure retries for transient failures
5. **Idempotency**: Make tasks idempotent
6. **Monitoring**: Set up alerts for failures
7. **Testing**: Test DAGs in development first

## Troubleshooting

### Task Failed to Schedule

```bash
# Check scheduler logs
kubectl logs -n airflow deployment/airflow-scheduler

# Check worker pods
kubectl get pods -n airflow

# Check task logs
kubectl logs -n airflow <worker-pod>
```

### GPU Not Available

```bash
# Check GPU nodes
kfctl cluster nodes

# Check node selector
kubectl describe pod <worker-pod> -n airflow
```

## Examples

See `services/airflow/examples/` for:
- ML training pipelines
- Data preprocessing workflows
- Hyperparameter sweeps
- Model deployment pipelines
- Cost optimization examples

## Support

- Airflow Docs: https://airflow.apache.org/docs/
- Gryvia Issues: https://github.com/zyvorai/gryvia/issues
