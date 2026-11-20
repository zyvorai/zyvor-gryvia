// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkspaceType defines the type of development environment
type WorkspaceType string

const (
	WorkspaceTypeJupyter WorkspaceType = "jupyter"
	WorkspaceTypeVSCode  WorkspaceType = "vscode"
)

// GryviaWorkspaceSpec defines the desired state of GryviaWorkspace
type GryviaWorkspaceSpec struct {
	// Type is the workspace environment type (jupyter, vscode)
	Type WorkspaceType `json:"type"`

	// GPUCount is the number of GPUs to attach to the workspace
	GPUCount int32 `json:"gpuCount,omitempty"`

	// GPUType is the preferred GPU type
	GPUType string `json:"gpuType,omitempty"`

	// Image is the container image (defaults to a standard JupyterLab or VS Code image)
	Image string `json:"image,omitempty"`

	// Storage is the PVC size for persistent workspace data (e.g., "50Gi")
	Storage string `json:"storage,omitempty"`

	// StorageClassName is the storage class for the workspace PVC
	StorageClassName string `json:"storageClassName,omitempty"`

	// IdleTimeoutMinutes is the number of minutes of inactivity before scaling down
	IdleTimeoutMinutes int32 `json:"idleTimeoutMinutes,omitempty"`

	// MaxLifetimeHours is the maximum workspace lifetime in hours before auto-shutdown
	MaxLifetimeHours int32 `json:"maxLifetimeHours,omitempty"`

	// Env is a list of additional environment variables for the workspace
	Env map[string]string `json:"env,omitempty"`

	// Resources defines CPU/memory requests and limits
	CPURequest string `json:"cpuRequest,omitempty"`
	MemRequest string `json:"memRequest,omitempty"`
	CPULimit   string `json:"cpuLimit,omitempty"`
	MemLimit   string `json:"memLimit,omitempty"`

	// Paused indicates whether the workspace should be scaled down
	Paused bool `json:"paused,omitempty"`
}

// WorkspacePhase represents the current state of a workspace
type WorkspacePhase string

const (
	WorkspacePhasePending     WorkspacePhase = "Pending"
	WorkspacePhaseRunning     WorkspacePhase = "Running"
	WorkspacePhaseIdle        WorkspacePhase = "Idle"
	WorkspacePhasePaused      WorkspacePhase = "Paused"
	WorkspacePhaseTerminating WorkspacePhase = "Terminating"
	WorkspacePhaseFailed      WorkspacePhase = "Failed"
)

// GryviaWorkspaceStatus defines the observed state of GryviaWorkspace
type GryviaWorkspaceStatus struct {
	// Phase is the current workspace phase
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// PodName is the name of the workspace pod
	PodName string `json:"podName,omitempty"`

	// URL is the access URL for the workspace
	URL string `json:"url,omitempty"`

	// LastActivity is the timestamp of the last detected user activity
	LastActivity *metav1.Time `json:"lastActivity,omitempty"`

	// StartTime is when the workspace pod started
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// PVCName is the name of the persistent volume claim for the workspace
	PVCName string `json:"pvcName,omitempty"`

	// ServiceName is the name of the Kubernetes Service exposing the workspace
	ServiceName string `json:"serviceName,omitempty"`

	// Message provides additional status information
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
//+kubebuilder:printcolumn:name="GPUs",type=integer,JSONPath=`.spec.gpuCount`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaWorkspace is the Schema for the gryviaworkspaces API
type GryviaWorkspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaWorkspaceSpec   `json:"spec,omitempty"`
	Status GryviaWorkspaceStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaWorkspaceList contains a list of GryviaWorkspace
type GryviaWorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaWorkspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaWorkspace{}, &GryviaWorkspaceList{})
}
