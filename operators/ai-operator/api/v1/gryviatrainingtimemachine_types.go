// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaTrainingTimeMachineSpec defines the desired state of GryviaTrainingTimeMachine
type GryviaTrainingTimeMachineSpec struct {
	// SourceJob is the name of the GryviaAIJob to track checkpoints for
	SourceJob string `json:"sourceJob"`

	// Timeline configures checkpoint timeline indexing
	Timeline *TimelineConfig `json:"timeline,omitempty"`

	// Retention defines the checkpoint retention policy
	Retention *RetentionConfig `json:"retention,omitempty"`

	// Forks defines fork requests to create new training jobs from checkpoints
	Forks []ForkSpec `json:"forks,omitempty"`
}

// TimelineConfig configures checkpoint timeline indexing
type TimelineConfig struct {
	// Enabled enables checkpoint timeline indexing
	Enabled bool `json:"enabled,omitempty"`

	// IndexCheckpoints enables automatic checkpoint discovery and indexing
	IndexCheckpoints bool `json:"indexCheckpoints,omitempty"`

	// MetadataPerCheckpoint defines metadata fields to extract per checkpoint
	MetadataPerCheckpoint []CheckpointMetadataField `json:"metadataPerCheckpoint,omitempty"`
}

// CheckpointMetadataField defines a metadata field to extract per checkpoint
type CheckpointMetadataField struct {
	// Name of the metadata field (e.g., loss, accuracy, lr)
	Name string `json:"name"`

	// Source type for the metadata value (metric, log, file)
	Source string `json:"source"`

	// Path to extract value (log pattern or file path)
	Path string `json:"path,omitempty"`
}

// RetentionConfig defines the checkpoint retention policy
type RetentionConfig struct {
	// KeepAll keeps all checkpoints (overrides other retention settings)
	KeepAll bool `json:"keepAll,omitempty"`

	// KeepEveryNth keeps every Nth checkpoint
	KeepEveryNth int `json:"keepEveryNth,omitempty"`

	// KeepBest keeps the N best checkpoints by primary metric
	KeepBest int `json:"keepBest,omitempty"`

	// KeepMilestones is a list of specific checkpoint steps to always keep
	KeepMilestones []int `json:"keepMilestones,omitempty"`

	// MaxStorageGi is the maximum storage in GiB for all retained checkpoints
	MaxStorageGi int `json:"maxStorageGi,omitempty"`
}

// ForkSpec defines a fork request to create a new training job from a checkpoint
type ForkSpec struct {
	// Name is a unique name for this fork
	Name string `json:"name"`

	// FromCheckpoint selects the checkpoint to fork from
	FromCheckpoint CheckpointSelector `json:"fromCheckpoint"`

	// Overrides are modifications to apply to the forked job
	Overrides *ForkOverrides `json:"overrides,omitempty"`

	// NewJobName is the name for the forked GryviaAIJob (auto-generated if omitted)
	NewJobName string `json:"newJobName,omitempty"`
}

// CheckpointSelector selects a specific checkpoint
type CheckpointSelector struct {
	// Step selects a checkpoint at a specific training step
	Step *int `json:"step,omitempty"`

	// Epoch selects a checkpoint at a specific epoch
	Epoch *int `json:"epoch,omitempty"`

	// Metric selects the checkpoint with the best metric value
	Metric *MetricSelector `json:"metric,omitempty"`
}

// MetricSelector selects a checkpoint by metric value
type MetricSelector struct {
	// Name is the metric name (e.g., val_loss, accuracy)
	Name string `json:"name"`

	// Selector picks the checkpoint with min or max metric value
	Selector string `json:"selector"`
}

// ForkOverrides defines overrides to apply to a forked job
type ForkOverrides struct {
	// Env is a list of environment variable overrides
	Env []EnvOverride `json:"env,omitempty"`

	// Args is a list of argument overrides
	Args []string `json:"args,omitempty"`
}

// EnvOverride defines an environment variable override
type EnvOverride struct {
	// Name of the environment variable
	Name string `json:"name"`

	// Value of the environment variable
	Value string `json:"value"`
}

// GryviaTrainingTimeMachineStatus defines the observed state of GryviaTrainingTimeMachine
type GryviaTrainingTimeMachineStatus struct {
	// CheckpointTimeline is the status of indexed checkpoints
	CheckpointTimeline *CheckpointTimelineStatus `json:"checkpointTimeline,omitempty"`

	// Forks is the status of each fork request
	Forks []ForkStatus `json:"forks,omitempty"`

	// StorageUsed is the total storage used by retained checkpoints (e.g., "45Gi")
	StorageUsed string `json:"storageUsed,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// CheckpointTimelineStatus holds the status of the checkpoint timeline
type CheckpointTimelineStatus struct {
	// TotalCheckpoints is the total number of indexed checkpoints
	TotalCheckpoints int `json:"totalCheckpoints,omitempty"`

	// BestCheckpoint is the checkpoint with the best primary metric value
	BestCheckpoint *CheckpointInfo `json:"bestCheckpoint,omitempty"`

	// LatestCheckpoint is the most recently indexed checkpoint
	LatestCheckpoint *CheckpointInfo `json:"latestCheckpoint,omitempty"`
}

// CheckpointInfo describes a specific checkpoint
type CheckpointInfo struct {
	// Step is the training step of this checkpoint
	Step int `json:"step,omitempty"`

	// Epoch is the training epoch of this checkpoint
	Epoch int `json:"epoch,omitempty"`

	// MetricName is the primary metric name
	MetricName string `json:"metricName,omitempty"`

	// MetricValue is the primary metric value at this checkpoint
	MetricValue float64 `json:"metricValue,omitempty"`

	// Timestamp is when this checkpoint was created
	Timestamp *metav1.Time `json:"timestamp,omitempty"`
}

// ForkStatus represents the current status of a fork
type ForkStatus struct {
	// Name is the fork name
	Name string `json:"name"`

	// SourceStep is the training step from which the fork was created
	SourceStep int `json:"sourceStep,omitempty"`

	// ForkedJob is the name of the forked GryviaAIJob
	ForkedJob string `json:"forkedJob,omitempty"`

	// Status is the current status (Pending, Creating, Running, Succeeded, Failed)
	Status string `json:"status,omitempty"`

	// CurrentMetric is the current primary metric value of the forked job
	CurrentMetric float64 `json:"currentMetric,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Source-Job",type=string,JSONPath=`.spec.sourceJob`
//+kubebuilder:printcolumn:name="Checkpoints",type=integer,JSONPath=`.status.checkpointTimeline.totalCheckpoints`
//+kubebuilder:printcolumn:name="Storage",type=string,JSONPath=`.status.storageUsed`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaTrainingTimeMachine is the Schema for the gryviatrainingtimemachines API
type GryviaTrainingTimeMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaTrainingTimeMachineSpec   `json:"spec,omitempty"`
	Status GryviaTrainingTimeMachineStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaTrainingTimeMachineList contains a list of GryviaTrainingTimeMachine
type GryviaTrainingTimeMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaTrainingTimeMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaTrainingTimeMachine{}, &GryviaTrainingTimeMachineList{})
}
