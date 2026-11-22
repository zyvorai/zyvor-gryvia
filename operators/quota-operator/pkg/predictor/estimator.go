// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package predictor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/budget"
)

// JobEstimate holds cost/time/queue predictions for a job
type JobEstimate struct {
	// EstimatedDuration is the predicted training duration
	EstimatedDuration time.Duration

	// EstimatedCost is the predicted total cost in the configured currency
	EstimatedCost float64

	// EstimatedQueueWait is the predicted queue wait time
	EstimatedQueueWait time.Duration

	// Confidence is a 0-1 confidence score for this estimate
	Confidence float64

	// SimilarJobCount is the number of similar historical jobs used
	SimilarJobCount int

	// StorageCost is the estimated storage cost component
	StorageCost float64

	// NetworkCost is the estimated network cost component
	NetworkCost float64
}

// AlternativeConfig represents an alternative job configuration
type AlternativeConfig struct {
	// Name is the strategy name
	Name string

	// GPUType is the suggested GPU type
	GPUType string

	// GPUCount is the suggested GPU count
	GPUCount int32

	// EstimatedCost is the estimated cost with this configuration
	EstimatedCost float64

	// EstimatedDuration is the estimated duration with this configuration
	EstimatedDuration time.Duration

	// EstimatedQueueWait is the estimated queue wait with this configuration
	EstimatedQueueWait time.Duration

	// CostSavings is the savings compared to the original configuration
	CostSavings float64

	// TimeDifference is the time difference compared to original (positive = slower)
	TimeDifference time.Duration

	// IsSpot indicates if this uses spot/preemptible instances
	IsSpot bool
}

// SimilarJob represents a historical job matched by similarity
type SimilarJob struct {
	// Job is the historical GryviaAIJob
	Job gryviav1.GryviaAIJob

	// SimilarityScore is how similar this job is to the query (0-1)
	SimilarityScore float64
}

// storageCostPerGBHour is the approximate storage cost per GB per hour
const storageCostPerGBHour = 0.0001

// networkCostPerGB is the approximate network egress cost per GB
const networkCostPerGB = 0.08

// spotDiscount is the discount factor for spot instances
const spotDiscount = 0.4

// EstimateJobCost produces a cost/time/queue estimate for a job based on
// historical data and the predictor configuration.
func EstimateJobCost(
	ctx context.Context,
	k8sClient client.Client,
	predictor *gryviav1.GryviaCostPredictor,
	job *gryviav1.GryviaAIJob,
	allJobs []gryviav1.GryviaAIJob,
) (*JobEstimate, error) {
	// Find similar historical jobs
	similarJobs := FindSimilarJobs(predictor, job, allJobs)

	minSamples := predictor.Spec.HistoricalData.MinimumSamples
	if minSamples <= 0 {
		minSamples = 10
	}

	estimate := &JobEstimate{
		SimilarJobCount: len(similarJobs),
	}

	if len(similarJobs) < minSamples {
		// Not enough data: fall back to a simple rate-based estimate
		estimate.Confidence = 0.1
		rate := getGPURate(predictor, job.Spec.GpuType)
		// Default estimate: 1 hour per GPU
		estimate.EstimatedDuration = time.Hour
		estimate.EstimatedCost = rate * float64(job.Spec.TotalGPUs())
		return estimate, nil
	}

	// Estimate training duration
	estimate.EstimatedDuration = estimateTrainingTime(predictor, similarJobs)

	// Estimate GPU cost
	rate := getGPURate(predictor, job.Spec.GpuType)
	gpuCost := rate * estimate.EstimatedDuration.Hours() * float64(job.Spec.TotalGPUs())
	estimate.EstimatedCost = gpuCost

	// Add storage cost if configured
	if predictor.Spec.Models.CostEstimation.IncludeStorageCost {
		storageGB := parseStorageGB(job.Spec.StorageRequest)
		estimate.StorageCost = storageGB * storageCostPerGBHour * estimate.EstimatedDuration.Hours()
		estimate.EstimatedCost += estimate.StorageCost
	}

	// Add network cost if configured
	if predictor.Spec.Models.CostEstimation.IncludeNetworkCost {
		// Estimate network usage from similar jobs (approximate)
		estimatedEgressGB := float64(job.Spec.TotalGPUs()) * 10.0 // rough heuristic
		estimate.NetworkCost = estimatedEgressGB * networkCostPerGB
		estimate.EstimatedCost += estimate.NetworkCost
	}

	// Estimate queue wait
	estimate.EstimatedQueueWait = QueueWaitEstimate(ctx, k8sClient, predictor, job, allJobs)

	// Calculate confidence based on sample size and variance
	estimate.Confidence = calculateConfidence(similarJobs, minSamples)

	return estimate, nil
}

// FindSimilarJobs finds historical jobs similar to the given job based on
// the configured similarity factors.
func FindSimilarJobs(
	predictor *gryviav1.GryviaCostPredictor,
	job *gryviav1.GryviaAIJob,
	allJobs []gryviav1.GryviaAIJob,
) []SimilarJob {
	lookbackDays := predictor.Spec.HistoricalData.LookbackDays
	if lookbackDays <= 0 {
		lookbackDays = 90
	}
	cutoff := time.Now().Add(-time.Duration(lookbackDays) * 24 * time.Hour)

	factors := predictor.Spec.HistoricalData.SimilarityFactors
	if len(factors) == 0 {
		factors = []string{"gpuType", "gpuCount", "modelType"}
	}

	factorSet := make(map[string]bool, len(factors))
	for _, f := range factors {
		factorSet[f] = true
	}

	var similar []SimilarJob

	for _, candidate := range allJobs {
		// Only consider completed jobs
		if candidate.Status.Phase != "Succeeded" && candidate.Status.Phase != "Failed" {
			continue
		}

		// Must have both start and completion times for duration calculation
		if candidate.Status.StartTime == nil || candidate.Status.CompletionTime == nil {
			continue
		}

		// Only consider jobs within the lookback window
		if candidate.Status.CompletionTime.Time.Before(cutoff) {
			continue
		}

		score := computeSimilarity(factorSet, job, &candidate)
		if score > 0 {
			similar = append(similar, SimilarJob{
				Job:             candidate,
				SimilarityScore: score,
			})
		}
	}

	// Sort by similarity score descending
	sort.Slice(similar, func(i, j int) bool {
		return similar[i].SimilarityScore > similar[j].SimilarityScore
	})

	return similar
}

// GenerateAlternatives produces alternative configurations that optimize
// for cost, time, or queue wait based on the predictor's strategy settings.
func GenerateAlternatives(
	ctx context.Context,
	k8sClient client.Client,
	predictor *gryviav1.GryviaCostPredictor,
	job *gryviav1.GryviaAIJob,
	allJobs []gryviav1.GryviaAIJob,
	baseEstimate *JobEstimate,
) ([]AlternativeConfig, error) {
	if !predictor.Spec.Alternatives.Enabled {
		return nil, nil
	}

	gpuTypes := predictor.Spec.Alternatives.GPUTypesToConsider
	if len(gpuTypes) == 0 {
		gpuTypes = []string{"H100", "A100-80G", "A100-40G", "L40", "V100", "T4"}
	}

	strategies := predictor.Spec.Alternatives.Strategies
	if len(strategies) == 0 {
		strategies = []gryviav1.AlternativeStrategy{
			{Name: "cost-optimized", Constraint: "minimize-cost"},
			{Name: "speed-optimized", Constraint: "minimize-time"},
		}
	}

	var alternatives []AlternativeConfig

	for _, strategy := range strategies {
		best, err := findBestAlternative(ctx, k8sClient, predictor, job, allJobs, baseEstimate, gpuTypes, strategy)
		if err != nil {
			continue
		}
		if best != nil {
			alternatives = append(alternatives, *best)
		}

		// Include spot estimate if configured
		if predictor.Spec.Alternatives.IncludeSpotEstimates && best != nil {
			spotAlt := *best
			spotAlt.Name = fmt.Sprintf("%s-spot", strategy.Name)
			spotAlt.EstimatedCost *= spotDiscount
			spotAlt.CostSavings = baseEstimate.EstimatedCost - spotAlt.EstimatedCost
			spotAlt.IsSpot = true
			alternatives = append(alternatives, spotAlt)
		}
	}

	return alternatives, nil
}

// QueueWaitEstimate estimates how long a job will wait in queue.
func QueueWaitEstimate(
	ctx context.Context,
	k8sClient client.Client,
	predictor *gryviav1.GryviaCostPredictor,
	job *gryviav1.GryviaAIJob,
	allJobs []gryviav1.GryviaAIJob,
) time.Duration {
	// Collect queue wait times from recent jobs
	lookbackDays := predictor.Spec.HistoricalData.LookbackDays
	if lookbackDays <= 0 {
		lookbackDays = 90
	}
	cutoff := time.Now().Add(-time.Duration(lookbackDays) * 24 * time.Hour)

	var queueWaits []float64

	for _, j := range allJobs {
		if j.Status.StartTime == nil || j.CreationTimestamp.IsZero() {
			continue
		}
		if j.Status.StartTime.Time.Before(cutoff) {
			continue
		}

		// Queue wait = start time - creation time
		wait := j.Status.StartTime.Time.Sub(j.CreationTimestamp.Time)
		if wait < 0 {
			continue
		}

		// Only consider jobs with matching GPU type for queue estimation
		if job.Spec.GpuType != "" && j.Spec.GpuType != "" && job.Spec.GpuType != j.Spec.GpuType {
			continue
		}

		queueWaits = append(queueWaits, wait.Seconds())
	}

	if len(queueWaits) == 0 {
		return 5 * time.Minute // default estimate
	}

	method := predictor.Spec.Models.QueueTime.Method
	var estimatedSeconds float64

	switch method {
	case "percentile":
		// Use P75 for a conservative estimate
		sort.Float64s(queueWaits)
		idx := int(float64(len(queueWaits)) * 0.75)
		if idx >= len(queueWaits) {
			idx = len(queueWaits) - 1
		}
		estimatedSeconds = queueWaits[idx]

	case "moving-average":
		// Use the last N samples (N = min(20, len))
		n := 20
		if len(queueWaits) < n {
			n = len(queueWaits)
		}
		recent := queueWaits[len(queueWaits)-n:]
		sum := 0.0
		for _, w := range recent {
			sum += w
		}
		estimatedSeconds = sum / float64(n)

	default: // exponential-smoothing
		alpha := 0.3
		estimatedSeconds = queueWaits[0]
		for i := 1; i < len(queueWaits); i++ {
			estimatedSeconds = alpha*queueWaits[i] + (1-alpha)*estimatedSeconds
		}
	}

	// Factor in preemption risk: if the job has low priority, increase estimate
	if predictor.Spec.Models.QueueTime.IncludePreemptionRisk {
		if job.Spec.Priority < 50 {
			// Low priority jobs are more likely to be preempted or wait longer
			preemptionMultiplier := 1.0 + float64(50-job.Spec.Priority)/100.0
			estimatedSeconds *= preemptionMultiplier
		}
	}

	// Count currently queued jobs requesting the same GPU type
	queuedCount := 0
	for _, j := range allJobs {
		if j.Status.Phase == "Pending" || j.Status.Phase == "Queued" {
			if job.Spec.GpuType == "" || j.Spec.GpuType == job.Spec.GpuType {
				queuedCount++
			}
		}
	}

	// Adjust for current queue depth
	if queuedCount > 0 {
		estimatedSeconds *= (1.0 + float64(queuedCount)*0.1)
	}

	return time.Duration(estimatedSeconds) * time.Second
}

// computeSimilarity calculates a similarity score between a target job and a candidate
func computeSimilarity(factors map[string]bool, target *gryviav1.GryviaAIJob, candidate *gryviav1.GryviaAIJob) float64 {
	if len(factors) == 0 {
		return 0
	}

	totalFactors := 0
	matchedFactors := 0.0

	if factors["gpuType"] {
		totalFactors++
		if target.Spec.GpuType == candidate.Spec.GpuType {
			matchedFactors += 1.0
		}
	}

	if factors["gpuCount"] {
		totalFactors++
		if target.Spec.GPUs == candidate.Spec.GPUs {
			matchedFactors += 1.0
		} else {
			// Partial match based on GPU count ratio
			ratio := float64(min32(target.Spec.GPUs, candidate.Spec.GPUs)) / float64(max32(target.Spec.GPUs, candidate.Spec.GPUs))
			if ratio > 0.5 {
				matchedFactors += ratio
			}
		}
	}

	if factors["modelType"] {
		totalFactors++
		if target.Spec.Model != "" && target.Spec.Model == candidate.Spec.Model {
			matchedFactors += 1.0
		}
	}

	if factors["datasetSize"] {
		totalFactors++
		targetSize := parseStorageGB(target.Spec.StorageRequest)
		candidateSize := parseStorageGB(candidate.Spec.StorageRequest)
		if targetSize > 0 && candidateSize > 0 {
			ratio := math.Min(targetSize, candidateSize) / math.Max(targetSize, candidateSize)
			if ratio > 0.3 {
				matchedFactors += ratio
			}
		}
	}

	if factors["framework"] {
		totalFactors++
		targetFW := getFramework(target)
		candidateFW := getFramework(candidate)
		if targetFW != "" && targetFW == candidateFW {
			matchedFactors += 1.0
		}
	}

	if factors["distributedConfig"] {
		totalFactors++
		targetDist := target.Spec.Distributed != nil && target.Spec.Distributed.Enabled
		candidateDist := candidate.Spec.Distributed != nil && candidate.Spec.Distributed.Enabled
		if targetDist == candidateDist {
			matchedFactors += 1.0
		}
	}

	if totalFactors == 0 {
		return 0
	}

	return matchedFactors / float64(totalFactors)
}

// estimateTrainingTime estimates training duration from similar historical jobs
func estimateTrainingTime(predictor *gryviav1.GryviaCostPredictor, similarJobs []SimilarJob) time.Duration {
	if len(similarJobs) == 0 {
		return time.Hour
	}

	method := predictor.Spec.Models.TrainingTime.Method

	var durations []weightedDuration
	for _, sj := range similarJobs {
		if sj.Job.Status.StartTime == nil || sj.Job.Status.CompletionTime == nil {
			continue
		}
		d := sj.Job.Status.CompletionTime.Time.Sub(sj.Job.Status.StartTime.Time)
		if d > 0 {
			durations = append(durations, weightedDuration{
				duration: d,
				weight:   sj.SimilarityScore,
			})
		}
	}

	if len(durations) == 0 {
		return time.Hour
	}

	switch method {
	case "linear-regression":
		// Simple weighted linear regression using duration and similarity
		// For this implementation, we use weighted average as a practical approximation
		return weightedAverage(durations)

	case "percentile":
		// Use P50 (median) of sorted durations
		sort.Slice(durations, func(i, j int) bool {
			return durations[i].duration < durations[j].duration
		})
		idx := len(durations) / 2
		return durations[idx].duration

	default: // weighted-average
		return weightedAverage(durations)
	}
}

type weightedDuration struct {
	duration time.Duration
	weight   float64
}

func weightedAverage(durations []weightedDuration) time.Duration {
	totalWeight := 0.0
	weightedSum := 0.0
	for _, wd := range durations {
		weightedSum += float64(wd.duration.Nanoseconds()) * wd.weight
		totalWeight += wd.weight
	}
	if totalWeight == 0 {
		return time.Hour
	}
	return time.Duration(int64(weightedSum / totalWeight))
}

// calculateConfidence calculates confidence based on sample size and variance
func calculateConfidence(similarJobs []SimilarJob, minSamples int) float64 {
	if len(similarJobs) == 0 {
		return 0
	}

	// Base confidence from sample size (capped at 0.5)
	sampleConfidence := math.Min(float64(len(similarJobs))/float64(minSamples*3), 0.5)

	// Similarity-based confidence (average similarity score, capped at 0.5)
	totalSimilarity := 0.0
	for _, sj := range similarJobs {
		totalSimilarity += sj.SimilarityScore
	}
	avgSimilarity := totalSimilarity / float64(len(similarJobs))
	similarityConfidence := math.Min(avgSimilarity, 1.0) * 0.5

	return math.Min(sampleConfidence+similarityConfidence, 1.0)
}

// findBestAlternative finds the best alternative configuration for a given strategy
func findBestAlternative(
	ctx context.Context,
	k8sClient client.Client,
	predictor *gryviav1.GryviaCostPredictor,
	job *gryviav1.GryviaAIJob,
	allJobs []gryviav1.GryviaAIJob,
	baseEstimate *JobEstimate,
	gpuTypes []string,
	strategy gryviav1.AlternativeStrategy,
) (*AlternativeConfig, error) {
	var best *AlternativeConfig
	var bestScore float64

	for _, gpuType := range gpuTypes {
		// Skip the same configuration
		if gpuType == job.Spec.GpuType {
			continue
		}

		// Create a hypothetical job with this GPU type
		altJob := &gryviav1.GryviaAIJob{}
		*altJob = *job
		altJob.Spec.GpuType = gpuType

		// Find similar jobs for this configuration
		altSimilar := FindSimilarJobs(predictor, altJob, allJobs)
		if len(altSimilar) == 0 {
			continue
		}

		// Estimate duration with this config
		altDuration := estimateTrainingTime(predictor, altSimilar)

		// Estimate cost with this config
		rate := getGPURate(predictor, gpuType)
		altCost := rate * altDuration.Hours() * float64(job.Spec.TotalGPUs())

		// Estimate queue wait
		altQueueWait := QueueWaitEstimate(ctx, k8sClient, predictor, altJob, allJobs)

		alt := &AlternativeConfig{
			Name:               strategy.Name,
			GPUType:            gpuType,
			GPUCount:           job.Spec.TotalGPUs(),
			EstimatedCost:      altCost,
			EstimatedDuration:  altDuration,
			EstimatedQueueWait: altQueueWait,
			CostSavings:        baseEstimate.EstimatedCost - altCost,
			TimeDifference:     altDuration - baseEstimate.EstimatedDuration,
		}

		// Score the alternative based on the strategy constraint
		score := scoreAlternative(alt, baseEstimate, strategy.Constraint)

		if best == nil || score > bestScore {
			best = alt
			bestScore = score
		}
	}

	return best, nil
}

// scoreAlternative assigns a score to an alternative based on the optimization constraint
func scoreAlternative(alt *AlternativeConfig, base *JobEstimate, constraint string) float64 {
	switch constraint {
	case "minimize-cost":
		if base.EstimatedCost == 0 {
			return 0
		}
		return alt.CostSavings / base.EstimatedCost

	case "minimize-time":
		if base.EstimatedDuration == 0 {
			return 0
		}
		timeSaved := base.EstimatedDuration - alt.EstimatedDuration
		return float64(timeSaved) / float64(base.EstimatedDuration)

	case "minimize-queue":
		if base.EstimatedQueueWait == 0 {
			return 0
		}
		queueSaved := base.EstimatedQueueWait - alt.EstimatedQueueWait
		return float64(queueSaved) / float64(base.EstimatedQueueWait)

	case "balance":
		// Balanced score: equal weight to cost, time, and queue improvements
		costScore := 0.0
		if base.EstimatedCost > 0 {
			costScore = alt.CostSavings / base.EstimatedCost
		}
		timeScore := 0.0
		if base.EstimatedDuration > 0 {
			timeSaved := base.EstimatedDuration - alt.EstimatedDuration
			timeScore = float64(timeSaved) / float64(base.EstimatedDuration)
		}
		queueScore := 0.0
		if base.EstimatedQueueWait > 0 {
			queueSaved := base.EstimatedQueueWait - alt.EstimatedQueueWait
			queueScore = float64(queueSaved) / float64(base.EstimatedQueueWait)
		}
		return (costScore + timeScore + queueScore) / 3.0

	default:
		return 0
	}
}

// getGPURate returns the per-hour rate for a GPU type from predictor pricing config
func getGPURate(predictor *gryviav1.GryviaCostPredictor, gpuType string) float64 {
	if len(predictor.Spec.Pricing.PerGpuHour) > 0 {
		if rate, exists := predictor.Spec.Pricing.PerGpuHour[gpuType]; exists {
			return rate
		}
		// Check for a default entry
		if rate, exists := predictor.Spec.Pricing.PerGpuHour["default"]; exists {
			return rate
		}
	}
	// Fall back to the budget package's GPU rate
	return budget.GetGPURate(gpuType)
}

// parseStorageGB parses a Kubernetes quantity string to GB
func parseStorageGB(storageRequest string) float64 {
	if storageRequest == "" {
		return 0
	}

	// Simple parsing for common formats
	var value float64
	var unit string
	_, err := fmt.Sscanf(storageRequest, "%f%s", &value, &unit)
	if err != nil {
		// Try parsing as just a number (bytes)
		_, err = fmt.Sscanf(storageRequest, "%f", &value)
		if err != nil {
			return 0
		}
		return value / (1024 * 1024 * 1024)
	}

	switch unit {
	case "Ti":
		return value * 1024
	case "Gi":
		return value
	case "Mi":
		return value / 1024
	case "Ki":
		return value / (1024 * 1024)
	case "T":
		return value * 1000
	case "G":
		return value
	case "M":
		return value / 1000
	default:
		return value
	}
}

// getFramework extracts the framework from a job's distributed config
func getFramework(job *gryviav1.GryviaAIJob) string {
	if job.Spec.Distributed != nil {
		return job.Spec.Distributed.Framework
	}
	return ""
}

func min32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
