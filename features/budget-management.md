# Budget Management

Advanced budget management with cost controls, alerts, and forecasting.

> **Status: mostly design.** What runs: the quota-operator reconciles `GryviaBudget` once a
> minute, computing spend from the metered `GryviaUsageRecord`s into `status.state` and writing
> threshold Events (no email, Slack or webhook is sent); `gryvia budget` shows the result, and
> new jobs are blocked only with the opt-in admission gate (`aiOperator.admissionGate`). The
> rest of this document (alert channels, forecasting, rollover, approvals) is a design. The
> `kfctl` CLI used below does not exist; `kfctl` commands and `status` outputs in this document
> are illustrative. Numbers and outputs in the examples are made up.
>
> **Also available:** a simpler budget on `GryviaQuota`
> (`spec.budget.monthlyBudget`, `alertThreshold`, `hardLimit`). The quota operator computes
> `status.budgetStatus` (`spentThisMonth`, `remainingBudget`, `percentUsed`, `projectedSpend`),
> logs when the alert threshold is reached (no email/Slack/webhook is sent), and with
> `hardLimit: true` rejects pending `GryviaAIJob`s once 100% is reached
> (`operators/quota-operator/controllers/gryviaquota_controller.go`). The gateway and
> `gryvia quota` / `gryvia cost` surface these figures. Real-cluster and real-billing
> behaviour is unverified; cost is derived from the GPU SKU catalog rates, not from a cloud
> bill. No `gryvia_budget_*` Prometheus metrics are exported (see `monitoring/README.md`).

## Overview

Gryvia provides sophisticated budget management to control GPU compute costs:

- **Flexible Periods**: Daily, weekly, monthly, quarterly, annual
- **Multi-Dimensional Limits**: Cost, GPU hours, job count, concurrent resources
- **Graduated Alerts**: Email, Slack, webhooks at configurable thresholds
- **Enforcement Policies**: Warn, block, or throttle when budget exceeded
- **Cost Forecasting**: Predict end-of-period spending
- **Budget Rollover**: Carry unused budget to next period
- **Priority Overrides**: Allow critical jobs to exceed budgets

## Quick Start

### Create a Monthly Team Budget

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaBudget
metadata:
  name: ml-team-budget
spec:
  scope:
    type: team
    name: ml-research

  period:
    type: monthly

  limits:
    costUSD: 50000  # $50k/month
    maxConcurrentGPUs: 128

  alerts:
    - threshold: 75
      actions: [email]
      recipients: [team-lead@company.com]
    - threshold: 100
      actions: [block, email]
      recipients: [team-lead@company.com]

  enforcement:
    enabled: true
    action: block
```

Apply:
```bash
kubectl apply -f budget.yaml
```

Check status:
```bash
kubectl get gryviabudget ml-team-budget

# Output:
# NAME              SCOPE         PERIOD   BUDGET   USED     UTILIZATION   STATE
# ml-team-budget    ml-research   monthly  50000    37500    75%           warning
```

## Budget Scopes

### Team Budget

Controls spending for an entire team:

```yaml
spec:
  scope:
    type: team
    name: ml-research
```

### Namespace Budget

Controls spending in a Kubernetes namespace:

```yaml
spec:
  scope:
    type: namespace
    name: production
```

### User Budget

Controls spending for individual users:

```yaml
spec:
  scope:
    type: user
    name: alice

  limits:
    costUSD: 500  # $500/day for development
    maxConcurrentJobs: 5
```

### Project Budget

Controls spending for a specific project:

```yaml
spec:
  scope:
    type: project
    name: llm-training

  period:
    type: quarterly
    startDate: "2024-01-01T00:00:00Z"
    endDate: "2024-03-31T23:59:59Z"

  limits:
    costUSD: 500000  # $500k for the quarter
```

## Budget Periods

### Auto-Renewing Periods

```yaml
period:
  type: monthly  # Auto-creates new period each month
```

Supported types:
- `daily` - Resets daily at midnight UTC
- `weekly` - Resets Sunday midnight UTC
- `monthly` - Resets on 1st of month
- `quarterly` - Resets Q1 (Jan 1), Q2 (Apr 1), Q3 (Jul 1), Q4 (Oct 1)
- `annual` - Resets January 1st

### Custom Periods

```yaml
period:
  type: custom
  startDate: "2024-03-01T00:00:00Z"
  endDate: "2024-06-30T23:59:59Z"
```

## Budget Limits

### Cost Limits

```yaml
limits:
  # Total cost limit in USD
  costUSD: 50000
```

### GPU Hour Limits

```yaml
limits:
  # Total GPU hours across all types
  gpuHours: 2000

  # Per-GPU-type limits
  gpuTypeHours:
    H100: 200        # Max 200 H100 hours
    A100-80G: 1000
    A100-40G: 500
    T4: 300
```

### Concurrency Limits

```yaml
limits:
  # Maximum concurrent running jobs
  maxConcurrentJobs: 50

  # Maximum concurrent GPUs in use
  maxConcurrentGPUs: 128
```

## Alert Configuration

### Single Alert

```yaml
alerts:
  - threshold: 80  # At 80% budget consumption
    actions: [email]
    recipients:
      - team-lead@company.com
```

### Graduated Alerts

```yaml
alerts:
  # 50% - Informational
  - threshold: 50
    actions: [email]
    recipients: [team-lead@company.com]

  # 75% - Warning
  - threshold: 75
    actions: [email, slack]
    recipients:
      - team-lead@company.com

  # 90% - Critical
  - threshold: 90
    actions: [email, slack, webhook]
    recipients:
      - team-lead@company.com
      - finance@company.com

  # 100% - Block
  - threshold: 100
    actions: [block, email]
    recipients:
      - team-lead@company.com
      - finance@company.com
```

### Available Actions

- `email` - Send email notification
- `slack` - Post to Slack channel
- `webhook` - POST to webhook URL
- `block` - Block new job submissions

## Enforcement Policies

### Block New Jobs

```yaml
enforcement:
  enabled: true
  action: block  # Block new jobs when budget exceeded
  gracePeriod: 2h  # Allow 2 hours to resolve
```

### Warn Only

```yaml
enforcement:
  enabled: true
  action: warn  # Just send warnings, don't block
  gracePeriod: 24h
```

### Throttle Priority

```yaml
enforcement:
  enabled: true
  action: throttle  # Lower job priority when budget exceeded
  gracePeriod: 1h
```

## Budget Rollover

### Enable Rollover

```yaml
rollover:
  enabled: true
  maxRolloverPercent: 20  # Can carry over up to 20% unused budget
```

Example:
- Monthly budget: $50,000
- Used: $40,000
- Unused: $10,000
- Rollover (20%): $2,000
- Next month budget: $52,000

### Disable Rollover

```yaml
rollover:
  enabled: false  # Budget resets fully each period
```

## Priority Overrides

Allow high-priority jobs to exceed budget:

```yaml
priority:
  allowOverride: true
  overrideLimitPercent: 10  # Can use up to 110% of budget
```

Submit high-priority job:
Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: critical-job
spec:
  priority: high
  # This job can run even if budget at 105%
```

## Viewing Budget Status

### Get Budget

```bash
kubectl get gryviabudget ml-team-budget -o yaml
```

### Check Current Usage

```bash
# Using kubectl
kubectl get gryviabudget ml-team-budget -o jsonpath='{.status.usage}'

# Using kfctl
kfctl quota status --team ml-research

# Output:
# Budget Period: Jan 1 - Jan 31 (12 days remaining)
#
# Usage:
#   Cost: $37,500 / $50,000 (75%)
#   GPU Hours: 1,500 / 2,000 (75%)
#   Jobs: 342 total, 23 running
#
# State: WARNING
#
# Forecast:
#   Projected end-of-period cost: $64,285
#   Budget sufficient: NO
#   Estimated exhaustion: Jan 28
```

### Budget Forecast

View projected spending:

```bash
kubectl get gryviabudget ml-team-budget \
  -o jsonpath='{.status.forecast}' | jq
```

Output:
```json
{
  "projectedCostUSD": 64285,
  "projectedGPUHours": 2571,
  "budgetSufficient": false,
  "estimatedExhaustionDate": "2024-01-28T16:00:00Z"
}
```

## CLI Commands

> The `kfctl budget ...` commands below are a proposed interface; they do not exist. Use `gryvia quota` and `gryvia cost` for the figures that are implemented.

### Check Budget Status

```bash
# Team budget
kfctl budget status --team ml-research

# User budget
kfctl budget status --user alice

# All budgets
kfctl budget list
```

### Request Budget Increase

```bash
kfctl budget request-increase \
  --team ml-research \
  --amount 10000 \
  --reason "Critical LLM training deadline"
```

### Budget Reports

```bash
# Monthly report
kfctl budget report --month 2024-01

# Custom period
kfctl budget report \
  --start 2024-01-01 \
  --end 2024-01-31 \
  --team ml-research \
  --output report.pdf
```

## Budget Analytics

### Cost Breakdown

```bash
kfctl budget breakdown --team ml-research

# Output:
# Cost Breakdown for ml-research (Jan 2024):
#
# By GPU Type:
#   H100:        $18,000 (48%)  150 hours @ $120/hr
#   A100-80G:    $15,000 (40%)  750 hours @ $20/hr
#   A100-40G:    $3,200  (8.5%) 400 hours @ $8/hr
#   T4:          $600    (1.6%) 200 hours @ $3/hr
#
# By Job Type:
#   Training:    $28,500 (76%)
#   Inference:   $6,000  (16%)
#   Development: $3,000  (8%)
#
# By User:
#   alice:       $12,000 (32%)
#   bob:         $9,000  (24%)
#   charlie:     $8,500  (22.7%)
#   ...
```

### Efficiency Metrics

```bash
kfctl budget efficiency --team ml-research

# Output:
# Efficiency Metrics:
#
# GPU Utilization: 78.5%
# Cost per Successful Job: $109.65
# Wasted Budget (failed jobs): $2,340 (6.2%)
#
# Recommendations:
#   • Enable spot instances (potential $8,500 savings)
#   • Use MIG for small jobs (potential $3,200 savings)
#   • Right-size A100 jobs (potential $1,800 savings)
```

## Notifications

### Email Alerts

Configure SMTP in Gryvia:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: gryvia-config
data:
  smtp_host: smtp.company.com
  smtp_port: "587"
  smtp_from: gryvia@company.com
```

### Slack Integration

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: gryvia-slack
type: Opaque
stringData:
  webhook_url: https://hooks.slack.com/services/YOUR/WEBHOOK/URL
```

Budget alert in Slack:
```
🚨 Budget Alert: ml-research

Budget: $50,000
Used: $45,000 (90%)
Remaining: $5,000

Projected end-of-month: $64,285
⚠️ Budget will be exceeded

Action: Review spending and optimize workloads
```

### Webhook Integration

```yaml
alerts:
  - threshold: 90
    actions: [webhook]
    webhookURL: https://api.company.com/budget-alerts
```

Webhook payload:
```json
{
  "budget": "ml-team-budget",
  "scope": "ml-research",
  "threshold": 90,
  "usage": {
    "costUSD": 45000,
    "percent": 90
  },
  "forecast": {
    "projectedCostUSD": 64285,
    "budgetSufficient": false
  },
  "timestamp": "2024-01-20T10:30:00Z"
}
```

## Best Practices

### 1. Set Realistic Budgets

```bash
# Analyze historical usage
kfctl budget analyze --team ml-research --days 90

# Output: Recommended monthly budget: $47,500
```

### 2. Use Graduated Alerts

Don't just alert at 100%. Set alerts at 50%, 75%, 90%, and 100%.

### 3. Enable Forecasting

Review forecasts weekly to catch budget overruns early.

### 4. Combine Budget Types

```yaml
# Team-level budget
- scope: {type: team, name: ml-research}
  limits: {costUSD: 50000}

# User-level daily limits
- scope: {type: user, name: alice}
  limits: {costUSD: 1000}
  period: {type: daily}
```

### 5. Use Rollover for Flexibility

```yaml
rollover:
  enabled: true
  maxRolloverPercent: 20
```

Allows teams to save budget for large quarterly experiments.

### 6. Monitor Efficiency

```bash
# Weekly efficiency check
kfctl budget efficiency --team ml-research
```

## Troubleshooting

### Jobs Blocked by Budget

```bash
# Check budget status
kubectl get gryviabudget -o wide

# Request temporary override
kfctl budget override \
  --team ml-research \
  --duration 24h \
  --reason "Critical deadline"
```

### Budget Not Enforcing

```bash
# Check budget controller logs
kubectl logs -n gryvia-system deploy/budget-controller

# Verify budget is enabled
kubectl get gryviabudget my-budget -o yaml | grep enforcement
```

### Alerts Not Sending

```bash
# Test email configuration
kfctl budget test-alert --type email --recipient test@company.com

# Check alert history
kubectl get gryviabudget my-budget \
  -o jsonpath='{.status.alerts}' | jq
```

## Advanced Examples

### Multi-Tier Budget System

```yaml
# Organization-wide annual budget
---
apiVersion: gryvia.io/v1alpha1
kind: GryviaBudget
metadata:
  name: org-annual
spec:
  scope: {type: organization, name: engineering}
  period: {type: annual}
  limits: {costUSD: 2000000}  # $2M/year

# Department quarterly budgets
---
apiVersion: gryvia.io/v1alpha1
kind: GryviaBudget
metadata:
  name: ml-dept-q1
spec:
  scope: {type: department, name: machine-learning}
  period: {type: quarterly}
  limits: {costUSD: 300000}  # $300k/quarter

# Team monthly budgets
---
apiVersion: gryvia.io/v1alpha1
kind: GryviaBudget
metadata:
  name: ml-research-monthly
spec:
  scope: {type: team, name: ml-research}
  period: {type: monthly}
  limits: {costUSD: 50000}  # $50k/month

# User daily budgets
---
apiVersion: gryvia.io/v1alpha1
kind: GryviaBudget
metadata:
  name: alice-daily
spec:
  scope: {type: user, name: alice}
  period: {type: daily}
  limits: {costUSD: 1000}  # $1k/day
```

### Cost Center Chargeback

```bash
# Generate monthly chargeback report
kfctl budget chargeback --month 2024-01 --output chargeback.csv

# Output CSV:
# cost_center,team,users,gpu_hours,cost_usd
# CC-1001,ml-research,15,1500,37500
# CC-1002,cv-team,8,800,19200
# CC-1003,nlp-team,12,1200,28800
```

## Monitoring

### Grafana Dashboard

```json
{
  "panels": [
    {
      "title": "Budget Utilization",
      "targets": [{
        "expr": "gryvia_budget_utilization_percent"
      }]
    },
    {
      "title": "Budgets by State",
      "targets": [{
        "expr": "sum(gryvia_budget_state) by (state)"
      }]
    },
    {
      "title": "Projected Budget Exhaustion",
      "targets": [{
        "expr": "gryvia_budget_days_until_exhaustion"
      }]
    }
  ]
}
```

### Prometheus Alerts

```yaml
groups:
  - name: budget_alerts
    rules:
      - alert: BudgetExceeded
        expr: gryvia_budget_utilization_percent > 100
        annotations:
          summary: "Budget {{ $labels.budget }} exceeded"

      - alert: BudgetNearlyExhausted
        expr: gryvia_budget_days_until_exhaustion < 5
        annotations:
          summary: "Budget {{ $labels.budget }} will be exhausted in < 5 days"
```

## Support

- Budget Issues: https://github.com/zyvorai/gryvia/issues
- Finance Integration: https://github.com/zyvorai/gryvia/discussions
