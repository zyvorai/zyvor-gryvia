// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaGpuMemoryOptimizerSpec defines the desired state of GryviaGpuMemoryOptimizer
type GryviaGpuMemoryOptimizerSpec struct {
	// Scope defines what this optimizer watches
	Scope OptimizerScope `json:"scope"`

	// OomPrevention configures proactive OOM prevention
	OomPrevention *OomPreventionConfig `json:"oomPrevention,omitempty"`

	// RightSizing configures GPU memory right-sizing recommendations
	RightSizing *RightSizingConfig `json:"rightSizing,omitempty"`

	// InferencePacking configures inference model packing on shared GPUs
	InferencePacking *InferencePackingConfig `json:"inferencePacking,omitempty"`
}

// OptimizerScope defines the scope of the memory optimizer
type OptimizerScope struct {
	// Type is the scope type: cluster, namespace, or job-selector
	Type string `json:"type"`

	// Namespace is the target namespace (required when Type is "namespace")
	Namespace string `json:"namespace,omitempty"`

	// JobSelector selects jobs by labels (required when Type is "job-selector")
	JobSelector *JobSelector `json:"jobSelector,omitempty"`
}

// JobSelector defines a label selector for jobs
type JobSelector struct {
	// MatchLabels is a map of key-value label pairs to match
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

// OomPreventionConfig configures OOM prevention
type OomPreventionConfig struct {
	// Enabled enables OOM prevention monitoring
	Enabled bool `json:"enabled,omitempty"`

	// MemoryGrowthProfiling configures how memory growth is profiled
	MemoryGrowthProfiling *MemoryGrowthProfilingConfig `json:"memoryGrowthProfiling,omitempty"`

	// PreemptiveAction defines what to do when OOM is predicted
	PreemptiveAction *PreemptiveActionConfig `json:"preemptiveAction,omitempty"`
}

// MemoryGrowthProfilingConfig configures memory growth profiling
type MemoryGrowthProfilingConfig struct {
	// ProfileSteps is the number of profiling data points to collect before projecting
	ProfileSteps int `json:"profileSteps,omitempty"`

	// ProjectionMethod is the method to project future memory usage (linear, exponential, polynomial)
	ProjectionMethod string `json:"projectionMethod,omitempty"`
}

// PreemptiveActionConfig defines the action to take when OOM is predicted
type PreemptiveActionConfig struct {
	// Type is the action type: warn, auto-mitigate, or evict
	Type string `json:"type,omitempty"`

	// WarningThresholdPercent is the memory utilization percentage at which to trigger a warning
	WarningThresholdPercent int `json:"warningThresholdPercent,omitempty"`

	// AutoEnableGradientCheckpointing enables gradient checkpointing via env var injection
	AutoEnableGradientCheckpointing bool `json:"autoEnableGradientCheckpointing,omitempty"`

	// AutoEnableMixedPrecision enables mixed precision training via env var injection
	AutoEnableMixedPrecision bool `json:"autoEnableMixedPrecision,omitempty"`

	// AutoReduceBatchSize enables automatic batch size reduction via env var injection
	AutoReduceBatchSize bool `json:"autoReduceBatchSize,omitempty"`
}

// RightSizingConfig configures GPU memory right-sizing recommendations
type RightSizingConfig struct {
	// Enabled enables right-sizing recommendations
	Enabled bool `json:"enabled,omitempty"`

	// AnalysisWindow is the time window for analyzing memory usage patterns (e.g., "1h", "24h", "7d")
	AnalysisWindow string `json:"analysisWindow,omitempty"`

	// Recommendations defines which types of recommendations to generate
	Recommendations *RecommendationsConfig `json:"recommendations,omitempty"`

	// MinimumGpuMemoryHeadroom is the minimum headroom percentage above peak usage
	MinimumGpuMemoryHeadroom int `json:"minimumGpuMemoryHeadroom,omitempty"`
}

// RecommendationsConfig defines which recommendation types are enabled
type RecommendationsConfig struct {
	// GpuTypeDowngrade recommends downgrading to a GPU with less memory
	GpuTypeDowngrade bool `json:"gpuTypeDowngrade,omitempty"`

	// GpuCountReduction recommends reducing the number of GPUs
	GpuCountReduction bool `json:"gpuCountReduction,omitempty"`
}

// InferencePackingConfig configures inference model packing on shared GPUs
type InferencePackingConfig struct {
	// Enabled enables inference packing
	Enabled bool `json:"enabled,omitempty"`

	// MemoryUtilizationTarget is the target memory utilization percentage when packing
	MemoryUtilizationTarget int `json:"memoryUtilizationTarget,omitempty"`

	// CompatibilityCheck verifies model compatibility before co-locating on a GPU
	CompatibilityCheck bool `json:"compatibilityCheck,omitempty"`

	// IsolationLevel is the GPU isolation level for packed workloads (none, mps, mig)
	IsolationLevel string `json:"isolationLevel,omitempty"`
}

// GryviaGpuMemoryOptimizerStatus defines the observed state of GryviaGpuMemoryOptimizer
type GryviaGpuMemoryOptimizerStatus struct {
	// JobsAnalyzed is the total number of jobs analyzed
	JobsAnalyzed int `json:"jobsAnalyzed,omitempty"`

	// OomPrevented is the number of OOM events prevented
	OomPrevented int `json:"oomPrevented,omitempty"`

	// RightSizingRecommendations is the number of active right-sizing recommendations
	RightSizingRecommendations int `json:"rightSizingRecommendations,omitempty"`

	// MemoryEfficiency holds aggregate memory efficiency metrics
	MemoryEfficiency *MemoryEfficiencyStatus `json:"memoryEfficiency,omitempty"`

	// LastAnalysisTime is the timestamp of the last analysis run
	LastAnalysisTime *metav1.Time `json:"lastAnalysisTime,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MemoryEfficiencyStatus holds aggregate memory efficiency metrics
type MemoryEfficiencyStatus struct {
	// AvgPeakUtilization is the average peak GPU memory utilization percentage across jobs
	AvgPeakUtilization float64 `json:"avgPeakUtilization,omitempty"`

	// AvgSteadyStateUtilization is the average steady-state GPU memory utilization percentage
	AvgSteadyStateUtilization float64 `json:"avgSteadyStateUtilization,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope.type`
//+kubebuilder:printcolumn:name="OOM-Prevention",type=boolean,JSONPath=`.spec.oomPrevention.enabled`
//+kubebuilder:printcolumn:name="Right-Sizing",type=boolean,JSONPath=`.spec.rightSizing.enabled`
//+kubebuilder:printcolumn:name="Jobs-Analyzed",type=integer,JSONPath=`.status.jobsAnalyzed`
//+kubebuilder:printcolumn:name="OOM-Prevented",type=integer,JSONPath=`.status.oomPrevented`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaGpuMemoryOptimizer is the Schema for the gryviagpumemoryoptimizers API
type GryviaGpuMemoryOptimizer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaGpuMemoryOptimizerSpec   `json:"spec,omitempty"`
	Status GryviaGpuMemoryOptimizerStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaGpuMemoryOptimizerList contains a list of GryviaGpuMemoryOptimizer
type GryviaGpuMemoryOptimizerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaGpuMemoryOptimizer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaGpuMemoryOptimizer{}, &GryviaGpuMemoryOptimizerList{})
}
