package scheduler

import (
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	LabelGPUUtilization = "gryvia.io/gpu-utilization"
	LabelGPUMemoryUsed  = "gryvia.io/gpu-memory-used-percent"
	LabelNVLinkHealth   = "gryvia.io/nvlink-health"
	LabelRDMAHealth     = "gryvia.io/rdma-health"
	LabelStorageHealth  = "gryvia.io/storage-health"
)

type LiveNodeTelemetry struct {
	GPUUtilization float64
	GPUMemoryUsed  float64
	NVLinkHealth   float64
	RDMAHealth     float64
	StorageHealth  float64
}

func TelemetryFromLabels(labels map[string]string) LiveNodeTelemetry {
	return LiveNodeTelemetry{
		GPUUtilization: percent(labels[LabelGPUUtilization]),
		GPUMemoryUsed:  percent(labels[LabelGPUMemoryUsed]),
		NVLinkHealth:   percentDefault(labels[LabelNVLinkHealth], 100),
		RDMAHealth:     percentDefault(labels[LabelRDMAHealth], 100),
		StorageHealth:  percentDefault(labels[LabelStorageHealth], 100),
	}
}

func percent(s string) float64 { return percentDefault(s, 0) }
func percentDefault(s string, d float64) float64 {
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	if s == "" {
		return d
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return d
	}
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// LiveTelemetryScore returns 0..100. It rewards low utilization/fragmentation,
// healthy NVLink/RDMA, and healthy storage. This can be added to existing Gryvia
// node/fabric scores without replacing those proven filters.
func LiveTelemetryScore(t LiveNodeTelemetry, multiGPU, needsRDMA bool) int {
	score := 0.35*(100-t.GPUUtilization) + 0.15*(100-t.GPUMemoryUsed)
	if multiGPU {
		score += 0.25 * t.NVLinkHealth
	} else {
		score += 25
	}
	if needsRDMA {
		score += 0.15 * t.RDMAHealth
	} else {
		score += 15
	}
	score += 0.10 * t.StorageHealth
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return int(score + 0.5)
}

// Live telemetry is advisory ranking only; it never changes admission capacity.
const AnnotationLiveTelemetry = "gryvia.io/live-telemetry"
const AnnotationTelemetryObservedAt = "gryvia.io/telemetry-observed-at"

// FreshLiveTelemetryScore ignores missing, malformed, future or stale observations.
// Require both demand measurements; optional health values must be valid if supplied.
func FreshLiveTelemetryScore(labels, annotations map[string]string, now time.Time, multiGPU, needsRDMA bool) int {
	observed, err := time.Parse(time.RFC3339, annotations[AnnotationTelemetryObservedAt])
	if err != nil || observed.After(now) || now.Sub(observed) > time.Minute {
		return 0
	}
	for _, key := range []string{LabelGPUUtilization, LabelGPUMemoryUsed, LabelNVLinkHealth, LabelRDMAHealth, LabelStorageHealth} {
		raw := strings.TrimSuffix(strings.TrimSpace(labels[key]), "%")
		if raw == "" && key != LabelGPUUtilization && key != LabelGPUMemoryUsed {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return 0
		}
	}
	return LiveTelemetryScore(TelemetryFromLabels(labels), multiGPU, needsRDMA) / 4
}
