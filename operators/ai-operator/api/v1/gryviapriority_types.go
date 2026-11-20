// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaPrioritySpec defines the desired state of GryviaPriority
type GryviaPrioritySpec struct {
	// Value is the priority value (higher = more important, 0-1000000)
	Value int `json:"value"`

	// Description of this priority class
	Description string `json:"description,omitempty"`

	// PreemptionPolicy defines preemption behavior (Never, PreemptLowerPriority)
	PreemptionPolicy string `json:"preemptionPolicy,omitempty"`

	// QuotaOverride allows exceeding quota for this priority
	QuotaOverride *PriorityQuotaOverride `json:"quotaOverride,omitempty"`

	// SLA defines SLA guarantees for this priority
	SLA *PrioritySLA `json:"sla,omitempty"`
}

// PriorityQuotaOverride defines quota override settings
type PriorityQuotaOverride struct {
	// Enabled enables quota override
	Enabled bool `json:"enabled,omitempty"`

	// MaxOverridePercent is the max percentage over quota allowed
	MaxOverridePercent int `json:"maxOverridePercent,omitempty"`
}

// PrioritySLA defines SLA guarantees
type PrioritySLA struct {
	// MaxQueueTimeMinutes is the max queue time for this priority
	MaxQueueTimeMinutes int `json:"maxQueueTimeMinutes,omitempty"`

	// GuaranteedResources guarantees resources for this priority
	GuaranteedResources bool `json:"guaranteedResources,omitempty"`
}

// GryviaPriorityStatus defines the observed state of GryviaPriority
type GryviaPriorityStatus struct {
	// ActiveJobs is the number of jobs using this priority
	ActiveJobs int `json:"activeJobs,omitempty"`

	// QueuedJobs is the number of queued jobs with this priority
	QueuedJobs int `json:"queuedJobs,omitempty"`

	// PreemptionEvents is the number of preemption events
	PreemptionEvents int `json:"preemptionEvents,omitempty"`

	// PreemptionHistory tracks recent preemption events
	PreemptionHistory []PreemptionEvent `json:"preemptionHistory,omitempty"`

	// AvgQueueTime is the average queue time for this priority
	AvgQueueTime string `json:"avgQueueTime,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PreemptionEvent records a preemption event
type PreemptionEvent struct {
	// Timestamp of the event
	Timestamp metav1.Time `json:"timestamp,omitempty"`

	// PreemptedJob is the name of the preempted job
	PreemptedJob string `json:"preemptedJob,omitempty"`

	// PreemptedJobPriority is the priority of the preempted job
	PreemptedJobPriority int `json:"preemptedJobPriority,omitempty"`

	// PreemptingJob is the name of the preempting job
	PreemptingJob string `json:"preemptingJob,omitempty"`

	// Reason for the preemption
	Reason string `json:"reason,omitempty"`

	// GracePeriodUsed indicates if the preempted job had time to checkpoint
	GracePeriodUsed bool `json:"gracePeriodUsed,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

// GryviaPriority is the Schema for the gryviapriorities API
type GryviaPriority struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaPrioritySpec   `json:"spec,omitempty"`
	Status GryviaPriorityStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaPriorityList contains a list of GryviaPriority
type GryviaPriorityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaPriority `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaPriority{}, &GryviaPriorityList{})
}
