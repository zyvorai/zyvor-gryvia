// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaTrainingProfilerSpec defines the desired state of GryviaTrainingProfiler
type GryviaTrainingProfilerSpec struct {
	// Target configuration for which jobs to profile
	Target ProfilerTarget `json:"target"`

	// Metrics to collect during profiling
	Metrics ProfilerMetrics `json:"metrics,omitempty"`

	// Analysis configuration
	Analysis ProfilerAnalysis `json:"analysis,omitempty"`

	// Output configuration
	Output ProfilerOutput `json:"output,omitempty"`
}

// ProfilerTarget defines which jobs to profile
type ProfilerTarget struct {
	// Type of profiling: auto, on-demand, or job-ref
	Type string `json:"type,omitempty"`

	// JobSelector is a label selector to match GryviaAIJob resources
	JobSelector map[string]string `json:"jobSelector,omitempty"`

	// JobRef is the name of a specific GryviaAIJob to profile (used with type: job-ref)
	JobRef string `json:"jobRef,omitempty"`

	// AutoProfile defines automatic profiling parameters
	AutoProfile *AutoProfileConfig `json:"autoProfile,omitempty"`
}

// AutoProfileConfig defines automatic profiling parameters
type AutoProfileConfig struct {
	// WarmupSteps is the number of training steps to skip before profiling begins
	WarmupSteps int32 `json:"warmupSteps,omitempty"`

	// ProfileSteps is the number of steps to profile
	ProfileSteps int32 `json:"profileSteps,omitempty"`

	// CooldownMinutes is the minimum minutes between profiling sessions for the same job
	CooldownMinutes int32 `json:"cooldownMinutes,omitempty"`
}

// ProfilerMetrics defines which metrics to collect
type ProfilerMetrics struct {
	// GpuCompute defines GPU compute metrics to collect
	GpuCompute *GpuComputeMetrics `json:"gpuCompute,omitempty"`

	// GpuMemory defines GPU memory metrics to collect
	GpuMemory *GpuMemoryMetrics `json:"gpuMemory,omitempty"`

	// DataPipeline defines data pipeline metrics to collect
	DataPipeline *DataPipelineMetrics `json:"dataPipeline,omitempty"`

	// Communication defines distributed communication metrics to collect
	Communication *CommunicationMetrics `json:"communication,omitempty"`
}

// GpuComputeMetrics defines GPU compute metrics
type GpuComputeMetrics struct {
	// SMUtilization enables streaming multiprocessor utilization collection
	SMUtilization bool `json:"smUtilization,omitempty"`

	// TensorCoreUtilization enables tensor core utilization collection
	TensorCoreUtilization bool `json:"tensorCoreUtilization,omitempty"`

	// FlopsEfficiency enables FLOPS efficiency calculation
	FlopsEfficiency bool `json:"flopsEfficiency,omitempty"`
}

// GpuMemoryMetrics defines GPU memory metrics
type GpuMemoryMetrics struct {
	// MemoryBandwidthUtilization enables HBM bandwidth utilization measurement
	MemoryBandwidthUtilization bool `json:"memoryBandwidthUtilization,omitempty"`

	// PeakMemoryUsage enables peak GPU memory usage tracking
	PeakMemoryUsage bool `json:"peakMemoryUsage,omitempty"`
}

// DataPipelineMetrics defines data pipeline metrics
type DataPipelineMetrics struct {
	// IoWaitRatio enables measurement of time GPU waits for data
	IoWaitRatio bool `json:"ioWaitRatio,omitempty"`

	// DataloaderThroughput enables data loading throughput measurement
	DataloaderThroughput bool `json:"dataloaderThroughput,omitempty"`
}

// CommunicationMetrics defines distributed communication metrics
type CommunicationMetrics struct {
	// NcclBandwidth enables NCCL collective bandwidth measurement
	NcclBandwidth bool `json:"ncclBandwidth,omitempty"`

	// AllReduceTime enables allreduce operation time measurement
	AllReduceTime bool `json:"allReduceTime,omitempty"`

	// ComputeCommOverlap enables compute-communication overlap measurement
	ComputeCommOverlap bool `json:"computeCommOverlap,omitempty"`
}

// ProfilerAnalysis defines analysis configuration
type ProfilerAnalysis struct {
	// CompareBaseline enables comparison against known baselines for the GPU type
	CompareBaseline bool `json:"compareBaseline,omitempty"`

	// BaselineSource defines the source of baseline data (builtin or custom)
	BaselineSource string `json:"baselineSource,omitempty"`

	// Recommendations defines which recommendation categories to enable
	Recommendations *RecommendationConfig `json:"recommendations,omitempty"`

	// SeverityThreshold is the minimum severity level for recommendations (info, warning, critical)
	SeverityThreshold string `json:"severityThreshold,omitempty"`
}

// RecommendationConfig defines which recommendation categories are enabled
type RecommendationConfig struct {
	// BatchSize enables batch size adjustment recommendations
	BatchSize bool `json:"batchSize,omitempty"`

	// DataLoading enables data loading optimization recommendations
	DataLoading bool `json:"dataLoading,omitempty"`

	// MixedPrecision enables mixed precision training recommendations
	MixedPrecision bool `json:"mixedPrecision,omitempty"`

	// CompilationOptimization enables torch.compile or XLA optimization recommendations
	CompilationOptimization bool `json:"compilationOptimization,omitempty"`

	// DistributedStrategy enables distributed training strategy recommendations
	DistributedStrategy bool `json:"distributedStrategy,omitempty"`

	// GpuTypeRecommendation enables alternative GPU type recommendations
	GpuTypeRecommendation bool `json:"gpuTypeRecommendation,omitempty"`
}

// ProfilerOutput defines output configuration
type ProfilerOutput struct {
	// StoreInJobStatus stores profiling results in GryviaAIJob status
	StoreInJobStatus bool `json:"storeInJobStatus,omitempty"`

	// EmitEvents emits Kubernetes events for recommendations
	EmitEvents bool `json:"emitEvents,omitempty"`

	// PrometheusMetrics exposes profiling data as Prometheus metrics
	PrometheusMetrics bool `json:"prometheusMetrics,omitempty"`
}

// GryviaTrainingProfilerStatus defines the observed state of GryviaTrainingProfiler
type GryviaTrainingProfilerStatus struct {
	// JobsProfiled is the total number of jobs profiled
	JobsProfiled int32 `json:"jobsProfiled,omitempty"`

	// AvgMFU is the average Model FLOPS Utilization across profiled jobs (0-100)
	AvgMFU float64 `json:"avgMFU,omitempty"`

	// TopRecommendations is the list of most recent high-impact recommendations
	TopRecommendations []ProfilerRecommendation `json:"topRecommendations,omitempty"`

	// ClusterEfficiencyScore is the overall cluster GPU efficiency score (0-100)
	ClusterEfficiencyScore float64 `json:"clusterEfficiencyScore,omitempty"`

	// LastProfileTime is the timestamp of the last profiling run
	LastProfileTime *metav1.Time `json:"lastProfileTime,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ProfilerRecommendation represents a single profiling recommendation
type ProfilerRecommendation struct {
	// JobName is the name of the job this recommendation applies to
	JobName string `json:"jobName"`

	// Category is the recommendation category (batchSize, dataLoading, mixedPrecision, etc.)
	Category string `json:"category"`

	// Severity is the recommendation severity (info, warning, critical)
	Severity string `json:"severity"`

	// Description is a human-readable description of the recommendation
	Description string `json:"description"`

	// EstimatedImpact is the estimated performance improvement
	EstimatedImpact string `json:"estimatedImpact"`

	// Timestamp is when the recommendation was generated
	Timestamp metav1.Time `json:"timestamp"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="JobsProfiled",type=integer,JSONPath=`.status.jobsProfiled`
//+kubebuilder:printcolumn:name="AvgMFU",type=number,JSONPath=`.status.avgMFU`
//+kubebuilder:printcolumn:name="EfficiencyScore",type=number,JSONPath=`.status.clusterEfficiencyScore`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaTrainingProfiler is the Schema for the gryviatrainingprofilers API
type GryviaTrainingProfiler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaTrainingProfilerSpec   `json:"spec,omitempty"`
	Status GryviaTrainingProfilerStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaTrainingProfilerList contains a list of GryviaTrainingProfiler
type GryviaTrainingProfilerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaTrainingProfiler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaTrainingProfiler{}, &GryviaTrainingProfilerList{})
}
