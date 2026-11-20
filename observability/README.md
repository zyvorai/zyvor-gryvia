# Advanced Observability

Reference patterns for distributed tracing, log aggregation, and metrics.

> **Status: reference material and templates, not a shipped feature.** No Gryvia
> component (operators, gateway, CLI, collector) emits OpenTelemetry spans or ships a
> Jaeger, OTel collector, Loki or Promtail deployment; `observability/distributed-tracing.yaml`
> is an unapplied manifest template and nothing in the Helm charts installs it. The
> Python snippets below are generic examples for your own training code (the
> `opentelemetry.instrumentation.torch` `TorchInstrumentor` shown in the DDP example is
> illustrative and is not a package this repo provides or verifies). The metric names used
> in the PromQL examples (`gpu_utilization`, `training_loss`, ...) are application-defined
> examples, not Gryvia metrics. What does ship: `helm/observability` (an optional
> kube-prometheus-stack wrapper, not installed by default) and the eBPF collector's own
> Prometheus metrics and flight recorder (collector disabled by default; see `monitoring/`
> for which `gryvia_*` metrics actually exist). The retention numbers under Best Practices
> are suggestions, not defaults. Nothing here was verified against a running cluster.

## Components

### 1. Distributed Tracing (Jaeger + OpenTelemetry)

Track requests across distributed GPU training jobs.

**Features:**
- End-to-end trace visualization
- Performance bottleneck identification
- Dependency mapping
- Latency analysis

**Access Jaeger UI:**
```bash
kubectl port-forward -n gryvia-system svc/jaeger-query 16686:16686
open http://localhost:16686
```

### 2. Log Aggregation (Loki + Grafana)

Centralized log management for all components.

**Install Loki:**
```bash
helm repo add grafana https://grafana.github.io/helm-charts
helm install loki grafana/loki-stack \
  --namespace gryvia-system \
  --set grafana.enabled=false
```

### 3. Metrics (Prometheus + Grafana)

Optional: `helm/observability` wraps kube-prometheus-stack; it is not installed by default. See `monitoring/`.

## Distributed Tracing

### Instrument Training Code

```python
# train.py
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
import os

# Setup tracing
trace.set_tracer_provider(TracerProvider())
tracer = trace.get_tracer(__name__)

# Configure OTLP exporter
otlp_exporter = OTLPSpanExporter(
    endpoint=os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "otel-collector:4317"),
    insecure=True
)

span_processor = BatchSpanProcessor(otlp_exporter)
trace.get_tracer_provider().add_span_processor(span_processor)

def train_epoch(model, dataloader, optimizer):
    with tracer.start_as_current_span("train_epoch") as span:
        span.set_attribute("epoch", epoch)
        span.set_attribute("batch_size", batch_size)

        for batch_idx, (data, target) in enumerate(dataloader):
            with tracer.start_as_current_span("train_batch") as batch_span:
                batch_span.set_attribute("batch_idx", batch_idx)

                # Forward pass
                with tracer.start_as_current_span("forward"):
                    output = model(data)
                    loss = criterion(output, target)

                # Backward pass
                with tracer.start_as_current_span("backward"):
                    loss.backward()

                # Optimizer step
                with tracer.start_as_current_span("optimizer_step"):
                    optimizer.step()
                    optimizer.zero_grad()

                batch_span.set_attribute("loss", float(loss))

# Main training loop
with tracer.start_as_current_span("training_job") as root_span:
    root_span.set_attribute("model", "gpt-2")
    root_span.set_attribute("gpus", 8)

    for epoch in range(num_epochs):
        train_epoch(model, train_loader, optimizer)

        # Validation
        with tracer.start_as_current_span("validation"):
            val_loss = validate(model, val_loader)

        root_span.add_event("epoch_completed", {
            "epoch": epoch,
            "train_loss": train_loss,
            "val_loss": val_loss
        })
```

### PyTorch DDP Tracing

```python
import torch.distributed as dist
from opentelemetry.instrumentation.torch import TorchInstrumentor

# Auto-instrument PyTorch
TorchInstrumentor().instrument()

# Your DDP code
model = DistributedDataParallel(model)

# Traces automatically include:
# - All-reduce operations
# - Gradient synchronization
# - Communication patterns
```

### View Traces

**Jaeger UI shows:**
- Complete request timeline
- Service dependencies
- Latency breakdown
- Error tracking
- Resource utilization

**Example queries:**
```
# Find slow training iterations
service="training-job" AND duration > 1s

# Find failed batches
service="training-job" AND error=true

# GPU communication traces
service="training-job" AND operation="all_reduce"
```

## Log Aggregation

### Configure Log Shipping

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: promtail-config
  namespace: gryvia-system
data:
  promtail.yaml: |
    server:
      http_listen_port: 3101

    clients:
      - url: http://loki:3100/loki/api/v1/push

    positions:
      filename: /tmp/positions.yaml

    scrape_configs:
      # Scrape Gryvia job logs
      - job_name: gryvia-jobs
        kubernetes_sd_configs:
          - role: pod
            namespaces:
              names:
                - default
        relabel_configs:
          - source_labels: [__meta_kubernetes_pod_label_job_name]
            target_label: job
          - source_labels: [__meta_kubernetes_pod_label_team]
            target_label: team
          - source_labels: [__meta_kubernetes_namespace]
            target_label: namespace
```

### Query Logs

**LogQL queries:**
```logql
# All logs from a job
{job="training-job-123"}

# Error logs
{namespace="default"} |= "ERROR"

# GPU out of memory errors
{namespace="default"} |~ "CUDA out of memory"

# Slow data loading
{job=~".*training.*"} |~ "DataLoader" | json | duration > 100ms

# Cost by team
sum(rate({team="ml-research"}[5m])) by (job)
```

### Log Panels in Grafana

```json
{
  "targets": [
    {
      "expr": "{namespace=\"default\",job=~\".*training.*\"} |= \"loss\"",
      "refId": "A"
    }
  ],
  "title": "Training Losses",
  "type": "logs"
}
```

## Metrics

### Custom Metrics

```python
from prometheus_client import Counter, Histogram, Gauge

# Training metrics
train_loss = Gauge('training_loss', 'Current training loss')
gpu_utilization = Gauge('gpu_utilization', 'GPU utilization %', ['gpu_id'])
batch_processing_time = Histogram('batch_processing_seconds', 'Batch processing time')
epochs_completed = Counter('epochs_total', 'Total epochs completed')

# In training loop
train_loss.set(loss.item())
gpu_utilization.labels(gpu_id='0').set(get_gpu_util())
batch_processing_time.observe(batch_time)
epochs_completed.inc()
```

### PromQL Queries

```promql
# Average GPU utilization
avg(gpu_utilization)

# 95th percentile batch time
histogram_quantile(0.95, batch_processing_seconds_bucket)

# Training throughput (samples/sec)
rate(samples_processed_total[5m])

# Cost rate ($/hour)
sum(gpu_cost_per_hour) by (team)

# Job failure rate
rate(job_failures_total[1h]) / rate(job_starts_total[1h])
```

## Unified Dashboard

### Single Pane of Glass

Grafana dashboard combining:
- Traces (Jaeger)
- Logs (Loki)
- Metrics (Prometheus)

**Correlate across signals:**
```
1. See spike in latency (metrics)
2. Find corresponding traces
3. View logs from that time period
4. Identify root cause
```

### Example Dashboard

```json
{
  "dashboard": {
    "title": "GPU Training Observability",
    "panels": [
      {
        "title": "GPU Utilization",
        "targets": [{
          "expr": "avg(gpu_utilization) by (job)"
        }],
        "type": "graph"
      },
      {
        "title": "Recent Errors",
        "targets": [{
          "expr": "{namespace=\"default\"} |= \"ERROR\""
        }],
        "type": "logs"
      },
      {
        "title": "Trace Latency",
        "datasource": "Jaeger",
        "type": "trace"
      }
    ]
  }
}
```

## Alerting

### Trace-Based Alerts

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: trace-alerts
  namespace: gryvia-system
spec:
  groups:
    - name: tracing
      rules:
        # High error rate
        - alert: HighTraceErrorRate
          expr: rate(traces_total{error="true"}[5m]) > 0.05
          annotations:
            summary: "High trace error rate"

        # Slow traces
        - alert: SlowTraces
          expr: histogram_quantile(0.95, trace_duration_seconds_bucket) > 10
          annotations:
            summary: "P95 trace latency > 10s"
```

### Log-Based Alerts

```yaml
- alert: GPUOutOfMemory
  expr: |
    count_over_time({namespace="default"} |~ "CUDA out of memory"[5m]) > 0
  annotations:
    summary: "GPU OOM detected"
    description: "Job {{ $labels.job }} ran out of GPU memory"
```

## Performance Analysis

### Identify Bottlenecks

**Using Jaeger:**
1. Find slowest spans
2. Analyze service dependencies
3. Identify critical path
4. Optimize bottleneck

**Using Metrics:**
```promql
# Find slowest operation
topk(5, avg(operation_duration_seconds) by (operation))

# GPU idle time
100 - avg(gpu_utilization)

# Data loading bottleneck
avg(data_loading_time) / avg(batch_time)
```

### Optimization Workflow

```
1. Collect traces for training job
2. Identify slow spans (>100ms)
3. Analyze logs for errors
4. Check resource metrics
5. Implement optimization
6. Compare before/after traces
7. Validate improvement
```

## Cost Attribution

### Trace Cost

```python
from opentelemetry import trace

with tracer.start_as_current_span("training") as span:
    # Calculate cost
    gpu_hours = duration_hours * gpu_count
    cost = gpu_hours * hourly_rate

    # Add to span
    span.set_attribute("cost.gpu_hours", gpu_hours)
    span.set_attribute("cost.total_usd", cost)
    span.set_attribute("cost.team", "ml-research")
```

**Query costs:**
```
# Total cost by team
sum(span.cost.total_usd) by (cost.team)

# Cost per model
sum(span.cost.total_usd) by (model)
```

## Best Practices

1. **Sample Intelligently**:
   - 100% for errors
   - 10% for successful requests
   - 100% for slow requests

2. **Structure Logs**:
   - Use JSON format
   - Include context (job, team, etc.)
   - Add trace IDs to logs

3. **Meaningful Spans**:
   - Span per epoch
   - Span per batch
   - Span per model operation

4. **Cardinality**:
   - Avoid high-cardinality labels
   - Use bounded label values

5. **Retention**:
   - Traces: 7 days
   - Logs: 30 days
   - Metrics: 90 days

## Troubleshooting

### No Traces Appearing

```bash
# Check collector
kubectl logs -n gryvia-system deployment/otel-collector

# Verify endpoint
kubectl exec -it <job-pod> -- env | grep OTEL

# Test connectivity
kubectl exec -it <job-pod> -- \
  curl http://otel-collector.gryvia-system:4318/v1/traces
```

### High Cardinality

```bash
# Find high cardinality labels
kubectl exec -n gryvia-system prometheus-0 -- \
  promtool tsdb analyze /prometheus

# Solutions:
# - Remove user IDs from labels
# - Use bounded values
# - Aggregate in application
```

## Support

- Observability Issues: https://github.com/zyvorai/gryvia/issues
- Jaeger Docs: https://www.jaegertracing.io/docs/
- OpenTelemetry: https://opentelemetry.io/docs/
