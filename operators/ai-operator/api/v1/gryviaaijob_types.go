package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaAIJobSpec defines the desired state of GryviaAIJob
type GryviaAIJobSpec struct {
	// Type of workload (training, inference, fine-tuning, evaluation)
	Type string `json:"type"`

	// Model name (llama-70b, gpt-4, stable-diffusion, etc.)
	Model string `json:"model,omitempty"`

	// Number of GPUs required per pod. 0 is allowed and means a CPU-only job: no
	// nvidia.com/gpu limit and no GPU node selector are added (used for CI on
	// clusters without GPUs).
	GPUs int32 `json:"gpus"`

	// Preferred GPU type (H100, A100, L40, V100, T4, any)
	GpuType string `json:"gpuType,omitempty"`

	// Storage backend name
	Storage string `json:"storage,omitempty"`

	// StorageRequest is the PVC size to request (e.g., "100Gi")
	StorageRequest string `json:"storageRequest,omitempty"`

	// Network type (rdma, sriov, standard)
	Network string `json:"network,omitempty"`

	// Container image
	Image string `json:"image"`

	// Image pull policy
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// Container command
	Command []string `json:"command,omitempty"`

	// Container arguments
	Args []string `json:"args,omitempty"`

	// Environment variables
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Working directory
	WorkingDir string `json:"workingDir,omitempty"`

	// Distributed training configuration
	Distributed *DistributedConfig `json:"distributed,omitempty"`

	// Resource requirements
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Volumes
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// Volume mounts
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`

	// Job priority (0-100, higher = more important)
	Priority int32 `json:"priority,omitempty"`

	// Number of retries on failure. For a batch Job this is backoffLimit: 0 fails the job
	// on the first failed pod. Not used by the StatefulSet workload.
	RetryLimit int32 `json:"retryLimit,omitempty"`

	// Job timeout (e.g., 90m, 24h, 7d). For a batch Job this is activeDeadlineSeconds: the
	// job is Failed (DeadlineExceeded) once it has been active this long. Not used by the
	// StatefulSet workload.
	Timeout string `json:"timeout,omitempty"`

	// Node selector
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// WorkloadKind selects the Kubernetes workload that runs the job: "job" (an
	// Indexed batch/v1 Job that runs to completion, so Succeeded and Failed are
	// reachable) or "statefulset" (pods restart forever and never complete).
	// Default: "job" for training, fine-tuning and evaluation, "statefulset" for
	// inference. A job that already has a "<name>-training" StatefulSet or a Job
	// keeps that workload whatever this says (workloads are never migrated).
	// +kubebuilder:validation:Enum=job;statefulset
	WorkloadKind string `json:"workloadKind,omitempty"`

	// Suspend creates the batch Job suspended (no pods run) and keeps it
	// suspended while true. Only applies to workloadKind "job". Default false.
	// The label kueue.x-k8s.io/queue-name on the GryviaAIJob is copied to the Job
	// and, when present, the controller leaves spec.suspend to Kueue after creation.
	Suspend bool `json:"suspend,omitempty"`

	// QueueName is the Kueue LocalQueue (same namespace) that admits this job. Only used
	// when the ai-operator runs with --kueue-integration; the annotation gryvia.io/queue-name
	// is accepted as an alternative. When set, the batch Job is created suspended with the
	// label kueue.x-k8s.io/queue-name and Kueue unsuspends it once ALL its pods fit the
	// queue's quota (gang admission). Empty in a tenant-* namespace means the default queue
	// ("gryvia") if that LocalQueue exists. Only applies to workloadKind "job".
	// See docs/kueue-integration.md.
	QueueName string `json:"queueName,omitempty"`
}

// DistributedConfig defines distributed training configuration
type DistributedConfig struct {
	// Enable distributed training
	Enabled bool `json:"enabled,omitempty"`

	// Framework (pytorch, tensorflow, horovod, deepspeed, megatron)
	Framework string `json:"framework,omitempty"`

	// Number of nodes
	Nodes int32 `json:"nodes,omitempty"`

	// GPUs per node
	GpusPerNode int32 `json:"gpusPerNode,omitempty"`

	// Backend (nccl, gloo, mpi)
	Backend string `json:"backend,omitempty"`

	// Elastic lets a PyTorch (torchrun / torchelastic) job run with fewer workers than requested.
	// Run-to-completion Jobs only. See docs/elastic-training.md for what this does and does not do.
	Elastic *ElasticConfig `json:"elastic,omitempty"`
}

// ElasticConfig sets the lower bound of an elastic job; the upper bound is distributed.nodes (so
// quota and admission, which count distributed.nodes, stay correct). The Indexed Job asks for
// distributed.nodes workers, the launcher gets NNODES=minNodes:nodes, and the Job is declared
// successful once minNodes indexes have succeeded.
type ElasticConfig struct {
	// MinNodes is the fewest workers the training can run with (at least 1, at most distributed.nodes).
	MinNodes int32 `json:"minNodes"`
}

// ElasticBounds returns the worker bounds and true when the job is elastic. max is Distributed.Nodes.
func (d *DistributedConfig) ElasticBounds() (min, max int32, ok bool) {
	if d == nil || !d.Enabled || d.Elastic == nil {
		return 0, 0, false
	}
	return d.Elastic.MinNodes, d.Nodes, true
}

// GryviaAIJobStatus defines the observed state of GryviaAIJob
type GryviaAIJobStatus struct {
	// Current phase (Pending, Queued, Scheduling, Running, Succeeded, Failed, Rejected, Preempted, Cancelled, Unknown).
	// See docs/aijob-lifecycle.md for the state machine.
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Start time
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// Completion time
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Nodes allocated
	NodesAllocated []string `json:"nodesAllocated,omitempty"`

	// GPUs allocated
	GpusAllocated int32 `json:"gpusAllocated,omitempty"`

	// Number of retries
	Retries int32 `json:"retries,omitempty"`

	// Metrics
	Metrics *JobMetrics `json:"metrics,omitempty"`

	// Ready replicas
	ReplicasReady int32 `json:"replicasReady,omitempty"`

	// Human-readable message indicating details about the current phase
	Message string `json:"message,omitempty"`

	// PlacementExplanation records, only when fabric-aware scheduling was applied,
	// how per-node fabric health changed the node ranking (bounded to the top nodes)
	// +kubebuilder:validation:MaxItems=16
	PlacementExplanation []PlacementExplanation `json:"placementExplanation,omitempty"`

	// PlacementTopology is the topology group the operator kept a multi-node job in ("gryvia.io/ib-block=b1"),
	// set only with topology placement on; pods get soft node affinity toward it.
	// +optional
	PlacementTopology string `json:"placementTopology,omitempty"`

	// Dataset records how the dataset named by the gryvia.io/dataset annotation was placed, decided once when the
	// job is scheduled.
	// +optional
	Dataset *AIJobDataset `json:"dataset,omitempty"`

	// Checkpoint is set when a GryviaCheckpointGuard manages this job's checkpoints: what the trainer last
	// reported as committed, and the pods replaced after node losses.
	// +optional
	Checkpoint *AIJobCheckpoint `json:"checkpoint,omitempty"`
}

// AIJobDataset is the dataset locality decision for a job.
type AIJobDataset struct {
	// Name of the GryviaDataset.
	Name string `json:"name"`
	// LocalPools are the dataset's pools holding a ready, verified replica; nodes in them are preferred.
	LocalPools []string `json:"localPools,omitempty"`
	// Pool, PVCName and SubPath locate the replica mounted read-only at /datasets/<name> (empty when not mounted).
	Pool    string `json:"pool,omitempty"`
	PVCName string `json:"pvcName,omitempty"`
	SubPath string `json:"subPath,omitempty"`
	// NodeSelectors of the local pools, used for the preferred node affinity.
	NodeSelectors []map[string]string `json:"nodeSelectors,omitempty"`
	// Message explains the decision (why the replica is not mounted, for example).
	Message string `json:"message,omitempty"`
}

// PlacementExplanation is one node's line of the fabric-aware placement decision.
type PlacementExplanation struct {
	// Node is the node name
	Node string `json:"node"`

	// BaseScore is the topology/capacity score before the fabric adjustment
	BaseScore int32 `json:"baseScore"`

	// FabricPenalty is the points subtracted for fabric health (0 = none)
	FabricPenalty int32 `json:"fabricPenalty"`

	// FinalScore is what the node was ranked by
	FinalScore int32 `json:"finalScore"`

	// Reasons are the fabric signals behind the penalty
	// +kubebuilder:validation:MaxItems=8
	Reasons []string `json:"reasons,omitempty"`

	// SignalAgeSeconds is how old the node's fabric measurement was when used
	SignalAgeSeconds int64 `json:"signalAgeSeconds,omitempty"`
}

// JobMetrics represents job performance metrics
type JobMetrics struct {
	// Average GPU utilization %
	GpuUtilization float64 `json:"gpuUtilization,omitempty"`

	// Training throughput
	Throughput string `json:"throughput,omitempty"`

	// Current loss value
	Loss float64 `json:"loss,omitempty"`

	// Current epoch
	Epoch int32 `json:"epoch,omitempty"`

	// Current step
	Step int32 `json:"step,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
//+kubebuilder:printcolumn:name="GPUs",type=integer,JSONPath=`.spec.gpus`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaAIJob is the Schema for the gryviaaijobs API
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

// StorageSize returns the storage request size, defaulting to 100Gi
func (s *GryviaAIJobSpec) StorageSize() string {
	if s.StorageRequest != "" {
		return s.StorageRequest
	}
	return "100Gi"
}

// EnvVarFromCoreV1 creates a corev1.EnvVar from a key-value pair.
func EnvVarFromCoreV1(name, value string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, Value: value}
}

func init() {
	SchemeBuilder.Register(&GryviaAIJob{}, &GryviaAIJobList{})
}

// AIJobCheckpoint is the guard-managed checkpoint state of a job.
type AIJobCheckpoint struct {
	// Guard is the GryviaCheckpointGuard whose policy the pods got.
	Guard     string `json:"guard"`
	Directory string `json:"directory,omitempty"`
	// StatusConfigMap is where the trainer reports (committedStep, committedAt, currentStep).
	StatusConfigMap   string       `json:"statusConfigMap,omitempty"`
	LastCommittedStep int64        `json:"lastCommittedStep,omitempty"`
	LastCommittedAt   *metav1.Time `json:"lastCommittedAt,omitempty"`
	// CurrentStep is the last training step the trainer reported, committed or not.
	CurrentStep int64 `json:"currentStep,omitempty"`
	// NodeLossRecoveries counts node losses after which pods were replaced; LostSteps sums the steps trained
	// after the last commit at each loss (known only when the trainer reports currentStep).
	NodeLossRecoveries int32          `json:"nodeLossRecoveries,omitempty"`
	LostSteps          int64          `json:"lostSteps,omitempty"`
	LastNodeLoss       *NodeLossEvent `json:"lastNodeLoss,omitempty"`
}

// NodeLossEvent is one recovery from a lost node.
type NodeLossEvent struct {
	Node string      `json:"node"`
	At   metav1.Time `json:"at"`
	// Pods were force-deleted so the Job recreates their indexes on healthy nodes.
	Pods []string `json:"pods"`
	// ResumeStep is the last committed step when the node was lost: the replacement pods resume from it.
	ResumeStep int64 `json:"resumeStep"`
	LostSteps  int64 `json:"lostSteps,omitempty"`
}
