// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaHealthCheckSpec defines the desired state of GryviaHealthCheck
type GryviaHealthCheckSpec struct {
	// Target defines what to health check
	Target HealthCheckTarget `json:"target"`

	// Schedule is the cron schedule for health checks
	Schedule string `json:"schedule,omitempty"`

	// Checks defines the health checks to run
	Checks []HealthCheck `json:"checks,omitempty"`

	// OnFailure defines actions on health check failure
	OnFailure *FailureActions `json:"onFailure,omitempty"`

	// Remediation defines auto-remediation actions
	Remediation *RemediationConfig `json:"remediation,omitempty"`
}

// HealthCheckTarget defines the target for health checks
type HealthCheckTarget struct {
	// Type is the target type (node, gpu, job, cluster)
	Type string `json:"type"`

	// Selector restricts the target by labels
	Selector map[string]string `json:"selector,omitempty"`
}

// HealthCheck defines a single health check
type HealthCheck struct {
	// Name of the health check
	Name string `json:"name"`

	// Type of check (gpu-utilization, gpu-memory, gpu-temperature, etc.)
	Type string `json:"type"`

	// Threshold defines pass/fail thresholds
	Threshold *HealthCheckThreshold `json:"threshold,omitempty"`

	// Enabled controls whether this check runs
	Enabled bool `json:"enabled,omitempty"`
}

// HealthCheckThreshold defines thresholds for a health check
type HealthCheckThreshold struct {
	// Min is the minimum acceptable value
	Min *float64 `json:"min,omitempty"`

	// Max is the maximum acceptable value
	Max *float64 `json:"max,omitempty"`

	// Warning is the warning threshold
	Warning *float64 `json:"warning,omitempty"`

	// Critical is the critical threshold
	Critical *float64 `json:"critical,omitempty"`
}

// FailureActions defines actions on health check failure
type FailureActions struct {
	// Cordon cordons the node on failure
	Cordon bool `json:"cordon,omitempty"`

	// Drain drains the node on failure
	Drain bool `json:"drain,omitempty"`

	// Alert sends alerts on failure
	Alert bool `json:"alert,omitempty"`

	// AutoRemediate attempts automatic remediation
	AutoRemediate bool `json:"autoRemediate,omitempty"`
}

// RemediationConfig defines auto-remediation settings
type RemediationConfig struct {
	// GpuReset attempts a GPU reset
	GpuReset bool `json:"gpuReset,omitempty"`

	// DriverReload reloads the GPU driver
	DriverReload bool `json:"driverReload,omitempty"`

	// NodeReboot reboots the node
	NodeReboot bool `json:"nodeReboot,omitempty"`

	// MaxAttempts limits remediation attempts
	MaxAttempts int32 `json:"maxAttempts,omitempty"`
}

// HealthCheckResult holds the result of a single health check
type HealthCheckResult struct {
	// CheckName is the name of the check
	CheckName string `json:"checkName"`

	// Status is the check result (pass, warning, fail)
	Status string `json:"status"`

	// Value is the measured value
	Value string `json:"value,omitempty"`

	// Threshold is the threshold that was checked
	Threshold string `json:"threshold,omitempty"`

	// Message is a human-readable description
	Message string `json:"message,omitempty"`

	// Timestamp is when the check was performed
	Timestamp *metav1.Time `json:"timestamp,omitempty"`
}

// AffectedResource describes a resource affected by health check failures
type AffectedResource struct {
	// Type of the resource (node, gpu)
	Type string `json:"type"`

	// Name of the resource
	Name string `json:"name"`

	// Status of the resource
	Status string `json:"status"`
}

// RemediationEvent records a remediation attempt
type RemediationEvent struct {
	// Timestamp of the remediation attempt
	Timestamp *metav1.Time `json:"timestamp,omitempty"`

	// Action is the remediation action taken
	Action string `json:"action"`

	// Success indicates whether remediation succeeded
	Success bool `json:"success"`

	// Message describes the result
	Message string `json:"message,omitempty"`
}

// GryviaHealthCheckStatus defines the observed state of GryviaHealthCheck
type GryviaHealthCheckStatus struct {
	// Conditions report runtime capability and remediation state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastCheckTime is when the last health check ran
	LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`

	// NextCheckTime is when the next health check is scheduled
	NextCheckTime *metav1.Time `json:"nextCheckTime,omitempty"`

	// OverallHealth is the aggregate health (healthy, degraded, unhealthy, unknown)
	OverallHealth string `json:"overallHealth,omitempty"`

	// CheckResults are the results of individual checks
	CheckResults []HealthCheckResult `json:"checkResults,omitempty"`

	// AffectedResources lists resources affected by health issues
	AffectedResources []AffectedResource `json:"affectedResources,omitempty"`

	// RemediationHistory records remediation attempts
	RemediationHistory []RemediationEvent `json:"remediationHistory,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.type`
//+kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.overallHealth`
//+kubebuilder:printcolumn:name="Last-Check",type=string,JSONPath=`.status.lastCheckTime`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaHealthCheck is the Schema for the gryviahealthchecks API
type GryviaHealthCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaHealthCheckSpec   `json:"spec,omitempty"`
	Status GryviaHealthCheckStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaHealthCheckList contains a list of GryviaHealthCheck
type GryviaHealthCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaHealthCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaHealthCheck{}, &GryviaHealthCheckList{})
}
