package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaFederationSpec defines the desired state of GryviaFederation
type GryviaFederationSpec struct {
	// Clusters defines member clusters
	Clusters []FederationCluster `json:"clusters,omitempty"`

	// Distribution defines job distribution policy
	Distribution *FederationDistribution `json:"distribution,omitempty"`

	// Failover defines failover configuration
	Failover *FederationFailover `json:"failover,omitempty"`

	// LoadBalancing defines load balancing policy
	LoadBalancing *FederationLoadBalancing `json:"loadBalancing,omitempty"`

	// ResourceSharing defines resource sharing policy
	ResourceSharing *FederationResourceSharing `json:"resourceSharing,omitempty"`

	// CostManagement defines cost management policy
	CostManagement *FederationCostManagement `json:"costManagement,omitempty"`
}

// FederationCluster defines a member cluster
type FederationCluster struct {
	// Name of the cluster
	Name string `json:"name"`

	// Region of the cluster
	Region string `json:"region,omitempty"`

	// APIServer URL
	APIServer string `json:"apiServer,omitempty"`

	// Credentials to access the cluster
	Credentials *FederationCredentials `json:"credentials,omitempty"`

	// Capacity of the cluster
	Capacity *FederationClusterCapacity `json:"capacity,omitempty"`

	// Pricing for this cluster
	Pricing *FederationPricing `json:"pricing,omitempty"`

	// Network connectivity info
	Network *FederationNetwork `json:"network,omitempty"`

	// Enabled indicates if the cluster is active
	Enabled bool `json:"enabled,omitempty"`

	// Priority for scheduling preference
	Priority int `json:"priority,omitempty"`

	// MultiKueueCluster is the name of the MultiKueueCluster on this (manager) cluster that dispatches to this
	// member; failover evicts the Kueue Workloads assigned to it. Defaults to Name.
	MultiKueueCluster string `json:"multiKueueCluster,omitempty"`
}

// FederationCredentials defines cluster credentials
type FederationCredentials struct {
	// SecretRef is the name of the secret containing credentials
	SecretRef string `json:"secretRef,omitempty"`
}

// FederationClusterCapacity defines cluster capacity
type FederationClusterCapacity struct {
	// GPUTypes available in this cluster
	GPUTypes []FederationGPUCapacity `json:"gpuTypes,omitempty"`
}

// FederationGPUCapacity defines GPU capacity per type
type FederationGPUCapacity struct {
	// Type of GPU
	Type string `json:"type,omitempty"`

	// Total GPUs of this type
	Total int `json:"total,omitempty"`

	// Available GPUs of this type
	Available int `json:"available,omitempty"`
}

// FederationPricing defines cluster pricing
type FederationPricing struct {
	// Currency for pricing
	Currency string `json:"currency,omitempty"`

	// GPUHourlyRates per GPU type
	GPUHourlyRates map[string]float64 `json:"gpuHourlyRates,omitempty"`
}

// FederationNetwork defines network connectivity
type FederationNetwork struct {
	// Latency to other clusters
	Latency string `json:"latency,omitempty"`

	// Bandwidth available
	Bandwidth string `json:"bandwidth,omitempty"`
}

// FederationDistribution defines job distribution
type FederationDistribution struct {
	// Strategy for job distribution (cost-optimized, latency-optimized, locality-preferred, round-robin, capacity-based)
	Strategy string `json:"strategy,omitempty"`

	// Affinity rules
	Affinity *FederationAffinity `json:"affinity,omitempty"`

	// Constraints on distribution
	Constraints *FederationConstraints `json:"constraints,omitempty"`
}

// FederationAffinity defines affinity rules
type FederationAffinity struct {
	// RegionAffinity lists preferred regions
	RegionAffinity []string `json:"regionAffinity,omitempty"`

	// DataLocality prefers cluster where data is located
	DataLocality bool `json:"dataLocality,omitempty"`
}

// FederationConstraints defines distribution constraints
type FederationConstraints struct {
	// MaxLatency between clusters
	MaxLatency string `json:"maxLatency,omitempty"`

	// DataSovereignty defines data sovereignty rules
	DataSovereignty *FederationDataSovereignty `json:"dataSovereignty,omitempty"`
}

// FederationDataSovereignty defines data sovereignty rules
type FederationDataSovereignty struct {
	// Enabled enables data sovereignty checks
	Enabled bool `json:"enabled,omitempty"`

	// AllowedRegions lists allowed data regions
	AllowedRegions []string `json:"allowedRegions,omitempty"`
}

// FederationFailover defines failover configuration
type FederationFailover struct {
	// Enabled enables failover
	Enabled bool `json:"enabled,omitempty"`

	// Automatic enables automatic failover
	Automatic bool `json:"automatic,omitempty"`

	// HealthCheck defines health check settings
	HealthCheck *FederationHealthCheck `json:"healthCheck,omitempty"`

	// LeaseTimeout is how long a member must have been failing before its workloads count as fenced when its API
	// server cannot be reached to delete them (Go duration, default 5m). Set it no shorter than the time after which
	// the member stops its own workloads on its own (for example a node lease or self-fencing timeout): until then
	// an unreachable member may still be running the job.
	LeaseTimeout string `json:"leaseTimeout,omitempty"`
}

// FederationHealthCheck defines health check settings
type FederationHealthCheck struct {
	// Interval between health checks
	Interval string `json:"interval,omitempty"`

	// Timeout for health checks
	Timeout string `json:"timeout,omitempty"`

	// FailureThreshold before marking unhealthy
	FailureThreshold int `json:"failureThreshold,omitempty"`
}

// FederationLoadBalancing defines load balancing
type FederationLoadBalancing struct {
	// Enabled enables load balancing
	Enabled bool `json:"enabled,omitempty"`

	// Algorithm for load balancing (round-robin, least-loaded, weighted, random)
	Algorithm string `json:"algorithm,omitempty"`

	// Weights per cluster
	Weights map[string]int `json:"weights,omitempty"`
}

// FederationResourceSharing defines resource sharing
type FederationResourceSharing struct {
	// Enabled enables resource sharing
	Enabled bool `json:"enabled,omitempty"`

	// BurstingEnabled allows bursting to other clusters
	BurstingEnabled bool `json:"burstingEnabled,omitempty"`

	// MaxSharedPercentage is the max percentage to share
	MaxSharedPercentage int `json:"maxSharedPercentage,omitempty"`
}

// FederationCostManagement defines cost management
type FederationCostManagement struct {
	// Enabled enables cost management
	Enabled bool `json:"enabled,omitempty"`

	// MaxCostPerHour limits cost
	MaxCostPerHour float64 `json:"maxCostPerHour,omitempty"`

	// PreferCheaper prefers cheaper clusters
	PreferCheaper bool `json:"preferCheaper,omitempty"`
}

// GryviaFederationStatus defines the observed state of GryviaFederation
type GryviaFederationStatus struct {
	// State is the overall federation state (healthy, degraded, unavailable)
	State string `json:"state,omitempty"`

	// ClusterStatus is per-cluster status
	ClusterStatus []FederationClusterStatus `json:"clusterStatus,omitempty"`

	// AggregateStats are aggregate statistics
	AggregateStats *FederationAggregateStats `json:"aggregateStats,omitempty"`

	// JobDistribution tracks job distribution across clusters
	JobDistribution map[string]int `json:"jobDistribution,omitempty"`

	// Failovers lists the most recent workload moves (newest last, at most 20).
	Failovers []FederationFailoverRecord `json:"failovers,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FederationClusterStatus tracks per-cluster status
type FederationClusterStatus struct {
	// Name of the cluster
	Name string `json:"name,omitempty"`

	// State of the cluster (healthy, unhealthy, unreachable)
	State string `json:"state,omitempty"`

	// LastHealthCheck timestamp
	LastHealthCheck *metav1.Time `json:"lastHealthCheck,omitempty"`

	// Utilization of the cluster
	Utilization *FederationUtilization `json:"utilization,omitempty"`

	// ConsecutiveFailures counts failed health checks in a row; reset by a healthy check.
	ConsecutiveFailures int `json:"consecutiveFailures,omitempty"`

	// UnhealthySince is the first failed check of the current failure streak.
	UnhealthySince *metav1.Time `json:"unhealthySince,omitempty"`

	// Fenced is true once the member's workloads were deleted remotely (FenceMethod RemoteDelete) or its lease
	// timeout expired (LeaseExpired); only then are its workloads evicted for redispatch. Cleared when it is healthy.
	Fenced      bool         `json:"fenced,omitempty"`
	FenceMethod string       `json:"fenceMethod,omitempty"`
	FencedAt    *metav1.Time `json:"fencedAt,omitempty"`
}

// FederationFailoverRecord is one Kueue Workload moved off a failed member.
type FederationFailoverRecord struct {
	// Cluster the workload was moved off.
	Cluster string `json:"cluster"`
	// Namespace and Workload name on the manager.
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	// FenceMethod used for the member (RemoteDelete or LeaseExpired).
	FenceMethod string `json:"fenceMethod,omitempty"`
	// EvictedAt is when the Workload was deactivated; RequeuedAt when it was reactivated for redispatch.
	EvictedAt  metav1.Time  `json:"evictedAt"`
	RequeuedAt *metav1.Time `json:"requeuedAt,omitempty"`
}

// FederationUtilization tracks cluster utilization
type FederationUtilization struct {
	// GPUs currently in use
	GPUs int `json:"gpus,omitempty"`

	// Percentage utilization
	Percentage float64 `json:"percentage,omitempty"`
}

// FederationAggregateStats are aggregate statistics
type FederationAggregateStats struct {
	// TotalGPUs across all clusters
	TotalGPUs int `json:"totalGPUs,omitempty"`

	// AvailableGPUs across all clusters
	AvailableGPUs int `json:"availableGPUs,omitempty"`

	// JobsRunning across all clusters
	JobsRunning int `json:"jobsRunning,omitempty"`

	// TotalCostPerHour across all clusters
	TotalCostPerHour float64 `json:"totalCostPerHour,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

// GryviaFederation is the Schema for the gryviafederations API
type GryviaFederation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaFederationSpec   `json:"spec,omitempty"`
	Status GryviaFederationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaFederationList contains a list of GryviaFederation
type GryviaFederationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaFederation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaFederation{}, &GryviaFederationList{})
}
