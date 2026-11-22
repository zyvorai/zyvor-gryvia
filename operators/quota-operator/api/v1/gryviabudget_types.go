// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaBudgetSpec defines the desired state of GryviaBudget
type GryviaBudgetSpec struct {
	// Scope defines who this budget applies to
	Scope BudgetScope `json:"scope"`

	// Period defines the budget period
	Period BudgetPeriod `json:"period"`

	// Limits defines the budget limits
	Limits BudgetLimits `json:"limits,omitempty"`

	// Alerts defines alert thresholds
	Alerts []BudgetAlert `json:"alerts,omitempty"`

	// Enforcement defines how the budget is enforced
	Enforcement *BudgetEnforcement `json:"enforcement,omitempty"`

	// Rollover defines rollover policy
	Rollover *BudgetRollover `json:"rollover,omitempty"`

	// Priority defines priority override settings
	Priority *BudgetPriority `json:"priority,omitempty"`
}

// BudgetScope defines the scope of the budget
type BudgetScope struct {
	// Type is the scope type (team, namespace, user, project)
	Type string `json:"type"`

	// Name is the name of the scoped entity
	Name string `json:"name"`
}

// BudgetPeriod defines the budget period
type BudgetPeriod struct {
	// Type is the period type (daily, weekly, monthly, quarterly, annual, custom)
	Type string `json:"type"`

	// StartDate is the start of a custom period
	StartDate string `json:"startDate,omitempty"`

	// EndDate is the end of a custom period
	EndDate string `json:"endDate,omitempty"`
}

// BudgetLimits defines budget limits
type BudgetLimits struct {
	// CostUSD is the cost limit in USD
	CostUSD float64 `json:"costUSD,omitempty"`

	// GPUHours is the GPU hours limit
	GPUHours float64 `json:"gpuHours,omitempty"`

	// GPUTypeHours is per-GPU-type hour limits
	GPUTypeHours map[string]float64 `json:"gpuTypeHours,omitempty"`

	// MaxConcurrentJobs limits concurrent jobs
	MaxConcurrentJobs int `json:"maxConcurrentJobs,omitempty"`

	// MaxConcurrentGPUs limits concurrent GPU usage
	MaxConcurrentGPUs int `json:"maxConcurrentGPUs,omitempty"`
}

// BudgetAlert defines an alert threshold
type BudgetAlert struct {
	// Threshold is the percentage of budget consumed (0-100)
	Threshold float64 `json:"threshold"`

	// Actions to take when threshold is reached
	Actions []string `json:"actions"`

	// Recipients for notifications
	Recipients []string `json:"recipients,omitempty"`
}

// BudgetEnforcement defines enforcement policy
type BudgetEnforcement struct {
	// Enabled enables enforcement
	Enabled bool `json:"enabled,omitempty"`

	// Action is the enforcement action (warn, block, throttle)
	Action string `json:"action,omitempty"`

	// GracePeriod before enforcement
	GracePeriod string `json:"gracePeriod,omitempty"`
}

// BudgetRollover defines rollover policy
type BudgetRollover struct {
	// Enabled enables rollover
	Enabled bool `json:"enabled,omitempty"`

	// MaxRolloverPercent is the max percentage that can roll over
	MaxRolloverPercent int `json:"maxRolloverPercent,omitempty"`
}

// BudgetPriority defines priority override settings
type BudgetPriority struct {
	// AllowOverride allows high-priority jobs to exceed budget
	AllowOverride bool `json:"allowOverride,omitempty"`

	// OverrideLimitPercent is the percentage over budget allowed
	OverrideLimitPercent int `json:"overrideLimitPercent,omitempty"`
}

// GryviaBudgetStatus defines the observed state of GryviaBudget
type GryviaBudgetStatus struct {
	// CurrentPeriod tracks the current budget period
	CurrentPeriod *BudgetCurrentPeriod `json:"currentPeriod,omitempty"`

	// Usage tracks current resource consumption
	Usage *BudgetUsage `json:"usage,omitempty"`

	// Utilization tracks utilization percentages
	Utilization *BudgetUtilization `json:"utilization,omitempty"`

	// State is the budget state (active, warning, exceeded, blocked)
	State string `json:"state,omitempty"`

	// Alerts is the history of triggered alerts
	Alerts []BudgetAlertEvent `json:"alerts,omitempty"`

	// Forecast projects end-of-period usage
	Forecast *BudgetForecast `json:"forecast,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BudgetCurrentPeriod tracks the current period
type BudgetCurrentPeriod struct {
	// StartDate of the current period
	StartDate metav1.Time `json:"startDate,omitempty"`

	// EndDate of the current period
	EndDate metav1.Time `json:"endDate,omitempty"`

	// DaysRemaining in the current period
	DaysRemaining int `json:"daysRemaining,omitempty"`
}

// BudgetUsage tracks current usage
type BudgetUsage struct {
	// CostUSD is the cost consumed
	CostUSD float64 `json:"costUSD,omitempty"`

	// GPUHours is the GPU hours consumed
	GPUHours float64 `json:"gpuHours,omitempty"`

	// GPUTypeHours is per-GPU-type hours consumed
	GPUTypeHours map[string]float64 `json:"gpuTypeHours,omitempty"`

	// JobCount is total jobs run this period
	JobCount int `json:"jobCount,omitempty"`

	// CurrentConcurrentJobs is currently running jobs
	CurrentConcurrentJobs int `json:"currentConcurrentJobs,omitempty"`

	// CurrentConcurrentGPUs is currently allocated GPUs
	CurrentConcurrentGPUs int `json:"currentConcurrentGPUs,omitempty"`
}

// BudgetUtilization tracks utilization percentages
type BudgetUtilization struct {
	// CostPercent is cost utilization percentage
	CostPercent float64 `json:"costPercent,omitempty"`

	// GPUHoursPercent is GPU hours utilization percentage
	GPUHoursPercent float64 `json:"gpuHoursPercent,omitempty"`

	// JobsPercent is jobs utilization percentage
	JobsPercent float64 `json:"jobsPercent,omitempty"`
}

// BudgetAlertEvent records a triggered alert
type BudgetAlertEvent struct {
	// Timestamp of the alert
	Timestamp metav1.Time `json:"timestamp,omitempty"`

	// Threshold that was crossed
	Threshold float64 `json:"threshold,omitempty"`

	// Message describing the alert
	Message string `json:"message,omitempty"`
}

// BudgetForecast projects end-of-period usage
type BudgetForecast struct {
	// ProjectedCostUSD is the projected total cost
	ProjectedCostUSD float64 `json:"projectedCostUSD,omitempty"`

	// ProjectedGPUHours is the projected total GPU hours
	ProjectedGPUHours float64 `json:"projectedGPUHours,omitempty"`

	// BudgetSufficient indicates if budget will last the period
	BudgetSufficient bool `json:"budgetSufficient,omitempty"`

	// EstimatedExhaustionDate is when budget will run out
	EstimatedExhaustionDate *metav1.Time `json:"estimatedExhaustionDate,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

// GryviaBudget is the Schema for the gryviabudgets API
type GryviaBudget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaBudgetSpec   `json:"spec,omitempty"`
	Status GryviaBudgetStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaBudgetList contains a list of GryviaBudget
type GryviaBudgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaBudget `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaBudget{}, &GryviaBudgetList{})
}
