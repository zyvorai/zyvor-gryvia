// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaModelLineageSpec defines the desired state of GryviaModelLineage
type GryviaModelLineageSpec struct {
	// Model identifies the model being tracked
	Model ModelIdentity `json:"model"`

	// Provenance captures the origin and training details
	Provenance ProvenanceSpec `json:"provenance,omitempty"`

	// Compliance configures audit and regulatory compliance
	Compliance ComplianceSpec `json:"compliance,omitempty"`
}

// ModelIdentity identifies a model
type ModelIdentity struct {
	// Name is the model name
	Name string `json:"name"`

	// Version is the model version (semver or custom)
	Version string `json:"version"`

	// Registry is the model registry URL or reference
	Registry string `json:"registry,omitempty"`

	// Format is the model serialization format (pytorch, onnx, tensorrt, safetensors)
	Format string `json:"format,omitempty"`
}

// ProvenanceSpec captures model provenance
type ProvenanceSpec struct {
	// AutoCapture enables automatic provenance collection from referenced jobs
	AutoCapture bool `json:"autoCapture,omitempty"`

	// Code captures source code provenance
	Code CodeProvenance `json:"code,omitempty"`

	// Data captures training data provenance
	Data DataProvenance `json:"data,omitempty"`

	// Training captures training configuration
	Training TrainingProvenance `json:"training,omitempty"`

	// Infrastructure captures infrastructure details
	Infrastructure InfrastructureProvenance `json:"infrastructure,omitempty"`

	// Events captures anomalies, interventions, and checkpoints
	Events EventsProvenance `json:"events,omitempty"`

	// Evaluation captures evaluation results
	Evaluation EvaluationProvenance `json:"evaluation,omitempty"`
}

// CodeProvenance captures source code details
type CodeProvenance struct {
	// GitRepo is the git repository URL
	GitRepo string `json:"gitRepo,omitempty"`

	// GitCommit is the git commit SHA
	GitCommit string `json:"gitCommit,omitempty"`

	// GitBranch is the git branch name
	GitBranch string `json:"gitBranch,omitempty"`

	// ContainerImage is the container image used for training
	ContainerImage string `json:"containerImage,omitempty"`
}

// DataProvenance captures training data details
type DataProvenance struct {
	// Datasets lists the datasets used for training
	Datasets []DatasetReference `json:"datasets,omitempty"`
}

// DatasetReference references a dataset
type DatasetReference struct {
	// DatasetRef is the reference to the dataset (GryviaDataset name or URI)
	DatasetRef string `json:"datasetRef"`

	// Version is the dataset version
	Version string `json:"version,omitempty"`

	// Checksum is the SHA256 checksum of the dataset
	Checksum string `json:"checksum,omitempty"`
}

// TrainingProvenance captures training configuration
type TrainingProvenance struct {
	// JobRef references the GryviaAIJob that produced this model
	JobRef string `json:"jobRef,omitempty"`

	// Hyperparameters used during training
	Hyperparameters map[string]string `json:"hyperparameters,omitempty"`

	// DistributedConfig is a summary of distributed training configuration
	DistributedConfig string `json:"distributedConfig,omitempty"`
}

// InfrastructureProvenance captures infrastructure details
type InfrastructureProvenance struct {
	// GPUNodes lists the GPU node names used during training
	GPUNodes []string `json:"gpuNodes,omitempty"`

	// NetworkType is the network type used (rdma, sriov, standard)
	NetworkType string `json:"networkType,omitempty"`

	// StorageBackend is the storage backend used
	StorageBackend string `json:"storageBackend,omitempty"`

	// TotalGpuHours is the total GPU hours consumed
	TotalGpuHours float64 `json:"totalGpuHours,omitempty"`

	// TotalCost is the total cost in USD
	TotalCost float64 `json:"totalCost,omitempty"`
}

// EventsProvenance captures events during training
type EventsProvenance struct {
	// Anomalies detected during training
	Anomalies []AnomalyEvent `json:"anomalies,omitempty"`

	// Interventions are manual actions taken during training
	Interventions []InterventionEvent `json:"interventions,omitempty"`

	// CheckpointTimeline records checkpoint saves
	CheckpointTimeline []CheckpointEvent `json:"checkpointTimeline,omitempty"`
}

// AnomalyEvent records an anomaly detected during training
type AnomalyEvent struct {
	// Timestamp of the anomaly
	Timestamp metav1.Time `json:"timestamp"`

	// Type of anomaly
	Type string `json:"type"`

	// Description of the anomaly
	Description string `json:"description,omitempty"`
}

// InterventionEvent records a manual intervention
type InterventionEvent struct {
	// Timestamp of the intervention
	Timestamp metav1.Time `json:"timestamp"`

	// Action taken
	Action string `json:"action"`

	// Reason for the intervention
	Reason string `json:"reason,omitempty"`
}

// CheckpointEvent records a checkpoint save
type CheckpointEvent struct {
	// Timestamp of the checkpoint
	Timestamp metav1.Time `json:"timestamp"`

	// Epoch at the checkpoint
	Epoch int32 `json:"epoch,omitempty"`

	// Step at the checkpoint
	Step int32 `json:"step,omitempty"`

	// CheckpointPath is the storage path for the checkpoint
	CheckpointPath string `json:"checkpointPath,omitempty"`
}

// EvaluationProvenance captures evaluation results
type EvaluationProvenance struct {
	// Metrics are evaluation metrics (e.g., accuracy, f1, bleu)
	Metrics map[string]float64 `json:"metrics,omitempty"`

	// EvaluationJob references the evaluation GryviaAIJob
	EvaluationJob string `json:"evaluationJob,omitempty"`

	// Benchmarks are standardized benchmark results
	Benchmarks []BenchmarkResult `json:"benchmarks,omitempty"`
}

// BenchmarkResult records a benchmark evaluation
type BenchmarkResult struct {
	// Name of the benchmark
	Name string `json:"name"`

	// Score achieved
	Score float64 `json:"score"`

	// Date of the benchmark
	Date metav1.Time `json:"date,omitempty"`
}

// ComplianceSpec configures audit and regulatory compliance
type ComplianceSpec struct {
	// ImmutableRecord prevents modifications to lineage after creation
	ImmutableRecord bool `json:"immutableRecord,omitempty"`

	// CryptographicChain enables SHA256 hash chain for tamper detection
	CryptographicChain bool `json:"cryptographicChain,omitempty"`

	// SignedBy is the identity that signed the model artifact
	SignedBy string `json:"signedBy,omitempty"`

	// Attestation configures attestation generation
	Attestation AttestationSpec `json:"attestation,omitempty"`

	// RegulatoryFramework lists regulatory frameworks for compliance
	RegulatoryFramework []string `json:"regulatoryFramework,omitempty"`

	// DataPrivacy configures data privacy compliance
	DataPrivacy DataPrivacySpec `json:"dataPrivacy,omitempty"`
}

// AttestationSpec configures attestation
type AttestationSpec struct {
	// Format is the attestation format (in-toto, sigstore, custom)
	Format string `json:"format,omitempty"`

	// AttachToRegistry attaches attestation to container/model registry
	AttachToRegistry bool `json:"attachToRegistry,omitempty"`
}

// DataPrivacySpec configures data privacy compliance
type DataPrivacySpec struct {
	// PIIScanned indicates whether training data was scanned for PII
	PIIScanned bool `json:"piiScanned,omitempty"`

	// PIIFound indicates whether PII was found in training data
	PIIFound bool `json:"piiFound,omitempty"`

	// DPIACompleted indicates whether a Data Protection Impact Assessment was completed
	DPIACompleted bool `json:"dpiaCompleted,omitempty"`
}

// GryviaModelLineageStatus defines the observed state of GryviaModelLineage
type GryviaModelLineageStatus struct {
	// LineageComplete indicates whether all provenance data has been collected
	LineageComplete bool `json:"lineageComplete,omitempty"`

	// ProvenanceHash is the SHA256 hash of the complete provenance record
	ProvenanceHash string `json:"provenanceHash,omitempty"`

	// CreatedAt is the timestamp when lineage was created
	CreatedAt metav1.Time `json:"createdAt,omitempty"`

	// AttestationAttached indicates whether attestation was attached to registry
	AttestationAttached bool `json:"attestationAttached,omitempty"`

	// ComplianceStatus is the overall compliance status
	ComplianceStatus string `json:"complianceStatus,omitempty"`

	// ReproducibilityVerified indicates whether the model can be reproduced
	ReproducibilityVerified bool `json:"reproducibilityVerified,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced

// GryviaModelLineage is the Schema for the gryviamodellineages API
type GryviaModelLineage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaModelLineageSpec   `json:"spec,omitempty"`
	Status GryviaModelLineageStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaModelLineageList contains a list of GryviaModelLineage
type GryviaModelLineageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaModelLineage `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaModelLineage{}, &GryviaModelLineageList{})
}
