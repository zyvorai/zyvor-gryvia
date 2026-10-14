package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaTenantSpec defines the desired state of GryviaTenant
type GryviaTenantSpec struct {
	// DisplayName is the human-readable name
	DisplayName string `json:"displayName,omitempty"`

	// Description of the tenant
	Description string `json:"description,omitempty"`

	// ParentTenant for hierarchical organization
	ParentTenant string `json:"parentTenant,omitempty"`

	// Members defines tenant members
	Members []TenantMember `json:"members,omitempty"`

	// Quotas defines resource quotas
	Quotas *TenantQuotas `json:"quotas,omitempty"`

	// Priorities defines priority settings
	Priorities *TenantPriorities `json:"priorities,omitempty"`

	// NetworkPolicy defines network isolation
	NetworkPolicy *TenantNetworkPolicy `json:"networkPolicy,omitempty"`

	// StorageQuotas defines storage limits
	StorageQuotas *TenantStorageQuotas `json:"storageQuotas,omitempty"`

	// JobDefaults defines default job settings
	JobDefaults *TenantJobDefaults `json:"jobDefaults,omitempty"`

	// Billing defines billing configuration
	Billing *TenantBilling `json:"billing,omitempty"`

	// Notifications defines notification settings
	Notifications *TenantNotifications `json:"notifications,omitempty"`

	// Governance defines compliance and governance
	Governance *TenantGovernance `json:"governance,omitempty"`

	// AllowedSkus lists the GryviaGpuSku names this tenant may use; empty means all enabled SKUs
	AllowedSkus []string `json:"allowedSkus,omitempty"`

	// OIDCGroups lists identity-provider groups that get Kubernetes access to the tenant
	// namespace with the role oidcGroupRole (default member). Requires the API server to
	// accept the group claim (--oidc-groups-claim); the gateway resolves tenants separately.
	OIDCGroups []string `json:"oidcGroups,omitempty"`

	// OIDCGroupRole is the tenant role given to oidcGroups: viewer, member or admin
	// +kubebuilder:validation:Enum=viewer;member;admin
	OIDCGroupRole string `json:"oidcGroupRole,omitempty"`
}

// TenantMember defines a tenant member
type TenantMember struct {
	// Username of the member
	Username string `json:"username"`

	// Role of the member (admin, member, viewer)
	Role string `json:"role,omitempty"`

	// JoinedAt is when the member joined
	JoinedAt *metav1.Time `json:"joinedAt,omitempty"`
}

// TenantQuotas defines resource quotas
type TenantQuotas struct {
	// GPUHours defines GPU hours quota
	GPUHours *TenantGPUHoursQuota `json:"gpuHours,omitempty"`

	// CostUSD defines cost quota
	CostUSD *TenantCostQuota `json:"costUSD,omitempty"`

	// ConcurrentJobs is the max concurrent jobs
	ConcurrentJobs int `json:"concurrentJobs,omitempty"`

	// ConcurrentGPUs is the max concurrent GPUs
	ConcurrentGPUs int `json:"concurrentGPUs,omitempty"`

	// PerGPUType is per-GPU-type quotas
	PerGPUType map[string]TenantGPUTypeQuota `json:"perGPUType,omitempty"`

	// FairShareWeight is the tenant's weight in Kueue fair sharing (default 1). A tenant with
	// weight 3 is entitled to three times the contended capacity of a tenant with weight 1.
	// Applied only when the quota operator runs with --kueue-fair-sharing.
	// +kubebuilder:validation:Minimum=1
	FairShareWeight int `json:"fairShareWeight,omitempty"`

	// BorrowingLimit caps how many GPUs the tenant may borrow from the cohort above its quota.
	// Unset means no cap.
	// +kubebuilder:validation:Minimum=0
	BorrowingLimit *int `json:"borrowingLimit,omitempty"`

	// LendingLimit caps how many of the tenant's own GPUs other tenants may borrow. Unset means
	// all idle quota is lendable.
	// +kubebuilder:validation:Minimum=0
	LendingLimit *int `json:"lendingLimit,omitempty"`

	// Cohort places the tenant's queue in a named cohort instead of the default "gryvia", so
	// borrowing happens only among tenants sharing the name (for example one cohort per org).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Cohort string `json:"cohort,omitempty"`
}

// TenantGPUHoursQuota defines GPU hours quota
type TenantGPUHoursQuota struct {
	// Monthly GPU hours limit
	Monthly float64 `json:"monthly,omitempty"`

	// Burst capacity above quota
	Burst float64 `json:"burst,omitempty"`
}

// TenantCostQuota defines cost quota
type TenantCostQuota struct {
	// Monthly cost limit in USD
	Monthly float64 `json:"monthly,omitempty"`

	// Burst capacity above quota
	Burst float64 `json:"burst,omitempty"`
}

// TenantGPUTypeQuota defines per-GPU-type quota
type TenantGPUTypeQuota struct {
	// Count is the max concurrent GPUs of this type
	Count int `json:"count,omitempty"`

	// Hours is the max hours per month for this type
	Hours float64 `json:"hours,omitempty"`
}

// TenantPriorities defines priority settings
type TenantPriorities struct {
	// Default priority for this tenant
	Default string `json:"default,omitempty"`

	// AllowHighPriority allows high priority jobs
	AllowHighPriority bool `json:"allowHighPriority,omitempty"`

	// PreemptionAllowed allows preemption of this tenant's jobs
	PreemptionAllowed bool `json:"preemptionAllowed,omitempty"`
}

// TenantNetworkPolicy defines network isolation
type TenantNetworkPolicy struct {
	// Isolated isolates this tenant from others
	Isolated bool `json:"isolated,omitempty"`

	// AllowedTenants lists tenants allowed to communicate with
	AllowedTenants []string `json:"allowedTenants,omitempty"`
}

// TenantStorageQuotas defines storage limits
type TenantStorageQuotas struct {
	// Home directory quota
	Home string `json:"home,omitempty"`

	// Scratch storage quota
	Scratch string `json:"scratch,omitempty"`

	// Datasets storage quota
	Datasets string `json:"datasets,omitempty"`

	// Models storage quota
	Models string `json:"models,omitempty"`
}

// TenantJobDefaults defines default job settings
type TenantJobDefaults struct {
	// Image is the default container image
	Image string `json:"image,omitempty"`

	// PriorityClass is the default priority class
	PriorityClass string `json:"priorityClass,omitempty"`

	// Checkpointing enables checkpointing by default
	Checkpointing bool `json:"checkpointing,omitempty"`

	// RetryPolicy is the default retry policy
	RetryPolicy string `json:"retryPolicy,omitempty"`
}

// TenantBilling defines billing configuration
type TenantBilling struct {
	// CostCenter identifier
	CostCenter string `json:"costCenter,omitempty"`

	// BillingContact email
	BillingContact string `json:"billingContact,omitempty"`

	// PurchaseOrder number
	PurchaseOrder string `json:"purchaseOrder,omitempty"`

	// PaymentMethod (credit, invoice, prepaid)
	PaymentMethod string `json:"paymentMethod,omitempty"`
}

// TenantNotifications defines notification settings
type TenantNotifications struct {
	// Email notification recipients
	Email []string `json:"email,omitempty"`

	// Slack notification config
	Slack *TenantSlackConfig `json:"slack,omitempty"`
}

// TenantSlackConfig defines Slack notification settings
type TenantSlackConfig struct {
	// Channel is the Slack channel
	Channel string `json:"channel,omitempty"`

	// Webhook is the Slack webhook URL
	Webhook string `json:"webhook,omitempty"`
}

// TenantGovernance defines compliance and governance
type TenantGovernance struct {
	// DataClassification level (public, internal, confidential, restricted)
	DataClassification string `json:"dataClassification,omitempty"`

	// ComplianceRequirements (HIPAA, SOC2, GDPR, FedRAMP)
	ComplianceRequirements []string `json:"complianceRequirements,omitempty"`

	// AuditRetention defines how long to retain audit logs
	AuditRetention string `json:"auditRetention,omitempty"`
}

// GryviaTenantStatus defines the observed state of GryviaTenant
type GryviaTenantStatus struct {
	// MemberCount is the number of members
	MemberCount int `json:"memberCount,omitempty"`

	// CurrentUsage tracks current resource usage
	CurrentUsage *TenantCurrentUsage `json:"currentUsage,omitempty"`

	// QuotaUtilization tracks utilization percentages
	QuotaUtilization *TenantQuotaUtilization `json:"quotaUtilization,omitempty"`

	// Health is the tenant health status (healthy, warning, quota-exceeded)
	Health string `json:"health,omitempty"`

	// NamespacesManaged lists managed namespaces
	NamespacesManaged []string `json:"namespacesManaged,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// TenantCurrentUsage tracks current usage
type TenantCurrentUsage struct {
	// GPUHours consumed this month
	GPUHours float64 `json:"gpuHours,omitempty"`

	// CostUSD spent this month
	CostUSD float64 `json:"costUSD,omitempty"`

	// RunningJobs is the number of running jobs
	RunningJobs int `json:"runningJobs,omitempty"`

	// UsedGPUs is the number of GPUs in use
	UsedGPUs int `json:"usedGPUs,omitempty"`
}

// TenantQuotaUtilization tracks utilization percentages
type TenantQuotaUtilization struct {
	// GPUHoursPercent is GPU hours utilization
	GPUHoursPercent float64 `json:"gpuHoursPercent,omitempty"`

	// CostPercent is cost utilization
	CostPercent float64 `json:"costPercent,omitempty"`

	// JobsPercent is jobs utilization
	JobsPercent float64 `json:"jobsPercent,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

// GryviaTenant is the Schema for the gryviatenants API
type GryviaTenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaTenantSpec   `json:"spec,omitempty"`
	Status GryviaTenantStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaTenantList contains a list of GryviaTenant
type GryviaTenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaTenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaTenant{}, &GryviaTenantList{})
}
