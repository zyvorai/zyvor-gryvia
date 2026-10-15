package scheduler

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	AnnotationGPUShare     = "gryvia.io/gpu-share"
	AnnotationGPUMemory    = "gryvia.io/gpu-memory"
	AnnotationGPUIsolation = "gryvia.io/gpu-isolation"
	AnnotationMIGProfile   = "gryvia.io/mig-profile"

	GPUIsolationAuto = "auto"
	GPUIsolationMIG  = "mig"
	GPUIsolationMPS  = "mps"
	GPUIsolationDRA  = "dra"
	GPUIsolationFull = "full"
)

type GPUShareRequest struct {
	MilliGPU   int64
	MemoryMiB  int64
	Isolation  string
	MIGProfile string
}

type RuntimePlan struct {
	Mode         string
	ResourceName string
	ResourceQty  int64
	Env          map[string]string
	Annotations  map[string]string
	Reason       string
}

func ParseGPUShare(annotations map[string]string, wholeGPUs int32) (GPUShareRequest, error) {
	if annotations == nil || annotations[AnnotationGPUShare] == "" {
		return GPUShareRequest{MilliGPU: int64(wholeGPUs) * MilliGPU, Isolation: GPUIsolationFull}, nil
	}
	raw := strings.TrimSpace(annotations[AnnotationGPUShare])
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f <= 0 {
		return GPUShareRequest{}, fmt.Errorf("invalid %s=%q", AnnotationGPUShare, raw)
	}
	// Values <= 1 are fractions of one GPU; values >1 are whole/fractional GPU counts.
	milli := int64(math.Round(f * float64(MilliGPU)))
	if milli <= 0 {
		return GPUShareRequest{}, fmt.Errorf("GPU share rounds to zero")
	}

	mem := int64(0)
	if s := strings.TrimSpace(annotations[AnnotationGPUMemory]); s != "" {
		mem, err = strconv.ParseInt(s, 10, 64)
		if err != nil || mem < 0 {
			return GPUShareRequest{}, fmt.Errorf("invalid GPU memory MiB %q", s)
		}
	}
	mode := strings.ToLower(strings.TrimSpace(annotations[AnnotationGPUIsolation]))
	if mode == "" {
		mode = GPUIsolationAuto
	}
	switch mode {
	case GPUIsolationAuto, GPUIsolationMIG, GPUIsolationMPS, GPUIsolationDRA, GPUIsolationFull:
	default:
		return GPUShareRequest{}, fmt.Errorf("unsupported GPU isolation %q", mode)
	}
	return GPUShareRequest{
		MilliGPU: milli, MemoryMiB: mem, Isolation: mode,
		MIGProfile: strings.TrimSpace(annotations[AnnotationMIGProfile]),
	}, nil
}

// PlanGPURuntime maps Gryvia's vendor-neutral share into an execution backend.
// The scheduler accounts milli-GPUs, but hard isolation is delegated to the runtime.
func PlanGPURuntime(req GPUShareRequest, nodeLabels map[string]string) (RuntimePlan, error) {
	if req.MilliGPU <= 0 {
		return RuntimePlan{}, fmt.Errorf("milliGPU must be positive")
	}
	mode := req.Isolation
	if mode == "" {
		mode = GPUIsolationAuto
	}

	if req.MilliGPU%MilliGPU == 0 && mode == GPUIsolationAuto {
		mode = GPUIsolationFull
	}
	if mode == GPUIsolationAuto {
		switch {
		case nodeLabels["resource.k8s.io/dra-driver"] != "":
			mode = GPUIsolationDRA
		case req.MIGProfile != "" || nodeLabels["nvidia.com/mig.capable"] == "true":
			mode = GPUIsolationMIG
		default:
			mode = GPUIsolationMPS
		}
	}

	p := RuntimePlan{Mode: mode, Env: map[string]string{}, Annotations: map[string]string{}}
	switch mode {
	case GPUIsolationFull:
		if req.MilliGPU%MilliGPU != 0 {
			return RuntimePlan{}, fmt.Errorf("full GPU isolation requires an integer GPU count")
		}
		p.ResourceName, p.ResourceQty = "nvidia.com/gpu", req.MilliGPU/MilliGPU
		p.Reason = "whole GPU device-plugin allocation"
	case GPUIsolationMIG:
		profile := req.MIGProfile
		if profile == "" {
			profile = chooseMIGProfile(req.MilliGPU, req.MemoryMiB)
		}
		if profile == "" {
			return RuntimePlan{}, fmt.Errorf("no safe MIG profile for request")
		}
		p.ResourceName, p.ResourceQty = "nvidia.com/mig-"+profile, 1
		p.Annotations[AnnotationMIGProfile] = profile
		p.Reason = "hardware-isolated NVIDIA MIG slice"
	case GPUIsolationMPS:
		percent := int64(math.Ceil(float64(req.MilliGPU) * 100 / float64(MilliGPU)))
		if percent < 1 {
			percent = 1
		}
		if percent > 100 {
			percent = 100
		}
		p.ResourceName, p.ResourceQty = "nvidia.com/gpu", 1
		p.Env["CUDA_MPS_ACTIVE_THREAD_PERCENTAGE"] = strconv.FormatInt(percent, 10)
		p.Annotations["gryvia.io/mps-active-thread-percentage"] = strconv.FormatInt(percent, 10)
		p.Reason = "shared GPU through NVIDIA MPS; compute limited by active thread percentage"
		if req.MemoryMiB > 0 {
			// The container sees its single allocated GPU as device 0.
			p.Env["CUDA_MPS_PINNED_DEVICE_MEM_LIMIT"] = "0=" + strconv.FormatInt(req.MemoryMiB, 10) + "M"
			p.Annotations["gryvia.io/mps-pinned-device-mem-limit-mib"] = strconv.FormatInt(req.MemoryMiB, 10)
			p.Reason = "shared GPU through NVIDIA MPS; compute and device memory limited per client"
		}
	case GPUIsolationDRA:
		p.Annotations["gryvia.io/dra-milli-gpu"] = strconv.FormatInt(req.MilliGPU, 10)
		if req.MemoryMiB > 0 {
			p.Annotations["gryvia.io/dra-memory-mib"] = strconv.FormatInt(req.MemoryMiB, 10)
		}
		p.Reason = "Kubernetes Dynamic Resource Allocation claim"
	default:
		return RuntimePlan{}, fmt.Errorf("unsupported runtime mode %q", mode)
	}
	return p, nil
}

func chooseMIGProfile(milliGPU, memoryMiB int64) string {
	// Conservative common H100/A100 profiles. Explicit profile annotation always wins.
	type profile struct {
		share, mem int64
		name       string
	}
	ps := []profile{
		{125, 10000, "1g.10gb"},
		{250, 20000, "2g.20gb"},
		{375, 40000, "3g.40gb"},
		{500, 40000, "4g.40gb"},
		{875, 80000, "7g.80gb"},
	}
	for _, p := range ps {
		if milliGPU <= p.share && (memoryMiB == 0 || memoryMiB <= p.mem) {
			return p.name
		}
	}
	return ""
}
