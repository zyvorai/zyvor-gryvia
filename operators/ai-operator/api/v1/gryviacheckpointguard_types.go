package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaCheckpointGuardSpec defines the desired state of GryviaCheckpointGuard
type GryviaCheckpointGuardSpec struct {
	// JobSelector selects which GryviaAIJob resources this guard applies to
	JobSelector JobSelector `json:"jobSelector"`

	// CheckpointPolicy defines when and how checkpoints are taken
	CheckpointPolicy CheckpointPolicy `json:"checkpointPolicy"`

	// Validation defines how checkpoints are validated after creation
	Validation *CheckpointValidation `json:"validation,omitempty"`

	// Restore defines how checkpoints are restored on job restart
	Restore *RestorePolicy `json:"restore,omitempty"`

	// Monitoring defines observability settings for checkpoint operations
	Monitoring *CheckpointMonitoring `json:"monitoring,omitempty"`
}

// JobSelector defines label-based selection of GryviaAIJob resources
type JobSelector struct {
	// MatchLabels is a map of key-value pairs to match against job labels
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

// CheckpointPolicy defines the checkpoint schedule and triggers
type CheckpointPolicy struct {
	// IntervalMinutes is the interval in minutes between periodic checkpoints
	IntervalMinutes int32 `json:"intervalMinutes,omitempty"`

	// EmergencyCheckpoint defines triggers for immediate checkpoint creation
	EmergencyCheckpoint *EmergencyCheckpointConfig `json:"emergencyCheckpoint,omitempty"`

	// Replication defines how checkpoints are replicated for durability
	Replication *ReplicationConfig `json:"replication,omitempty"`

	// Directory is where matched jobs write coordinated checkpoints (GRYVIA_CHECKPOINT_DIR). It must be a clean
	// path below /data, the job's persistent spec.storage volume. Default /data/checkpoints.
	// +optional
	Directory string `json:"directory,omitempty"`

	// EverySteps is passed to matched jobs as GRYVIA_CHECKPOINT_EVERY (commit every N training steps).
	// +kubebuilder:validation:Minimum=1
	// +optional
	EverySteps int32 `json:"everySteps,omitempty"`
}

// EmergencyCheckpointConfig defines conditions that trigger an emergency checkpoint
type EmergencyCheckpointConfig struct {
	// Triggers is a list of conditions that trigger an emergency checkpoint.
	// Valid values: GpuHealthDegraded, SpotPreemptionSignal, MemoryPressure, LossDivergence, NvlinkDegraded
	Triggers []string `json:"triggers,omitempty"`
}

// ReplicationConfig defines checkpoint replication settings
type ReplicationConfig struct {
	// Backend is the storage backend for replicated checkpoints (s3, gcs, azure-blob, nfs, pvc)
	Backend string `json:"backend,omitempty"`

	// Copies is the number of checkpoint copies to maintain
	Copies int32 `json:"copies,omitempty"`

	// AsyncUpload enables asynchronous upload to avoid blocking training
	AsyncUpload bool `json:"asyncUpload,omitempty"`

	// Target is where rank 0 copies each committed step (GRYVIA_CHECKPOINT_REPLICA): file:///<path> on a
	// mounted volume or s3://<bucket>/<prefix> (the trainer needs boto3 and credentials). Empty = no replica.
	// +kubebuilder:validation:Pattern=`^(file:///|s3://)[^\s]*$`
	// +optional
	Target string `json:"target,omitempty"`
}

// CheckpointValidation defines how checkpoints are validated
type CheckpointValidation struct {
	// ChecksumVerify enables checksum verification after writing
	ChecksumVerify bool `json:"checksumVerify,omitempty"`

	// TensorShapeVerify enables tensor shape verification
	TensorShapeVerify bool `json:"tensorShapeVerify,omitempty"`

	// LoadTest enables a trial load of the checkpoint to verify integrity
	LoadTest bool `json:"loadTest,omitempty"`

	// RetainValidOnly retains only checkpoints that pass validation
	RetainValidOnly bool `json:"retainValidOnly,omitempty"`

	// RetentionCount is the number of valid checkpoints to retain
	RetentionCount int32 `json:"retentionCount,omitempty"`
}

// RestorePolicy defines how checkpoints are used during job recovery
type RestorePolicy struct {
	// AutoRestore enables automatic restore from the latest valid checkpoint
	AutoRestore bool `json:"autoRestore,omitempty"`

	// PreferNearStorage schedules restored jobs near checkpoint storage
	PreferNearStorage bool `json:"preferNearStorage,omitempty"`

	// InjectEnvVars defines environment variables injected into restored job pods
	InjectEnvVars map[string]string `json:"injectEnvVars,omitempty"`

	// NodeLossGraceSeconds is how long a matched job's pod may sit on a NotReady or deleted node before
	// AutoRestore force-deletes it so the Indexed Job recreates it elsewhere. Default 60.
	// +kubebuilder:validation:Minimum=15
	// +kubebuilder:validation:Maximum=3600
	// +optional
	NodeLossGraceSeconds int32 `json:"nodeLossGraceSeconds,omitempty"`
}

// CheckpointMonitoring defines observability settings
type CheckpointMonitoring struct {
	// ExportMetrics enables Prometheus metrics export for checkpoint operations
	ExportMetrics bool `json:"exportMetrics,omitempty"`

	// AlertOnFailure enables alerts when checkpoint operations fail
	AlertOnFailure bool `json:"alertOnFailure,omitempty"`
}

// GryviaCheckpointGuardStatus defines the observed state of GryviaCheckpointGuard
type GryviaCheckpointGuardStatus struct {
	// Phase is the current phase (Idle, Monitoring, Checkpointing, Restoring, Error)
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// TotalCheckpoints is the total number of checkpoints taken
	TotalCheckpoints int32 `json:"totalCheckpoints,omitempty"`

	// ValidCheckpoints is the number of checkpoints that passed validation
	ValidCheckpoints int32 `json:"validCheckpoints,omitempty"`

	// LastCheckpointTime is the timestamp of the last checkpoint
	LastCheckpointTime *metav1.Time `json:"lastCheckpointTime,omitempty"`

	// LastValidCheckpoint is the name/path of the last valid checkpoint
	LastValidCheckpoint string `json:"lastValidCheckpoint,omitempty"`

	// EmergencyCheckpointsTaken is the number of emergency checkpoints triggered
	EmergencyCheckpointsTaken int32 `json:"emergencyCheckpointsTaken,omitempty"`

	// StorageUsed is the total storage consumed by retained checkpoints (e.g., 42Gi)
	StorageUsed string `json:"storageUsed,omitempty"`

	// AvgCheckpointDuration is the average time to complete a checkpoint (e.g., 2m30s)
	AvgCheckpointDuration string `json:"avgCheckpointDuration,omitempty"`

	// MatchedJobs is the number of GryviaAIJob resources matched by jobSelector
	MatchedJobs int32 `json:"matchedJobs,omitempty"`

	// Jobs is the checkpoint state the matched jobs reported (needs the ai-operator checkpointGuard capability).
	// +optional
	Jobs []GuardedJob `json:"jobs,omitempty"`
}

// GuardedJob is one matched job's checkpoint state, copied from its status.checkpoint.
type GuardedJob struct {
	Name string `json:"name"`
	// Injected is true when the job's pods got the guard's checkpoint environment.
	Injected          bool         `json:"injected"`
	LastCommittedStep int64        `json:"lastCommittedStep,omitempty"`
	LastCommittedAt   *metav1.Time `json:"lastCommittedAt,omitempty"`
	// NodeLossRecoveries counts pods this guard's AutoRestore replaced after a node loss.
	NodeLossRecoveries int32  `json:"nodeLossRecoveries,omitempty"`
	Message            string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Valid",type=integer,JSONPath=`.status.validCheckpoints`
//+kubebuilder:printcolumn:name="LastCheckpoint",type=string,JSONPath=`.status.lastCheckpointTime`
//+kubebuilder:printcolumn:name="StorageUsed",type=string,JSONPath=`.status.storageUsed`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaCheckpointGuard is the Schema for the gryviacheckpointguards API
type GryviaCheckpointGuard struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaCheckpointGuardSpec   `json:"spec,omitempty"`
	Status GryviaCheckpointGuardStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaCheckpointGuardList contains a list of GryviaCheckpointGuard
type GryviaCheckpointGuardList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaCheckpointGuard `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaCheckpointGuard{}, &GryviaCheckpointGuardList{})
}
