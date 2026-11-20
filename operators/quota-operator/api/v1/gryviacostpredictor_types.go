// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaCostPredictorSpec defines the desired state of GryviaCostPredictor
type GryviaCostPredictorSpec struct {
	// HistoricalData configures how historical job data is collected and used
	HistoricalData HistoricalDataSpec `json:"historicalData,omitempty"`

	// Models configures the prediction models
	Models PredictionModelsSpec `json:"models,omitempty"`

	// Alternatives configures alternative configuration recommendations
	Alternatives AlternativesSpec `json:"alternatives,omitempty"`

	// Integration configures how the predictor integrates with the cluster
	Integration IntegrationSpec `json:"integration,omitempty"`

	// Pricing configures GPU pricing data
	Pricing PricingSpec `json:"pricing,omitempty"`
}

// HistoricalDataSpec configures historical data collection
type HistoricalDataSpec struct {
	// LookbackDays is the number of days of historical data to analyze
	LookbackDays int `json:"lookbackDays,omitempty"`

	// MinimumSamples is the minimum number of similar jobs needed for a prediction
	MinimumSamples int `json:"minimumSamples,omitempty"`

	// SimilarityFactors are the factors used to find similar historical jobs
	SimilarityFactors []string `json:"similarityFactors,omitempty"`
}

// PredictionModelsSpec configures prediction model parameters
type PredictionModelsSpec struct {
	// TrainingTime configures training time estimation
	TrainingTime TrainingTimeModelSpec `json:"trainingTime,omitempty"`

	// CostEstimation configures cost estimation
	CostEstimation CostEstimationModelSpec `json:"costEstimation,omitempty"`

	// QueueTime configures queue wait time estimation
	QueueTime QueueTimeModelSpec `json:"queueTime,omitempty"`
}

// TrainingTimeModelSpec configures training time estimation
type TrainingTimeModelSpec struct {
	// Method is the estimation method (linear-regression, weighted-average, percentile)
	Method string `json:"method,omitempty"`

	// Features used for training time estimation
	Features []string `json:"features,omitempty"`
}

// CostEstimationModelSpec configures cost estimation
type CostEstimationModelSpec struct {
	// IncludeStorageCost includes storage costs in estimate
	IncludeStorageCost bool `json:"includeStorageCost,omitempty"`

	// IncludeNetworkCost includes network egress costs in estimate
	IncludeNetworkCost bool `json:"includeNetworkCost,omitempty"`

	// Currency for cost estimates (default: USD)
	Currency string `json:"currency,omitempty"`
}

// QueueTimeModelSpec configures queue time estimation
type QueueTimeModelSpec struct {
	// Method is the estimation method (exponential-smoothing, moving-average, percentile)
	Method string `json:"method,omitempty"`

	// IncludePreemptionRisk factors in preemption risk when estimating queue time
	IncludePreemptionRisk bool `json:"includePreemptionRisk,omitempty"`
}

// AlternativesSpec configures alternative configuration recommendations
type AlternativesSpec struct {
	// Enabled enables alternative configuration suggestions
	Enabled bool `json:"enabled,omitempty"`

	// Strategies defines the optimization strategies to evaluate
	Strategies []AlternativeStrategy `json:"strategies,omitempty"`

	// GPUTypesToConsider lists GPU types to evaluate in alternatives
	GPUTypesToConsider []string `json:"gpuTypesToConsider,omitempty"`

	// IncludeSpotEstimates includes spot/preemptible instance estimates
	IncludeSpotEstimates bool `json:"includeSpotEstimates,omitempty"`
}

// AlternativeStrategy defines an optimization strategy
type AlternativeStrategy struct {
	// Name is the strategy name
	Name string `json:"name"`

	// Constraint is the optimization constraint (minimize-cost, minimize-time, minimize-queue, balance)
	Constraint string `json:"constraint"`
}

// IntegrationSpec configures cluster integration
type IntegrationSpec struct {
	// DryRunMode only estimates without acting on predictions
	DryRunMode bool `json:"dryRunMode,omitempty"`

	// WebhookAdmission enables validating webhook to annotate jobs
	WebhookAdmission bool `json:"webhookAdmission,omitempty"`

	// InjectEstimateAnnotation injects cost/time estimates as annotations
	InjectEstimateAnnotation bool `json:"injectEstimateAnnotation,omitempty"`
}

// PricingSpec configures GPU pricing
type PricingSpec struct {
	// ChargebackRef references a GryviaChargeback CR for pricing data
	ChargebackRef string `json:"chargebackRef,omitempty"`

	// PerGpuHour is an inline per-GPU-hour pricing map
	PerGpuHour map[string]float64 `json:"perGpuHour,omitempty"`
}

// GryviaCostPredictorStatus defines the observed state of GryviaCostPredictor
type GryviaCostPredictorStatus struct {
	// PredictionAccuracy tracks the accuracy of predictions
	PredictionAccuracy PredictionAccuracyStatus `json:"predictionAccuracy,omitempty"`

	// JobsEstimated is the total number of jobs estimated
	JobsEstimated int `json:"jobsEstimated,omitempty"`

	// TotalSavingsFromRecommendations is the total savings from alternative recommendations (USD)
	TotalSavingsFromRecommendations float64 `json:"totalSavingsFromRecommendations,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastUpdated is the timestamp of last status update
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`
}

// PredictionAccuracyStatus tracks prediction accuracy metrics
type PredictionAccuracyStatus struct {
	// TimeEstimate accuracy metrics
	TimeEstimate AccuracyMetrics `json:"timeEstimate,omitempty"`

	// CostEstimate accuracy metrics
	CostEstimate AccuracyMetrics `json:"costEstimate,omitempty"`
}

// AccuracyMetrics holds accuracy measurements
type AccuracyMetrics struct {
	// MAE is the mean absolute error
	MAE float64 `json:"mae,omitempty"`

	// Within25Percent is the percentage of estimates within 25% of actual
	Within25Percent float64 `json:"within25Percent,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

// GryviaCostPredictor is the Schema for the gryviacostpredictors API
type GryviaCostPredictor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaCostPredictorSpec   `json:"spec,omitempty"`
	Status GryviaCostPredictorStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaCostPredictorList contains a list of GryviaCostPredictor
type GryviaCostPredictorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaCostPredictor `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaCostPredictor{}, &GryviaCostPredictorList{})
}
