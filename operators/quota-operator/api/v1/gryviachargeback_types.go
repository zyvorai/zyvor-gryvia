// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaChargebackSpec defines the desired state of GryviaChargeback
type GryviaChargebackSpec struct {
	// Period defines the billing period
	Period ChargebackPeriod `json:"period,omitempty"`

	// AllocationModel defines how costs are allocated
	AllocationModel ChargebackAllocationModel `json:"allocationModel,omitempty"`

	// Pricing defines GPU and resource pricing
	Pricing ChargebackPricing `json:"pricing,omitempty"`

	// CostCenters defines cost center configurations
	CostCenters []CostCenter `json:"costCenters,omitempty"`

	// Reports defines report generation
	Reports *ChargebackReports `json:"reports,omitempty"`

	// Mode is showback (informational) or chargeback (billing)
	Mode string `json:"mode,omitempty"`
}

// ChargebackPeriod defines the billing period
type ChargebackPeriod struct {
	// Type is the period type (monthly, quarterly, annual, custom)
	Type string `json:"type,omitempty"`

	// StartDate for custom periods
	StartDate string `json:"startDate,omitempty"`

	// EndDate for custom periods
	EndDate string `json:"endDate,omitempty"`

	// Timezone for period calculations
	Timezone string `json:"timezone,omitempty"`
}

// ChargebackAllocationModel defines cost allocation
type ChargebackAllocationModel struct {
	// Method is the allocation method (actual-usage, reserved-capacity, equal-split, weighted)
	Method string `json:"method,omitempty"`

	// IncludePlatformCosts includes platform overhead
	IncludePlatformCosts bool `json:"includePlatformCosts,omitempty"`

	// PlatformOverhead is the overhead percentage
	PlatformOverhead float64 `json:"platformOverhead,omitempty"`
}

// ChargebackPricing defines pricing configuration
type ChargebackPricing struct {
	// Currency for pricing
	Currency string `json:"currency,omitempty"`

	// GPURates is per-GPU-type pricing
	GPURates map[string]GPURate `json:"gpuRates,omitempty"`

	// Storage pricing per GB/month
	Storage *StoragePricing `json:"storage,omitempty"`

	// Network pricing
	Network *NetworkPricing `json:"network,omitempty"`
}

// GPURate defines pricing for a GPU type
type GPURate struct {
	// HourlyRate is the on-demand hourly rate
	HourlyRate float64 `json:"hourlyRate,omitempty"`

	// MonthlyReserved is the monthly reserved rate
	MonthlyReserved float64 `json:"monthlyReserved,omitempty"`

	// SpotDiscount is the spot discount percentage
	SpotDiscount float64 `json:"spotDiscount,omitempty"`
}

// StoragePricing defines storage pricing
type StoragePricing struct {
	Standard    float64 `json:"standard,omitempty"`
	Performance float64 `json:"performance,omitempty"`
	Archive     float64 `json:"archive,omitempty"`
}

// NetworkPricing defines network pricing
type NetworkPricing struct {
	EgressPerGB  float64 `json:"egressPerGB,omitempty"`
	IngressPerGB float64 `json:"ingressPerGB,omitempty"`
}

// CostCenter defines a cost center
type CostCenter struct {
	// ID is the cost center identifier
	ID string `json:"id,omitempty"`

	// Name is the display name
	Name string `json:"name,omitempty"`

	// Description of the cost center
	Description string `json:"description,omitempty"`

	// Teams assigned to this cost center
	Teams []string `json:"teams,omitempty"`

	// Budget allocations
	Budget *CostCenterBudget `json:"budget,omitempty"`

	// BillingContact for the cost center
	BillingContact *BillingContact `json:"billingContact,omitempty"`
}

// CostCenterBudget defines budget allocations
type CostCenterBudget struct {
	Monthly   float64 `json:"monthly,omitempty"`
	Quarterly float64 `json:"quarterly,omitempty"`
	Annual    float64 `json:"annual,omitempty"`
}

// BillingContact defines billing contact info
type BillingContact struct {
	Name       string `json:"name,omitempty"`
	Email      string `json:"email,omitempty"`
	Department string `json:"department,omitempty"`
}

// ChargebackReports defines report generation
type ChargebackReports struct {
	// Enabled enables report generation
	Enabled bool `json:"enabled,omitempty"`

	// Frequency of reports (daily, weekly, monthly, quarterly)
	Frequency string `json:"frequency,omitempty"`

	// Formats for reports (pdf, csv, excel, json, html)
	Formats []string `json:"formats,omitempty"`

	// IncludeDetails includes detailed breakdown
	IncludeDetails bool `json:"includeDetails,omitempty"`
}

// GryviaChargebackStatus defines the observed state of GryviaChargeback
type GryviaChargebackStatus struct {
	// CurrentPeriod tracks the current period
	CurrentPeriod *ChargebackCurrentPeriod `json:"currentPeriod,omitempty"`

	// LastReport tracks the last generated report
	LastReport *ChargebackReportRef `json:"lastReport,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ChargebackCurrentPeriod tracks current period costs
type ChargebackCurrentPeriod struct {
	// StartDate of the current period
	StartDate string `json:"startDate,omitempty"`

	// EndDate of the current period
	EndDate string `json:"endDate,omitempty"`

	// TotalCost for the current period
	TotalCost float64 `json:"totalCost,omitempty"`

	// CostCenters is the breakdown by cost center
	CostCenters []CostCenterStatus `json:"costCenters,omitempty"`

	// ByResourceType is the breakdown by resource type
	ByResourceType *ResourceTypeCosts `json:"byResourceType,omitempty"`
}

// CostCenterStatus tracks cost center spending
type CostCenterStatus struct {
	// ID of the cost center
	ID string `json:"id,omitempty"`

	// Name of the cost center
	Name string `json:"name,omitempty"`

	// Cost for this cost center
	Cost float64 `json:"cost,omitempty"`

	// Budget for this cost center
	Budget float64 `json:"budget,omitempty"`

	// Variance from budget (negative = under, positive = over)
	Variance float64 `json:"variance,omitempty"`

	// PercentUsed of budget
	PercentUsed float64 `json:"percentUsed,omitempty"`
}

// ResourceTypeCosts tracks costs by resource type
type ResourceTypeCosts struct {
	Compute  float64 `json:"compute,omitempty"`
	Storage  float64 `json:"storage,omitempty"`
	Network  float64 `json:"network,omitempty"`
	Support  float64 `json:"support,omitempty"`
	Platform float64 `json:"platform,omitempty"`
}

// ChargebackReportRef tracks the last generated report
type ChargebackReportRef struct {
	// Timestamp when the report was generated
	Timestamp *metav1.Time `json:"timestamp,omitempty"`

	// Period covered by the report
	Period string `json:"period,omitempty"`

	// Path where the report is stored
	Path string `json:"path,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

// GryviaChargeback is the Schema for the gryviachargebacks API
type GryviaChargeback struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaChargebackSpec   `json:"spec,omitempty"`
	Status GryviaChargebackStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaChargebackList contains a list of GryviaChargeback
type GryviaChargebackList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaChargeback `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaChargeback{}, &GryviaChargebackList{})
}
