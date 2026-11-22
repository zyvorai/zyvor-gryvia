// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelWatchProvider is a model hub the watch polls.
type ModelWatchProvider string

const (
	ProviderHuggingFace ModelWatchProvider = "huggingface"
	// ProviderNGC is reserved; sources using it are reported as unsupported.
	ProviderNGC ModelWatchProvider = "ngc"
)

// Candidate phases of a model a watch has seen.
const (
	CandidateQueued    = "Queued"
	CandidateRunning   = "Running"
	CandidateSucceeded = "Succeeded"
	CandidateFailed    = "Failed"
	CandidateRejected  = "Rejected"
	CandidateBaseline  = "Baseline"
)

// GryviaModelWatchSpec defines the desired state of GryviaModelWatch
type GryviaModelWatchSpec struct {
	// Sources are the hub queries to poll; a model matching any of them is a candidate
	// +kubebuilder:validation:MinItems=1
	Sources []ModelWatchSource `json:"sources"`

	// LicenseAllowlist rejects candidates whose license tag (for example "apache-2.0") is not listed. Empty allows any
	LicenseAllowlist []string `json:"licenseAllowlist,omitempty"`

	// MaxParamsB rejects candidates with more parameters than this many billions (0: no limit)
	MaxParamsB int32 `json:"maxParamsB,omitempty"`

	// TokenSecretRef is a Secret in the watch's namespace holding a hub token (for gated models)
	TokenSecretRef *SecretKeyRef `json:"tokenSecretRef,omitempty"`

	// PollInterval is how often the hub is queried (default 1h, at least 5m)
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`

	// IncludeExisting runs the workflow for the models the first poll finds. Off by default: the first poll only
	// records them as the baseline, and later releases are what trigger runs
	IncludeExisting bool `json:"includeExisting,omitempty"`

	// RetrainOnNewRevision starts another run when an already-seen model gets a new commit. Off by default: one run
	// per model id
	RetrainOnNewRevision bool `json:"retrainOnNewRevision,omitempty"`

	// MaxConcurrentRuns caps the workflows of this watch that run at the same time (default 1)
	MaxConcurrentRuns int32 `json:"maxConcurrentRuns,omitempty"`

	// Sizing turns the parameter count into a GPU count for the job steps named in autoSizeSteps
	Sizing *ModelSizing `json:"sizing,omitempty"`

	// Suspend stops polling and launching; running workflows continue
	Suspend bool `json:"suspend,omitempty"`

	// WorkflowTemplate is the GryviaWorkflow created per candidate. Its strings may use {{model.id}},
	// {{model.name}}, {{model.slug}}, {{model.revision}}, {{model.paramsB}}, {{model.license}} and {{model.gpus}};
	// the same values reach every step as MODEL_ID, MODEL_REVISION, MODEL_PARAMS_B and MODEL_GPUS
	WorkflowTemplate GryviaWorkflowSpec `json:"workflowTemplate"`
}

// ModelWatchSource is one hub query.
type ModelWatchSource struct {
	// Provider is the hub (huggingface; ngc is reserved)
	// +kubebuilder:validation:Enum=huggingface;ngc
	Provider ModelWatchProvider `json:"provider"`

	// Author is the organisation or user (for example "Qwen", "meta-llama")
	Author string `json:"author"`

	// NameRegex filters the model id (the part after "author/")
	NameRegex string `json:"nameRegex,omitempty"`

	// PipelineTag filters on the hub task tag (for example "text-generation")
	PipelineTag string `json:"pipelineTag,omitempty"`

	// MinDownloads ignores models with fewer downloads
	MinDownloads int64 `json:"minDownloads,omitempty"`

	// Limit is how many of the most recently modified models are examined per poll (default 20, at most 100)
	Limit int32 `json:"limit,omitempty"`
}

// SecretKeyRef names a key of a Secret in the same namespace.
type SecretKeyRef struct {
	Name string `json:"name"`
	// Key defaults to "token"
	Key string `json:"key,omitempty"`
}

// ModelSizing estimates GPUs from parameters: weights in bf16 (2 bytes per parameter) times overheadPercent,
// spread over GPUs of gpuMemoryGB, rounded up to 1, 2, 4 or 8, then multiples of 8.
type ModelSizing struct {
	// GPUMemoryGB is the memory of one GPU (default 80)
	GPUMemoryGB int32 `json:"gpuMemoryGB,omitempty"`

	// OverheadPercent covers activations, LoRA optimizer state and KV cache (default 150, i.e. 1.5x the weights)
	OverheadPercent int32 `json:"overheadPercent,omitempty"`

	// MaxGPUs rejects candidates that need more (default 8)
	MaxGPUs int32 `json:"maxGPUs,omitempty"`

	// AutoSizeSteps are the job steps whose jobTemplate.gpus is replaced by the estimate when it is 0
	AutoSizeSteps []string `json:"autoSizeSteps,omitempty"`
}

// ModelCandidate is a model the watch has seen.
type ModelCandidate struct {
	// ID is the hub id ("author/name")
	ID string `json:"id"`

	// Revision is the commit the run was started for
	Revision string `json:"revision,omitempty"`

	// ParamsB is the parameter count in billions, as a decimal string
	ParamsB string `json:"paramsB,omitempty"`

	// License is the license tag
	License string `json:"license,omitempty"`

	// GPUs is the sizing estimate
	GPUs int32 `json:"gpus,omitempty"`

	// Phase is Queued, Running, Succeeded, Failed, Rejected or Baseline
	Phase string `json:"phase"`

	// Workflow is the GryviaWorkflow created for it
	Workflow string `json:"workflow,omitempty"`

	// FirstSeen is when the watch first saw it
	FirstSeen metav1.Time `json:"firstSeen"`

	// Message explains a rejection or failure
	Message string `json:"message,omitempty"`
}

// GryviaModelWatchStatus defines the observed state of GryviaModelWatch
type GryviaModelWatchStatus struct {
	// Phase is Watching, Suspended or Failed
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastPollTime is when the hub was last queried successfully
	LastPollTime *metav1.Time `json:"lastPollTime,omitempty"`

	// NextPollTime is when it is queried next
	NextPollTime *metav1.Time `json:"nextPollTime,omitempty"`

	// ActiveRuns is the number of workflows still running
	ActiveRuns int32 `json:"activeRuns,omitempty"`

	// Candidates are the models seen, newest first (at most 200 are kept)
	Candidates []ModelCandidate `json:"candidates,omitempty"`

	// Message provides additional status information
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activeRuns`
//+kubebuilder:printcolumn:name="Last Poll",type=date,JSONPath=`.status.lastPollTime`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaModelWatch polls a model hub and runs a GryviaWorkflow (typically download, fine-tune, evaluate, register)
// for every new model that passes its filters.
type GryviaModelWatch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaModelWatchSpec   `json:"spec,omitempty"`
	Status GryviaModelWatchStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaModelWatchList contains a list of GryviaModelWatch
type GryviaModelWatchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaModelWatch `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaModelWatch{}, &GryviaModelWatchList{})
}
