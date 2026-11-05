package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaQuotaSpec defines the desired state of GryviaQuota
type GryviaQuotaSpec struct {
	// Team is the name of the team this quota applies to
	Team string `json:"team"`

	// Namespaces that this quota applies to
	Namespaces []string `json:"namespaces"`

	// GPUQuota defines GPU allocation limits
	GPUQuota GPUQuotaSpec `json:"gpuQuota"`

	// Budget defines cost limits (optional)
	Budget *BudgetSpec `json:"budget,omitempty"`

	// Priority for this team (1-100, higher is more important)
	Priority int `json:"priority,omitempty"`

	// Network defines optional network limits (optional)
	Network *NetworkSpec `json:"network,omitempty"`

	// TokensPerDay caps the LLM gateway tokens (input plus output) requested with keys of the listed namespaces
	// per UTC day; further requests get HTTP 429. Enforced by the LLM gateway, not by this operator.
	// +kubebuilder:validation:Minimum=1
	TokensPerDay *int64 `json:"tokensPerDay,omitempty"`
}

// NetworkSpec defines optional network limits. Nothing reads them unless the
// per-node eBPF collector runs with -quota-pace, -cgroup-path and
// -quota-pace-sync (all off by default).
type NetworkSpec struct {
	// MaxEgressMbps caps the outbound TCP rate of each new connection opened by
	// pods in the listed namespaces, in megabits per second. It is enforced by the
	// eBPF collector (SO_MAX_PACING_RATE at connect time), only when that opt-in
	// is enabled; it is not a bandwidth guarantee and does not limit UDP, RDMA or
	// connections that already exist. Omit it for no cap.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32000
	MaxEgressMbps *int `json:"maxEgressMbps,omitempty"`
}

// GPUQuotaSpec defines GPU resource limits
type GPUQuotaSpec struct {
	// MaxGPUs is the maximum number of GPUs this team can use
	MaxGPUs int `json:"maxGPUs"`

	// MaxGPUsPerJob limits GPUs per individual job
	MaxGPUsPerJob int `json:"maxGPUsPerJob,omitempty"`

	// AllowedGPUTypes restricts which GPU models can be used
	AllowedGPUTypes []string `json:"allowedGPUTypes,omitempty"`

	// MaxRunningJobs limits concurrent jobs
	MaxRunningJobs int `json:"maxRunningJobs,omitempty"`

	// MaxQueuedJobs limits queued jobs
	MaxQueuedJobs int `json:"maxQueuedJobs,omitempty"`
}

// BudgetSpec defines cost limits
type BudgetSpec struct {
	// MonthlyBudget in USD
	MonthlyBudget float64 `json:"monthlyBudget"`

	// AlertThreshold sends notification at this percentage (0-100)
	AlertThreshold float64 `json:"alertThreshold,omitempty"`

	// HardLimit stops new jobs when budget is exceeded
	HardLimit bool `json:"hardLimit,omitempty"`
}

// GryviaQuotaStatus defines the observed state of GryviaQuota
type GryviaQuotaStatus struct {
	// Phase is the current phase (Active, BudgetExceeded, Suspended)
	Phase string `json:"phase,omitempty"`

	// CurrentUsage tracks current resource consumption
	CurrentUsage QuotaUsage `json:"currentUsage,omitempty"`

	// BudgetStatus tracks spending
	BudgetStatus *BudgetStatus `json:"budgetStatus,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastUpdated is the timestamp of last status update
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`
}

// QuotaUsage tracks current resource consumption
type QuotaUsage struct {
	// AllocatedGPUs is the current number of GPUs in use
	AllocatedGPUs int `json:"allocatedGPUs"`

	// RunningJobs is the current number of running jobs
	RunningJobs int `json:"runningJobs"`

	// QueuedJobs is the current number of queued jobs
	QueuedJobs int `json:"queuedJobs"`

	// GPUHours consumed this month
	GPUHours float64 `json:"gpuHours"`
}

// BudgetStatus tracks spending
type BudgetStatus struct {
	// SpentThisMonth in USD
	SpentThisMonth float64 `json:"spentThisMonth"`

	// RemainingBudget in USD
	RemainingBudget float64 `json:"remainingBudget"`

	// PercentUsed of monthly budget (0-100)
	PercentUsed float64 `json:"percentUsed"`

	// ProjectedSpend for the month based on current usage
	ProjectedSpend float64 `json:"projectedSpend"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

// GryviaQuota is the Schema for the gryviaquotas API
type GryviaQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaQuotaSpec   `json:"spec,omitempty"`
	Status GryviaQuotaStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaQuotaList contains a list of GryviaQuota
type GryviaQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaQuota `json:"items"`
}

// GryviaAIJobSpec is a minimal struct for quota tracking and cost prediction.
// Go JSON unmarshaling ignores unknown fields by default, so the full
// GryviaAIJob spec from the API server deserializes correctly into this type.
type GryviaAIJobSpec struct {
	GPUs    int32  `json:"gpus"`
	GpuType string `json:"gpuType,omitempty"`

	// MigProfile asks for MIG slices instead of whole GPUs (mirrors the AI operator field).
	MigProfile string `json:"migProfile,omitempty"`

	// Additional fields used by the cost predictor
	Model          string                      `json:"model,omitempty"`
	StorageRequest string                      `json:"storageRequest,omitempty"`
	Network        string                      `json:"network,omitempty"`
	Priority       int32                       `json:"priority,omitempty"`
	Distributed    *GryviaAIJobDistributedSpec `json:"distributed,omitempty"`
}

// GryviaAIJobDistributedSpec is a minimal distributed config used by the predictor
type GryviaAIJobDistributedSpec struct {
	Enabled     bool   `json:"enabled,omitempty"`
	Framework   string `json:"framework,omitempty"`
	Nodes       int32  `json:"nodes,omitempty"`
	GpusPerNode int32  `json:"gpusPerNode,omitempty"`
	Backend     string `json:"backend,omitempty"`
}

// GryviaAIJobStatus is a minimal struct for quota tracking and cost prediction.
// Go JSON unmarshaling ignores unknown fields by default. NEVER write it back with
// Status().Update: that drops every field it does not declare (gpusAllocated, replicasReady,
// placementExplanation, metrics, ...). Use patchJobPhase (a merge patch) instead.
type GryviaAIJobStatus struct {
	Phase          string             `json:"phase,omitempty"`
	StartTime      *metav1.Time       `json:"startTime,omitempty"`
	CompletionTime *metav1.Time       `json:"completionTime,omitempty"`
	Conditions     []metav1.Condition `json:"conditions,omitempty"`
	NodesAllocated []string           `json:"nodesAllocated,omitempty"`
	Retries        int32              `json:"retries,omitempty"`
	Message        string             `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Namespaced

// GryviaAIJob is a minimal duplicate of the AI operator's GryviaAIJob type,
// used by the quota operator to track AI job resources. It intentionally omits
// fields not needed for quota enforcement. Go JSON unmarshaling ignores
// unknown fields by default, so the full resource deserializes correctly.
type GryviaAIJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaAIJobSpec   `json:"spec,omitempty"`
	Status GryviaAIJobStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaAIJobList contains a list of GryviaAIJob
type GryviaAIJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaAIJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaQuota{}, &GryviaQuotaList{}, &GryviaAIJob{}, &GryviaAIJobList{})
}
