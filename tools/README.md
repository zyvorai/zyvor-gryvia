# Gryvia Tools

Collection of diagnostic and utility tools for Gryvia platform.

> **Status: helper scripts, lightly tested, not part of the released images or Helm charts.** They are
> run by hand with kubectl/helm/Python `kubernetes` access and have not been validated against a live
> cluster in CI. Every "Example Output" block below is illustrative (made-up numbers). Corrections to
> the descriptions in this file: the cost calculator uses a hard-coded rate table (`GPU_PRICING` in
> `cost-calculator.py`, e.g. H100 8.00 USD/hour), not your real billing; `profiler.py` reads
> `status.metrics` fields (`avgGPUUtilization`, ...) from the job, which nothing in this repo
> populates, so its numbers are only meaningful if you supply them (compare the in-cluster
> `GryviaTrainingProfiler`, which has the same input gap; see `features/training-profiler.md`);
> there is no `tools/Dockerfile`, `gryvia-tools` image or
> `gryvia-tools` ServiceAccount, so the container and Kubernetes-integration snippets are templates
> you must build yourself. Also present but not described below: `audit-tool.py` (compliance-style
> report from `GryviaAIJob`s), `migration-tool.py` (export/import between kube contexts; it also lists
> kinds that do not exist here, such as `gryviaqueues` and `gryviausers`), and `performance-profiler.py`
> (per-job profile with generated suggestions). None of these gives a compliance attestation.

## Tools

### 1. GPU Diagnostics (gpu-diagnostics.sh)

Comprehensive GPU health and performance checking.

**Usage:**
```bash
# Run on GPU node
./tools/gpu-diagnostics.sh

# Run in Kubernetes pod
kubectl exec -it <gpu-pod> -- /tools/gpu-diagnostics.sh
```

**Checks:**
- NVIDIA driver installation
- GPU health status (temperature, power, utilization)
- CUDA installation
- GPU topology
- NCCL installation
- NVLink status
- GPU memory errors
- Quick GPU benchmark (step 8 of 8; requires the tooling the script probes for, see `gpu-diagnostics.sh`)

**Output:**
- Console report with color-coded status
- Detailed report saved to `/tmp/gpu_diagnostics_<hostname>.txt`

### 2. Cost Calculator (cost-calculator.py)

Analyzes job history and provides cost projections.

**Usage:**
```bash
# Analyze last 30 days
python3 tools/cost-calculator.py

# Analyze last 7 days
python3 tools/cost-calculator.py --days 7

# Filter by team
python3 tools/cost-calculator.py --team ml-research

# Save to JSON
python3 tools/cost-calculator.py --output report.json

# Specific namespace
python3 tools/cost-calculator.py --namespace production
```

**Reports:**
- Total cost summary
- Monthly projections
- Cost by team
- Cost by GPU type
- Cost by project
- Top 20 most expensive jobs

**Example Output:**
```
================================================================================
                         GRYVIA COST REPORT
================================================================================

📊 SUMMARY
--------------------------------------------------------------------------------
Total Cost (last 30 days):    $45,234.50
Total Jobs:                   342
Average Cost per Job:         $132.26

📅 MONTHLY PROJECTION
--------------------------------------------------------------------------------
Current Month (MTD):          $18,234.50
Projected Month Total:        $54,703.50
Days Elapsed / Remaining:     10 / 20

👥 COST BY TEAM
--------------------------------------------------------------------------------
Team                                 Cost       Jobs      Avg/Job
--------------------------------------------------------------------------------
ml-research                   $25,432.10        156     $163.03
computer-vision               $12,345.60         98     $126.00
nlp                           $7,456.80          88     $84.74
```

### 3. and 4. Backup, restore and upgrade (removed)

`backup-restore.sh` and `upgrade.sh` were removed: they targeted a namespace and a Helm repository that do not
exist and updated deployments with `kubectl set image`, so they did not implement the supported procedure. Use
[`scripts/backup-crs.sh`](../scripts/backup-crs.sh) (`export` / `restore` of every `gryvia.io` object) and the
upgrade steps in [Operations](../website/docs/guides/OPERATIONS.md). The procedure is tested by
`scripts/upgrade-test.sh` (upgrade, rollback, backup/restore round trip on kind; CI workflow `upgrade.yml`).

### 5. GPU Profiler (profiler.py)

Analyzes GPU utilization and provides optimization recommendations.

**Usage:**
```bash
# Profile specific job
python3 tools/profiler.py --job pytorch-training \
  --gpu-type A100-80G \
  --gpu-count 8

# Profile all jobs
python3 tools/profiler.py --all

# Profile jobs in specific namespace
python3 tools/profiler.py --all --namespace ml-research

# Save results to JSON
python3 tools/profiler.py --job my-job --output report.json
```

**Analysis:**
- GPU utilization patterns
- Memory usage efficiency
- Compute vs memory bottlenecks
- Cost optimization opportunities
- Performance recommendations

**Recommendations:**
- Batch size adjustments
- GPU type optimization
- Data loading improvements
- Memory optimization techniques
- Cost-saving opportunities

**Example Output:**
```
╔════════════════════════════════════════════════════════════════╗
║          Gryvia GPU Profiler                              ║
╚════════════════════════════════════════════════════════════════╝

📊 UTILIZATION SUMMARY
──────────────────────────────────────────────────────────────────
Efficiency Score:       45.5/100
GPU Utilization:        42.3%
GPU Memory Usage:       28.7%
Runtime:                4.25 hours

⚠️  ISSUES DETECTED
──────────────────────────────────────────────────────────────────
[HIGH] Low GPU utilization: 42.3%
[MEDIUM] Low GPU memory usage: 28.7%

💡 RECOMMENDATIONS
──────────────────────────────────────────────────────────────────
[HIGH] Increase Batch Size
  Category: performance
  Low GPU utilization indicates underutilization. Increase batch size.
  Action: Try increasing batch size by 50-100%
  Expected Impact: 30-50% improvement in GPU utilization

[MEDIUM] Use Smaller GPU Type
  Category: cost
  Memory usage is only 28.7%. You may be overpaying for GPU capacity.
  Action: Consider switching to A100-40G instead of A100-80G
  Expected Impact: 40-50% cost reduction

💰 COST ANALYSIS
──────────────────────────────────────────────────────────────────
Current Cost:           $816.00

Potential Savings:
  • Use Smaller GPU Type
    $408.00 (50.0%)
  • Improve GPU utilization to 80%+
    $204.00 (25.0%)

Total Potential Savings: $612.00
```

## Installation

### Local Installation

```bash
# Install dependencies for cost calculator
pip install kubernetes

# Make scripts executable
chmod +x tools/*.sh
chmod +x tools/*.py
```

### Container Usage

Template only: `tools/Dockerfile` does not exist; write one first.

```bash
# Build tools container
docker build -t gryvia-tools -f tools/Dockerfile tools/

# Run cost calculator
docker run -v ~/.kube:/root/.kube gryvia-tools \
  python3 cost-calculator.py

# Run GPU diagnostics (on GPU node)
docker run --gpus all gryvia-tools \
  bash gpu-diagnostics.sh
```

## Kubernetes Integration

### Deploy as CronJob

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: daily-cost-report
  namespace: gryvia-system
spec:
  schedule: "0 9 * * *"  # Daily at 9 AM
  jobTemplate:
    spec:
      template:
        spec:
          serviceAccountName: gryvia-tools
          containers:
          - name: cost-calculator
            image: gryvia-tools:1.0.0
            command:
              - python3
              - /tools/cost-calculator.py
              - --output
              - /reports/cost-report.json
            volumeMounts:
              - name: reports
                mountPath: /reports
          volumes:
            - name: reports
              persistentVolumeClaim:
                claimName: cost-reports-pvc
          restartPolicy: OnFailure
```

### Deploy as DaemonSet (GPU Diagnostics)

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: gpu-diagnostics
  namespace: gryvia-system
spec:
  selector:
    matchLabels:
      app: gpu-diagnostics
  template:
    metadata:
      labels:
        app: gpu-diagnostics
    spec:
      nodeSelector:
        gryvia.io/gpu: "true"
      hostPID: true
      containers:
      - name: diagnostics
        image: gryvia-tools:1.0.0
        command:
          - /bin/bash
          - -c
          - |
            while true; do
              /tools/gpu-diagnostics.sh > /var/log/gpu-diagnostics.log
              sleep 3600
            done
        volumeMounts:
          - name: nvidia
            mountPath: /usr/local/nvidia
          - name: logs
            mountPath: /var/log
        securityContext:
          privileged: true
      volumes:
        - name: nvidia
          hostPath:
            path: /usr/local/nvidia
        - name: logs
          hostPath:
            path: /var/log
```

## Troubleshooting

### Cost Calculator Issues

**Problem**: "No module named 'kubernetes'"
```bash
pip install kubernetes
```

**Problem**: "Unauthorized" error
```bash
# Ensure kubeconfig is accessible
export KUBECONFIG=~/.kube/config

# Or copy into cluster
kubectl create secret generic kubeconfig \
  --from-file=config=$HOME/.kube/config \
  -n gryvia-system
```

### GPU Diagnostics Issues

**Problem**: "nvidia-smi: command not found"
```bash
# Install NVIDIA drivers first
# Or run on a node with GPU drivers installed
```

**Problem**: "Permission denied"
```bash
# Run with appropriate permissions
sudo ./gpu-diagnostics.sh

# Or in Kubernetes with privileged pod
```

## Best Practices

### Cost Reporting

1. **Regular Reports**: Run daily cost reports
2. **Team Attribution**: Ensure all jobs have team labels
3. **Budget Alerts**: Set up alerts when costs exceed thresholds
4. **Trend Analysis**: Track costs over time

### GPU Monitoring

1. **Scheduled Checks**: Run diagnostics hourly
2. **Alert on Issues**: Integrate with monitoring system
3. **Temperature Monitoring**: Alert on high temperatures
4. **Error Tracking**: Monitor ECC errors

### Automation

1. **CronJobs**: Automate regular tasks
2. **Alerting**: Integrate with Prometheus/Alertmanager
3. **Dashboards**: Visualize metrics in Grafana
4. **Reporting**: Send reports via email/Slack

## Contributing

To add new tools:

1. Create tool in `tools/` directory
2. Add documentation to this README
3. Create Dockerfile if needed (none exists today)
4. Add examples
5. Submit pull request

## Support

- Issues: https://github.com/zyvorai/gryvia/issues
- Documentation: https://github.com/zyvorai/gryvia/docs
- Discussions: https://github.com/zyvorai/gryvia/discussions
