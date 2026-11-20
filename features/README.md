# Advanced Features

> **Status: partly implemented.** The ai-operator runs `GryviaWorkflow`: `dependsOn`, the condition
> `steps.<name>.status == 'Succeeded'`, retries, timeouts, a cron `schedule` and step outputs work (see
> [ML controllers](../docs/ml-controllers.md) and [Model factory](../docs/model-factory.md)). Fan-out and the
> richer retry strategies and conditions shown below are design. The `kfctl` CLI in this file does not exist
> (the `gryvia` CLI has no `workflow` command), there is no Argo export, and the `gryvia_workflow_*` metrics
> are not exported. Outputs and numbers are illustrative. Features that do
> have a registered controller are documented in their own pages in this directory (checkpoint guard,
> cost predictor, GPU memory optimizer, live experiment, model lineage, training profiler, training time
> machine); budget management and priority/preemption are mostly designs (see their status notes).

## Job Dependencies and DAG Workflows

Build complex ML workflows with job dependencies and conditional execution.

### Features

- **DAG Workflows**: Define directed acyclic graphs of jobs
- **Dependencies**: Jobs wait for upstream completion
- **Conditional Execution**: Run jobs based on conditions
- **Fan-out/Fan-in**: Parallel execution with aggregation
- **Dynamic DAGs**: Generate workflows programmatically
- **Cross-Workflow Dependencies**: Jobs depend on other workflows
- **Scheduled Workflows**: Cron-based execution
- **Retry Strategies**: Automatic retries with backoff

### Simple Linear Workflow

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaWorkflow
metadata:
  name: training-pipeline
spec:
  jobs:
    - name: preprocess
      template:
        command: [python, preprocess.py]

    - name: train
      dependsOn: [preprocess]
      template:
        command: [python, train.py]

    - name: evaluate
      dependsOn: [train]
      template:
        command: [python, evaluate.py]
```

**Execution**: preprocess → train → evaluate

### Conditional Execution

```yaml
jobs:
  - name: train
    template:
      command: [python, train.py]

  - name: deploy
    dependsOn: [train]
    condition: "{{jobs.train.metrics.accuracy}} > 0.95"
    template:
      command: [python, deploy.py]
```

**Deploy only if accuracy > 95%**

### Fan-out/Fan-in Pattern

```yaml
jobs:
  # Generate 10 configs
  - name: generate-configs
    template:
      command: [python, generate_configs.py]

  # Train 10 models in parallel
  - name: train-{{item}}
    dependsOn: [generate-configs]
    forEach:
      items: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9]
      parallelism: 5  # 5 at a time
    template:
      command: [python, train.py, --config={{item}}]

  # Aggregate results
  - name: select-best
    dependsOn: [train-*]  # Wait for all
    template:
      command: [python, select_best.py]
```

### Adaptive Retry

```yaml
jobs:
  - name: train-small-gpu
    template:
      resources:
        gpuType: A100-40G
        gpuCount: 4

  - name: train-large-gpu
    dependsOn: [train-small-gpu]
    condition: |
      {{jobs.train-small-gpu.status}} == 'Failed' AND
      {{jobs.train-small-gpu.error}} contains 'out of memory'
    template:
      resources:
        gpuType: A100-80G  # Retry with more memory
        gpuCount: 4
```

### Scheduled Workflows

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaWorkflow
metadata:
  name: daily-retraining
spec:
  schedule: "0 2 * * *"  # Daily at 2 AM
  concurrencyPolicy: Forbid

  jobs:
    - name: fetch-data
      template:
        command: [python, fetch.py]

    - name: train
      dependsOn: [fetch-data]
      template:
        command: [python, train.py]
```

### Cross-Workflow Dependencies

```yaml
# Workflow B depends on Workflow A
spec:
  jobs:
    - name: use-model
      dependsOn:
        - workflow: train-workflow
          job: export-model
      template:
        command: [python, use_model.py]
```

### Dynamic DAG

```yaml
jobs:
  # Plan determines which jobs to run
  - name: plan
    template:
      command: [python, plan.py, --output=/plan.json]

  # Jobs created dynamically
  - name: dynamic-{{item.name}}
    dependsOn: [plan]
    forEach:
      items: "{{jobs.plan.output.jobs}}"
    template:
      framework: "{{item.framework}}"
      resources:
        gpuType: "{{item.gpu_type}}"
      command: "{{item.command}}"
```

### Error Handling

```yaml
jobs:
  - name: train
    template:
      command: [python, train.py]
    retryStrategy:
      limit: 3
      backoff:
        duration: 5m
        factor: 2  # 5m, 10m, 20m

  # Run cleanup on failure
  - name: cleanup
    dependsOn: [train]
    condition: "{{jobs.train.status}} == 'Failed'"
    template:
      command: [python, cleanup.py]
```

## CLI Commands

Proposed interface only; `kfctl` does not exist.

```bash
# Submit workflow
kfctl workflow submit pipeline.yaml

# List workflows
kfctl workflow list

# Get status
kfctl workflow status training-pipeline

# View execution graph
kfctl workflow graph training-pipeline

# Output:
# validate-data (✓ Succeeded, 5m)
#   └─> preprocess-data (⚙ Running, 12m)
#       └─> train-model (⏳ Pending)
#           └─> evaluate-model (⏳ Pending)
#               ├─> export-onnx (⏳ Pending)
#               └─> export-tensorrt (⏳ Pending)
#                   └─> deploy-model (⏳ Pending)

# View logs for specific job
kfctl workflow logs training-pipeline train-model

# Cancel workflow
kfctl workflow cancel training-pipeline

# Retry failed jobs
kfctl workflow retry training-pipeline

# Pause workflow
kfctl workflow pause training-pipeline

# Resume workflow
kfctl workflow resume training-pipeline

# Export workflow definition
kfctl workflow export training-pipeline > workflow.yaml
```

## Workflow Patterns

### 1. Linear Pipeline
```
A → B → C → D
```

### 2. Parallel Branches
```
    ┌─> B ─┐
A ─┤      ├─> E
    └─> C ─┘
     └─> D ─┘
```

### 3. Diamond
```
    ┌─> B ─┐
A ─┤      ├─> D
    └─> C ─┘
```

### 4. Fan-out/Fan-in
```
    ┌─> B1 ─┐
    ├─> B2 ─┤
A ─┼─> B3 ─┼─> C
    ├─> B4 ─┤
    └─> B5 ─┘
```

## Best Practices

1. **Keep DAGs Simple**: Avoid overly complex dependencies
2. **Use Conditions**: Skip unnecessary work
3. **Limit Parallelism**: Don't overwhelm cluster
4. **Add Retries**: Handle transient failures
5. **Set Timeouts**: Prevent hanging workflows
6. **Use Templates**: Reuse job definitions
7. **Monitor Progress**: Track workflow execution
8. **Handle Failures**: Define cleanup jobs

## Monitoring

### Grafana Dashboard

```json
{
  "panels": [
    {
      "title": "Workflow Status",
      "targets": [{
        "expr": "sum(gryvia_workflow_jobs_total) by (status)"
      }]
    },
    {
      "title": "Workflow Duration",
      "targets": [{
        "expr": "histogram_quantile(0.95, gryvia_workflow_duration_seconds_bucket)"
      }]
    }
  ]
}
```

### Metrics

```prometheus
# Total workflows
gryvia_workflows_total 150

# Workflow status
gryvia_workflow_status{workflow="training-pipeline",status="running"} 1

# Job dependencies
gryvia_workflow_dependencies_total{workflow="training-pipeline"} 7

# Workflow duration
gryvia_workflow_duration_seconds{workflow="training-pipeline"} 3600
```

## Integration with Argo

Proposed (not implemented): export to Argo Workflows:

```bash
# Convert to Argo
kfctl workflow export training-pipeline --format argo > argo-workflow.yaml

# Submit to Argo
argo submit argo-workflow.yaml
```

## Examples

See `features/job-dependencies.yaml` for complete examples:
- Simple linear workflows
- Conditional execution
- Fan-out/Fan-in
- Hyperparameter sweeps
- Scheduled retraining
- Dynamic DAGs
- Error handling

## Support

- Workflow Issues: https://github.com/zyvorai/gryvia/issues
- Workflow Patterns: https://github.com/zyvorai/gryvia/discussions
