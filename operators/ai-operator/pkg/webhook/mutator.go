// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const (
	// Default NCCL environment variable values.
	defaultNCCLDebug        = "WARN"
	defaultNCCLIBDisable    = "1" // Disabled by default; enabled for RDMA.
	defaultNCCLNetGDRLevel  = "0" // Disabled by default; set for RDMA.
	defaultNCCLSocketIfname = "eth0"

	// Default resource limits when not specified by the user.
	defaultCPULimit    = "8"
	defaultMemoryLimit = "32Gi"

	// Annotations for SR-IOV and RDMA.
	annotationSRIOVNetwork = "k8s.v1.cni.cncf.io/networks"
	annotationRDMA         = "gryvia.io/rdma"
	annotationSRIOV        = "gryvia.io/sriov"
	annotationTopology     = "gryvia.io/topology-aware"
	annotationGPUAffinity  = "gryvia.io/gpu-affinity"
)

// GryviaAIJobMutator implements a MutatingWebhook for GryviaAIJob.
// It injects NCCL environment variables, topology-aware scheduling
// annotations, default resource limits, and SR-IOV annotations.
type GryviaAIJobMutator struct {
	Client  client.Client
	decoder admission.Decoder
	log     logr.Logger
}

// NewGryviaAIJobMutator creates a new mutator.
func NewGryviaAIJobMutator(c client.Client) *GryviaAIJobMutator {
	return &GryviaAIJobMutator{
		Client: c,
		log:    ctrl.Log.WithName("webhook").WithName("mutator"),
	}
}

// Handle processes an admission request for GryviaAIJob mutation.
func (m *GryviaAIJobMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	job := &gryviav1.GryviaAIJob{}

	if err := m.decoder.Decode(req, job); err != nil {
		m.log.Error(err, "Failed to decode GryviaAIJob")
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to decode request: %w", err))
	}

	m.log.Info("Mutating GryviaAIJob", "name", job.Name, "namespace", job.Namespace)

	// Apply mutations.
	m.injectNCCLEnvVars(job)
	m.injectTopologyAnnotations(job)
	m.setDefaultResourceLimits(job)
	m.addNetworkAnnotations(job)
	m.setDefaultImagePullPolicy(job)

	// Marshal the mutated object.
	marshaledJob, err := json.Marshal(job)
	if err != nil {
		m.log.Error(err, "Failed to marshal mutated GryviaAIJob")
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("failed to marshal response: %w", err))
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledJob)
}

// injectNCCLEnvVars adds NCCL-related environment variables to the job spec.
// These are critical for distributed GPU training performance.
func (m *GryviaAIJobMutator) injectNCCLEnvVars(job *gryviav1.GryviaAIJob) {
	// Only inject NCCL vars for distributed jobs or multi-GPU jobs.
	if job.Spec.GPUs <= 1 && (job.Spec.Distributed == nil || !job.Spec.Distributed.Enabled) {
		return
	}

	ncclVars := map[string]string{
		"NCCL_DEBUG":         defaultNCCLDebug,
		"NCCL_IB_DISABLE":    defaultNCCLIBDisable,
		"NCCL_SOCKET_IFNAME": defaultNCCLSocketIfname,
	}

	// For RDMA-enabled jobs, configure NCCL for InfiniBand.
	if job.Spec.Network == "rdma" {
		ncclVars["NCCL_IB_DISABLE"] = "0"
		ncclVars["NCCL_NET_GDR_LEVEL"] = "5"
		ncclVars["NCCL_IB_HCA"] = "mlx5"
		ncclVars["NCCL_IB_GID_INDEX"] = "3"
		ncclVars["NCCL_IB_TIMEOUT"] = "23"
		ncclVars["NCCL_IB_RETRY_CNT"] = "7"
	}

	// For SR-IOV jobs, adjust socket interface.
	if job.Spec.Network == "sriov" {
		ncclVars["NCCL_SOCKET_IFNAME"] = "net1"
	}

	// Add NCCL P2P and SHM settings for multi-GPU within a node.
	if job.Spec.GPUs > 1 {
		ncclVars["NCCL_P2P_LEVEL"] = "NVL"
		ncclVars["NCCL_SHM_DISABLE"] = "0"
	}

	// Inject without overwriting user-specified values.
	existingVars := make(map[string]bool)
	for _, env := range job.Spec.Env {
		existingVars[env.Name] = true
	}

	for name, value := range ncclVars {
		if !existingVars[name] {
			job.Spec.Env = append(job.Spec.Env, corev1.EnvVar{
				Name:  name,
				Value: value,
			})
		}
	}

	m.log.Info("Injected NCCL environment variables",
		"job", job.Name,
		"network", job.Spec.Network,
		"varsInjected", len(ncclVars),
	)
}

// injectTopologyAnnotations adds scheduling annotations that influence
// topology-aware placement.
func (m *GryviaAIJobMutator) injectTopologyAnnotations(job *gryviav1.GryviaAIJob) {
	if job.Annotations == nil {
		job.Annotations = make(map[string]string)
	}

	// Enable topology-aware scheduling for multi-GPU or distributed jobs.
	if job.Spec.GPUs > 1 || (job.Spec.Distributed != nil && job.Spec.Distributed.Enabled) {
		job.Annotations[annotationTopology] = "true"
	}

	// Set GPU affinity preference based on GPU count.
	if job.Spec.GPUs >= 4 {
		// For large GPU requests, prefer same NUMA node placement.
		job.Annotations[annotationGPUAffinity] = "same-numa"
	} else if job.Spec.GPUs > 1 {
		// For smaller multi-GPU, prefer NVLink-connected GPUs.
		job.Annotations[annotationGPUAffinity] = "nvlink-preferred"
	}

	// For distributed training, add inter-node topology hints.
	if job.Spec.Distributed != nil && job.Spec.Distributed.Enabled {
		job.Annotations["gryvia.io/distributed-framework"] = job.Spec.Distributed.Framework
		if job.Spec.Distributed.Backend != "" {
			job.Annotations["gryvia.io/distributed-backend"] = job.Spec.Distributed.Backend
		}

		// Prefer nodes on the same switch/rack for reduced latency.
		job.Annotations["gryvia.io/prefer-same-rack"] = "true"
	}
}

// setDefaultResourceLimits sets CPU and memory limits if the user has not
// specified them. This prevents unbounded resource consumption.
func (m *GryviaAIJobMutator) setDefaultResourceLimits(job *gryviav1.GryviaAIJob) {
	// Set default requests.
	if job.Spec.Resources.Requests == nil {
		job.Spec.Resources.Requests = corev1.ResourceList{}
	}

	if _, ok := job.Spec.Resources.Requests[corev1.ResourceCPU]; !ok {
		// Default: 2 CPUs per GPU.
		cpuCount := job.Spec.GPUs * 2
		if cpuCount <= 0 {
			cpuCount = 2
		}
		job.Spec.Resources.Requests[corev1.ResourceCPU] = *resource.NewQuantity(int64(cpuCount), resource.DecimalSI)
	}

	if _, ok := job.Spec.Resources.Requests[corev1.ResourceMemory]; !ok {
		// Default: 8Gi per GPU.
		memGB := job.Spec.GPUs * 8
		if memGB <= 0 {
			memGB = 8
		}
		job.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(fmt.Sprintf("%dGi", memGB))
	}

	// Set default limits.
	if job.Spec.Resources.Limits == nil {
		job.Spec.Resources.Limits = corev1.ResourceList{}
	}

	if _, ok := job.Spec.Resources.Limits[corev1.ResourceCPU]; !ok {
		job.Spec.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(defaultCPULimit)
	}

	if _, ok := job.Spec.Resources.Limits[corev1.ResourceMemory]; !ok {
		job.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(defaultMemoryLimit)
	}
}

// addNetworkAnnotations adds SR-IOV and RDMA annotations for jobs that
// require specialized networking.
func (m *GryviaAIJobMutator) addNetworkAnnotations(job *gryviav1.GryviaAIJob) {
	if job.Annotations == nil {
		job.Annotations = make(map[string]string)
	}

	switch job.Spec.Network {
	case "rdma":
		job.Annotations[annotationRDMA] = "true"
		if _, exists := job.Annotations[annotationSRIOVNetwork]; !exists {
			job.Annotations[annotationSRIOVNetwork] = "rdma-network"
		}

		// Add node selector for RDMA-capable nodes.
		if job.Spec.NodeSelector == nil {
			job.Spec.NodeSelector = make(map[string]string)
		}
		job.Spec.NodeSelector["gryvia.io/rdma"] = "true"

	case "sriov":
		job.Annotations[annotationSRIOV] = "true"
		if _, exists := job.Annotations[annotationSRIOVNetwork]; !exists {
			job.Annotations[annotationSRIOVNetwork] = "sriov-network"
		}

		// Add node selector for SR-IOV-capable nodes.
		if job.Spec.NodeSelector == nil {
			job.Spec.NodeSelector = make(map[string]string)
		}
		job.Spec.NodeSelector["gryvia.io/sriov"] = "true"

		// Request SR-IOV VF resources.
		if job.Spec.Resources.Limits == nil {
			job.Spec.Resources.Limits = corev1.ResourceList{}
		}
		if _, exists := job.Spec.Resources.Limits["openshift.io/sriov_netdevice"]; !exists {
			job.Spec.Resources.Limits["openshift.io/sriov_netdevice"] = resource.MustParse("1")
		}

	default:
		// Standard networking, no special annotations needed.
	}
}

// setDefaultImagePullPolicy sets a default image pull policy if not specified.
func (m *GryviaAIJobMutator) setDefaultImagePullPolicy(job *gryviav1.GryviaAIJob) {
	if job.Spec.ImagePullPolicy == "" {
		job.Spec.ImagePullPolicy = corev1.PullIfNotPresent
	}
}

// InjectDecoder injects the admission decoder.
func (m *GryviaAIJobMutator) InjectDecoder(d admission.Decoder) error {
	m.decoder = d
	return nil
}
