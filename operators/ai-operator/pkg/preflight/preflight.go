// Package preflight is the Go port of the gateway's workload preflight
// (services/api-gateway/intelligence/engine.py, preflight): a weight-memory estimate and node-shape,
// GPU-type, RDMA and interconnect checks, evaluated against pools built from the cluster's nodes.
//
// A job opts in by carrying gryvia.io/model-params-billions; the other inputs are optional
// annotations. Quota and budget are not checked here (the admission gate already does that). The
// result is an estimate: extra memory (activations, optimizer state, KV cache, runtime) must be
// supplied by the submitter, and a node label is not a qualified runtime probe.
package preflight

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// Annotations on a GryviaAIJob.
const (
	AnnotationParams        = "gryvia.io/model-params-billions"
	AnnotationWeightBits    = "gryvia.io/weight-bits"
	AnnotationTensorPar     = "gryvia.io/tensor-parallel"
	AnnotationExtraGiB      = "gryvia.io/extra-memory-gib"
	AnnotationFastInterconn = "gryvia.io/require-fast-interconnect"
)

// Node labels read into pools.
const (
	LabelGPU          = "gryvia.io/gpu"
	LabelGPUCount     = "gryvia.io/gpu-count"
	LabelGPUMemory    = "gryvia.io/gpu-memory"  // whole GiB per GPU
	LabelNvidiaMemory = "nvidia.com/gpu.memory" // MiB per GPU (GPU feature discovery)
	LabelRDMA         = "gryvia.io/rdma"
	LabelInterconnect = "gryvia.io/interconnect" // NVLink or NVSwitch
)

// States, as in the gateway report.
const (
	StateBlocked    = "Blocked"
	StateIncomplete = "Incomplete"
	StateCandidate  = "Candidate"
)

// Input is what preflight needs from a job.
type Input struct {
	GPUType                 string
	Nodes                   int32
	GPUsPerNode             int32
	ParamsBillions          float64
	WeightBits              int
	TensorParallel          int32
	ExtraGiBPerGPU          float64
	RequireRDMA             bool
	RequireFastInterconnect bool
}

// Pool is a group of identical nodes. MemoryGiB 0 means unknown.
type Pool struct {
	Name         string  `json:"name"`
	GPUType      string  `json:"gpuType"`
	Nodes        int32   `json:"nodes"`
	GPUsPerNode  int32   `json:"gpusPerNode"`
	MemoryGiB    float64 `json:"memoryGiB"`
	RDMA         bool    `json:"rdma"`
	Interconnect string  `json:"interconnect"` // none, nvlink, nvswitch
}

// Candidate is one pool's verdict.
type Candidate struct {
	Pool     string   `json:"pool"`
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons,omitempty"`
}

// Result is the preflight report.
type Result struct {
	State        string      `json:"state"`
	PerGPUGiB    float64     `json:"estimatedMemoryGiBPerGPU"`
	RequiredGPUs int32       `json:"requiredGPUs"`
	Blockers     []string    `json:"blockers,omitempty"`
	Unknown      []string    `json:"unknown,omitempty"`
	Candidates   []Candidate `json:"candidates,omitempty"`
}

// Summary is a one-line explanation for a warning or a rejection.
func (r Result) Summary() string {
	parts := append([]string{}, r.Blockers...)
	for _, c := range r.Candidates {
		if !c.Eligible && len(c.Reasons) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %s", c.Pool, strings.Join(c.Reasons, ", ")))
		}
	}
	return fmt.Sprintf("preflight %s (estimated %.1f GiB per GPU): %s", r.State, r.PerGPUGiB, strings.Join(parts, "; "))
}

// FromJob reads the preflight inputs. requested is false when the job does not opt in. errs lists
// malformed annotations; they are static and the webhook denies them.
func FromJob(job *gryviav1.GryviaAIJob) (in Input, requested bool, errs []string) {
	a := job.Annotations
	raw, ok := a[AnnotationParams]
	if !ok {
		return in, false, nil
	}
	requested = true
	in.GPUType = job.Spec.GpuType
	if in.GPUType == "any" {
		in.GPUType = ""
	}
	in.Nodes, in.GPUsPerNode = 1, job.Spec.GPUs
	if d := job.Spec.Distributed; d != nil && d.Enabled {
		in.Nodes = d.Nodes
		if d.GpusPerNode > 0 {
			in.GPUsPerNode = d.GpusPerNode
		}
	}
	in.RequireRDMA = job.Spec.Network == "rdma"
	in.WeightBits, in.TensorParallel = 16, 1

	if v, err := strconv.ParseFloat(raw, 64); err != nil || v <= 0 || v > 100000 || math.IsNaN(v) {
		errs = append(errs, fmt.Sprintf("%s must be a positive number of billions (at most 100000), got %q", AnnotationParams, raw))
	} else {
		in.ParamsBillions = v
	}
	if raw, ok := a[AnnotationWeightBits]; ok {
		switch raw {
		case "4", "8", "16", "32":
			in.WeightBits, _ = strconv.Atoi(raw)
		default:
			errs = append(errs, fmt.Sprintf("%s must be 4, 8, 16 or 32, got %q", AnnotationWeightBits, raw))
		}
	}
	if raw, ok := a[AnnotationTensorPar]; ok {
		v, err := strconv.ParseInt(raw, 10, 32)
		switch {
		case err != nil || v < 1:
			errs = append(errs, fmt.Sprintf("%s must be a positive integer, got %q", AnnotationTensorPar, raw))
		case in.GPUsPerNode > 0 && int32(v) > in.GPUsPerNode:
			errs = append(errs, fmt.Sprintf("%s (%d) exceeds GPUs per node (%d)", AnnotationTensorPar, v, in.GPUsPerNode))
		default:
			in.TensorParallel = int32(v)
		}
	}
	if raw, ok := a[AnnotationExtraGiB]; ok {
		if v, err := strconv.ParseFloat(raw, 64); err != nil || v < 0 || v > 1e6 || math.IsNaN(v) {
			errs = append(errs, fmt.Sprintf("%s must be a non-negative number of GiB, got %q", AnnotationExtraGiB, raw))
		} else {
			in.ExtraGiBPerGPU = v
		}
	}
	if raw, ok := a[AnnotationFastInterconn]; ok {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s must be true or false, got %q", AnnotationFastInterconn, raw))
		}
		in.RequireFastInterconnect = b
	}
	if in.GPUsPerNode <= 0 {
		errs = append(errs, fmt.Sprintf("%s needs a GPU job (spec.gpus or distributed.gpusPerNode > 0)", AnnotationParams))
	}
	return in, requested, errs
}

// PerGPUGiB is the estimated memory per GPU: decimal billions of parameters, binary GiB.
func PerGPUGiB(in Input) float64 {
	weight := in.ParamsBillions * 1e9 * float64(in.WeightBits) / 8 / (1024 * 1024 * 1024)
	return weight/float64(in.TensorParallel) + in.ExtraGiBPerGPU
}

// Evaluate checks the input against the pools. With no pools the result is Incomplete, not
// Blocked: an empty or scaling cluster must not reject every job.
func Evaluate(in Input, pools []Pool) Result {
	perGPU := PerGPUGiB(in)
	res := Result{PerGPUGiB: math.Round(perGPU*1000) / 1000, RequiredGPUs: in.Nodes * in.GPUsPerNode}
	if len(pools) == 0 {
		res.State = StateIncomplete
		res.Unknown = append(res.Unknown, "no GPU nodes registered")
		return res
	}
	eligible, eligibleKnown := false, false
	for _, p := range pools {
		var why []string
		if in.GPUType != "" && p.GPUType != in.GPUType {
			why = append(why, "GPU type mismatch")
		}
		if p.Nodes < in.Nodes || p.GPUsPerNode < in.GPUsPerNode {
			why = append(why, "insufficient node shape")
		}
		if p.Nodes*p.GPUsPerNode < res.RequiredGPUs {
			why = append(why, "insufficient GPUs")
		}
		known := p.MemoryGiB > 0
		if known && p.MemoryGiB < perGPU {
			why = append(why, "estimated memory exceeds per-GPU capacity")
		}
		if in.RequireRDMA && !p.RDMA {
			why = append(why, "RDMA required")
		}
		if in.RequireFastInterconnect && p.Interconnect == "none" {
			why = append(why, "fast GPU interconnect required")
		}
		c := Candidate{Pool: p.Name, Eligible: len(why) == 0, Reasons: why}
		if c.Eligible {
			eligible = true
			if known {
				eligibleKnown = true
			}
		}
		res.Candidates = append(res.Candidates, c)
	}
	switch {
	case !eligible:
		res.State = StateBlocked
		res.Blockers = append(res.Blockers, "no eligible node pool")
	case !eligibleKnown:
		res.State = StateIncomplete
		res.Unknown = append(res.Unknown, "GPU memory unknown on every eligible pool (label "+LabelGPUMemory+")")
	default:
		res.State = StateCandidate
	}
	return res
}

// PoolsFromNodes groups GPU nodes by GPU type, GPUs per node, memory, RDMA and interconnect.
func PoolsFromNodes(nodes []corev1.Node) []Pool {
	byKey := map[string]*Pool{}
	for i := range nodes {
		n := &nodes[i]
		if n.Spec.Unschedulable {
			continue
		}
		gpus := int32(0)
		if q, ok := n.Status.Allocatable["nvidia.com/gpu"]; ok {
			gpus = int32(q.Value())
		}
		if v, err := strconv.ParseInt(n.Labels[LabelGPUCount], 10, 32); err == nil && int32(v) > gpus {
			gpus = int32(v)
		}
		if gpus <= 0 {
			continue
		}
		mem := 0.0
		if v, err := strconv.ParseFloat(n.Labels[LabelGPUMemory], 64); err == nil && v > 0 {
			mem = v
		} else if v, err := strconv.ParseFloat(n.Labels[LabelNvidiaMemory], 64); err == nil && v > 0 {
			mem = v / 1024
		}
		inter := "none"
		switch strings.ToLower(n.Labels[LabelInterconnect]) {
		case "nvswitch":
			inter = "nvswitch"
		case "nvlink":
			inter = "nvlink"
		}
		rdma := n.Labels[LabelRDMA] == "true"
		p := Pool{GPUType: n.Labels[LabelGPU], GPUsPerNode: gpus, MemoryGiB: mem, RDMA: rdma, Interconnect: inter}
		key := fmt.Sprintf("%s/%d/%g/%t/%s", p.GPUType, p.GPUsPerNode, p.MemoryGiB, p.RDMA, p.Interconnect)
		if existing, ok := byKey[key]; ok {
			existing.Nodes++
			continue
		}
		name := p.GPUType
		if name == "" {
			name = "unlabelled"
		}
		p.Name = fmt.Sprintf("%s-x%d", strings.ToLower(name), gpus)
		p.Nodes = 1
		byKey[key] = &p
	}
	out := make([]Pool, 0, len(byKey))
	for _, p := range byKey {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name+fmt.Sprint(out[i].MemoryGiB) < out[j].Name+fmt.Sprint(out[j].MemoryGiB)
	})
	seen := map[string]int{}
	for i := range out {
		seen[out[i].Name]++
		if seen[out[i].Name] > 1 {
			out[i].Name = fmt.Sprintf("%s-%d", out[i].Name, seen[out[i].Name])
		}
	}
	return out
}
