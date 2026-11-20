// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaLiveExperimentSpec defines the desired state of GryviaLiveExperiment
type GryviaLiveExperimentSpec struct {
	// Description is a human-readable description of the experiment
	Description string `json:"description,omitempty"`

	// Jobs is the list of jobs to compare in this experiment
	Jobs []ExperimentJob `json:"jobs"`

	// Comparison defines how to compare the jobs
	Comparison ComparisonConfig `json:"comparison"`

	// Strategy defines comparison timing and early stopping policy
	Strategy *StrategyConfig `json:"strategy,omitempty"`

	// Notifications configures alerting and leaderboard updates
	Notifications *NotificationConfig `json:"notifications,omitempty"`
}

// ExperimentJob references a GryviaAIJob to include in the experiment
type ExperimentJob struct {
	// Name is a friendly name for this job in the leaderboard
	Name string `json:"name"`

	// JobRef is the name of the GryviaAIJob resource to track
	JobRef string `json:"jobRef"`
}

// ComparisonConfig defines how metrics are compared across jobs
type ComparisonConfig struct {
	// PrimaryMetric is the name of the primary metric to compare (e.g. loss, accuracy)
	PrimaryMetric string `json:"primaryMetric"`

	// Direction indicates whether to minimize or maximize the primary metric
	Direction string `json:"direction"`

	// SecondaryMetrics are additional metrics to factor into ranking
	SecondaryMetrics []SecondaryMetric `json:"secondaryMetrics,omitempty"`

	// MetricSource defines how to collect metrics
	MetricSource *MetricSourceConfig `json:"metricSource,omitempty"`
}

// SecondaryMetric defines a secondary metric with a weight for composite scoring
type SecondaryMetric struct {
	// Name is the metric name
	Name string `json:"name"`

	// Weight is the weight of this metric in the composite score (0-1)
	Weight float64 `json:"weight"`
}

// MetricSourceConfig defines how to collect metrics from jobs
type MetricSourceConfig struct {
	// Type is the source type: log-pattern or prometheus
	Type string `json:"type"`

	// Patterns maps metric names to regex patterns for log-pattern source
	Patterns map[string]string `json:"patterns,omitempty"`
}

// StrategyConfig defines comparison timing and early stopping
type StrategyConfig struct {
	// ComparisonPoints defines at which points to compare jobs
	ComparisonPoints []ComparisonPoint `json:"comparisonPoints,omitempty"`

	// EarlyTermination defines the early termination policy
	EarlyTermination *EarlyTerminationConfig `json:"earlyTermination,omitempty"`

	// AnomalyDetection configures anomaly detection
	AnomalyDetection *AnomalyDetectionConfig `json:"anomalyDetection,omitempty"`

	// Intervention configures automated intervention actions
	Intervention *InterventionConfig `json:"intervention,omitempty"`
}

// ComparisonPoint defines a point at which to compare jobs
type ComparisonPoint struct {
	// Type is the unit for comparison points: epoch or step
	Type string `json:"type"`

	// Values are the specific point values at which to compare
	Values []int `json:"values"`
}

// EarlyTerminationConfig defines the early termination policy
type EarlyTerminationConfig struct {
	// Enabled indicates whether early termination is enabled
	Enabled bool `json:"enabled"`

	// Method is the statistical method: bayesian, median, or percentile
	Method string `json:"method,omitempty"`

	// MinRunFraction is the minimum fraction of total run before termination (0-1)
	MinRunFraction float64 `json:"minRunFraction,omitempty"`

	// ConfidenceLevel is the confidence level required for termination (0-1)
	ConfidenceLevel float64 `json:"confidenceLevel,omitempty"`

	// GraceEpochs is the number of epochs to wait before considering termination
	GraceEpochs int `json:"graceEpochs,omitempty"`
}

// AnomalyDetectionConfig configures anomaly detection
type AnomalyDetectionConfig struct {
	// LossPlateauDetection detects when loss stops decreasing
	LossPlateauDetection bool `json:"lossPlateauDetection,omitempty"`

	// LossDivergenceDetection detects when loss starts increasing
	LossDivergenceDetection bool `json:"lossDivergenceDetection,omitempty"`

	// GradientExplosionDetection detects NaN/Inf loss values
	GradientExplosionDetection bool `json:"gradientExplosionDetection,omitempty"`
}

// InterventionConfig configures automated intervention
type InterventionConfig struct {
	// Enabled indicates whether automated intervention is enabled
	Enabled bool `json:"enabled"`

	// Actions is the list of intervention actions (terminate, notify, reduce-lr, checkpoint)
	Actions []string `json:"actions,omitempty"`
}

// NotificationConfig configures alerting and leaderboard updates
type NotificationConfig struct {
	// RealTimeLeaderboard enables real-time leaderboard updates in status
	RealTimeLeaderboard bool `json:"realTimeLeaderboard,omitempty"`

	// OnNewLeader sends notification when leaderboard leader changes
	OnNewLeader bool `json:"onNewLeader,omitempty"`

	// OnTermination sends notification when a job is early-terminated
	OnTermination bool `json:"onTermination,omitempty"`

	// Channel is the notification channel (e.g. slack://channel, email://addr)
	Channel string `json:"channel,omitempty"`
}

// GryviaLiveExperimentStatus defines the observed state of GryviaLiveExperiment
type GryviaLiveExperimentStatus struct {
	// Phase is the current phase: Pending, Running, Completed, Failed
	Phase string `json:"phase,omitempty"`

	// StartTime is when the experiment started
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// Leaderboard is the ranked list of jobs by performance
	Leaderboard []LeaderboardEntry `json:"leaderboard,omitempty"`

	// GPUHoursSaved is the estimated GPU-hours saved by early termination
	GPUHoursSaved float64 `json:"gpuHoursSaved,omitempty"`

	// CostSaved is the estimated cost saved in dollars
	CostSaved float64 `json:"costSaved,omitempty"`

	// AnomaliesDetected is the number of anomalies detected across all jobs
	AnomaliesDetected int `json:"anomaliesDetected,omitempty"`
}

// LeaderboardEntry represents a single job's position in the leaderboard
type LeaderboardEntry struct {
	// Rank is the position in the leaderboard (1-based)
	Rank int `json:"rank"`

	// Job is the job name
	Job string `json:"job"`

	// PrimaryMetricValue is the current value of the primary metric
	PrimaryMetricValue float64 `json:"primaryMetricValue"`

	// Status is Running or TerminatedEarly
	Status string `json:"status"`

	// Reason is the reason for early termination (if applicable)
	Reason string `json:"reason,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Leader",type=string,JSONPath=`.status.leaderboard[0].job`
//+kubebuilder:printcolumn:name="GPUHoursSaved",type=number,JSONPath=`.status.gpuHoursSaved`
//+kubebuilder:printcolumn:name="CostSaved",type=number,JSONPath=`.status.costSaved`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaLiveExperiment is the Schema for the gryvialiveexperiments API
type GryviaLiveExperiment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaLiveExperimentSpec   `json:"spec,omitempty"`
	Status GryviaLiveExperimentStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaLiveExperimentList contains a list of GryviaLiveExperiment
type GryviaLiveExperimentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaLiveExperiment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaLiveExperiment{}, &GryviaLiveExperimentList{})
}
