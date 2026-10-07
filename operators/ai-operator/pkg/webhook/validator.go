package webhook

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/preflight"
)

// ValidatePath is the URL path the validating handler is served on.
const ValidatePath = "/validate-gryvia-io-v1alpha1-gryviaaijob"

var (
	webhookScheme = runtime.NewScheme()

	validJobTypes = []string{"training", "inference", "fine-tuning", "evaluation"}
	validNetworks = []string{"standard", "rdma", "sriov"}
)

func init() {
	utilruntime.Must(gryviav1.AddToScheme(webhookScheme))
}

// GryviaAIJobValidator implements a ValidatingWebhook for GryviaAIJob.
//
// Only rules that depend on the object itself are enforced (deny). Checks
// that depend on live cluster state (node GPU capacity, GPU type labels) are
// reported as admission warnings only, because nodes may be scaled up later
// and a stale or unreachable view must never block job creation.
type GryviaAIJobValidator struct {
	Client  client.Client
	decoder admission.Decoder
	log     logr.Logger
}

// NewGryviaAIJobValidator creates a new validator. Client may be nil, in which
// case cluster-state warnings are skipped.
func NewGryviaAIJobValidator(c client.Client) *GryviaAIJobValidator {
	return &GryviaAIJobValidator{
		Client:  c,
		decoder: admission.NewDecoder(webhookScheme),
		log:     ctrl.Log.WithName("webhook").WithName("validator"),
	}
}

// Handle processes an admission request for GryviaAIJob validation.
func (v *GryviaAIJobValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	// Deletes carry no object to validate.
	if req.Operation == admissionv1.Delete {
		return admission.Allowed("delete is not validated")
	}

	job := &gryviav1.GryviaAIJob{}
	if err := v.decoder.Decode(req, job); err != nil {
		v.log.Error(err, "Failed to decode GryviaAIJob")
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to decode request: %w", err))
	}
	if job.Name == "" {
		// generateName requests arrive without a name; the apiserver names them later.
		job.Name = req.Name
	}

	if errs := ValidateJob(job); len(errs) > 0 {
		return admission.Denied("invalid GryviaAIJob: " + strings.Join(errs, "; "))
	}

	// Policy (quota and tenant catalog) applies to new jobs only, so a later policy change never blocks an
	// update to a job that already exists.
	if req.Operation == admissionv1.Create {
		ns := req.Namespace
		if ns == "" {
			ns = job.Namespace
		}
		if policy, ok := v.loadPolicy(ctx, ns); ok {
			if reasons := checkGPUPolicy(job, policy); len(reasons) > 0 {
				return admission.Denied("not allowed by quota policy: " + strings.Join(reasons, "; "))
			}
		}
	}

	resp := admission.Allowed("GryviaAIJob is valid")
	if warnings := v.clusterWarnings(ctx, job); len(warnings) > 0 {
		resp = resp.WithWarnings(warnings...)
	}
	return resp
}

// ValidateJob runs every static rule and returns all violations found.
func ValidateJob(job *gryviav1.GryviaAIJob) []string {
	var errs []string
	add := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if job.Name != "" {
		if msgs := validation.IsDNS1123Subdomain(job.Name); len(msgs) > 0 {
			errs = append(errs, fmt.Sprintf("metadata.name %q is invalid: %s", job.Name, strings.Join(msgs, ", ")))
		}
	}
	add(validateType(job))
	add(validateGPUCount(job))
	add(validateNetwork(job))
	add(validateWorkload(job))
	add(validateImage(job))
	add(validatePriority(job))
	add(validateDistributedConfig(job))
	add(validateResourceRequests(job))
	if _, _, perrs := preflight.FromJob(job); len(perrs) > 0 {
		errs = append(errs, perrs...)
	}
	return errs
}

func validateType(job *gryviav1.GryviaAIJob) error {
	for _, t := range validJobTypes {
		if job.Spec.Type == t {
			return nil
		}
	}
	if job.Spec.Type == "" {
		return fmt.Errorf("spec.type is required; valid values: %s", strings.Join(validJobTypes, ", "))
	}
	return fmt.Errorf("spec.type %q is not supported; valid values: %s", job.Spec.Type, strings.Join(validJobTypes, ", "))
}

func validateGPUCount(job *gryviav1.GryviaAIJob) error {
	// 0 is allowed: a CPU-only job (no nvidia.com/gpu limit, no GPU node selector).
	if job.Spec.GPUs < 0 {
		return fmt.Errorf("spec.gpus must not be negative, got %d", job.Spec.GPUs)
	}
	return nil
}

func validateNetwork(job *gryviav1.GryviaAIJob) error {
	if job.Spec.Network == "" {
		return nil
	}
	for _, n := range validNetworks {
		if job.Spec.Network == n {
			return nil
		}
	}
	return fmt.Errorf("spec.network %q is not supported; valid values: %s", job.Spec.Network, strings.Join(validNetworks, ", "))
}

func validateWorkload(job *gryviav1.GryviaAIJob) error {
	switch job.Spec.WorkloadKind {
	case "", gryviav1.WorkloadKindJob, gryviav1.WorkloadKindStatefulSet:
	default:
		return fmt.Errorf("spec.workloadKind %q is not supported; valid values: job, statefulset", job.Spec.WorkloadKind)
	}
	if _, err := job.Spec.TimeoutSeconds(); err != nil {
		return fmt.Errorf("spec.timeout: %w", err)
	}
	if job.Spec.RetryLimit < 0 {
		return fmt.Errorf("spec.retryLimit must not be negative, got %d", job.Spec.RetryLimit)
	}
	return nil
}

// clusterWarnings compares the job with the nodes currently in the cluster.
// It never denies and swallows all errors.
func (v *GryviaAIJobValidator) clusterWarnings(ctx context.Context, job *gryviav1.GryviaAIJob) []string {
	if v.Client == nil {
		return nil
	}
	nodes := &corev1.NodeList{}
	if err := v.Client.List(ctx, nodes); err != nil {
		v.log.Error(err, "Failed to list nodes; skipping cluster warnings")
		return nil
	}
	var warnings []string

	gpusPerNode := job.Spec.GPUs
	if job.Spec.Distributed != nil && job.Spec.Distributed.Enabled && job.Spec.Distributed.GpusPerNode > 0 {
		gpusPerNode = job.Spec.Distributed.GpusPerNode
	}
	maxNodeGPUs := int32(0)
	types := map[string]bool{}
	for _, node := range nodes.Items {
		nodeGPUs := int32(0)
		if gpuAlloc, ok := node.Status.Allocatable["nvidia.com/gpu"]; ok {
			nodeGPUs = int32(gpuAlloc.Value())
		}
		if countStr, exists := node.Labels["gryvia.io/gpu-count"]; exists {
			if count, err := strconv.ParseInt(countStr, 10, 32); err == nil && int32(count) > nodeGPUs {
				nodeGPUs = int32(count)
			}
		}
		if nodeGPUs > maxNodeGPUs {
			maxNodeGPUs = nodeGPUs
		}
		if t, ok := node.Labels["gryvia.io/gpu"]; ok {
			types[t] = true
		}
	}
	if maxNodeGPUs > 0 && gpusPerNode > maxNodeGPUs {
		warnings = append(warnings, fmt.Sprintf("requested %d GPUs per node exceeds the largest current node (%d GPUs); "+
			"the job stays pending until a larger node exists or you use distributed training", gpusPerNode, maxNodeGPUs))
	}
	if t := job.Spec.GpuType; t != "" && t != "any" && len(types) > 0 && !types[t] {
		warnings = append(warnings, fmt.Sprintf("no node currently carries GPU type %q (label gryvia.io/gpu)", t))
	}
	if in, ok, errs := preflight.FromJob(job); ok && len(errs) == 0 {
		if r := preflight.Evaluate(in, preflight.PoolsFromNodes(nodes.Items)); r.State != preflight.StateCandidate {
			warnings = append(warnings, r.Summary())
		}
	}
	return warnings
}

// validateDistributedConfig checks that the distributed training configuration
// is internally consistent.
func validateDistributedConfig(job *gryviav1.GryviaAIJob) error {
	dist := job.Spec.Distributed
	if dist == nil || !dist.Enabled {
		return nil
	}

	if dist.Nodes <= 0 {
		return fmt.Errorf("distributed.nodes must be greater than 0 when distributed training is enabled, got %d", dist.Nodes)
	}

	if dist.GpusPerNode < 0 {
		return fmt.Errorf("distributed.gpusPerNode must be non-negative, got %d", dist.GpusPerNode)
	}

	if e := dist.Elastic; e != nil {
		if e.MinNodes < 1 || e.MinNodes > dist.Nodes {
			return fmt.Errorf("distributed.elastic.minNodes must be between 1 and distributed.nodes (%d), got %d", dist.Nodes, e.MinNodes)
		}
		if e.DesiredNodes != 0 && (e.DesiredNodes < e.MinNodes || e.DesiredNodes > dist.Nodes) {
			return fmt.Errorf("distributed.elastic.desiredNodes must be between minNodes (%d) and distributed.nodes (%d), got %d",
				e.MinNodes, dist.Nodes, e.DesiredNodes)
		}
		if dist.Framework != "" && dist.Framework != "pytorch" {
			return fmt.Errorf("distributed.elastic is for PyTorch (torchrun); framework %q is not supported", dist.Framework)
		}
	}

	// Validate framework.
	validFrameworks := map[string]bool{
		"pytorch":    true,
		"tensorflow": true,
		"horovod":    true,
		"deepspeed":  true,
		"megatron":   true,
		"":           true, // Empty is allowed (defaults to pytorch).
	}
	if !validFrameworks[dist.Framework] {
		return fmt.Errorf("distributed.framework %q is not supported; valid values: pytorch, tensorflow, horovod, deepspeed, megatron",
			dist.Framework)
	}

	// Validate backend.
	validBackends := map[string]bool{
		"nccl": true,
		"gloo": true,
		"mpi":  true,
		"":     true,
	}
	if !validBackends[dist.Backend] {
		return fmt.Errorf("distributed.backend %q is not supported; valid values: nccl, gloo, mpi",
			dist.Backend)
	}

	// Sanity: nodes * gpusPerNode should not be unreasonably large.
	gpusPerNode := dist.GpusPerNode
	if gpusPerNode == 0 {
		gpusPerNode = job.Spec.GPUs
	}
	totalGPUs := dist.Nodes * gpusPerNode
	if totalGPUs > 1024 {
		return fmt.Errorf("total GPU count (%d nodes x %d GPUs/node = %d) seems unreasonably large; "+
			"maximum supported is 1024", dist.Nodes, gpusPerNode, totalGPUs)
	}

	return nil
}

// validateResourceRequests checks that resource requests are properly formed.
func validateResourceRequests(job *gryviav1.GryviaAIJob) error {
	// Validate CPU requests if specified.
	if cpu, ok := job.Spec.Resources.Requests[corev1.ResourceCPU]; ok {
		if cpu.Cmp(resource.MustParse("0")) <= 0 {
			return fmt.Errorf("CPU request must be positive, got %s", cpu.String())
		}
	}

	// Validate memory requests if specified.
	if mem, ok := job.Spec.Resources.Requests[corev1.ResourceMemory]; ok {
		if mem.Cmp(resource.MustParse("0")) <= 0 {
			return fmt.Errorf("memory request must be positive, got %s", mem.String())
		}
	}

	// Check that limits >= requests when both are specified.
	if cpu, ok := job.Spec.Resources.Requests[corev1.ResourceCPU]; ok {
		if cpuLimit, okL := job.Spec.Resources.Limits[corev1.ResourceCPU]; okL {
			if cpuLimit.Cmp(cpu) < 0 {
				return fmt.Errorf("CPU limit (%s) must be >= CPU request (%s)", cpuLimit.String(), cpu.String())
			}
		}
	}

	if mem, ok := job.Spec.Resources.Requests[corev1.ResourceMemory]; ok {
		if memLimit, okL := job.Spec.Resources.Limits[corev1.ResourceMemory]; okL {
			if memLimit.Cmp(mem) < 0 {
				return fmt.Errorf("memory limit (%s) must be >= memory request (%s)", memLimit.String(), mem.String())
			}
		}
	}

	return nil
}

// validateImage checks that an image is specified.
func validateImage(job *gryviav1.GryviaAIJob) error {
	if job.Spec.Image == "" {
		return fmt.Errorf("spec.image is required")
	}
	return nil
}

// validatePriority checks that priority is within the valid range.
func validatePriority(job *gryviav1.GryviaAIJob) error {
	if job.Spec.Priority < 0 || job.Spec.Priority > 100 {
		return fmt.Errorf("spec.priority must be between 0 and 100, got %d", job.Spec.Priority)
	}
	return nil
}

// InjectDecoder injects the admission decoder.
func (v *GryviaAIJobValidator) InjectDecoder(d admission.Decoder) error {
	v.decoder = d
	return nil
}
