// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// =============================================================================
// GryviaFlowPolicy - Intent-based network policy
// =============================================================================

// GryviaFlowPolicySpec defines the desired state of GryviaFlowPolicy
type GryviaFlowPolicySpec struct {
	// Source defines the traffic source selector
	Source *FlowEndpoint `json:"source,omitempty"`

	// Destination defines the traffic destination selector
	Destination *FlowEndpoint `json:"destination,omitempty"`

	// Protocol is the network protocol (tcp, udp, icmp, any)
	Protocol string `json:"protocol,omitempty"`

	// Action specifies what to do with matching traffic (allow, deny, log)
	Action string `json:"action,omitempty"`

	// Intent describes the desired network behavior (low-latency, high-throughput, secure, default)
	Intent string `json:"intent,omitempty"`

	// Priority determines policy evaluation order (0-1000, lower is higher priority)
	Priority int `json:"priority,omitempty"`
}

// FlowEndpoint identifies a network endpoint by service, namespace, or labels
type FlowEndpoint struct {
	// Service is the Kubernetes service name
	Service string `json:"service,omitempty"`

	// Namespace is the Kubernetes namespace
	Namespace string `json:"namespace,omitempty"`

	// Port is the network port (destination only)
	Port int `json:"port,omitempty"`

	// Labels are key-value selectors for pod matching
	Labels map[string]string `json:"labels,omitempty"`
}

// GryviaFlowPolicyStatus defines the observed state of GryviaFlowPolicy
type GryviaFlowPolicyStatus struct {
	// Phase is the current reconciliation phase
	Phase string `json:"phase,omitempty"`

	// CiliumPolicyRef is the name of the generated CiliumNetworkPolicy
	CiliumPolicyRef string `json:"ciliumPolicyRef,omitempty"`

	// Enforced indicates whether the policy is actively enforced
	Enforced bool `json:"enforced,omitempty"`

	// LastApplied is when the policy was last applied
	LastApplied metav1.Time `json:"lastApplied,omitempty"`

	// MatchedFlows is the number of flows matching this policy
	MatchedFlows int64 `json:"matchedFlows,omitempty"`

	// Conditions report whether the data sources behind this status are reachable. The condition
	// SourceAvailable is False (with a reason and message) when the collector, Netra or the kube
	// objects this status is derived from are not available, so an empty status is never read as
	// "nothing happened".
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GryviaFlowPolicy is the Schema for the gryviaflowpolicies API
type GryviaFlowPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaFlowPolicySpec   `json:"spec,omitempty"`
	Status GryviaFlowPolicyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaFlowPolicyList contains a list of GryviaFlowPolicy
type GryviaFlowPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaFlowPolicy `json:"items"`
}

// =============================================================================
// GryviaTrafficInsight - Real-time traffic analysis
// =============================================================================

// GryviaTrafficInsightSpec defines the desired state of GryviaTrafficInsight
type GryviaTrafficInsightSpec struct {
	// Service is the target service to analyze
	Service string `json:"service,omitempty"`

	// Namespace is the namespace of the target service
	Namespace string `json:"namespace,omitempty"`

	// Window is the duration window for traffic analysis (e.g., 5m, 1h)
	Window string `json:"window,omitempty"`

	// Metrics is the list of metrics to collect (latency, throughput, drops, retransmits)
	Metrics []string `json:"metrics,omitempty"`
}

// TopTalker represents a service with high traffic volume
type TopTalker struct {
	// Service is the name of the communicating service
	Service string `json:"service,omitempty"`

	// Bytes is the total bytes transferred
	Bytes int64 `json:"bytes,omitempty"`

	// Latency is the average latency to this service
	Latency string `json:"latency,omitempty"`
}

// TrafficAnomaly represents a detected traffic anomaly
type TrafficAnomaly struct {
	// Type is the anomaly classification
	Type string `json:"type,omitempty"`

	// Severity is the anomaly severity level
	Severity string `json:"severity,omitempty"`

	// Description provides human-readable details
	Description string `json:"description,omitempty"`

	// Detected is when the anomaly was first observed
	Detected metav1.Time `json:"detected,omitempty"`
}

// GryviaTrafficInsightStatus defines the observed state of GryviaTrafficInsight
type GryviaTrafficInsightStatus struct {
	// P50Latency is the 50th percentile latency
	P50Latency string `json:"p50Latency,omitempty"`

	// P99Latency is the 99th percentile latency
	P99Latency string `json:"p99Latency,omitempty"`

	// ThroughputBps is the current throughput in bytes per second
	ThroughputBps int64 `json:"throughputBps,omitempty"`

	// DropCount is the number of dropped packets
	DropCount int64 `json:"dropCount,omitempty"`

	// TopTalkers lists services with the most traffic
	TopTalkers []TopTalker `json:"topTalkers,omitempty"`

	// Anomalies lists detected traffic anomalies
	Anomalies []TrafficAnomaly `json:"anomalies,omitempty"`

	// LastUpdated is the timestamp of the last metrics update
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	// Conditions report whether the data sources behind this status are reachable. The condition
	// SourceAvailable is False (with a reason and message) when the collector, Netra or the kube
	// objects this status is derived from are not available, so an empty status is never read as
	// "nothing happened".
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GryviaTrafficInsight is the Schema for the gryviatrafficinsights API
type GryviaTrafficInsight struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaTrafficInsightSpec   `json:"spec,omitempty"`
	Status GryviaTrafficInsightStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaTrafficInsightList contains a list of GryviaTrafficInsight
type GryviaTrafficInsightList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaTrafficInsight `json:"items"`
}

// =============================================================================
// GryviaAutoPolicy - Self-healing firewall / auto-policy generation
// =============================================================================

// GryviaAutoPolicySpec defines the desired state of GryviaAutoPolicy
type GryviaAutoPolicySpec struct {
	// Mode determines the operational mode (learn, suggest, enforce)
	Mode string `json:"mode,omitempty"`

	// LearningWindow is the duration for the learning phase (e.g., 24h, 7d)
	LearningWindow string `json:"learningWindow,omitempty"`

	// TargetNamespaces is the list of namespaces to monitor
	TargetNamespaces []string `json:"targetNamespaces,omitempty"`

	// ExcludeServices is the list of services to exclude from policy generation
	ExcludeServices []string `json:"excludeServices,omitempty"`

	// ApprovalRequired indicates whether human approval is needed before enforcement
	ApprovalRequired bool `json:"approvalRequired,omitempty"`
}

// SuggestedPolicy represents a policy recommendation based on learned traffic
type SuggestedPolicy struct {
	// Source is the traffic source service
	Source string `json:"source,omitempty"`

	// Destination is the traffic destination service
	Destination string `json:"destination,omitempty"`

	// Port is the destination port
	Port int `json:"port,omitempty"`

	// Confidence is the confidence score (0.0 - 1.0)
	Confidence float64 `json:"confidence,omitempty"`
}

// GryviaAutoPolicyStatus defines the observed state of GryviaAutoPolicy
type GryviaAutoPolicyStatus struct {
	// Phase is the current operational phase (learning, suggesting, enforcing)
	Phase string `json:"phase,omitempty"`

	// LearnedPolicies is the number of traffic patterns learned
	LearnedPolicies int `json:"learnedPolicies,omitempty"`

	// SuggestedPolicies is the list of suggested network policies
	SuggestedPolicies []SuggestedPolicy `json:"suggestedPolicies,omitempty"`

	// AppliedPolicies is the number of policies that have been enforced
	AppliedPolicies int `json:"appliedPolicies,omitempty"`

	// LastLearned is the timestamp of the last learning cycle
	LastLearned metav1.Time `json:"lastLearned,omitempty"`

	// Conditions report whether the data sources behind this status are reachable. The condition
	// SourceAvailable is False (with a reason and message) when the collector, Netra or the kube
	// objects this status is derived from are not available, so an empty status is never read as
	// "nothing happened".
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GryviaAutoPolicy is the Schema for the gryviaautopolicies API
type GryviaAutoPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaAutoPolicySpec   `json:"spec,omitempty"`
	Status GryviaAutoPolicyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaAutoPolicyList contains a list of GryviaAutoPolicy
type GryviaAutoPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaAutoPolicy `json:"items"`
}

// =============================================================================
// GryviaTraceSession - Network trace/debug session
// =============================================================================

// GryviaTraceSessionSpec defines the desired state of GryviaTraceSession
type GryviaTraceSessionSpec struct {
	// Service is the target service to trace
	Service string `json:"service,omitempty"`

	// Namespace is the namespace of the target service
	Namespace string `json:"namespace,omitempty"`

	// Duration is the trace session lifetime (e.g., 5m, 30m)
	Duration string `json:"duration,omitempty"`

	// Level is the trace depth (l3, l4, l7)
	Level string `json:"level,omitempty"`

	// Filters define traffic filters for the trace session
	Filters *TraceFilters `json:"filters,omitempty"`

	// CaptureHeaders enables HTTP header capture (L7 only)
	CaptureHeaders bool `json:"captureHeaders,omitempty"`
}

// TraceFilters define packet matching criteria for trace sessions
type TraceFilters struct {
	// SrcIP filters by source IP address
	SrcIP string `json:"srcIP,omitempty"`

	// DstIP filters by destination IP address
	DstIP string `json:"dstIP,omitempty"`

	// Port filters by destination port
	Port int `json:"port,omitempty"`

	// Protocol filters by network protocol (tcp, udp, icmp, any)
	Protocol string `json:"protocol,omitempty"`
}

// TraceResultRef is a reference to stored trace data
type TraceResultRef struct {
	// Kind is the type of resource storing trace data (ConfigMap, PersistentVolumeClaim)
	Kind string `json:"kind,omitempty"`

	// Name is the name of the storage resource
	Name string `json:"name,omitempty"`

	// Namespace is the namespace of the storage resource
	Namespace string `json:"namespace,omitempty"`
}

// GryviaTraceSessionStatus defines the observed state of GryviaTraceSession
type GryviaTraceSessionStatus struct {
	// Phase is the current session phase (active, completed, expired)
	Phase string `json:"phase,omitempty"`

	// FlowsCaptured is the number of network flows captured
	FlowsCaptured int64 `json:"flowsCaptured,omitempty"`

	// StartTime is when the trace session began
	StartTime metav1.Time `json:"startTime,omitempty"`

	// EndTime is when the trace session ended
	EndTime metav1.Time `json:"endTime,omitempty"`

	// ResultRef references the stored trace data
	ResultRef *TraceResultRef `json:"resultRef,omitempty"`

	// Conditions report whether the data sources behind this status are reachable. The condition
	// SourceAvailable is False (with a reason and message) when the collector, Netra or the kube
	// objects this status is derived from are not available, so an empty status is never read as
	// "nothing happened".
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GryviaTraceSession is the Schema for the gryviatracesessions API
type GryviaTraceSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaTraceSessionSpec   `json:"spec,omitempty"`
	Status GryviaTraceSessionStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaTraceSessionList contains a list of GryviaTraceSession
type GryviaTraceSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaTraceSession `json:"items"`
}

// =============================================================================
// GryviaServiceGraph - Service dependency graph
// =============================================================================

// GryviaServiceGraphSpec defines the desired state of GryviaServiceGraph
type GryviaServiceGraphSpec struct {
	// Namespaces is the list of namespaces to include in the graph
	Namespaces []string `json:"namespaces,omitempty"`

	// RefreshInterval is the interval between graph refreshes (e.g., 30s, 5m)
	RefreshInterval string `json:"refreshInterval,omitempty"`

	// IncludeExternal includes traffic to/from external endpoints
	IncludeExternal bool `json:"includeExternal,omitempty"`

	// Depth is the maximum graph traversal depth (1-10)
	Depth int `json:"depth,omitempty"`
}

// ServiceGraphNode represents a service in the dependency graph
type ServiceGraphNode struct {
	// Name is the service name
	Name string `json:"name,omitempty"`

	// Namespace is the service namespace
	Namespace string `json:"namespace,omitempty"`

	// Type is the node type (service, deployment, external)
	Type string `json:"type,omitempty"`

	// Health is the service health status
	Health string `json:"health,omitempty"`
}

// ServiceGraphEdge represents a connection between services
type ServiceGraphEdge struct {
	// Source is the source service name
	Source string `json:"source,omitempty"`

	// Destination is the destination service name
	Destination string `json:"destination,omitempty"`

	// Protocol is the network protocol used
	Protocol string `json:"protocol,omitempty"`

	// Port is the destination port
	Port int `json:"port,omitempty"`

	// LatencyP99 is the 99th percentile latency
	LatencyP99 string `json:"latencyP99,omitempty"`

	// Throughput is the traffic throughput
	Throughput string `json:"throughput,omitempty"`

	// Verdict indicates the traffic verdict (forwarded, dropped, error). Empty when the source does not
	// report verdicts (the eBPF collector graph does not).
	Verdict string `json:"verdict,omitempty"`

	// LatencyP50 is the smoothed median latency of the edge as measured by the collector (a moving
	// average of observed flow latencies, not a percentile over a window)
	LatencyP50 string `json:"latencyP50,omitempty"`

	// BytesTotal is the number of bytes the collector counted on this edge since it started or
	// the edge was created (summed over nodes)
	BytesTotal int64 `json:"bytesTotal,omitempty"`

	// FlowCount is the number of flow events counted on this edge (summed over nodes)
	FlowCount int64 `json:"flowCount,omitempty"`
}

// GryviaServiceGraphStatus defines the observed state of GryviaServiceGraph
type GryviaServiceGraphStatus struct {
	// Nodes is the list of services in the graph
	Nodes []ServiceGraphNode `json:"nodes,omitempty"`

	// Edges is the list of connections between services
	Edges []ServiceGraphEdge `json:"edges,omitempty"`

	// LastUpdated is the timestamp of the last graph refresh
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	// Conditions report whether the data sources behind this status are reachable. The condition
	// SourceAvailable is False (with a reason and message) when the collector, Netra or the kube
	// objects this status is derived from are not available, so an empty status is never read as
	// "nothing happened".
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GryviaServiceGraph is the Schema for the gryviaservicegraphs API
type GryviaServiceGraph struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaServiceGraphSpec   `json:"spec,omitempty"`
	Status GryviaServiceGraphStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaServiceGraphList contains a list of GryviaServiceGraph
type GryviaServiceGraphList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaServiceGraph `json:"items"`
}

// =============================================================================
// GryviaNetworkAnomaly - Network anomaly detection
// =============================================================================

// GryviaNetworkAnomalySpec defines the desired state of GryviaNetworkAnomaly
type GryviaNetworkAnomalySpec struct {
	// TargetService is the service to monitor for anomalies
	TargetService string `json:"targetService,omitempty"`

	// DetectionRules define the anomaly detection thresholds
	DetectionRules []DetectionRule `json:"detectionRules,omitempty"`

	// AlertWebhook is the URL to send anomaly alerts to
	AlertWebhook string `json:"alertWebhook,omitempty"`

	// AutoMitigate enables automatic mitigation of detected anomalies
	AutoMitigate bool `json:"autoMitigate,omitempty"`
}

// DetectionRule defines a threshold-based anomaly detection rule
type DetectionRule struct {
	// Metric is the network metric to monitor (latency, throughput, connections, errors, drops)
	Metric string `json:"metric,omitempty"`

	// Operator is the comparison operator (gt, lt, gte, lte, eq)
	Operator string `json:"operator,omitempty"`

	// Threshold is the threshold value for the metric
	Threshold float64 `json:"threshold,omitempty"`

	// Window is the time window for metric evaluation (e.g., 5m, 1h)
	Window string `json:"window,omitempty"`
}

// NetworkAnomalyEvent represents a detected network anomaly
type NetworkAnomalyEvent struct {
	// Type is the anomaly classification
	Type string `json:"type,omitempty"`

	// Severity is the anomaly severity (low, medium, high, critical)
	Severity string `json:"severity,omitempty"`

	// Detected is when the anomaly was detected
	Detected metav1.Time `json:"detected,omitempty"`

	// Description provides human-readable details
	Description string `json:"description,omitempty"`

	// Mitigated indicates whether automatic mitigation was applied
	Mitigated bool `json:"mitigated,omitempty"`
}

// GryviaNetworkAnomalyStatus defines the observed state of GryviaNetworkAnomaly
type GryviaNetworkAnomalyStatus struct {
	// Anomalies is the list of detected anomalies
	Anomalies []NetworkAnomalyEvent `json:"anomalies,omitempty"`

	// LastCheck is the timestamp of the last anomaly check
	LastCheck metav1.Time `json:"lastCheck,omitempty"`

	// Conditions report whether the data sources behind this status are reachable. The condition
	// SourceAvailable is False (with a reason and message) when the collector, Netra or the kube
	// objects this status is derived from are not available, so an empty status is never read as
	// "nothing happened".
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GryviaNetworkAnomaly is the Schema for the gryvianetworkanomalies API
type GryviaNetworkAnomaly struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaNetworkAnomalySpec   `json:"spec,omitempty"`
	Status GryviaNetworkAnomalyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaNetworkAnomalyList contains a list of GryviaNetworkAnomaly
type GryviaNetworkAnomalyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaNetworkAnomaly `json:"items"`
}

// =============================================================================
// GryviaSecurityPolicy - eBPF-based security detection and enforcement
// =============================================================================

// SecurityDetectionRule defines a single security detection rule
type SecurityDetectionRule struct {
	// Type is the detection type (escape, mining, exfiltration, privesc, driver_fim)
	Type string `json:"type,omitempty"`

	// Enabled indicates whether this detection rule is active
	Enabled bool `json:"enabled,omitempty"`

	// Sensitivity controls the detection sensitivity (low, medium, high)
	Sensitivity string `json:"sensitivity,omitempty"`
}

// GryviaSecurityPolicySpec defines the desired state of GryviaSecurityPolicy
type GryviaSecurityPolicySpec struct {
	// TargetNamespaces is the list of namespaces to monitor
	TargetNamespaces []string `json:"targetNamespaces,omitempty"`

	// DetectionRules defines the security detection rules to apply
	DetectionRules []SecurityDetectionRule `json:"detectionRules,omitempty"`

	// AlertWebhook is the URL to send security alerts to
	AlertWebhook string `json:"alertWebhook,omitempty"`

	// AutoBlock is DEPRECATED and ignored: the operator never blocks anything on a security alert
	// (there is no enforcement path behind it). The field is kept so existing objects still validate.
	// A condition explains this when it is set to true.
	// +optional
	AutoBlock bool `json:"autoBlock,omitempty"`
}

// GryviaSecurityPolicyStatus defines the observed state of GryviaSecurityPolicy
type GryviaSecurityPolicyStatus struct {
	// Phase is the current reconciliation phase
	Phase string `json:"phase,omitempty"`

	// ActiveDetections is the number of active detection rules
	ActiveDetections int `json:"activeDetections,omitempty"`

	// AlertsTriggered is the total number of alerts triggered
	AlertsTriggered int `json:"alertsTriggered,omitempty"`

	// LastAlert is the timestamp of the last alert
	LastAlert metav1.Time `json:"lastAlert,omitempty"`

	// DetectionCounts maps detection type to count of triggered alerts
	DetectionCounts map[string]int `json:"detectionCounts,omitempty"`

	// Conditions report whether the data sources behind this status are reachable. The condition
	// SourceAvailable is False (with a reason and message) when the collector, Netra or the kube
	// objects this status is derived from are not available, so an empty status is never read as
	// "nothing happened".
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GryviaSecurityPolicy is the Schema for the gryviasecuritypolicies API
type GryviaSecurityPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaSecurityPolicySpec   `json:"spec,omitempty"`
	Status GryviaSecurityPolicyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaSecurityPolicyList contains a list of GryviaSecurityPolicy
type GryviaSecurityPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaSecurityPolicy `json:"items"`
}

// =============================================================================
// GryviaNetworkCost - Network cost tracking and reporting
// =============================================================================

// CostPerGB defines the cost rates for different traffic zones
type CostPerGB struct {
	// SameZone is the cost per GB for same-zone traffic
	SameZone float64 `json:"sameZone,omitempty"`

	// CrossZone is the cost per GB for cross-zone traffic
	CrossZone float64 `json:"crossZone,omitempty"`

	// InternetEgress is the cost per GB for internet egress traffic
	InternetEgress float64 `json:"internetEgress,omitempty"`
}

// CostCenterMapping maps a namespace to a team and cost center
type CostCenterMapping struct {
	// Namespace is the Kubernetes namespace
	Namespace string `json:"namespace,omitempty"`

	// Team is the team responsible for this namespace
	Team string `json:"team,omitempty"`

	// CostCenter is the cost center identifier
	CostCenter string `json:"costCenter,omitempty"`
}

// NetworkCostReport contains a periodic cost report for a namespace/team
type NetworkCostReport struct {
	// Period is the reporting period (e.g., "2024-01-15")
	Period string `json:"period,omitempty"`

	// Namespace is the Kubernetes namespace
	Namespace string `json:"namespace,omitempty"`

	// Team is the team responsible
	Team string `json:"team,omitempty"`

	// SameZoneBytes is the total bytes for same-zone traffic
	SameZoneBytes int64 `json:"sameZoneBytes,omitempty"`

	// CrossZoneBytes is the total bytes for cross-zone traffic
	CrossZoneBytes int64 `json:"crossZoneBytes,omitempty"`

	// ExternalBytes is the total bytes for external traffic
	ExternalBytes int64 `json:"externalBytes,omitempty"`

	// TotalCostUSD is the total cost in USD for this period
	TotalCostUSD float64 `json:"totalCostUSD,omitempty"`
}

// GryviaNetworkCostSpec defines the desired state of GryviaNetworkCost
type GryviaNetworkCostSpec struct {
	// TargetNamespaces is the list of namespaces to track costs for
	TargetNamespaces []string `json:"targetNamespaces,omitempty"`

	// CostPerGB defines the cost rates for different traffic zones
	CostPerGB CostPerGB `json:"costPerGB,omitempty"`

	// ReportingInterval is the interval between cost reports (e.g., "1h", "24h")
	ReportingInterval string `json:"reportingInterval,omitempty"`

	// CostCenters maps namespaces to teams and cost centers
	CostCenters []CostCenterMapping `json:"costCenters,omitempty"`
}

// GryviaNetworkCostStatus defines the observed state of GryviaNetworkCost
type GryviaNetworkCostStatus struct {
	// Phase is the current reconciliation phase
	Phase string `json:"phase,omitempty"`

	// Reports is the list of generated cost reports
	Reports []NetworkCostReport `json:"reports,omitempty"`

	// LastReport is the timestamp of the last cost report
	LastReport metav1.Time `json:"lastReport,omitempty"`

	// Conditions report whether the data sources behind this status are reachable. The condition
	// SourceAvailable is False (with a reason and message) when the collector, Netra or the kube
	// objects this status is derived from are not available, so an empty status is never read as
	// "nothing happened".
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GryviaNetworkCost is the Schema for the gryvianetworkcosts API
type GryviaNetworkCost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaNetworkCostSpec   `json:"spec,omitempty"`
	Status GryviaNetworkCostStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaNetworkCostList contains a list of GryviaNetworkCost
type GryviaNetworkCostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaNetworkCost `json:"items"`
}

// =============================================================================
// GryviaTrainingInsight - NCCL/training communication analysis
// =============================================================================

// RankStat contains per-rank communication statistics
type RankStat struct {
	// Rank is the distributed training rank
	Rank int `json:"rank,omitempty"`

	// AvgLatencyNs is the average communication latency in nanoseconds
	AvgLatencyNs int64 `json:"avgLatencyNs,omitempty"`

	// TotalBytes is the total bytes communicated by this rank
	TotalBytes int64 `json:"totalBytes,omitempty"`

	// IsStraggler indicates whether this rank is identified as a straggler
	IsStraggler bool `json:"isStraggler,omitempty"`
}

// StragglerInfo provides details about a detected straggler rank
type StragglerInfo struct {
	// Rank is the straggler rank
	Rank int `json:"rank,omitempty"`

	// SlowdownFactor is how much slower this rank is compared to the median
	SlowdownFactor float64 `json:"slowdownFactor,omitempty"`

	// Reason describes the suspected cause of the slowdown
	Reason string `json:"reason,omitempty"`
}

// GryviaTrainingInsightSpec defines the desired state of GryviaTrainingInsight
type GryviaTrainingInsightSpec struct {
	// TargetJob is the name of the GryviaAIJob to analyze
	TargetJob string `json:"targetJob,omitempty"`

	// AnalysisWindow is the time window for analysis (e.g., "5m", "1h")
	AnalysisWindow string `json:"analysisWindow,omitempty"`

	// Metrics is the list of analysis metrics to collect
	// Supported: collective_timing, straggler_detection, communication_ratio, pattern_analysis
	Metrics []string `json:"metrics,omitempty"`
}

// GryviaTrainingInsightStatus defines the observed state of GryviaTrainingInsight
type GryviaTrainingInsightStatus struct {
	// Phase is the current analysis phase
	Phase string `json:"phase,omitempty"`

	// RankStats contains per-rank communication statistics
	RankStats []RankStat `json:"rankStats,omitempty"`

	// CommPattern is the detected communication pattern (ring_allreduce, tree_allreduce, pipelined)
	CommPattern string `json:"commPattern,omitempty"`

	// CommComputeRatio is the ratio of communication time to compute time
	CommComputeRatio float64 `json:"commComputeRatio,omitempty"`

	// Stragglers lists detected straggler ranks
	Stragglers []StragglerInfo `json:"stragglers,omitempty"`

	// Bottleneck identifies the primary bottleneck (compute, communication, data_loading)
	Bottleneck string `json:"bottleneck,omitempty"`

	// SignalRef is the GryviaFabricSignal the figures below were read from
	SignalRef string `json:"signalRef,omitempty"`

	// NCCLP99Ms is the p99 latency in milliseconds of straggler-flagged NCCL collectives
	NCCLP99Ms float64 `json:"ncclP99ms,omitempty"`

	// CollectiveMaxSkewMs is the largest slowest-minus-fastest NCCL call duration among matched collectives
	CollectiveMaxSkewMs float64 `json:"collectiveMaxSkewMs,omitempty"`

	// OverlapIdleRatio is the fraction of the window the job spent in cudaDeviceSynchronize inside an in-flight allreduce
	OverlapIdleRatio float64 `json:"overlapIdleRatio,omitempty"`

	// RDMARetryRate is the ratio of RDMA retry/RNR error completions to posted sends
	RDMARetryRate float64 `json:"rdmaRetryRate,omitempty"`

	// ScoreDelta is the fabric penalty in [0,1] published by the collector (0 = healthy fabric)
	ScoreDelta float64 `json:"scoreDelta,omitempty"`

	// LastAnalysis is the timestamp of the last analysis
	LastAnalysis metav1.Time `json:"lastAnalysis,omitempty"`

	// Conditions report whether the data sources behind this status are reachable. The condition
	// SourceAvailable is False (with a reason and message) when the collector, Netra or the kube
	// objects this status is derived from are not available, so an empty status is never read as
	// "nothing happened".
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GryviaTrainingInsight is the Schema for the gryviatraininginsights API
type GryviaTrainingInsight struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaTrainingInsightSpec   `json:"spec,omitempty"`
	Status GryviaTrainingInsightStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaTrainingInsightList contains a list of GryviaTrainingInsight
type GryviaTrainingInsightList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaTrainingInsight `json:"items"`
}

// =============================================================================
// GryviaInferenceInsight - Inference latency breakdown analysis
// =============================================================================

// LatencyBreakdown provides per-phase latency measurements
type LatencyBreakdown struct {
	// DNSNs is the DNS resolution latency in nanoseconds
	DNSNs int64 `json:"dnsNs,omitempty"`

	// TCPConnectNs is the TCP connection establishment latency in nanoseconds
	TCPConnectNs int64 `json:"tcpConnectNs,omitempty"`

	// TLSHandshakeNs is the TLS handshake latency in nanoseconds
	TLSHandshakeNs int64 `json:"tlsHandshakeNs,omitempty"`

	// GPUQueueNs is the GPU queue waiting latency in nanoseconds
	GPUQueueNs int64 `json:"gpuQueueNs,omitempty"`

	// GPUExecNs is the GPU execution latency in nanoseconds
	GPUExecNs int64 `json:"gpuExecNs,omitempty"`

	// PostprocessNs is the post-processing latency in nanoseconds
	PostprocessNs int64 `json:"postprocessNs,omitempty"`

	// TotalNs is the total end-to-end latency in nanoseconds
	TotalNs int64 `json:"totalNs,omitempty"`
}

// GryviaInferenceInsightSpec defines the desired state of GryviaInferenceInsight
type GryviaInferenceInsightSpec struct {
	// TargetService is the name of the GryviaInferenceService to analyze
	TargetService string `json:"targetService,omitempty"`

	// AnalysisWindow is the time window for analysis (e.g., "5m", "1h")
	AnalysisWindow string `json:"analysisWindow,omitempty"`
}

// GryviaInferenceInsightStatus defines the observed state of GryviaInferenceInsight
type GryviaInferenceInsightStatus struct {
	// Phase is the current analysis phase
	Phase string `json:"phase,omitempty"`

	// LatencyBreakdown provides per-phase latency measurements
	LatencyBreakdown LatencyBreakdown `json:"latencyBreakdown,omitempty"`

	// P50TotalNs is the 50th percentile total latency in nanoseconds
	P50TotalNs int64 `json:"p50TotalNs,omitempty"`

	// P95TotalNs is the 95th percentile total latency in nanoseconds
	P95TotalNs int64 `json:"p95TotalNs,omitempty"`

	// P99TotalNs is the 99th percentile total latency in nanoseconds
	P99TotalNs int64 `json:"p99TotalNs,omitempty"`

	// Bottleneck identifies the phase with the highest latency contribution
	Bottleneck string `json:"bottleneck,omitempty"`

	// SignalRef is the GryviaFabricSignal the figures below were read from
	SignalRef string `json:"signalRef,omitempty"`

	// Engine names the serving engine whose own metrics fill the fields below (vllm, triton, tgi, mixed)
	Engine string `json:"engine,omitempty"`

	// TTFTP99Ms is the p99 time to first token in milliseconds (unset = not measured)
	TTFTP99Ms *float64 `json:"ttftP99ms,omitempty"`

	// ITLP99Ms is the p99 inter-token latency in milliseconds (unset = not measured)
	ITLP99Ms *float64 `json:"itlP99ms,omitempty"`

	// QueueTimeP99Ms is the p99 time in milliseconds a request waited in the engine queue (unset = not measured)
	QueueTimeP99Ms *float64 `json:"queueTimeP99ms,omitempty"`

	// E2EP99Ms is the p99 end-to-end latency in milliseconds inside the engine (unset = not measured)
	E2EP99Ms *float64 `json:"e2eP99ms,omitempty"`

	// InferWaitP99Ms is the p99 network wait in milliseconds from accept to first read on inference ports (eBPF)
	InferWaitP99Ms float64 `json:"inferWaitP99ms,omitempty"`

	// RequestsWaiting is the number of requests queued in the engine
	RequestsWaiting *int64 `json:"requestsWaiting,omitempty"`

	// KVCacheUsage is the KV cache usage in [0,1] (worst replica)
	KVCacheUsage *float64 `json:"kvCacheUsage,omitempty"`

	// LastAnalysis is the timestamp of the last analysis
	LastAnalysis metav1.Time `json:"lastAnalysis,omitempty"`

	// Conditions report whether the data sources behind this status are reachable. The condition
	// SourceAvailable is False (with a reason and message) when the collector, Netra or the kube
	// objects this status is derived from are not available, so an empty status is never read as
	// "nothing happened".
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GryviaInferenceInsight is the Schema for the gryviainferenceinsights API
type GryviaInferenceInsight struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaInferenceInsightSpec   `json:"spec,omitempty"`
	Status GryviaInferenceInsightStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaInferenceInsightList contains a list of GryviaInferenceInsight
type GryviaInferenceInsightList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaInferenceInsight `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&GryviaFlowPolicy{}, &GryviaFlowPolicyList{},
		&GryviaTrafficInsight{}, &GryviaTrafficInsightList{},
		&GryviaAutoPolicy{}, &GryviaAutoPolicyList{},
		&GryviaTraceSession{}, &GryviaTraceSessionList{},
		&GryviaServiceGraph{}, &GryviaServiceGraphList{},
		&GryviaNetworkAnomaly{}, &GryviaNetworkAnomalyList{},
		&GryviaSecurityPolicy{}, &GryviaSecurityPolicyList{},
		&GryviaNetworkCost{}, &GryviaNetworkCostList{},
		&GryviaTrainingInsight{}, &GryviaTrainingInsightList{},
		&GryviaInferenceInsight{}, &GryviaInferenceInsightList{},
	)
}
