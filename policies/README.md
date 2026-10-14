# Scheduling Policies

Advanced scheduling policies for optimizing GPU resource allocation in Gryvia.

> **Status: design catalog; the policy framework described here does not exist.** There is no
> `SchedulingPolicy` CRD (so `kubectl apply -f policies/scheduling-policies.yaml` is rejected), no
> `gryvia.io/priority` or `gryvia.io/scheduling-policy` annotation handling, no `spec.schedulingPolicy`
> on `GryviaQuota`, no `scheduling-policies.json` dashboard, no `kfctl` CLI, and no adaptive or ML
> policy switching. What is implemented today, in `operators/ai-operator/pkg/scheduler`: the `GryviaAIJob`
> controller picks nodes with a fixed heuristic (ready nodes with enough free GPUs, `nodeSelector`,
> then a score favouring matching `gryvia.io/gpu` label, RDMA and NVLink/NVSwitch labels, free GPUs and
> GPU memory). Gang admission and queues come from Kueue or the opt-in native queue gate ([docs/native-scheduler.md](../docs/native-scheduler.md)); nothing else here enforces DRF. Quota limits (max GPUs, running jobs, allowed GPU
> types, hard budget) are enforced by the quota operator by rejecting pending jobs. Backfilling, bin
> packing, locality, topology-aware, SLA and preemption policies, borrowing and the percentages and
> savings quoted below are design targets, not measured. `GryviaPriority` maps to a
> PriorityClass and `GryviaReservation` taints reserved nodes (opt-in); neither implements the policies below.

## Available Policies

### 1. Priority-Based Scheduling

Schedule jobs based on priority levels (critical, high, medium, low).

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: critical-training
  annotations:
    gryvia.io/priority: critical
spec:
  # ... job spec
```

**Use Cases:**
- Production model training (critical)
- Research experiments (high)
- Development testing (medium)
- Batch processing (low)

### 2. Fair Share Scheduling

Distribute GPU resources fairly among teams using Dominant Resource Fairness (DRF).

**Features:**
- Guaranteed minimum GPUs per team
- Maximum GPU caps
- Dynamic rebalancing
- Prevents resource hogging

**Example:**
```yaml
teams:
  - name: ml-research
    fairShareWeight: 40  # 40% of cluster
    minGPUs: 8           # Always guaranteed
    maxGPUs: 32          # Never exceed
```

### 3. Cost-Optimized Scheduling

Minimize infrastructure costs through intelligent scheduling.

**Strategies:**
- **Spot Instances**: Use cheaper spot/preemptible instances
- **GPU Right-Sizing**: Recommend appropriate GPU types
- **Time-Based**: Encourage off-peak usage

**Savings:**
- Up to 70% with spot instances
- 30-50% with right-sizing
- 30% off-peak discount

### 4. Locality-Aware Scheduling

Schedule jobs near data sources to minimize data transfer.

**Locality Levels:**
1. Node-local (best)
2. Rack-local
3. Zone-local
4. Region-local
5. Remote (worst)

**Use Cases:**
- Large dataset training
- Distributed ML pipelines
- Multi-regional deployments

### 5. Gang Scheduling

Schedule all replicas together (all-or-nothing) for distributed training.

**Benefits:**
- No partial deployments
- Optimal for MPI/NCCL jobs
- Prevents resource deadlocks

**Example:**
```yaml
spec:
  distributed:
    enabled: true
    strategy: gang
    minMembers: 8  # Need all 8 GPUs
```

### 6. Bin Packing

Consolidate workloads to minimize number of active nodes.

**Objective:** Maximize utilization, reduce fragmentation

**Use Cases:**
- Cost reduction (fewer active nodes)
- Energy efficiency
- Resource consolidation

### 7. Backfilling

Fill scheduling gaps with small jobs while waiting for large jobs.

**Criteria:**
- Short duration jobs (<1 hour)
- Small GPU count (≤4)
- Low/medium priority

**Benefits:**
- Improved cluster utilization
- Reduced average wait time
- Better throughput

### 8. Quota-Based Scheduling

Enforce team quotas strictly.

**Quota Types:**
- GPU hours
- Concurrent GPUs
- Budget limits

**Enforcement:**
- **Strict**: Reject jobs exceeding quota
- **Soft**: Allow temporary overage
- **Advisory**: Warn only

**Borrowing:**
```yaml
borrowing:
  enabled: true
  maxBorrowPercent: 20  # Borrow 20% from others
  paybackPeriod: 86400  # Return in 24 hours
```

### 9. Preemption Policy

Preempt lower-priority jobs for higher-priority ones.

**Protection:**
- Minimum runtime before preemption
- Required checkpointing
- Grace period for cleanup

**Victim Selection:**
- Youngest-first
- Oldest-first
- Random

### 10. GPU Affinity Scheduling

Match jobs to appropriate GPU types.

**Rules:**
```yaml
# Large models → High memory GPUs
large-model:
  requiredGPUMemory: ">= 80GB"
  preferredGPUs: [H100, A100-80G]

# Inference → Cost-effective GPUs
inference:
  preferredGPUs: [T4, L4]

# Multi-GPU → NVLink required
multi-gpu:
  requiredFeatures: [nvlink]
```

### 11. Topology-Aware Scheduling

Consider PCIe and network topology for multi-GPU jobs.

**Topology Levels:**
1. NUMA node (best)
2. PCIe switch
3. NVLink domain
4. Rack
5. Datacenter

**Metrics:**
- GPU-to-GPU bandwidth
- GPU-to-CPU bandwidth
- Network latency

### 12. Adaptive Scheduling

Dynamically switch policies based on cluster state.

**Modes:**
```yaml
- utilization > 90% → bin-packing
- utilization < 30% → spread
- queue length > 50 → fair-share
- off-peak hours    → cost-optimized
```

**Machine Learning:**
- Learns from historical patterns
- Optimizes for job completion time
- Adjusts every hour

### 13. SLA-Based Scheduling

Guarantee service level agreements.

**SLA Levels:**

| Level    | Max Wait | Guaranteed GPUs | Availability |
|----------|----------|-----------------|--------------|
| Platinum | 5 min    | 16              | 99.9%        |
| Gold     | 15 min   | 8               | 99.0%        |
| Silver   | 1 hour   | 4               | 95.0%        |

**Violations:**
- Priority boost
- Resource reservation
- Customer credit (10%)

## Policy Configuration

### Apply Policy

```bash
# Apply scheduling policy
kubectl apply -f policies/scheduling-policies.yaml

# Set default policy
kubectl patch configmap gryvia-config -n gryvia-system \
  -p '{"data":{"default-scheduling-policy":"fair-share"}}'
```

### Per-Job Policy

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  annotations:
    gryvia.io/scheduling-policy: cost-optimized
spec:
  # ... job spec
```

### Per-Team Policy

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaQuota
metadata:
  name: ml-research
spec:
  schedulingPolicy: fair-share
  # ... quota spec
```

## Policy Comparison

| Policy | Goal | Best For | Complexity |
|--------|------|----------|------------|
| Priority | Fast scheduling | Production workloads | Low |
| Fair Share | Fairness | Multi-tenant | Medium |
| Cost-Optimized | Cost reduction | Budget-conscious | Medium |
| Locality | Performance | Large datasets | High |
| Gang | Distributed training | MPI/NCCL jobs | Low |
| Bin Packing | Utilization | Cost reduction | Medium |
| Backfilling | Throughput | Mixed workloads | High |
| Quota | Budget control | Cost management | Low |
| Preemption | Priority enforcement | Critical jobs | High |
| Affinity | Performance | GPU matching | Medium |
| Topology | Multi-GPU perf | Large-scale training | High |
| Adaptive | Optimization | Variable workloads | High |
| SLA | Guarantees | Enterprise | Medium |

## Monitoring

### Policy Effectiveness

```bash
# View policy metrics
kubectl get schedulingpolicy fair-share -o yaml

# Check scheduling decisions
kubectl describe gryviaaijob <job-name> | grep "Scheduling Decision"

# Policy performance
kfctl policy metrics fair-share
```

### Grafana Dashboard

Import `monitoring/grafana-dashboards/scheduling-policies.json`

**Panels:**
- Jobs per policy
- Average wait time
- Policy violations
- Cost savings
- Utilization by policy

## Best Practices

1. **Start Simple**: Begin with priority-based
2. **Monitor First**: Observe patterns before optimizing
3. **Combine Policies**: Use multiple policies together
4. **Test Thoroughly**: Test in dev before production
5. **Document Decisions**: Document why you chose a policy
6. **Review Regularly**: Review effectiveness monthly
7. **User Education**: Educate users on policy choices

## Examples

### Production Setup

```yaml
# Default: Fair share for general use
defaultPolicy: fair-share

# Critical workloads
productionJobs:
  policy: priority
  priority: critical

# Cost-sensitive workloads
batchProcessing:
  policy: cost-optimized
  spotInstances: true

# Research experiments
research:
  policy: backfilling
  priority: low
```

### Multi-Tenant Setup

```yaml
# Fair share with quotas
policy: fair-share
quotaEnforcement: strict

teams:
  - ml-research: 40%
  - computer-vision: 30%
  - nlp: 20%
  - general: 10%
```

## Troubleshooting

### Job Not Scheduling

```bash
# Check policy
kubectl get schedulingpolicy

# View scheduling events
kubectl describe gryviaaijob <job-name>

# Check quota
kfctl quota <team-name>
```

### Policy Not Applied

```bash
# Verify policy exists
kubectl get schedulingpolicy <policy-name>

# Check controller logs
kubectl logs -n gryvia-system deployment/gryvia-gpu-operator

# Test policy
kfctl policy test <policy-name>
```

## Advanced Topics

### Custom Policies

Create custom scheduling policies:

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: SchedulingPolicy
metadata:
  name: custom-ml-policy
spec:
  type: custom
  webhook:
    url: http://my-scheduler.default.svc:8080/schedule
    timeout: 5s
```

### Policy Composition

Combine multiple policies:

```yaml
spec:
  policies:
    - name: fair-share
      weight: 50
    - name: cost-optimized
      weight: 30
    - name: locality-aware
      weight: 20
```

## Support

- Policy Questions: https://github.com/zyvorai/gryvia/discussions
- Issues: https://github.com/zyvorai/gryvia/issues
