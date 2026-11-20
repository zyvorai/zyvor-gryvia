# Gryvia Quota Operator

The Quota Operator manages GPU quotas and budgets for teams in Gryvia. It enforces resource limits, tracks spending, and ensures fair resource allocation across teams.

> **Status.** This operator registers these controllers in `main.go`: `GryviaQuota` (quota and budget evaluation, namespace labelling, rejection of pending jobs), `GryviaTenant` (creates namespace `tenant-<name>`, a ResourceQuota, a LimitRange and, when isolation is requested, a NetworkPolicy), `GryviaUsageRecord` (metering from job wall-clock time, refreshed every minute while a job runs and finalised when it ends) and `GryviaCostPredictor`. GPU prices come from the `GryviaGpuSku` catalog, with a built-in default table used only when no SKU exists. `GryviaBudget` (status computed from usage records, see `docs/gpuaas-completion.md`) is registered too, and `GryviaReservation` (node taints and labels) behind `--enable-reservations`; `GryviaTenant` also creates per-role RoleBindings behind `--tenant-rbac`. `GryviaChargeback` is registered too (actual-usage chargeback, see `docs/platform-completion.md`). The ai-operator admission webhook (chart `webhook.enabled`, default on, `failurePolicy: Ignore`) also denies jobs whose GPU type is not allowed by the namespace quota or tenant SKUs, or that exceed the per-job GPU limit, and fails open when quotas cannot be read. Enforcement has not been exercised on a large multi-team cluster.

## Features

- **GPU Quotas** - Limit max GPUs per team and per job
- **Job Limits** - Control max running and queued jobs
- **Budget Management** - Track spending and enforce budget limits
- **GPU Type Restrictions** - Allow/deny specific GPU models per team
- **Priority field** - `spec.priority` is part of the schema; the quota controller does not use it for scheduling today
- **Namespace Labeling** - Automatic team labeling of namespaces
- **Cost Tracking** - GPU-hour tracking priced from the `GryviaGpuSku` catalog (built-in default table when no SKU exists)

## Architecture

```
GryviaQuota CR
       ↓
Quota Operator (1min reconcile loop)
       ↓
  ┌────┴─────┬──────────┬─────────┐
  ↓          ↓          ↓         ↓
Label     Calculate  Enforce   Track
Namespaces Usage     Quota     Budget
  ↓          ↓          ↓         ↓
Team Tags  GPU Count  Reject    Monthly
           Job Count  Jobs      Spending
```

## Installation

```bash
# Apply CRD
kubectl apply -f crds/   # or install the Helm chart, which ships the CRDs

# Deploy operator (the Helm chart is the supported path; config/ holds raw manifests)
kubectl apply -f operators/quota-operator/config/

# Verify deployment
kubectl get pods -n gryvia-system -l app=quota-operator
```

## Usage

### Create Team Quota

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaQuota
metadata:
  name: team-ml
spec:
  team: ml-research
  namespaces:
    - ml-training
    - ml-inference
  priority: 80

  gpuQuota:
    maxGPUs: 32
    maxGPUsPerJob: 16
    maxRunningJobs: 5
    maxQueuedJobs: 10
    allowedGPUTypes:
      - H100
      - A100-80G

  budget:
    monthlyBudget: 50000.00
    alertThreshold: 80.0
    hardLimit: true
```

### Check Quota Status

```bash
kubectl get gryviaquota team-ml -o yaml
```

Output:
```yaml
status:
  phase: Active
  currentUsage:
    allocatedGPUs: 16
    runningJobs: 2
    queuedJobs: 3
    gpuHours: 2456.5
  budgetStatus:
    spentThisMonth: 19652.00
    remainingBudget: 30348.00
    percentUsed: 39.3
    projectedSpend: 45234.00
  lastUpdated: "2024-01-15T10:30:00Z"
```

## Implementation Details

### Quota Controller

**Main Reconciler** (`controllers/gryviaquota_controller.go`)
- Reconciles every 1 minute
- Uses `EnqueueRequestsFromMapFunc` to watch GryviaAIJob changes and map them to the corresponding GryviaQuota objects, ensuring quotas are re-evaluated promptly when jobs change
- Labels namespaces with team info
- Calculates current GPU usage
- Enforces quota limits (including `AllowedGPUTypes` -- pending jobs requesting a disallowed GPU type are marked `Rejected` with reason `GpuTypeNotAllowed`)
- Tracks budget spending
- Leader election is enabled by default

### Usage Tracking

**Usage Calculator** (`pkg/usage/usage.go`)
- Counts allocated GPUs across team namespaces
- Tracks running and queued jobs
- Calculates GPU-hours for billing
- Filters by current month for budget tracking

### Budget Management

**Budget Calculator** (`pkg/budget`, on `pkg/spend`)
- Spend = sum of `GryviaUsageRecord.spec.cost` (open records included, each priced with its own SKU rate and currency), per scope and period
- Distributed jobs count `nodes x gpusPerNode` GPUs
- Projected spending based on burn rate
- `GryviaBudget` states `active|warning|exceeded|blocked` derived from `spec.alerts` and `spec.enforcement`
- Hard limit enforcement: reactive here, and before creation by the ai-operator admission gate (`--admission-gate`)
- Mixed or non-USD currencies are reported, not enforced

Semantics and limits: `docs/gpuaas-completion.md`.

**Default Pricing:**
```go
H100:     $8.00/hour
A100-80G: $4.00/hour
A100-40G: $3.50/hour
L40:      $2.50/hour
V100:     $2.00/hour
T4:       $1.00/hour
default:  $1.00/hour   (unknown GPU types)
```

The authoritative table is `pkg/pricing/pricing.go`. Once any `GryviaGpuSku` exists in the cluster, the SKU catalog is the source of truth and this table is ignored. These are placeholder rates, not real cloud prices.

### Quota Enforcement

When quota is exceeded:
1. **GPU Limit**: When allocated GPUs exceed `maxGPUs` (or running jobs exceed `maxRunningJobs`), the quota phase becomes `QuotaExceeded`. Pending jobs larger than `maxGPUsPerJob` are then rejected.
2. **Budget Limit**: If `hardLimit: true` and the budget is used up, the phase becomes `BudgetExceeded` and pending jobs are rejected.
3. **GPU Type**: Pending jobs requesting a GPU type outside `allowedGPUTypes` (or with no enabled SKU among the tenant's allowed SKUs) are rejected with a `Rejected` condition.

Job rejection is atomic — if the status update to mark a job as "Rejected"
fails, the operator returns an error and retries on the next reconciliation,
ensuring no job silently bypasses quota enforcement.

## Examples

### High-Priority LLM Team

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaQuota
metadata:
  name: team-nlp
spec:
  team: nlp
  namespaces: [nlp-training]
  priority: 90  # Highest priority

  gpuQuota:
    maxGPUs: 64
    maxGPUsPerJob: 32
    allowedGPUTypes: [H100]  # H100 only

  budget:
    monthlyBudget: 100000.00
    hardLimit: true
```

### Development Team with Soft Limits

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaQuota
metadata:
  name: team-dev
spec:
  team: development
  namespaces: [dev-experiments]
  priority: 50

  gpuQuota:
    maxGPUs: 8
    maxGPUsPerJob: 4
    allowedGPUTypes: [T4, A10]  # Lower-cost GPUs

  budget:
    monthlyBudget: 5000.00
    alertThreshold: 80.0
    hardLimit: false  # Warnings only
```

## Monitoring

### View All Quotas

```bash
kubectl get gryviaquotas
```

Output:
```
NAME        TEAM           PHASE    ALLOCATED   MAX    SPENT      BUDGET
team-ml     ml-research    Active   16/32       32     $19,652    $50,000
team-cv     computer-vision Active   8/24        24     $8,234     $30,000
team-nlp    nlp            Active   32/64       64     $78,456    $100,000
team-dev    development    Active   4/8         8      $1,234     $5,000
```

### Check Budget Status

```bash
kubectl get gryviaquota team-ml -o jsonpath='{.status.budgetStatus}' | jq
```

Output:
```json
{
  "spentThisMonth": 19652.00,
  "remainingBudget": 30348.00,
  "percentUsed": 39.3,
  "projectedSpend": 45234.00
}
```

### Alert on Budget Threshold

The operator sets a condition when budget threshold is reached:

```bash
kubectl get gryviaquota team-ml -o jsonpath='{.status.conditions[?(@.type=="BudgetAlert")]}'
```

## Integration with AI Workload Operator

The Quota Operator works alongside the AI operator:

1. **Pre-Submission Validation**: The ai-operator admission webhook (when enabled) denies jobs with a disallowed GPU type or above the per-job GPU limit at create time
2. **Job Rejection**: The quota controller marks pending jobs as "Rejected" if quota is exceeded
3. **Cost Attribution**: `GryviaUsageRecord` metering records GPU-hours per tenant. There is no chargeback controller yet

### Job Rejection Example

```yaml
# Job status when quota exceeded
status:
  phase: Rejected
  message: "Budget exceeded for team ml-research"
```

## Custom Pricing

Update GPU pricing for your infrastructure:

```go
import "github.com/zyvorai/gryvia/operators/quota-operator/pkg/budget"

// Set custom H100 pricing (returns error on invalid input)
if err := budget.UpdatePricing("H100", 10.00); err != nil {
    log.Fatal(err)  // e.g. empty GPU type, negative rate, NaN
}

// Set custom on-prem pricing
if err := budget.UpdatePricing("A100-80G", 0.50); err != nil {
    log.Fatal(err)
}
```

`UpdatePricing` validates its inputs and returns an error if the GPU type
is empty or the hourly rate is negative, NaN, or infinite.

## RBAC

The operator requires:
- `gryviaquotas`: Full CRUD
- `gryviaaijobs`: Get, List, Watch, Update, Patch
- `namespaces`: Get, List, Watch, Update, Patch
- `resourcequotas`, `limitranges`, `networkpolicies`: managed for `GryviaTenant` namespaces (see `config/deployment.yaml` and the Helm chart for the exact rules)

## Troubleshooting

**Quota Not Enforced**
```bash
# Check operator logs
kubectl logs -n gryvia-system -l app=quota-operator

# Verify quota exists
kubectl get gryviaquota

# Check namespace labels
kubectl get namespace ml-training -o yaml | grep gryvia.io/team
```

**Incorrect Budget Calculation**
```bash
# Verify GPU pricing
kubectl get gryviagpuskus   # catalog prices; the default table is in pkg/pricing/pricing.go

# Check job timestamps
kubectl get gryviaaijobs -n ml-training -o yaml | grep startTime

```

**Jobs Not Being Rejected**
```bash
# Check quota status
kubectl get gryviaquota team-ml -o jsonpath='{.status.phase}'

# Verify job namespace has correct team label
kubectl get namespace ml-training -o jsonpath='{.metadata.labels}'

# Check job phase
kubectl get gryviaaijob -n ml-training <job-name> -o jsonpath='{.status.phase}'
```

## Performance

- **Reconciliation**: Every 1 minute for quotas
- No memory, CPU or scale benchmarks have been measured for this operator

## Roadmap

- [ ] Prometheus metrics export (quota usage, budget)
- [x] Webhook for admission control: implemented in the ai-operator (GPU type and per-job limit)
- [ ] Slack/Email notifications for budget alerts
- [ ] Grafana dashboard for quota visualization
- [ ] Historical usage reporting
- [ ] Quota templates for common team sizes
- [ ] Fair-share scheduling based on quota
- [ ] GPU time banking (roll-over unused quota)

## Best Practices

1. **Set Realistic Limits**: Base `maxGPUs` on historical usage patterns
2. **Use Priorities**: Assign higher priority to production teams
3. **Enable Alerts**: Set `alertThreshold` to 80% for advance warning
4. **Soft Limits for Dev**: Use `hardLimit: false` for experimentation teams
5. **GPU Type Restrictions**: Limit expensive GPUs (H100) to teams that need them
6. **Review Monthly**: Adjust budgets based on actual spending patterns

## Example Deployment

```bash
# Create quotas for 3 teams
kubectl apply -f examples/quota/team-ml-quota.yaml
kubectl apply -f examples/quota/team-cv-quota.yaml
kubectl apply -f examples/quota/team-nlp-quota.yaml

# Verify all quotas are active
kubectl get gryviaquotas

# Launch a training job
kubectl apply -f - <<EOF
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: llm-training
  namespace: nlp-training
spec:
  type: training
  gpus: 32
  gpuType: H100
  image: nvcr.io/nvidia/pytorch:24.01-py3
  command: ["torchrun", "train.py"]
EOF

# Check quota updated
kubectl get gryviaquota team-nlp -o jsonpath='{.status.currentUsage}'
```
