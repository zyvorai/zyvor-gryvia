// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package memory

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// GpuMemorySample represents a single GPU memory usage data point
type GpuMemorySample struct {
	// NodeName is the Kubernetes node name
	NodeName string

	// GpuIndex is the GPU index on the node
	GpuIndex int

	// GpuUUID is the GPU UUID
	GpuUUID string

	// GpuType is the GPU model (H100, A100, etc.)
	GpuType string

	// MemoryUsed in MB
	MemoryUsed int

	// MemoryTotal in MB
	MemoryTotal int

	// Utilization percentage (0-100)
	Utilization int

	// Timestamp of the sample
	Timestamp time.Time
}

// OOMPrediction represents the result of an OOM prediction
type OOMPrediction struct {
	// OomLikely indicates whether OOM is predicted
	OomLikely bool

	// TimeToOOM is the estimated time until OOM occurs
	TimeToOOM time.Duration

	// CurrentUtilization is the current memory utilization percentage
	CurrentUtilization float64

	// ProjectedPeakUtilization is the projected peak memory utilization
	ProjectedPeakUtilization float64

	// GrowthRateMBPerSec is the memory growth rate in MB/s
	GrowthRateMBPerSec float64

	// Confidence is the prediction confidence (0-1)
	Confidence float64
}

// UtilizationAnalysis represents the result of a utilization analysis
type UtilizationAnalysis struct {
	// PeakUtilization is the peak memory utilization percentage
	PeakUtilization float64

	// SteadyStateUtilization is the steady-state memory utilization percentage
	SteadyStateUtilization float64

	// AverageUtilization is the average memory utilization percentage
	AverageUtilization float64

	// Variance is the variance in utilization
	Variance float64

	// SampleCount is the number of samples used for the analysis
	SampleCount int
}

// Predictor performs OOM prediction and memory analysis for GPUs
type Predictor struct {
	mu      sync.RWMutex
	samples map[string][]GpuMemorySample // keyed by "nodeName/gpu-index"
	maxAge  time.Duration                // maximum age of samples to retain
}

// NewPredictor creates a new memory predictor
func NewPredictor(maxAge time.Duration) *Predictor {
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	return &Predictor{
		samples: make(map[string][]GpuMemorySample),
		maxAge:  maxAge,
	}
}

// maxSamplesPerGPU is the hard cap on samples stored per GPU to prevent unbounded growth.
const maxSamplesPerGPU = 1000

// RecordSample adds a memory sample for a GPU
func (p *Predictor) RecordSample(sample GpuMemorySample) {
	key := fmt.Sprintf("%s/gpu-%d", sample.NodeName, sample.GpuIndex)

	p.mu.Lock()
	defer p.mu.Unlock()

	// Prune old samples
	p.pruneSamplesLocked(key)

	p.samples[key] = append(p.samples[key], sample)

	// Enforce hard cap to prevent unbounded growth
	if len(p.samples[key]) > maxSamplesPerGPU {
		p.samples[key] = p.samples[key][len(p.samples[key])-maxSamplesPerGPU:]
	}
}

// SampleCount returns the number of samples for a GPU
func (p *Predictor) SampleCount(gpuKey string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.samples[gpuKey])
}

// PredictOOM predicts whether OOM will occur for a given GPU
func (p *Predictor) PredictOOM(gpuKey string, projectionMethod string) (*OOMPrediction, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	samples := p.samples[gpuKey]
	if len(samples) < 2 {
		return nil, fmt.Errorf("insufficient samples for prediction (need at least 2, have %d)", len(samples))
	}

	// Get the latest sample
	latest := samples[len(samples)-1]
	if latest.MemoryTotal <= 0 {
		return nil, fmt.Errorf("invalid memory total: %d", latest.MemoryTotal)
	}

	currentUtilPct := float64(latest.MemoryUsed) / float64(latest.MemoryTotal) * 100.0

	// Calculate growth rate based on projection method
	var growthRateMBPerSec float64
	var projectedPeakPct float64
	var confidence float64

	switch projectionMethod {
	case "exponential":
		growthRateMBPerSec, projectedPeakPct, confidence = p.exponentialProjection(samples)
	case "polynomial":
		growthRateMBPerSec, projectedPeakPct, confidence = p.polynomialProjection(samples)
	default: // "linear"
		growthRateMBPerSec, projectedPeakPct, confidence = p.linearProjection(samples)
	}

	prediction := &OOMPrediction{
		CurrentUtilization:       currentUtilPct,
		ProjectedPeakUtilization: projectedPeakPct,
		GrowthRateMBPerSec:       growthRateMBPerSec,
		Confidence:               confidence,
	}

	// Determine if OOM is likely
	if growthRateMBPerSec > 0 && projectedPeakPct >= 100.0 {
		prediction.OomLikely = true

		// Calculate time to OOM
		remainingMB := float64(latest.MemoryTotal - latest.MemoryUsed)
		if growthRateMBPerSec > 0 {
			timeToOOMSec := remainingMB / growthRateMBPerSec
			prediction.TimeToOOM = time.Duration(timeToOOMSec) * time.Second
		}
	} else if currentUtilPct > 95.0 {
		// Already very close to OOM
		prediction.OomLikely = true
		prediction.TimeToOOM = 0
	}

	return prediction, nil
}

// linearProjection uses linear regression to project memory growth
func (p *Predictor) linearProjection(samples []GpuMemorySample) (growthRate float64, projectedPeak float64, confidence float64) {
	n := len(samples)
	if n < 2 {
		return 0, 0, 0
	}

	// Use time since first sample as x, memory used as y
	firstTime := samples[0].Timestamp
	memoryTotal := float64(samples[0].MemoryTotal)

	var sumX, sumY, sumXY, sumXX float64
	for _, s := range samples {
		x := s.Timestamp.Sub(firstTime).Seconds()
		y := float64(s.MemoryUsed)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}

	nf := float64(n)
	denom := nf*sumXX - sumX*sumX
	if denom == 0 {
		return 0, float64(samples[n-1].MemoryUsed) / memoryTotal * 100.0, 0.5
	}

	// Slope (growth rate in MB/s)
	slope := (nf*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / nf

	// Calculate R-squared for confidence
	meanY := sumY / nf
	var ssTot, ssRes float64
	for _, s := range samples {
		x := s.Timestamp.Sub(firstTime).Seconds()
		y := float64(s.MemoryUsed)
		predicted := slope*x + intercept
		ssTot += (y - meanY) * (y - meanY)
		ssRes += (y - predicted) * (y - predicted)
	}

	rSquared := 0.0
	if ssTot > 0 {
		rSquared = 1.0 - ssRes/ssTot
	}
	if rSquared < 0 {
		rSquared = 0
	}

	// Project 1 hour into the future
	projectionTime := samples[n-1].Timestamp.Sub(firstTime).Seconds() + 3600.0
	projectedMB := slope*projectionTime + intercept
	projectedPeakPct := projectedMB / memoryTotal * 100.0

	// Clamp to valid range
	if projectedPeakPct > 200.0 {
		projectedPeakPct = 200.0
	}
	if projectedPeakPct < 0 {
		projectedPeakPct = 0
	}

	return slope, projectedPeakPct, rSquared
}

// exponentialProjection uses exponential fitting to project memory growth
func (p *Predictor) exponentialProjection(samples []GpuMemorySample) (growthRate float64, projectedPeak float64, confidence float64) {
	n := len(samples)
	if n < 2 {
		return 0, 0, 0
	}

	firstTime := samples[0].Timestamp
	memoryTotal := float64(samples[0].MemoryTotal)

	// Take log of memory values for linear regression in log space
	var sumX, sumLogY, sumXLogY, sumXX float64
	validCount := 0

	for _, s := range samples {
		y := float64(s.MemoryUsed)
		if y <= 0 {
			continue
		}
		x := s.Timestamp.Sub(firstTime).Seconds()
		logY := math.Log(y)
		sumX += x
		sumLogY += logY
		sumXLogY += x * logY
		sumXX += x * x
		validCount++
	}

	if validCount < 2 {
		return p.linearProjection(samples)
	}

	nf := float64(validCount)
	denom := nf*sumXX - sumX*sumX
	if denom == 0 {
		return p.linearProjection(samples)
	}

	// Exponential parameters: y = a * e^(b*x)
	b := (nf*sumXLogY - sumX*sumLogY) / denom
	logA := (sumLogY - b*sumX) / nf
	a := math.Exp(logA)

	// Current growth rate at the latest time point
	latestTime := samples[n-1].Timestamp.Sub(firstTime).Seconds()
	currentGrowthRate := a * b * math.Exp(b*latestTime)

	// Project 1 hour into the future
	projectionTime := latestTime + 3600.0
	projectedMB := a * math.Exp(b*projectionTime)
	projectedPeakPct := projectedMB / memoryTotal * 100.0

	// Cap projectedPeakPct to avoid unreasonable values
	if projectedPeakPct > 200.0 {
		projectedPeakPct = 200.0
	}
	if projectedPeakPct < 0 {
		projectedPeakPct = 0
	}

	// Calculate confidence based on fit quality
	var ssRes, ssTot float64
	meanLogY := sumLogY / nf
	for _, s := range samples {
		y := float64(s.MemoryUsed)
		if y <= 0 {
			continue
		}
		x := s.Timestamp.Sub(firstTime).Seconds()
		logY := math.Log(y)
		predicted := logA + b*x
		ssRes += (logY - predicted) * (logY - predicted)
		ssTot += (logY - meanLogY) * (logY - meanLogY)
	}

	rSquared := 0.0
	if ssTot > 0 {
		rSquared = 1.0 - ssRes/ssTot
	}
	if rSquared < 0 {
		rSquared = 0
	}

	return currentGrowthRate, projectedPeakPct, rSquared
}

// polynomialProjection uses quadratic polynomial fitting to project memory growth
func (p *Predictor) polynomialProjection(samples []GpuMemorySample) (growthRate float64, projectedPeak float64, confidence float64) {
	n := len(samples)
	if n < 3 {
		// Fall back to linear with fewer samples
		return p.linearProjection(samples)
	}

	firstTime := samples[0].Timestamp
	memoryTotal := float64(samples[0].MemoryTotal)

	// Quadratic fit: y = a + b*x + c*x^2
	// Using normal equations for least squares
	var sumX, sumY, sumXX, sumXXX, sumXXXX, sumXY, sumXXY float64
	for _, s := range samples {
		x := s.Timestamp.Sub(firstTime).Seconds()
		y := float64(s.MemoryUsed)
		xx := x * x
		sumX += x
		sumY += y
		sumXX += xx
		sumXXX += xx * x
		sumXXXX += xx * xx
		sumXY += x * y
		sumXXY += xx * y
	}

	nf := float64(n)

	// Solve the 3x3 system using Cramer's rule
	// | n     sumX   sumXX  | | a |   | sumY   |
	// | sumX  sumXX  sumXXX | | b | = | sumXY  |
	// | sumXX sumXXX sumXXXX| | c |   | sumXXY |

	det := nf*(sumXX*sumXXXX-sumXXX*sumXXX) -
		sumX*(sumX*sumXXXX-sumXXX*sumXX) +
		sumXX*(sumX*sumXXX-sumXX*sumXX)

	if math.Abs(det) < 1e-10 {
		return p.linearProjection(samples)
	}

	a := (sumY*(sumXX*sumXXXX-sumXXX*sumXXX) -
		sumX*(sumXY*sumXXXX-sumXXY*sumXXX) +
		sumXX*(sumXY*sumXXX-sumXXY*sumXX)) / det

	b := (nf*(sumXY*sumXXXX-sumXXY*sumXXX) -
		sumY*(sumX*sumXXXX-sumXXX*sumXX) +
		sumXX*(sumX*sumXXY-sumXY*sumXX)) / det

	c := (nf*(sumXX*sumXXY-sumXXX*sumXY) -
		sumX*(sumX*sumXXY-sumXY*sumXX) +
		sumY*(sumX*sumXXX-sumXX*sumXX)) / det

	// Current growth rate (derivative at latest point)
	latestTime := samples[n-1].Timestamp.Sub(firstTime).Seconds()
	currentGrowthRate := b + 2*c*latestTime

	// Project 1 hour into the future
	projectionTime := latestTime + 3600.0
	projectedMB := a + b*projectionTime + c*projectionTime*projectionTime
	projectedPeakPct := projectedMB / memoryTotal * 100.0

	// Cap unreasonable values
	if projectedPeakPct > 200.0 {
		projectedPeakPct = 200.0
	}
	if projectedPeakPct < 0 {
		projectedPeakPct = 0
	}

	// Calculate R-squared
	meanY := sumY / nf
	var ssRes, ssTot float64
	for _, s := range samples {
		x := s.Timestamp.Sub(firstTime).Seconds()
		y := float64(s.MemoryUsed)
		predicted := a + b*x + c*x*x
		ssRes += (y - predicted) * (y - predicted)
		ssTot += (y - meanY) * (y - meanY)
	}

	rSquared := 0.0
	if ssTot > 0 {
		rSquared = 1.0 - ssRes/ssTot
	}
	if rSquared < 0 {
		rSquared = 0
	}

	return currentGrowthRate, projectedPeakPct, rSquared
}

// AnalyzeUtilization returns a utilization analysis for a given GPU
func (p *Predictor) AnalyzeUtilization(gpuKey string) *UtilizationAnalysis {
	p.mu.RLock()
	defer p.mu.RUnlock()

	samples := p.samples[gpuKey]
	if len(samples) == 0 {
		return nil
	}

	analysis := &UtilizationAnalysis{
		SampleCount: len(samples),
	}

	var totalUtil, peakUtil float64
	var validSamples int
	for _, s := range samples {
		if s.MemoryTotal <= 0 {
			continue
		}
		validSamples++
		utilPct := float64(s.MemoryUsed) / float64(s.MemoryTotal) * 100.0
		totalUtil += utilPct
		if utilPct > peakUtil {
			peakUtil = utilPct
		}
	}

	if validSamples == 0 {
		return nil
	}
	analysis.AverageUtilization = totalUtil / float64(validSamples)
	analysis.PeakUtilization = peakUtil

	// Steady-state utilization: use median of the middle 60% of samples (sorted by time)
	// This filters out startup spikes and temporary peaks
	if len(samples) >= 5 {
		start := len(samples) / 5   // skip first 20%
		end := len(samples) * 4 / 5 // skip last 20%
		steadySamples := samples[start:end]

		var steadyTotal float64
		for _, s := range steadySamples {
			if s.MemoryTotal > 0 {
				steadyTotal += float64(s.MemoryUsed) / float64(s.MemoryTotal) * 100.0
			}
		}
		analysis.SteadyStateUtilization = steadyTotal / float64(len(steadySamples))
	} else {
		analysis.SteadyStateUtilization = analysis.AverageUtilization
	}

	// Calculate variance
	var varianceSum float64
	for _, s := range samples {
		if s.MemoryTotal <= 0 {
			continue
		}
		utilPct := float64(s.MemoryUsed) / float64(s.MemoryTotal) * 100.0
		diff := utilPct - analysis.AverageUtilization
		varianceSum += diff * diff
	}
	analysis.Variance = varianceSum / float64(len(samples))

	return analysis
}

// pruneSamplesLocked removes samples older than maxAge. Must be called with mu held.
func (p *Predictor) pruneSamplesLocked(key string) {
	samples := p.samples[key]
	if len(samples) == 0 {
		return
	}

	cutoff := time.Now().Add(-p.maxAge)
	firstValid := 0
	for i, s := range samples {
		if s.Timestamp.After(cutoff) {
			firstValid = i
			break
		}
		if i == len(samples)-1 {
			// All samples are old, keep the last one
			firstValid = i
		}
	}

	if firstValid > 0 {
		p.samples[key] = samples[firstValid:]
	}
}

// Reset clears all recorded samples
func (p *Predictor) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.samples = make(map[string][]GpuMemorySample)
}

// GpuKeys returns all tracked GPU keys
func (p *Predictor) GpuKeys() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	keys := make([]string, 0, len(p.samples))
	for k := range p.samples {
		keys = append(keys, k)
	}
	return keys
}
