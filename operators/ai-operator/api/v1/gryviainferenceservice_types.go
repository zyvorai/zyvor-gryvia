package v1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InferenceBackend defines the supported inference serving backends
type InferenceBackend string

const (
	BackendTriton      InferenceBackend = "triton"
	BackendVLLM        InferenceBackend = "vllm"
	BackendTensorRTLLM InferenceBackend = "tensorrt-llm"
	BackendTorchServe  InferenceBackend = "torchserve"
)

// Annotations the LLM gateway writes on a GryviaInferenceService it routes to (RFC 3339 times).
const (
	// AnnotationLastRequest is when the gateway last proxied a request to the service (written at most once a minute).
	AnnotationLastRequest = "gryvia.io/last-request"
	// AnnotationWakeRequested asks the controller to scale a service in phase ScaledToZero back up. The controller
	// removes it once it has acted on it.
	AnnotationWakeRequested = "gryvia.io/wake-requested"
)

// GryviaInferenceServiceSpec defines the desired state of GryviaInferenceService
type GryviaInferenceServiceSpec struct {
	// ModelRef is the name of the GryviaModelRegistry resource to serve
	ModelRef string `json:"modelRef"`

	// Backend is the inference backend (triton, vllm, tensorrt-llm, torchserve)
	Backend InferenceBackend `json:"backend"`

	// Replicas is the desired number of serving replicas
	Replicas int32 `json:"replicas,omitempty"`

	// GPUCount is the number of GPUs per replica
	GPUCount int32 `json:"gpuCount,omitempty"`

	// GPUType is the preferred GPU type for inference
	GPUType string `json:"gpuType,omitempty"`

	// Image overrides the default container image for the backend
	Image string `json:"image,omitempty"`

	// Args are additional arguments passed to the serving container
	Args []string `json:"args,omitempty"`

	// Autoscaling configures horizontal pod autoscaling
	Autoscaling *AutoscalingConfig `json:"autoscaling,omitempty"`

	// Canary configures canary deployment and traffic splitting
	Canary *CanaryConfig `json:"canary,omitempty"`

	// HealthCheck configures health checking and automatic rollback
	HealthCheck *HealthCheckConfig `json:"healthCheck,omitempty"`

	// ServicePort is the port the inference service listens on (default: 8080)
	ServicePort int32 `json:"servicePort,omitempty"`

	// ScaleToZero scales the service to zero replicas after a period without requests and back up when the LLM
	// gateway receives a request for it. Only traffic through the gateway counts and wakes it: callers of
	// status.endpoint get no activator. Not supported together with an enabled canary.
	ScaleToZero *ScaleToZeroConfig `json:"scaleToZero,omitempty"`

	// SLO keeps latency and error objectives by raising the HPA's minimum replicas while an objective is breached
	// and the engine is overloaded, and lowering it again after a run of healthy windows. Needs autoscaling
	// enabled and the operator's inference Prometheus URL; not used together with scale-to-zero.
	// +optional
	SLO *InferenceSLO `json:"slo,omitempty"`
}

// InferenceSLO is a service-level objective for the stable track. At least one objective is required.
type InferenceSLO struct {
	// Enabled turns SLO control on.
	Enabled bool `json:"enabled"`

	// MaxTTFTMilliseconds is the objective for the worst replica's p99 time to first token (engine metrics read by
	// the collector, gryvia_inference_latency_seconds{metric="ttft_p99"}).
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxTTFTMilliseconds int32 `json:"maxTTFTMilliseconds,omitempty"`

	// MaxInterTokenMilliseconds is the objective for the worst replica's p99 inter-token latency.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxInterTokenMilliseconds int32 `json:"maxInterTokenMilliseconds,omitempty"`

	// MaxErrorRate is the objective for the stable track's error ratio (serving sidecar counters), 0 to 1.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	MaxErrorRate *float64 `json:"maxErrorRate,omitempty"`

	// MinRequests in a window before it is evaluated. Default 100.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinRequests int32 `json:"minRequests,omitempty"`

	// WindowSeconds is the evaluation window and interval. Default 60.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=600
	// +optional
	WindowSeconds int32 `json:"windowSeconds,omitempty"`

	// ScaleUpStep is how many replicas the floor rises per breached, overloaded window. Default 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +optional
	ScaleUpStep int32 `json:"scaleUpStep,omitempty"`

	// ScaleDownAfterWindows is how many consecutive healthy windows lower the floor by one. Default 10.
	// +kubebuilder:validation:Minimum=3
	// +kubebuilder:validation:Maximum=1000
	// +optional
	ScaleDownAfterWindows int32 `json:"scaleDownAfterWindows,omitempty"`

	// MetricsJob is the job label the collector uses for this service's engine metrics. Default: the service name
	// (pods of an SLO-controlled service carry gryvia.io/job=<name>, which the collector's discovery uses).
	// +kubebuilder:validation:MaxLength=63
	// +optional
	MetricsJob string `json:"metricsJob,omitempty"`
}

// InferenceSLOStatus reports SLO control.
type InferenceSLOStatus struct {
	// State is Healthy, Breached, ScaledUp, ScaledDown, InsufficientTraffic or Unknown.
	State string `json:"state,omitempty"`
	// FloorReplicas is the HPA minimum the SLO controller currently sets (0 while it sets none).
	FloorReplicas int32 `json:"floorReplicas,omitempty"`
	// HealthyWindows is the run of consecutive healthy windows.
	HealthyWindows int32 `json:"healthyWindows,omitempty"`
	// Breaches counts breached windows since SLO control was enabled.
	Breaches int32 `json:"breaches,omitempty"`
	// LastEvaluated is when the last window was evaluated.
	LastEvaluated *metav1.Time `json:"lastEvaluated,omitempty"`
	// Message explains the last decision with the measured values.
	Message string `json:"message,omitempty"`
}

// ScaleToZeroConfig configures idle scale-down.
type ScaleToZeroConfig struct {
	// Enabled turns idle scale-down on.
	Enabled bool `json:"enabled"`

	// IdleSeconds without a request (or since the last wake-up) before the service is scaled to zero. Default 900.
	// +kubebuilder:validation:Minimum=60
	// +optional
	IdleSeconds int32 `json:"idleSeconds,omitempty"`

	// ColdStartTimeoutSeconds is how long the LLM gateway holds a request while the service starts, and how much
	// longer a service that is not ready may run idle before it is scaled to zero. Default 300.
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=3600
	// +optional
	ColdStartTimeoutSeconds int32 `json:"coldStartTimeoutSeconds,omitempty"`
}

// Defaults for ScaleToZeroConfig.
const (
	DefaultScaleToZeroIdleSeconds      = 900
	DefaultScaleToZeroColdStartSeconds = 300
)

// Idle is the idle period before scale-down.
func (c *ScaleToZeroConfig) Idle() time.Duration {
	if c == nil || c.IdleSeconds <= 0 {
		return DefaultScaleToZeroIdleSeconds * time.Second
	}
	return time.Duration(c.IdleSeconds) * time.Second
}

// ColdStart is how long a request may wait for a woken service.
func (c *ScaleToZeroConfig) ColdStart() time.Duration {
	if c == nil || c.ColdStartTimeoutSeconds <= 0 {
		return DefaultScaleToZeroColdStartSeconds * time.Second
	}
	return time.Duration(c.ColdStartTimeoutSeconds) * time.Second
}

// AutoscalingConfig defines HPA configuration for inference services
type AutoscalingConfig struct {
	// Enabled turns on horizontal pod autoscaling
	Enabled bool `json:"enabled"`

	// MinReplicas is the minimum number of replicas
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the maximum number of replicas
	MaxReplicas int32 `json:"maxReplicas"`

	// TargetGPUUtilization is the target GPU utilization percentage for scaling
	TargetGPUUtilization int32 `json:"targetGPUUtilization,omitempty"`

	// TargetRequestsPerSecond is the target request rate per replica for scaling
	TargetRequestsPerSecond int32 `json:"targetRequestsPerSecond,omitempty"`
}

// CanaryConfig defines canary deployment settings
type CanaryConfig struct {
	// Enabled turns on canary deployment
	Enabled bool `json:"enabled"`

	// Weight is the percentage of traffic routed to the canary (0-100)
	Weight int32 `json:"weight"`

	// ModelVersion is the model version (GryviaModelRegistry name) to deploy as canary
	ModelVersion string `json:"modelVersion"`

	// AutoPromote automatically promotes canary to primary if health checks pass
	AutoPromote bool `json:"autoPromote,omitempty"`

	// PromoteAfterSeconds is the duration in seconds to wait before auto-promoting
	PromoteAfterSeconds int64 `json:"promoteAfterSeconds,omitempty"`
}

// HealthCheckConfig defines health checking for inference services
type HealthCheckConfig struct {
	// Path is the HTTP health check path (default: /health)
	Path string `json:"path,omitempty"`

	// IntervalSeconds is the interval between health checks
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// FailureThreshold is the number of consecutive failures before marking unhealthy
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// AutoRollback enables automatic rollback to the previous version on failure
	AutoRollback bool `json:"autoRollback,omitempty"`
}

// GryviaInferenceServiceStatus defines the observed state of GryviaInferenceService
type GryviaInferenceServiceStatus struct {
	// Phase is the current phase (Pending, Deploying, Ready, ScaledToZero, Failed, RollingBack)
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Endpoint is the URL at which the inference service can be reached
	Endpoint string `json:"endpoint,omitempty"`

	// ReadyReplicas is the number of ready serving replicas
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// CanaryStatus holds the status of the canary deployment
	CanaryStatus *CanaryStatus `json:"canaryStatus,omitempty"`

	// DeploymentName is the name of the Kubernetes Deployment
	DeploymentName string `json:"deploymentName,omitempty"`

	// ServiceName is the name of the Kubernetes Service
	ServiceName string `json:"serviceName,omitempty"`

	// LastHealthCheck is the timestamp of the last health check
	LastHealthCheck *metav1.Time `json:"lastHealthCheck,omitempty"`

	// HealthStatus is the current health (Healthy, Unhealthy, Unknown)
	HealthStatus string `json:"healthStatus,omitempty"`

	// ConsecutiveFailures is the number of consecutive health check failures
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// Message provides additional status information
	Message string `json:"message,omitempty"`

	// ScaledToZeroAt is when the service was scaled to zero for being idle; unset while it runs.
	ScaledToZeroAt *metav1.Time `json:"scaledToZeroAt,omitempty"`

	// LastWakeAt is when the service was last woken (scaled up from zero, or a wake request while running), or when
	// scale-to-zero was turned on; the idle period is counted from it or from the last request, whichever is later.
	LastWakeAt *metav1.Time `json:"lastWakeAt,omitempty"`

	// SLO reports SLO control (spec.slo).
	SLO *InferenceSLOStatus `json:"slo,omitempty"`
}

// CanaryStatus holds the observed state of a canary deployment
type CanaryStatus struct {
	// Active indicates whether a canary is currently deployed
	Active bool `json:"active"`

	// Weight is the current traffic weight for the canary
	Weight int32 `json:"weight,omitempty"`

	// DeploymentName is the name of the canary Deployment
	DeploymentName string `json:"deploymentName,omitempty"`

	// ReadyReplicas is the number of ready canary replicas
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Health is the canary health status
	Health string `json:"health,omitempty"`

	// StartedAt is when the canary deployment started
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="ModelRef",type=string,JSONPath=`.spec.modelRef`
//+kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backend`
//+kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.readyReplicas`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaInferenceService is the Schema for the gryviainferenceservices API
type GryviaInferenceService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaInferenceServiceSpec   `json:"spec,omitempty"`
	Status GryviaInferenceServiceStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaInferenceServiceList contains a list of GryviaInferenceService
type GryviaInferenceServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaInferenceService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaInferenceService{}, &GryviaInferenceServiceList{})
}
