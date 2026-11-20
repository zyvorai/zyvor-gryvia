# Gryvia Workflow Examples

Argo Workflows integration for complex ML pipelines.

> **Status.** These are Argo `Workflow` manifests, not Gryvia's own `GryviaWorkflow` (which the ai-operator runs; see [ML workflow examples](../ml-workflow/README.md) and [ML controllers](../../docs/ml-controllers.md)). They have not been run against a live Argo and Gryvia install here, and Gryvia ships no Argo integration code. The MLflow server they mention is an example sketch (`services/mlflow/`), not something Gryvia deploys. Argo's install URL and version are pinned to what the README author used; check them before use.

## Prerequisites

Install Argo Workflows:

```bash
kubectl create namespace argo
kubectl apply -n argo -f https://github.com/argoproj/argo-workflows/releases/download/v3.5.4/install.yaml
```

## Workflows

### 1. Training Pipeline (training-pipeline.yaml)

Complete ML training pipeline with data preparation, training, evaluation, and export.

**Stages:**
1. Data Preparation
2. Distributed Training
3. Model Evaluation
4. Model Export (ONNX)

**Submit:**
```bash
argo submit -n default examples/workflows/training-pipeline.yaml \
  --parameter model-name=gpt-2 \
  --parameter dataset=openwebtext \
  --parameter gpu-type=H100 \
  --parameter gpu-count=8
```

**Monitor:**
```bash
argo watch -n default ml-training-pipeline
argo logs -n default ml-training-pipeline
```

### 2. Hyperparameter Sweep (hyperparameter-sweep.yaml)

Parallel hyperparameter optimization with MLflow tracking.

**Features:**
- Random search over hyperparameter space
- Parallel trial execution
- MLflow experiment tracking
- Automatic best model selection

**Submit:**
```bash
argo submit -n default examples/workflows/hyperparameter-sweep.yaml \
  --parameter model=resnet50 \
  --parameter dataset=imagenet \
  --parameter trials=20
```

**View Results:**
```bash
# Port-forward MLflow
kubectl port-forward svc/mlflow 5000:5000

# Open http://localhost:5000
```

## Workflow Patterns

### Sequential Steps

```yaml
steps:
  - - name: step1
      template: task1
  - - name: step2
      template: task2
  - - name: step3
      template: task3
```

### Parallel Steps

```yaml
steps:
  - - name: parallel-task1
      template: task1
    - name: parallel-task2
      template: task2
    - name: parallel-task3
      template: task3
```

### Conditional Execution

```yaml
steps:
  - - name: train
      template: training
  - - name: evaluate
      template: evaluation
      when: "{{steps.train.status}} == Succeeded"
```

### Parameter Sweeps

```yaml
steps:
  - - name: parallel-trials
      template: trial
      arguments:
        parameters:
          - name: lr
            value: "{{item.lr}}"
      withParam: "{{workflow.parameters.configs}}"
```

## Best Practices

### 1. Resource Management

Set resource limits for each step:

```yaml
resources:
  limits:
    memory: 512Gi
    cpu: 64
    nvidia.com/gpu: 8
```

### 2. Error Handling

Use retry policies:

```yaml
retryStrategy:
  limit: 3
  retryPolicy: "Always"
  backoff:
    duration: "1m"
    factor: 2
    maxDuration: "10m"
```

### 3. Data Management

Use PVCs for sharing data:

```yaml
volumeClaimTemplates:
  - metadata:
      name: workspace
    spec:
      accessModes: ["ReadWriteMany"]
      resources:
        requests:
          storage: 1Ti
```

### 4. Monitoring

Add labels for tracking:

```yaml
metadata:
  labels:
    team: ml-research
    project: llama-training
    experiment: exp-001
```

### 5. Cost Optimization

Use appropriate GPU types:

```yaml
# Development/testing
gpuType: T4

# Training
gpuType: A100-80G

# Large-scale training
gpuType: H100
```

## Advanced Examples

### Data Parallel Training

```yaml
- name: data-parallel
  parallelism: 4
  steps:
    - - name: shard-{{item}}
        template: train-shard
        arguments:
          parameters:
            - name: shard
              value: "{{item}}"
        withSequence:
          count: "4"
```

### Model Pipeline

```yaml
- name: model-pipeline
  dag:
    tasks:
      - name: preprocess
        template: preprocess-data

      - name: train
        dependencies: [preprocess]
        template: train-model

      - name: evaluate
        dependencies: [train]
        template: evaluate-model

      - name: deploy
        dependencies: [evaluate]
        template: deploy-model
        when: "{{tasks.evaluate.outputs.parameters.accuracy}} > 0.95"
```

### A/B Testing

```yaml
- name: ab-test
  steps:
    - - name: train-model-a
        template: training
        arguments:
          parameters:
            - name: config
              value: "config-a.yaml"

      - name: train-model-b
        template: training
        arguments:
          parameters:
            - name: config
              value: "config-b.yaml"

    - - name: compare-models
        template: comparison
```

## Integration with Gryvia

Workflow steps that create `GryviaAIJob` resources go through the normal Gryvia path, so the ai-operator (node selection), quota enforcement, usage metering and Prometheus metrics apply to those jobs. Plain Argo pods that do not create a `GryviaAIJob` are outside Gryvia's quota and metering.

## Troubleshooting

### View Workflow Status

```bash
argo list -n default
argo get -n default <workflow-name>
```

### Debug Failed Steps

```bash
argo logs -n default <workflow-name> <step-name>
kubectl describe gryviaaijob <job-name>
```

### Retry Failed Workflow

```bash
argo retry -n default <workflow-name>
```

### Delete Workflow

```bash
argo delete -n default <workflow-name>
```

## CI/CD Integration

### GitHub Actions

```yaml
name: ML Training Pipeline

on:
  push:
    branches: [main]

jobs:
  submit-workflow:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3

      - name: Submit Argo Workflow
        env:
          # Pass untrusted values through env, never interpolate them into the script.
          MODEL_NAME: ${{ github.sha }}
        run: |
          argo submit -n default workflows/training-pipeline.yaml \
            --parameter model-name="$MODEL_NAME" \
            --wait
```

### GitLab CI

```yaml
train:
  stage: train
  script:
    - argo submit -n default workflows/training-pipeline.yaml --wait
  only:
    - main
```

## Support

- Argo Workflows Docs: https://argoproj.github.io/argo-workflows/
- Gryvia Issues: https://github.com/zyvorai/gryvia/issues
