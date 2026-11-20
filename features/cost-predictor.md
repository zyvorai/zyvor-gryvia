# Cost Predictor

Predict costs, training time, and queue wait for AI/ML jobs before submission.

> **Status: controller implemented (quota-operator); estimates are heuristics, accuracy unmeasured.**
> `GryviaCostPredictor` has a registered controller. It annotates pending or
> `gryvia.io/dry-run: "true"` jobs with `estimated-cost`, `estimated-duration`,
> `estimated-queue-wait`, `cost-confidence` and a best cheaper alternative, using similarity-weighted
> history of past `GryviaAIJob`s (`operators/quota-operator/pkg/predictor`). Gaps versus the text below:
> `linear-regression` is currently the same weighted average as the default; the `webhookAdmission`
> and `chargebackRef` fields exist in the CRD but nothing reads them (`GryviaChargeback` has its
> own controller, which the predictor does not consult); with no similar history the time estimate falls back to one hour. Example numbers
> below (costs, MAE, "342 jobs estimated") are illustrative, not measured, and no accuracy claim
> has been validated against real clusters. Prices are whatever you configure, not real quotes.

## Overview

GryviaCostPredictor analyzes historical job data to provide accurate estimates for new jobs:

- **Cost Estimation**: Predict total GPU, storage, and network costs before a job starts
- **Training Time Estimation**: Estimate how long a job will take based on similar historical jobs
- **Queue Wait Estimation**: Predict queue wait time based on current cluster load and job priority
- **Alternative Recommendations**: Suggest cheaper or faster GPU configurations
- **Accuracy Tracking**: Continuously measure and improve prediction accuracy

## Quick Start

### Create a Cost Predictor

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaCostPredictor
metadata:
  name: default-predictor
spec:
  historicalData:
    lookbackDays: 90
    minimumSamples: 10
    similarityFactors:
      - gpuType
      - gpuCount
      - modelType
      - datasetSize

  models:
    trainingTime:
      method: weighted-average
    costEstimation:
      includeStorageCost: true
      currency: USD
    queueTime:
      method: exponential-smoothing
      includePreemptionRisk: true

  alternatives:
    enabled: true
    strategies:
      - name: cost-optimized
        constraint: minimize-cost
      - name: speed-optimized
        constraint: minimize-time
    gpuTypesToConsider:
      - H100
      - A100-80G
      - A100-40G
      - L40
      - V100

  pricing:
    perGpuHour:
      H100: 8.00
      A100-80G: 4.00
      A100-40G: 3.00
      L40: 2.50
      V100: 2.00
      T4: 0.75
```

Apply:
```bash
kubectl apply -f cost-predictor.yaml
```

### Get a Cost Estimate for a Job

Submit a job with the dry-run annotation to get an estimate without running it:

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: llm-training-estimate
  annotations:
    gryvia.io/dry-run: "true"
spec:
  type: training
  model: llama-70b
  gpus: 8
  gpuType: H100
  image: training:latest
```

After the predictor processes it, check the annotations:

```bash
kubectl get gryviaaijob llm-training-estimate -o jsonpath='{.metadata.annotations}'
```

Output:
```json
{
  "gryvia.io/dry-run": "true",
  "gryvia.io/estimated-cost": "384.00",
  "gryvia.io/estimated-duration": "6h0m0s",
  "gryvia.io/estimated-queue-wait": "12m30s",
  "gryvia.io/cost-confidence": "0.75",
  "gryvia.io/alternative-gpu": "A100-80G",
  "gryvia.io/alternative-cost": "192.00",
  "gryvia.io/potential-savings": "192.00"
}
```

## Historical Data Configuration

### Lookback Window

Control how far back the predictor looks for similar jobs:

```yaml
historicalData:
  lookbackDays: 90  # Analyze last 90 days of jobs
```

### Minimum Samples

Set the minimum number of similar historical jobs required for a reliable prediction:

```yaml
historicalData:
  minimumSamples: 10  # Need at least 10 similar jobs
```

If fewer similar jobs are found, the predictor falls back to a simple rate-based estimate with low confidence.

### Similarity Factors

Configure which factors are used to find similar jobs:

```yaml
historicalData:
  similarityFactors:
    - gpuType         # Match by GPU model
    - gpuCount        # Match by number of GPUs
    - modelType       # Match by ML model name
    - datasetSize     # Match by dataset size
    - framework       # Match by training framework
    - distributedConfig  # Match by distributed vs single-node
```

## Prediction Models

### Training Time

Choose the estimation method:

```yaml
models:
  trainingTime:
    method: weighted-average  # Weight by similarity score
    features:
      - gpuCount
      - modelSize
      - datasetSize
```

Available methods:
- `weighted-average` (default) - Weighted average of similar job durations, weighted by similarity score
- `linear-regression` - Linear regression using specified features
- `percentile` - P50 (median) of similar job durations

### Cost Estimation

```yaml
models:
  costEstimation:
    includeStorageCost: true    # Include PVC storage costs
    includeNetworkCost: false   # Include network egress costs
    currency: USD
```

### Queue Time

```yaml
models:
  queueTime:
    method: exponential-smoothing
    includePreemptionRisk: true  # Low-priority jobs get higher estimates
```

Available methods:
- `exponential-smoothing` (default) - Exponential smoothing with alpha=0.3
- `moving-average` - Moving average of last 20 queue waits
- `percentile` - P75 of historical queue waits

## Alternative Recommendations

The predictor can suggest alternative configurations:

```yaml
alternatives:
  enabled: true
  strategies:
    - name: cost-optimized
      constraint: minimize-cost
    - name: speed-optimized
      constraint: minimize-time
    - name: queue-optimized
      constraint: minimize-queue
    - name: balanced
      constraint: balance
  gpuTypesToConsider:
    - H100
    - A100-80G
    - A100-40G
    - L40
    - V100
    - T4
  includeSpotEstimates: true
```

### Optimization Constraints

- `minimize-cost` - Find the cheapest GPU configuration
- `minimize-time` - Find the fastest GPU configuration
- `minimize-queue` - Find the configuration with shortest queue wait
- `balance` - Balance cost, time, and queue improvements equally

## Integration

### Annotation Injection

Automatically annotate pending jobs with estimates:

```yaml
integration:
  injectEstimateAnnotation: true  # Auto-annotate new jobs
```

### Dry-Run Mode

Test the predictor without modifying any jobs:

```yaml
integration:
  dryRunMode: true  # Only log estimates, don't annotate
```

### Webhook Admission

Enable a validating webhook to inject estimates at admission time:

```yaml
integration:
  webhookAdmission: true
```

## Pricing

### Inline Pricing

```yaml
pricing:
  perGpuHour:
    H100: 8.00
    A100-80G: 4.00
    A100-40G: 3.00
    L40: 2.50
    V100: 2.00
    T4: 0.75
```

### Chargeback Reference

Reference an existing GryviaChargeback CR:

```yaml
pricing:
  chargebackRef: company-pricing
```

## Monitoring Accuracy

Check predictor status:

```bash
kubectl get gryviacostpredictor default-predictor
```

Output:
```
NAME                JOBS-ESTIMATED  TIME-MAE  COST-MAE  SAVINGS  AGE
default-predictor   342             1.2       45.50     12500    30d
```

Detailed status:

```bash
kubectl get gryviacostpredictor default-predictor -o yaml
```

Status fields:
- `predictionAccuracy.timeEstimate.mae` - Mean absolute error for time estimates (hours)
- `predictionAccuracy.timeEstimate.within25Percent` - Percentage of time estimates within 25% of actual
- `predictionAccuracy.costEstimate.mae` - Mean absolute error for cost estimates (USD)
- `predictionAccuracy.costEstimate.within25Percent` - Percentage of cost estimates within 25% of actual
- `jobsEstimated` - Total number of jobs estimated
- `totalSavingsFromRecommendations` - Total savings from followed recommendations (USD)

## Architecture

1. The controller watches for new/pending GryviaAIJob resources
2. For each job, it finds similar historical jobs using configured similarity factors
3. It estimates training time using the configured method (weighted-average, regression, percentile)
4. It calculates cost from estimated time, GPU count, and pricing data
5. It estimates queue wait from historical queue times and current cluster load
6. It generates alternative configurations if alternatives are enabled
7. It annotates the job with estimates (unless in dry-run mode)
8. When jobs complete, it compares estimates to actuals and updates accuracy metrics

## Best Practices

1. Start with at least 30 days of historical job data before relying on predictions
2. Use `minimumSamples: 10` or higher for reliable estimates
3. Enable `includeStorageCost` for jobs with large datasets
4. Use `dryRunMode: true` initially to validate predictions before enabling auto-annotation
5. Monitor the `within25Percent` accuracy metrics; retune similarity factors if accuracy drops
6. Keep pricing data up to date, especially for on-premises clusters with custom rates

## Support

- Issues: https://github.com/zyvorai/gryvia/issues
- Discussions: https://github.com/zyvorai/gryvia/discussions
