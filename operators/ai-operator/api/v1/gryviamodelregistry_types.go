// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelStage represents the promotion stage of a model
type ModelStage string

const (
	ModelStageDev        ModelStage = "dev"
	ModelStageStaging    ModelStage = "staging"
	ModelStageProduction ModelStage = "production"
	ModelStageArchived   ModelStage = "archived"
)

// GryviaModelRegistrySpec defines the desired state of GryviaModelRegistry
type GryviaModelRegistrySpec struct {
	// ModelName is the name of the model
	ModelName string `json:"modelName"`

	// Version is the semantic version of this model entry
	Version string `json:"version"`

	// Source identifies where this model was produced
	Source ModelSource `json:"source,omitempty"`

	// Artifacts defines where the model files are stored
	Artifacts ModelArtifacts `json:"artifacts"`

	// Stage is the current promotion stage (dev, staging, production, archived)
	Stage ModelStage `json:"stage,omitempty"`

	// Description is a human-readable description of this model version
	Description string `json:"description,omitempty"`

	// Metadata holds arbitrary key-value metadata (e.g., framework, metrics)
	Metadata map[string]string `json:"metadata,omitempty"`

	// AutoServe enables automatic inference deployment when the model reaches production stage
	AutoServe bool `json:"autoServe,omitempty"`

	// ServingConfig defines the configuration for auto-serving when stage is production
	ServingConfig *ServingConfig `json:"servingConfig,omitempty"`

	// PromotionPolicy lets the controller move a staging entry to production when its evaluation metric beats
	// every production entry of the same modelName
	PromotionPolicy *PromotionPolicy `json:"promotionPolicy,omitempty"`

	// RollbackPolicy lets the controller roll a production entry that serves a shared service back to
	// status.previousVersion when its metric crosses a threshold (for example after a scheduled evaluation wrote a
	// new score into spec.metadata)
	RollbackPolicy *RollbackPolicy `json:"rollbackPolicy,omitempty"`
}

// RollbackPolicy archives the serving entry and brings back the version it replaced when spec.metadata[metric]
// falls below threshold (maximize) or rises above it (minimize).
type RollbackPolicy struct {
	// Metric is the spec.metadata key holding the latest evaluation result
	Metric string `json:"metric"`

	// Threshold is the bound the metric must stay at or above (maximize) or at or below (minimize)
	// +kubebuilder:validation:Pattern=`^-?[0-9]+(\.[0-9]+)?$`
	Threshold string `json:"threshold"`

	// Direction is maximize (default) or minimize
	// +kubebuilder:validation:Enum=maximize;minimize
	Direction string `json:"direction,omitempty"`
}

// PromotionPolicy compares a metric from spec.metadata against the production entries of the same modelName.
// Numbers are decimal strings, so the schema needs no floating-point type.
type PromotionPolicy struct {
	// Metric is the spec.metadata key holding the evaluation result (for example "eval_score")
	Metric string `json:"metric"`

	// Direction is maximize (default) or minimize
	// +kubebuilder:validation:Enum=maximize;minimize
	Direction string `json:"direction,omitempty"`

	// MinDelta is how much the metric must improve on the best production entry (default "0")
	MinDelta string `json:"minDelta,omitempty"`

	// Threshold is an absolute bound the metric must reach (at least for maximize, at most for minimize)
	Threshold string `json:"threshold,omitempty"`
}

// ModelSource identifies the training job or pipeline that produced the model
type ModelSource struct {
	// JobRef is the name of the GryviaAIJob that produced this model
	JobRef string `json:"jobRef,omitempty"`

	// TunerRef is the name of the GryviaAutoTuner that produced this model
	TunerRef string `json:"tunerRef,omitempty"`

	// WorkflowRef is the name of the GryviaWorkflow that produced this model
	WorkflowRef string `json:"workflowRef,omitempty"`
}

// ModelArtifacts defines the storage location of model files
type ModelArtifacts struct {
	// S3Path is the S3 URI (e.g., s3://bucket/path/to/model)
	S3Path string `json:"s3Path,omitempty"`

	// PVCName is the PersistentVolumeClaim storing the model
	PVCName string `json:"pvcName,omitempty"`

	// SubPath is the path within the PVC or S3 bucket
	SubPath string `json:"subPath,omitempty"`

	// Format is the model format (pytorch, onnx, tensorrt, safetensors)
	Format string `json:"format,omitempty"`

	// SizeBytes is the total size of the model artifacts in bytes
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// ServingConfig defines how a model should be served when auto-deployed
type ServingConfig struct {
	// Backend is the inference backend to use (triton, vllm, tensorrt-llm, torchserve)
	Backend string `json:"backend,omitempty"`

	// Replicas is the number of serving replicas
	Replicas int32 `json:"replicas,omitempty"`

	// GPUCount is the number of GPUs per replica
	GPUCount int32 `json:"gpuCount,omitempty"`

	// GPUType is the preferred GPU type for serving
	GPUType string `json:"gpuType,omitempty"`

	// ServiceName shares one GryviaInferenceService between the versions of a model. The first production
	// version creates it; a later one is rolled out as its canary (autoPromote, autoRollback) and, once promoted,
	// becomes its modelRef while the version it replaced is archived. Empty keeps one "<entry>-serving" per entry.
	ServiceName string `json:"serviceName,omitempty"`

	// CanaryWeight is the share of pods, in percent, a new version gets as canary (default 10)
	CanaryWeight int32 `json:"canaryWeight,omitempty"`

	// PromoteAfterSeconds is how long a healthy canary runs before it is promoted (default 300)
	PromoteAfterSeconds int64 `json:"promoteAfterSeconds,omitempty"`

	// Args are extra arguments of the serving container (for example vLLM's --max-model-len)
	Args []string `json:"args,omitempty"`

	// ServicePort is the port the serving container listens on and is probed on. Set it when Args move the
	// server off its backend default (vLLM 8000, Triton 8000, TorchServe 8080).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	ServicePort int32 `json:"servicePort,omitempty"`
}

// GryviaModelRegistryStatus defines the observed state of GryviaModelRegistry
type GryviaModelRegistryStatus struct {
	// Phase is the current phase (Registered, Deploying, Serving, Failed)
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ServingEndpoint is the URL of the deployed inference service
	ServingEndpoint string `json:"servingEndpoint,omitempty"`

	// DeployedAt is when the model was deployed for serving
	DeployedAt *metav1.Time `json:"deployedAt,omitempty"`

	// Health is the health status of the serving deployment (Healthy, Unhealthy, Unknown)
	Health string `json:"health,omitempty"`

	// InferenceServiceName is the name of the GryviaInferenceService created for this model
	InferenceServiceName string `json:"inferenceServiceName,omitempty"`

	// PreviousVersion is the version that was previously in production (for rollback)
	PreviousVersion string `json:"previousVersion,omitempty"`

	// RegisteredAt is when the model was first registered
	RegisteredAt *metav1.Time `json:"registeredAt,omitempty"`

	// Message provides additional status information
	Message string `json:"message,omitempty"`

	// PromotionDecision is the last promotionPolicy outcome (Promoted, Rejected, Waiting)
	PromotionDecision string `json:"promotionDecision,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.modelName`
//+kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
//+kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.spec.stage`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaModelRegistry is the Schema for the gryviamodelregistries API
type GryviaModelRegistry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaModelRegistrySpec   `json:"spec,omitempty"`
	Status GryviaModelRegistryStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaModelRegistryList contains a list of GryviaModelRegistry
type GryviaModelRegistryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaModelRegistry `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaModelRegistry{}, &GryviaModelRegistryList{})
}
