package v1

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

const annotationGPUShare = "gryvia.io/gpu-share"

var migMemoryRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*g(?:b)?\s*$`)

// TotalGPUs is the number of whole GPU units the job requests. It mirrors the AI operator's
// placement: a distributed job runs distributed.nodes pods with distributed.gpusPerNode GPUs
// each (gpusPerNode falls back to 1 when spec.gpus is set, 0 for a CPU-only job); any other
// job holds spec.gpus. Reading spec.gpus alone undercounts distributed jobs.
func (s GryviaAIJobSpec) TotalGPUs() int32 {
	if s.Distributed != nil && s.Distributed.Enabled {
		nodes := s.Distributed.Nodes
		if nodes < 1 {
			nodes = 1
		}
		per := s.Distributed.GpusPerNode
		if per <= 0 {
			if s.GPUs == 0 {
				return 0
			}
			per = 1
		}
		return nodes * per
	}
	if s.GPUs < 0 {
		return 1
	}
	return s.GPUs
}

// EffectiveGPUs is the physical GPU fraction billed for the job. Whole GPUs bill as TotalGPUs.
// With gryvia.io/gpu-share set: values <= 1 are a fraction of each requested GPU; values > 1 are
// an absolute GPU count. With migProfile and no share, memory is parsed from the profile (e.g.
// 1g.10gb) and divided by an assumed 80 GB device unless the gpuType embeds a size.
func (s GryviaAIJobSpec) EffectiveGPUs(annotations map[string]string) float64 {
	whole := float64(s.TotalGPUs())
	if whole < 0 {
		whole = 0
	}
	if annotations != nil {
		if raw := strings.TrimSpace(annotations[annotationGPUShare]); raw != "" {
			if f, err := strconv.ParseFloat(raw, 64); err == nil && f > 0 {
				if f <= 1 {
					return roundGPUs(whole * f)
				}
				return roundGPUs(f)
			}
		}
	}
	if s.MigProfile != "" && whole > 0 {
		if frac := migProfileFraction(s.MigProfile, s.GpuType); frac > 0 && frac < 1 {
			return roundGPUs(whole * frac)
		}
	}
	return whole
}

func roundGPUs(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func migProfileFraction(profile, gpuType string) float64 {
	m := migMemoryRe.FindStringSubmatch(strings.TrimSpace(profile))
	if len(m) < 2 {
		return 0
	}
	sliceGB, err := strconv.ParseFloat(m[1], 64)
	if err != nil || sliceGB <= 0 {
		return 0
	}
	deviceGB := 80.0
	gt := strings.ToUpper(gpuType)
	switch {
	case strings.Contains(gt, "40"):
		deviceGB = 40
	case strings.Contains(gt, "48"):
		deviceGB = 48
	case strings.Contains(gt, "24"):
		deviceGB = 24
	case strings.Contains(gt, "16"):
		deviceGB = 16
	case strings.Contains(gt, "80"):
		deviceGB = 80
	case strings.Contains(gt, "141"), strings.Contains(gt, "H200"):
		deviceGB = 141
	}
	if sliceGB >= deviceGB {
		return 1
	}
	return sliceGB / deviceGB
}
