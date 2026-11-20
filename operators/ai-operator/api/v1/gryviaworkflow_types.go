// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StepType defines the kind of step in a workflow
type StepType string

const (
	StepTypeJob     StepType = "job"
	StepTypeScript  StepType = "script"
	StepTypeWebhook StepType = "webhook"
	// StepTypeRegister creates a GryviaModelRegistry entry from earlier step outputs.
	StepTypeRegister StepType = "register"
	// StepTypeRegistry changes an existing GryviaModelRegistry entry: writes metadata or asks for a rollback.
	StepTypeRegistry StepType = "registry"
)

// GryviaWorkflowSpec defines the desired state of GryviaWorkflow
type GryviaWorkflowSpec struct {
	// Steps is the list of workflow steps
	Steps []WorkflowStep `json:"steps"`

	// Parameters are global parameters passed to all steps
	Parameters map[string]string `json:"parameters,omitempty"`

	// Schedule is an optional five-field cron expression (UTC). With a schedule the workflow waits for the next
	// fire time, runs, and when the run is finished waits for the next fire time again. A run is never started
	// while the previous one is still going.
	Schedule string `json:"schedule,omitempty"`
}

// WorkflowStep defines a single step in the workflow DAG
type WorkflowStep struct {
	// Name is the unique name of this step within the workflow
	Name string `json:"name"`

	// Type of step: job, script, webhook, register or registry
	Type StepType `json:"type,omitempty"`

	// DependsOn is a list of step names that must complete before this step runs
	DependsOn []string `json:"dependsOn,omitempty"`

	// Condition is a CEL expression that must evaluate to true for this step to run.
	// The expression has access to previous step outputs and statuses.
	// Example: "steps.train.status == 'Succeeded' && steps.train.metrics.accuracy > 0.95"
	Condition string `json:"condition,omitempty"`

	// JobTemplate is the GryviaAIJob spec to run for job-type steps
	JobTemplate *GryviaAIJobSpec `json:"jobTemplate,omitempty"`

	// Script is the shell script to execute for script-type steps
	Script *ScriptStep `json:"script,omitempty"`

	// Webhook is the HTTP request to make for webhook-type steps
	Webhook *WebhookStep `json:"webhook,omitempty"`

	// Register is the model registry entry to create for register-type steps
	Register *RegisterStep `json:"register,omitempty"`

	// Registry is the change to an existing model registry entry for registry-type steps
	Registry *RegistryStep `json:"registry,omitempty"`

	// Retries is the number of times to retry this step on failure
	Retries int32 `json:"retries,omitempty"`

	// RetryBackoffSeconds is the initial backoff duration between retries in seconds
	RetryBackoffSeconds int32 `json:"retryBackoffSeconds,omitempty"`

	// TimeoutSeconds is the maximum duration for this step in seconds
	TimeoutSeconds int64 `json:"timeoutSeconds,omitempty"`
}

// ScriptStep defines an inline script to run
type ScriptStep struct {
	// Image is the container image to run the script in
	Image string `json:"image"`

	// Command is the script content to execute
	Command []string `json:"command"`

	// Args are arguments passed to the command
	Args []string `json:"args,omitempty"`
}

// WebhookStep defines an HTTP webhook call
type WebhookStep struct {
	// URL is the webhook endpoint
	URL string `json:"url"`

	// Method is the HTTP method (GET, POST, PUT)
	Method string `json:"method,omitempty"`

	// Headers are HTTP headers to include
	Headers map[string]string `json:"headers,omitempty"`

	// Body is the request body (JSON)
	Body string `json:"body,omitempty"`

	// SuccessCondition is a JSONPath expression on the response to determine success
	SuccessCondition string `json:"successCondition,omitempty"`
}

// RegisterStep creates a GryviaModelRegistry entry. Its string fields may use {{steps.<name>.outputs.<key>}},
// {{parameters.<key>}}, {{workflow.name}} and {{workflow.run}} placeholders.
type RegisterStep struct {
	// Name is the registry object name (default "<workflow>-<step>", with the run number for scheduled workflows)
	Name string `json:"name,omitempty"`

	// ModelName is the model name of the entry
	ModelName string `json:"modelName"`

	// Version is the version of the entry
	Version string `json:"version"`

	// Artifacts is where the model files are
	Artifacts ModelArtifacts `json:"artifacts"`

	// Stage is the stage the entry starts in (dev or staging; default staging)
	Stage ModelStage `json:"stage,omitempty"`

	// Description of the entry
	Description string `json:"description,omitempty"`

	// Metadata is copied into the entry (for example the evaluation score a promotionPolicy compares)
	Metadata map[string]string `json:"metadata,omitempty"`

	// AutoServe, ServingConfig, PromotionPolicy and RollbackPolicy are copied into the entry
	AutoServe       bool             `json:"autoServe,omitempty"`
	ServingConfig   *ServingConfig   `json:"servingConfig,omitempty"`
	PromotionPolicy *PromotionPolicy `json:"promotionPolicy,omitempty"`
	RollbackPolicy  *RollbackPolicy  `json:"rollbackPolicy,omitempty"`
}

// RegistryStep changes an existing GryviaModelRegistry entry in the workflow's namespace. Entry and the metadata
// values may use the same placeholders as a register step.
type RegistryStep struct {
	// Entry is the registry object name. Set entry or serviceName.
	Entry string `json:"entry,omitempty"`

	// ServiceName picks the entry a GryviaInferenceService serves when the step runs: its spec.modelRef (a shared
	// service of servingConfig.serviceName or one created by hand). A scheduled evaluation of the live service uses this.
	ServiceName string `json:"serviceName,omitempty"`

	// Action is updateMetadata (merge metadata into spec.metadata) or rollback (ask the controller to roll the
	// entry back to status.previousVersion on its shared service)
	// +kubebuilder:validation:Enum=updateMetadata;rollback
	Action string `json:"action"`

	// Metadata is merged into spec.metadata for updateMetadata
	Metadata map[string]string `json:"metadata,omitempty"`
}

// StepPhase represents the current state of a workflow step
type StepPhase string

const (
	StepPhasePending   StepPhase = "Pending"
	StepPhaseRunning   StepPhase = "Running"
	StepPhaseSucceeded StepPhase = "Succeeded"
	StepPhaseFailed    StepPhase = "Failed"
	StepPhaseSkipped   StepPhase = "Skipped"
)

// StepStatus holds the status of a single workflow step
type StepStatus struct {
	// Name is the step name
	Name string `json:"name"`

	// Phase is the current phase of this step
	Phase StepPhase `json:"phase"`

	// JobName is the name of the GryviaAIJob created for this step (if applicable)
	JobName string `json:"jobName,omitempty"`

	// StartTime is when the step started
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the step finished
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// RetriesAttempted is the number of retries attempted so far
	RetriesAttempted int32 `json:"retriesAttempted,omitempty"`

	// Outputs are the values the step reported: "gryvia.io/output-<key>" annotations on its GryviaAIJob, or a
	// JSON object of strings in the termination message of its pod (rank 0 for a job step)
	Outputs map[string]string `json:"outputs,omitempty"`

	// Message provides additional status information
	Message string `json:"message,omitempty"`
}

// GryviaWorkflowStatus defines the observed state of GryviaWorkflow
type GryviaWorkflowStatus struct {
	// Phase is the overall workflow phase (Pending, Running, Succeeded, Failed)
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// StepStatuses is the status of each step in the workflow
	StepStatuses []StepStatus `json:"stepStatuses,omitempty"`

	// StartTime is when the workflow started
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the workflow finished
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message provides additional information about the current phase
	Message string `json:"message,omitempty"`

	// Run counts the runs of a scheduled workflow (0 for the only run of an unscheduled one)
	Run int32 `json:"run,omitempty"`

	// LastScheduleTime is when the schedule last started a run
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// NextScheduleTime is when the schedule starts the next run
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Steps",type=integer,JSONPath=`.status.stepsTotal`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaWorkflow is the Schema for the gryviaworkflows API
type GryviaWorkflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaWorkflowSpec   `json:"spec,omitempty"`
	Status GryviaWorkflowStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaWorkflowList contains a list of GryviaWorkflow
type GryviaWorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaWorkflow `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaWorkflow{}, &GryviaWorkflowList{})
}
