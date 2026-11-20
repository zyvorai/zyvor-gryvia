// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Agent phases.
const (
	AgentPending = "Pending"
	AgentReady   = "Ready"
	AgentFailed  = "Failed"
)

// Agent tool types.
const (
	AgentToolRetrieval = "retrieval"
	AgentToolHTTP      = "http"
	AgentToolZyntra    = "zyntra"
)

// AgentRetrievalTool searches a GryviaVectorIndex through the LLM gateway's POST /v1/retrieve
type AgentRetrievalTool struct {
	// VectorIndexRef is a GryviaVectorIndex in the agent's namespace
	// +kubebuilder:validation:MinLength=1
	VectorIndexRef string `json:"vectorIndexRef"`

	// TopK is the number of chunks returned to the model
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +kubebuilder:default=4
	// +optional
	TopK int32 `json:"topK,omitempty"`
}

// AgentHTTPTool calls an HTTP endpoint chosen by the model, restricted to an allowlist of URL prefixes
type AgentHTTPTool struct {
	// URLs are the allowed URL prefixes (http:// or https://). The runtime refuses any other URL, and the agent's
	// NetworkPolicy only opens egress to these hosts' ports
	// +kubebuilder:validation:MinItems=1
	URLs []string `json:"urls"`

	// Method is the HTTP method the tool uses; POST sends the model's body argument
	// +kubebuilder:validation:Enum=GET;POST
	// +kubebuilder:default=GET
	// +optional
	Method string `json:"method,omitempty"`
}

// AgentZyntraTool searches and reads the business objects of a Zyntra ontology and, with Propose, drafts Zyntra
// proposals that people approve in Zyntra. The model sees it as <name>_search, <name>_object and <name>_propose
type AgentZyntraTool struct {
	// URL is Zyntra's base URL (http:// or https://). The agent's NetworkPolicy opens egress to its host and port
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// TokenSecretRef is a key of a Secret in the agent's namespace holding a Zyntra service token (zyntra
	// service-token) with the viewer role, plus proposer when Propose is set. Zyntra refuses approver and admin
	// roles for service tokens, so the agent cannot approve what it proposes
	TokenSecretRef corev1.SecretKeySelector `json:"tokenSecretRef"`

	// Propose offers the model a tool that drafts a proposal for a Zyntra action
	// +optional
	Propose bool `json:"propose,omitempty"`

	// Actions limits the actions the model may propose (Zyntra action ids); empty allows any action the token may
	// propose
	// +optional
	Actions []string `json:"actions,omitempty"`
}

// AgentTool is a function the agent's model may call
type AgentTool struct {
	// Name is the function name shown to the model
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_-]{1,64}$`
	Name string `json:"name"`

	// Description tells the model when to use the tool
	// +optional
	Description string `json:"description,omitempty"`

	// Type selects the tool implementation
	// +kubebuilder:validation:Enum=retrieval;http;zyntra
	Type string `json:"type"`

	// Retrieval configures a retrieval tool (type retrieval)
	// +optional
	Retrieval *AgentRetrievalTool `json:"retrieval,omitempty"`

	// HTTP configures an HTTP tool (type http)
	// +optional
	HTTP *AgentHTTPTool `json:"http,omitempty"`

	// Zyntra configures a Zyntra ontology tool (type zyntra)
	// +optional
	Zyntra *AgentZyntraTool `json:"zyntra,omitempty"`
}

// GryviaAgentSpec defines the desired state of GryviaAgent
type GryviaAgentSpec struct {
	// Model is the LLM gateway model the agent calls (a GryviaInferenceService name in the agent's namespace)
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// SystemPrompt is prepended to every conversation
	// +optional
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// Tools the model may call
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Tools []AgentTool `json:"tools,omitempty"`

	// MaxSteps caps the model calls per request; the last step offers no tools, so the model must answer
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +kubebuilder:default=5
	// +optional
	MaxSteps int32 `json:"maxSteps,omitempty"`

	// Replicas of the agent runtime; 0 scales it down
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Image overrides the reference agent runtime. It must serve POST /v1/chat/completions and GET /healthz on
	// port 8080 and read its configuration from AGENT_CONFIG
	// +optional
	Image string `json:"image,omitempty"`

	// Resources for the runtime container
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// GryviaAgentStatus defines the observed state of GryviaAgent
type GryviaAgentStatus struct {
	// Phase: Pending, Ready or Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message explains the phase
	// +optional
	Message string `json:"message,omitempty"`

	// Endpoint is the in-cluster OpenAI-compatible base URL of the agent
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ReadyReplicas of the runtime Deployment
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// KeySecret is the Secret in the agent's namespace holding its LLM gateway key
	// +optional
	KeySecret string `json:"keySecret,omitempty"`

	// ObservedGeneration is the spec generation the status reflects
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: ToolsResolved, Available
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:path=gryviaagents,scope=Namespaced,shortName=gag
//+kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaAgent is a tool-calling agent served as an OpenAI-compatible endpoint: a runtime Deployment that calls a
// model through the LLM gateway with its own key, behind an egress-restricting NetworkPolicy
type GryviaAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaAgentSpec   `json:"spec,omitempty"`
	Status GryviaAgentStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaAgentList contains a list of GryviaAgent
type GryviaAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaAgent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaAgent{}, &GryviaAgentList{})
}
