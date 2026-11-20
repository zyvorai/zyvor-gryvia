// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaReservationSpec defines the desired state of GryviaReservation
type GryviaReservationSpec struct {
	// Owner defines who can use this reservation
	Owner ReservationOwner `json:"owner"`

	// Resources defines what to reserve
	Resources ReservationResources `json:"resources"`

	// Schedule defines when the reservation is active
	Schedule ReservationSchedule `json:"schedule"`

	// Guarantees defines reservation guarantees
	Guarantees *ReservationGuarantees `json:"guarantees,omitempty"`

	// Billing defines cost and billing options
	Billing *ReservationBilling `json:"billing,omitempty"`

	// Notifications defines notification settings
	Notifications *ReservationNotifications `json:"notifications,omitempty"`
}

// ReservationOwner defines who owns the reservation
type ReservationOwner struct {
	// Type is the owner type (user, team, project, namespace)
	Type string `json:"type"`

	// Name is the owner name
	Name string `json:"name"`
}

// ReservationResources defines what to reserve
type ReservationResources struct {
	// GpuType is the GPU model to reserve
	GpuType string `json:"gpuType,omitempty"`

	// GpuCount is the number of GPUs to reserve
	GpuCount int32 `json:"gpuCount,omitempty"`

	// Nodes lists specific nodes to reserve
	Nodes []string `json:"nodes,omitempty"`
}

// ReservationSchedule defines when the reservation is active
type ReservationSchedule struct {
	// Type is the schedule type: immediate (active now until endTime, if set), scheduled
	// (active from startTime until endTime) or recurring (active during each window that
	// starts at a recurrence.cron fire time and lasts recurrence.duration, until endTime)
	Type string `json:"type"`

	// StartTime is the scheduled start time
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// EndTime is the scheduled end time
	EndTime *metav1.Time `json:"endTime,omitempty"`

	// Recurrence defines recurring schedule
	Recurrence *ReservationRecurrence `json:"recurrence,omitempty"`

	// AutoExtend extends reservation if jobs are still running
	AutoExtend bool `json:"autoExtend,omitempty"`
}

// ReservationRecurrence defines a recurring schedule
type ReservationRecurrence struct {
	// Cron is a five-field cron expression (minute hour day-of-month month day-of-week,
	// UTC): each fire time opens a reservation window. Names and macros are not supported.
	Cron string `json:"cron,omitempty"`

	// Duration is how long each window lasts, e.g. 8h, 90m or 2d
	Duration string `json:"duration,omitempty"`
}

// ReservationGuarantees defines reservation guarantees
type ReservationGuarantees struct {
	// Exclusive means reserved resources are exclusive to owner
	Exclusive bool `json:"exclusive,omitempty"`

	// Preemptible means the reservation can be preempted
	Preemptible bool `json:"preemptible,omitempty"`

	// SLA defines service level agreement
	SLA *ReservationSLA `json:"sla,omitempty"`
}

// ReservationSLA defines SLA parameters
type ReservationSLA struct {
	// Availability is the guaranteed availability percentage
	Availability float64 `json:"availability,omitempty"`

	// MaxQueueTime is the maximum time to wait for reserved resources
	MaxQueueTime string `json:"maxQueueTime,omitempty"`
}

// ReservationBilling defines billing options
type ReservationBilling struct {
	// ChargeWhenIdle charges for reserved time even if not used
	ChargeWhenIdle bool `json:"chargeWhenIdle,omitempty"`

	// Discount is the discount percentage for reservations
	Discount float64 `json:"discount,omitempty"`

	// Prepaid indicates upfront payment
	Prepaid bool `json:"prepaid,omitempty"`
}

// ReservationNotifications defines notification settings
type ReservationNotifications struct {
	// BeforeExpiry notifies before expiry
	BeforeExpiry string `json:"beforeExpiry,omitempty"`

	// OnExpiry notifies on expiry
	OnExpiry bool `json:"onExpiry,omitempty"`

	// OnJobsFinish notifies when all jobs finish
	OnJobsFinish bool `json:"onJobsFinish,omitempty"`
}

// ReservationUtilization holds utilization metrics
type ReservationUtilization struct {
	// TotalReservedTime is the total reserved time
	TotalReservedTime string `json:"totalReservedTime,omitempty"`

	// TotalUsedTime is the total used time
	TotalUsedTime string `json:"totalUsedTime,omitempty"`

	// UtilizationPercent is the utilization percentage
	UtilizationPercent float64 `json:"utilizationPercent,omitempty"`

	// JobsRun is the number of jobs run during the reservation
	JobsRun int32 `json:"jobsRun,omitempty"`
}

// ReservationCost holds cost information
type ReservationCost struct {
	// TotalCost is the total reservation cost
	TotalCost float64 `json:"totalCost,omitempty"`

	// UsedCost is the cost for actual usage
	UsedCost float64 `json:"usedCost,omitempty"`

	// WastedCost is the cost for idle time
	WastedCost float64 `json:"wastedCost,omitempty"`
}

// GryviaReservationStatus defines the observed state of GryviaReservation
type GryviaReservationStatus struct {
	// State is the current state (pending, active, expired, cancelled)
	State string `json:"state,omitempty"`

	// ActualStartTime is the actual start time
	ActualStartTime *metav1.Time `json:"actualStartTime,omitempty"`

	// ActualEndTime is the actual end time
	ActualEndTime *metav1.Time `json:"actualEndTime,omitempty"`

	// AllocatedNodes lists the allocated node names
	AllocatedNodes []string `json:"allocatedNodes,omitempty"`

	// AllocatedGPUs is the number of allocated GPUs
	AllocatedGPUs int32 `json:"allocatedGPUs,omitempty"`

	// UtilizationMetrics holds utilization data
	UtilizationMetrics *ReservationUtilization `json:"utilizationMetrics,omitempty"`

	// Cost holds cost information
	Cost *ReservationCost `json:"cost,omitempty"`

	// Message explains why a reservation is pending or only partly allocated
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner.name`
//+kubebuilder:printcolumn:name="GPUs",type=integer,JSONPath=`.spec.resources.gpuCount`
//+kubebuilder:printcolumn:name="GPU-Type",type=string,JSONPath=`.spec.resources.gpuType`
//+kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
//+kubebuilder:printcolumn:name="Start",type=string,JSONPath=`.status.actualStartTime`
//+kubebuilder:printcolumn:name="End",type=string,JSONPath=`.spec.schedule.endTime`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaReservation is the Schema for the gryviareservations API
type GryviaReservation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaReservationSpec   `json:"spec,omitempty"`
	Status GryviaReservationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaReservationList contains a list of GryviaReservation
type GryviaReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaReservation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaReservation{}, &GryviaReservationList{})
}
