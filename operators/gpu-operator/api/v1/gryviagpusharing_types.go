// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaGPUSharingPolicySpec defines the desired state of GryviaGPUSharingPolicy
type GryviaGPUSharingPolicySpec struct {
	// NodeSelector defines which nodes this policy applies to
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Strategy is the sharing strategy (time-slicing, mig, fractional, spatial)
	Strategy string `json:"strategy"`

	// TimeSlicing defines time-slicing configuration
	TimeSlicing *GPUTimeSlicingConfig `json:"timeSlicing,omitempty"`

	// FractionalGPU defines fractional GPU configuration
	FractionalGPU *GPUFractionalConfig `json:"fractionalGPU,omitempty"`

	// MIG defines Multi-Instance GPU configuration
	MIG *GPUMIGConfig `json:"mig,omitempty"`

	// QoS defines quality of service settings
	QoS *GPUSharingQoS `json:"qos,omitempty"`

	// Priority defines priority-based allocation
	Priority *GPUSharingPriority `json:"priority,omitempty"`

	// TenantQuotas defines per-tenant GPU sharing quotas
	TenantQuotas []GPUTenantQuota `json:"tenantQuotas,omitempty"`
}

// GPUTimeSlicingConfig defines time-slicing settings
type GPUTimeSlicingConfig struct {
	// Enabled enables time-slicing
	Enabled bool `json:"enabled,omitempty"`

	// MaxPodsPerGPU is the max pods sharing each GPU
	MaxPodsPerGPU int `json:"maxPodsPerGPU,omitempty"`

	// SliceDuration is the time slice duration
	SliceDuration string `json:"sliceDuration,omitempty"`

	// MemoryLimitPercent is the memory limit per pod
	MemoryLimitPercent int `json:"memoryLimitPercent,omitempty"`

	// Enforcement is soft or hard
	Enforcement string `json:"enforcement,omitempty"`
}

// GPUFractionalConfig defines fractional GPU settings
type GPUFractionalConfig struct {
	// Enabled enables fractional GPUs
	Enabled bool `json:"enabled,omitempty"`

	// Granularity of fractions (1/2, 1/4, 1/8, 1/16)
	Granularity string `json:"granularity,omitempty"`

	// Oversubscription settings
	Oversubscription *GPUOversubscription `json:"oversubscription,omitempty"`
}

// GPUOversubscription defines oversubscription settings
type GPUOversubscription struct {
	// Enabled enables oversubscription
	Enabled bool `json:"enabled,omitempty"`

	// MaxRatio is the max oversubscription ratio (e.g., 1.5 = 150%)
	MaxRatio float64 `json:"maxRatio,omitempty"`
}

// GPUMIGConfig defines MIG configuration
type GPUMIGConfig struct {
	// Enabled enables MIG
	Enabled bool `json:"enabled,omitempty"`

	// Profiles defines MIG profile distribution
	Profiles []GPUMIGProfile `json:"profiles,omitempty"`
}

// GPUMIGProfile defines a MIG profile
type GPUMIGProfile struct {
	// Name of the MIG profile (e.g., 1g.10gb, 3g.40gb)
	Name string `json:"name,omitempty"`

	// Count of this profile per GPU
	Count int `json:"count,omitempty"`
}

// GPUSharingQoS defines quality of service
type GPUSharingQoS struct {
	// GuaranteedMemory guarantees memory allocation
	GuaranteedMemory bool `json:"guaranteedMemory,omitempty"`

	// GuaranteedCompute guarantees compute allocation
	GuaranteedCompute bool `json:"guaranteedCompute,omitempty"`

	// IsolationLevel (none, memory, compute, full)
	IsolationLevel string `json:"isolationLevel,omitempty"`
}

// GPUSharingPriority defines priority-based allocation
type GPUSharingPriority struct {
	// Enabled enables priority-based allocation
	Enabled bool `json:"enabled,omitempty"`

	// HighPriorityShare is the percentage for high priority jobs
	HighPriorityShare int `json:"highPriorityShare,omitempty"`

	// PreemptLowPriority allows preempting low priority workloads
	PreemptLowPriority bool `json:"preemptLowPriority,omitempty"`
}

// GPUTenantQuota defines per-tenant GPU sharing quota
type GPUTenantQuota struct {
	// Tenant is the tenant or team name
	Tenant string `json:"tenant"`

	// MaxFractionalGPUs is the max fractional GPU units
	MaxFractionalGPUs int `json:"maxFractionalGPUs,omitempty"`

	// MaxMemoryGB is the max GPU memory in GB
	MaxMemoryGB int `json:"maxMemoryGB,omitempty"`
}

// GryviaGPUSharingPolicyStatus defines the observed state of GryviaGPUSharingPolicy
type GryviaGPUSharingPolicyStatus struct {
	// AffectedNodes lists nodes where this policy is applied
	AffectedNodes []string `json:"affectedNodes,omitempty"`

	// TotalGPUs is the total physical GPUs
	TotalGPUs int `json:"totalGPUs,omitempty"`

	// EffectiveGPUs is the total allocatable GPU units after sharing
	EffectiveGPUs int `json:"effectiveGPUs,omitempty"`

	// CurrentAllocations tracks current allocations
	CurrentAllocations *GPUSharingAllocations `json:"currentAllocations,omitempty"`

	// PerPodUtilization tracks per-pod GPU utilization on shared GPUs
	PerPodUtilization []GPUPodUtilization `json:"perPodUtilization,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GPUSharingAllocations tracks current allocations
type GPUSharingAllocations struct {
	// Physical is the number of physical GPUs allocated
	Physical int `json:"physical,omitempty"`

	// Fractional is the number of fractional allocations
	Fractional int `json:"fractional,omitempty"`

	// Utilization is the average GPU utilization (0-1)
	Utilization float64 `json:"utilization,omitempty"`
}

// GPUPodUtilization tracks per-pod GPU utilization
type GPUPodUtilization struct {
	// PodName is the name of the pod
	PodName string `json:"podName,omitempty"`

	// Namespace of the pod
	Namespace string `json:"namespace,omitempty"`

	// GPUIndex is the GPU index being used
	GPUIndex int `json:"gpuIndex,omitempty"`

	// NodeName is the node where the pod runs
	NodeName string `json:"nodeName,omitempty"`

	// Utilization is the GPU utilization percentage
	Utilization float64 `json:"utilization,omitempty"`

	// MemoryUsedMB is the GPU memory used in MB
	MemoryUsedMB int `json:"memoryUsedMB,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

// GryviaGPUSharingPolicy is the Schema for the gryviagpusharingpolicies API
type GryviaGPUSharingPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaGPUSharingPolicySpec   `json:"spec,omitempty"`
	Status GryviaGPUSharingPolicyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaGPUSharingPolicyList contains a list of GryviaGPUSharingPolicy
type GryviaGPUSharingPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaGPUSharingPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaGPUSharingPolicy{}, &GryviaGPUSharingPolicyList{})
}
