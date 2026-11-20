// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package profiler

import (
	"fmt"
	"math"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// Severity levels for recommendations
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Recommendation categories
const (
	CategoryBatchSize               = "batchSize"
	CategoryDataLoading             = "dataLoading"
	CategoryMixedPrecision          = "mixedPrecision"
	CategoryCompilationOptimization = "compilationOptimization"
	CategoryDistributedStrategy     = "distributedStrategy"
	CategoryGpuTypeRecommendation   = "gpuTypeRecommendation"
)

// GpuMetrics represents collected GPU performance metrics for a single job
type GpuMetrics struct {
	// JobName is the name of the GryviaAIJob
	JobName string

	// GpuType is the GPU model (H100, A100, V100, T4)
	GpuType string

	// GpuCount is the number of GPUs used
	GpuCount int32

	// SMUtilization is the streaming multiprocessor utilization (0-100%)
	SMUtilization float64

	// TensorCoreUtilization is the tensor core utilization (0-100%)
	TensorCoreUtilization float64

	// AchievedTFLOPS is the measured TFLOPS during profiling
	AchievedTFLOPS float64

	// MemoryBandwidthUtilization is HBM bandwidth utilization (0-100%)
	MemoryBandwidthUtilization float64

	// PeakMemoryUsageGB is peak GPU memory usage in GB
	PeakMemoryUsageGB float64

	// TotalMemoryGB is total GPU memory in GB
	TotalMemoryGB float64

	// IoWaitRatio is the fraction of time GPU waits for data (0-1)
	IoWaitRatio float64

	// DataloaderThroughput is data loading speed in samples/sec
	DataloaderThroughput float64

	// NcclBandwidthGBps is NCCL collective bandwidth in GB/s
	NcclBandwidthGBps float64

	// AllReduceTimeFraction is fraction of step time in allreduce (0-1)
	AllReduceTimeFraction float64

	// ComputeCommOverlap is the compute-communication overlap fraction (0-1)
	ComputeCommOverlap float64

	// IsDistributed indicates if the job uses distributed training
	IsDistributed bool

	// UsesMixedPrecision indicates if mixed precision is enabled
	UsesMixedPrecision bool

	// UsesCompilation indicates if torch.compile or XLA is enabled
	UsesCompilation bool
}

// GpuBaseline represents expected performance baselines for a GPU type
type GpuBaseline struct {
	// PeakTFLOPS is the theoretical peak TFLOPS (FP16/BF16 with tensor cores)
	PeakTFLOPS float64

	// PeakMemoryBandwidthGBps is the peak HBM bandwidth in GB/s
	PeakMemoryBandwidthGBps float64

	// MemoryGB is the total GPU memory in GB
	MemoryGB float64

	// GoodMFU is the MFU threshold considered good for this GPU
	GoodMFU float64

	// GoodSMUtil is the SM utilization threshold considered good
	GoodSMUtil float64

	// NcclExpectedBandwidthGBps is expected NCCL bandwidth for NVLink interconnect
	NcclExpectedBandwidthGBps float64

	// CostRelative is relative cost factor (1.0 = baseline)
	CostRelative float64
}

// Recommendation represents an actionable profiling recommendation
type Recommendation struct {
	// Category is the recommendation category
	Category string

	// Severity is the recommendation severity level
	Severity string

	// Description is a human-readable explanation
	Description string

	// EstimatedImpact is the estimated performance improvement
	EstimatedImpact string
}

// AnalysisResult contains the complete analysis output for a job
type AnalysisResult struct {
	// JobName is the job that was analyzed
	JobName string

	// MFU is the calculated Model FLOPS Utilization (0-100%)
	MFU float64

	// EfficiencyScore is an overall efficiency score (0-100)
	EfficiencyScore float64

	// Recommendations is the list of actionable recommendations
	Recommendations []Recommendation
}

// knownBaselines contains performance baselines for known GPU types
var knownBaselines = map[string]GpuBaseline{
	"H100": {
		PeakTFLOPS:                989.0, // FP16 Tensor Core
		PeakMemoryBandwidthGBps:   3350.0,
		MemoryGB:                  80.0,
		GoodMFU:                   45.0,
		GoodSMUtil:                80.0,
		NcclExpectedBandwidthGBps: 450.0, // NVSwitch 4th gen
		CostRelative:              4.0,
	},
	"A100": {
		PeakTFLOPS:                312.0, // FP16 Tensor Core
		PeakMemoryBandwidthGBps:   2039.0,
		MemoryGB:                  80.0,
		GoodMFU:                   40.0,
		GoodSMUtil:                75.0,
		NcclExpectedBandwidthGBps: 300.0, // NVSwitch 3rd gen
		CostRelative:              2.5,
	},
	"V100": {
		PeakTFLOPS:                125.0, // FP16 Tensor Core
		PeakMemoryBandwidthGBps:   900.0,
		MemoryGB:                  32.0,
		GoodMFU:                   35.0,
		GoodSMUtil:                70.0,
		NcclExpectedBandwidthGBps: 150.0, // NVLink 2
		CostRelative:              1.0,
	},
	"T4": {
		PeakTFLOPS:                65.0, // FP16 Tensor Core
		PeakMemoryBandwidthGBps:   320.0,
		MemoryGB:                  16.0,
		GoodMFU:                   30.0,
		GoodSMUtil:                65.0,
		NcclExpectedBandwidthGBps: 0.0, // No NVLink
		CostRelative:              0.3,
	},
}

// CalculateMFU computes Model FLOPS Utilization as a percentage.
// MFU = (achieved TFLOPS / peak TFLOPS) * 100
func CalculateMFU(achievedTFLOPS float64, gpuType string, gpuCount int32) float64 {
	baseline, ok := knownBaselines[gpuType]
	if !ok {
		// Unknown GPU: use A100 as default baseline
		baseline = knownBaselines["A100"]
	}

	totalPeakTFLOPS := baseline.PeakTFLOPS * float64(gpuCount)
	if totalPeakTFLOPS == 0 {
		return 0
	}

	mfu := (achievedTFLOPS / totalPeakTFLOPS) * 100
	return math.Min(mfu, 100.0)
}

// AnalyzeGpuEfficiency takes GPU metrics and produces a complete analysis with recommendations.
// The config parameter controls which recommendation categories are enabled and the
// minimum severity threshold.
func AnalyzeGpuEfficiency(metrics GpuMetrics, config *gryviav1.ProfilerAnalysis) *AnalysisResult {
	result := &AnalysisResult{
		JobName: metrics.JobName,
	}

	baseline, ok := knownBaselines[metrics.GpuType]
	if !ok {
		baseline = knownBaselines["A100"]
	}

	// Calculate MFU
	result.MFU = CalculateMFU(metrics.AchievedTFLOPS, metrics.GpuType, metrics.GpuCount)

	// Calculate overall efficiency score as weighted average of key metrics
	result.EfficiencyScore = calculateEfficiencyScore(metrics, baseline)

	// Determine severity threshold
	threshold := SeverityWarning
	if config != nil && config.SeverityThreshold != "" {
		threshold = config.SeverityThreshold
	}

	// Generate recommendations
	var recs []Recommendation

	recConfig := defaultRecommendationConfig()
	if config != nil && config.Recommendations != nil {
		recConfig = config.Recommendations
	}

	if recConfig.BatchSize {
		recs = append(recs, analyzeBatchSize(metrics, baseline)...)
	}
	if recConfig.DataLoading {
		recs = append(recs, analyzeDataLoading(metrics)...)
	}
	if recConfig.MixedPrecision {
		recs = append(recs, analyzeMixedPrecision(metrics, baseline)...)
	}
	if recConfig.CompilationOptimization {
		recs = append(recs, analyzeCompilation(metrics)...)
	}
	if recConfig.DistributedStrategy {
		recs = append(recs, analyzeDistributed(metrics, baseline)...)
	}
	if recConfig.GpuTypeRecommendation {
		recs = append(recs, analyzeGpuType(metrics, baseline)...)
	}

	// Filter by severity threshold
	result.Recommendations = filterBySeverity(recs, threshold)

	// Sort recommendations: critical first, then warning, then info
	sort.Slice(result.Recommendations, func(i, j int) bool {
		return severityRank(result.Recommendations[i].Severity) > severityRank(result.Recommendations[j].Severity)
	})

	return result
}

// ToProfilerRecommendations converts analysis results to the CRD status type
func ToProfilerRecommendations(results []*AnalysisResult, maxRecommendations int) []gryviav1.ProfilerRecommendation {
	var all []gryviav1.ProfilerRecommendation
	now := metav1.Now()

	for _, result := range results {
		for _, rec := range result.Recommendations {
			all = append(all, gryviav1.ProfilerRecommendation{
				JobName:         result.JobName,
				Category:        rec.Category,
				Severity:        rec.Severity,
				Description:     rec.Description,
				EstimatedImpact: rec.EstimatedImpact,
				Timestamp:       now,
			})
		}
	}

	// Sort by severity (critical first)
	sort.Slice(all, func(i, j int) bool {
		return severityRank(all[i].Severity) > severityRank(all[j].Severity)
	})

	if len(all) > maxRecommendations {
		all = all[:maxRecommendations]
	}

	return all
}

// AverageEfficiency calculates mean efficiency score across multiple results
func AverageEfficiency(results []*AnalysisResult) float64 {
	if len(results) == 0 {
		return 0
	}
	sum := 0.0
	for _, r := range results {
		sum += r.EfficiencyScore
	}
	return sum / float64(len(results))
}

// AverageMFU calculates mean MFU across multiple results
func AverageMFU(results []*AnalysisResult) float64 {
	if len(results) == 0 {
		return 0
	}
	sum := 0.0
	for _, r := range results {
		sum += r.MFU
	}
	return sum / float64(len(results))
}

// calculateEfficiencyScore produces a 0-100 score from weighted metrics
func calculateEfficiencyScore(metrics GpuMetrics, baseline GpuBaseline) float64 {
	// Weights for different components
	const (
		weightSM         = 0.25
		weightTensorCore = 0.25
		weightMemBW      = 0.15
		weightDataPipe   = 0.15
		weightComm       = 0.10
		weightMemUsage   = 0.10
	)

	score := 0.0

	// SM utilization component (normalize against good threshold)
	if baseline.GoodSMUtil > 0 {
		smScore := math.Min(metrics.SMUtilization/baseline.GoodSMUtil*100, 100)
		score += smScore * weightSM
	}

	// Tensor core utilization component
	tcScore := metrics.TensorCoreUtilization
	score += tcScore * weightTensorCore

	// Memory bandwidth component
	if baseline.PeakMemoryBandwidthGBps > 0 {
		mbScore := metrics.MemoryBandwidthUtilization
		score += mbScore * weightMemBW
	}

	// Data pipeline component (low IO wait = good)
	dataPipeScore := (1.0 - metrics.IoWaitRatio) * 100
	if dataPipeScore < 0 {
		dataPipeScore = 0
	}
	score += dataPipeScore * weightDataPipe

	// Communication component (only for distributed)
	if metrics.IsDistributed {
		commScore := metrics.ComputeCommOverlap * 100
		if baseline.NcclExpectedBandwidthGBps > 0 && metrics.NcclBandwidthGBps > 0 {
			bwRatio := metrics.NcclBandwidthGBps / baseline.NcclExpectedBandwidthGBps
			commScore = (commScore + math.Min(bwRatio*100, 100)) / 2
		}
		score += commScore * weightComm
	} else {
		// Redistribute communication weight to other factors
		score += 100 * weightComm * 0.5
	}

	// Memory usage component (using 60-90% of memory is optimal)
	if metrics.TotalMemoryGB > 0 {
		memRatio := metrics.PeakMemoryUsageGB / metrics.TotalMemoryGB
		var memScore float64
		switch {
		case memRatio < 0.3:
			memScore = memRatio / 0.3 * 60 // Under-utilizing memory
		case memRatio <= 0.9:
			memScore = 60 + (memRatio-0.3)/0.6*40 // Sweet spot
		default:
			memScore = 100 - (memRatio-0.9)/0.1*30 // Risk of OOM
		}
		if memScore < 0 {
			memScore = 0
		}
		score += memScore * weightMemUsage
	}

	return math.Round(score*100) / 100
}

func analyzeBatchSize(metrics GpuMetrics, baseline GpuBaseline) []Recommendation {
	var recs []Recommendation

	// Low SM utilization + low memory usage suggests batch size is too small
	if metrics.TotalMemoryGB > 0 {
		memRatio := metrics.PeakMemoryUsageGB / metrics.TotalMemoryGB
		if metrics.SMUtilization < baseline.GoodSMUtil*0.6 && memRatio < 0.5 {
			headroom := metrics.TotalMemoryGB - metrics.PeakMemoryUsageGB
			recs = append(recs, Recommendation{
				Category:        CategoryBatchSize,
				Severity:        SeverityCritical,
				Description:     fmt.Sprintf("GPU memory only %.0f%% utilized (%.1fGB free of %.1fGB) with SM utilization at %.1f%%. Increase batch size to improve throughput.", memRatio*100, headroom, metrics.TotalMemoryGB, metrics.SMUtilization),
				EstimatedImpact: fmt.Sprintf("%.0f-%.0f%% throughput improvement", (1-memRatio)*30, (1-memRatio)*60),
			})
		} else if metrics.SMUtilization < baseline.GoodSMUtil*0.8 && memRatio < 0.6 {
			recs = append(recs, Recommendation{
				Category:        CategoryBatchSize,
				Severity:        SeverityWarning,
				Description:     fmt.Sprintf("SM utilization (%.1f%%) below target (%.1f%%) with memory headroom available. Consider increasing batch size.", metrics.SMUtilization, baseline.GoodSMUtil),
				EstimatedImpact: "10-25% throughput improvement",
			})
		}
	}

	// Very high memory usage suggests batch size is at the edge
	if metrics.TotalMemoryGB > 0 {
		memRatio := metrics.PeakMemoryUsageGB / metrics.TotalMemoryGB
		if memRatio > 0.95 {
			recs = append(recs, Recommendation{
				Category:        CategoryBatchSize,
				Severity:        SeverityWarning,
				Description:     fmt.Sprintf("GPU memory usage at %.0f%% (%.1fGB/%.1fGB). Risk of OOM. Consider gradient accumulation or reducing batch size slightly.", memRatio*100, metrics.PeakMemoryUsageGB, metrics.TotalMemoryGB),
				EstimatedImpact: "Prevents OOM crashes and training interruptions",
			})
		}
	}

	return recs
}

func analyzeDataLoading(metrics GpuMetrics) []Recommendation {
	var recs []Recommendation

	// High IO wait means GPU is starved for data
	if metrics.IoWaitRatio > 0.3 {
		recs = append(recs, Recommendation{
			Category:        CategoryDataLoading,
			Severity:        SeverityCritical,
			Description:     fmt.Sprintf("GPU idle %.0f%% of the time waiting for data. Data loading is the bottleneck.", metrics.IoWaitRatio*100),
			EstimatedImpact: fmt.Sprintf("Up to %.0f%% throughput improvement by fixing data pipeline", metrics.IoWaitRatio*80),
		})
	} else if metrics.IoWaitRatio > 0.1 {
		recs = append(recs, Recommendation{
			Category:        CategoryDataLoading,
			Severity:        SeverityWarning,
			Description:     fmt.Sprintf("GPU spends %.0f%% of time waiting for data. Increase num_workers or enable prefetching.", metrics.IoWaitRatio*100),
			EstimatedImpact: fmt.Sprintf("%.0f-%.0f%% throughput improvement", metrics.IoWaitRatio*30, metrics.IoWaitRatio*60),
		})
	} else if metrics.IoWaitRatio > 0.05 {
		recs = append(recs, Recommendation{
			Category:        CategoryDataLoading,
			Severity:        SeverityInfo,
			Description:     fmt.Sprintf("Minor data loading overhead detected (%.1f%% IO wait). Consider pin_memory=True or persistent_workers=True.", metrics.IoWaitRatio*100),
			EstimatedImpact: "2-5% throughput improvement",
		})
	}

	return recs
}

func analyzeMixedPrecision(metrics GpuMetrics, baseline GpuBaseline) []Recommendation {
	var recs []Recommendation

	if metrics.UsesMixedPrecision {
		// Mixed precision is enabled; check if tensor cores are being used effectively
		if metrics.TensorCoreUtilization < 30 {
			recs = append(recs, Recommendation{
				Category:        CategoryMixedPrecision,
				Severity:        SeverityWarning,
				Description:     fmt.Sprintf("Mixed precision is enabled but tensor core utilization is only %.1f%%. Check that model operations are compatible with FP16/BF16 (avoid custom kernels that bypass tensor cores).", metrics.TensorCoreUtilization),
				EstimatedImpact: "20-40% throughput improvement by fixing tensor core usage",
			})
		}
		return recs
	}

	// Mixed precision not enabled; recommend it
	if metrics.TensorCoreUtilization < 10 {
		expectedSpeedup := "1.5-2x"
		if metrics.GpuType == "H100" || metrics.GpuType == "A100" {
			expectedSpeedup = "2-3x"
		}
		recs = append(recs, Recommendation{
			Category:        CategoryMixedPrecision,
			Severity:        SeverityCritical,
			Description:     fmt.Sprintf("Tensor cores are barely used (%.1f%% utilization). Enable mixed precision training (AMP) to leverage FP16/BF16 tensor core acceleration on %s.", metrics.TensorCoreUtilization, metrics.GpuType),
			EstimatedImpact: fmt.Sprintf("%s throughput improvement with AMP", expectedSpeedup),
		})
	}

	return recs
}

func analyzeCompilation(metrics GpuMetrics) []Recommendation {
	var recs []Recommendation

	if metrics.UsesCompilation {
		return recs // Already using compilation
	}

	// Recommend torch.compile if SM utilization is moderate and no compilation is used
	if metrics.SMUtilization > 40 && metrics.SMUtilization < 80 {
		recs = append(recs, Recommendation{
			Category:        CategoryCompilationOptimization,
			Severity:        SeverityInfo,
			Description:     fmt.Sprintf("SM utilization at %.1f%% without graph compilation. torch.compile() can fuse operations and reduce kernel launch overhead for an additional speedup.", metrics.SMUtilization),
			EstimatedImpact: "10-30% throughput improvement with torch.compile",
		})
	} else if metrics.SMUtilization >= 80 {
		recs = append(recs, Recommendation{
			Category:        CategoryCompilationOptimization,
			Severity:        SeverityInfo,
			Description:     "Good SM utilization. torch.compile() may still help by fusing kernels and reducing memory traffic.",
			EstimatedImpact: "5-15% throughput improvement with torch.compile",
		})
	}

	return recs
}

func analyzeDistributed(metrics GpuMetrics, baseline GpuBaseline) []Recommendation {
	var recs []Recommendation

	if !metrics.IsDistributed {
		// Only recommend distributed training if using multiple GPUs makes sense
		if metrics.GpuCount == 1 && metrics.TotalMemoryGB > 0 {
			memRatio := metrics.PeakMemoryUsageGB / metrics.TotalMemoryGB
			if memRatio > 0.85 {
				recs = append(recs, Recommendation{
					Category:        CategoryDistributedStrategy,
					Severity:        SeverityInfo,
					Description:     fmt.Sprintf("Single GPU is nearly out of memory (%.0f%% used). Consider multi-GPU data parallelism or model parallelism (FSDP/DeepSpeed ZeRO) to train larger batches or larger models.", memRatio*100),
					EstimatedImpact: "Enables larger models and batch sizes",
				})
			}
		}
		return recs
	}

	// Distributed training analysis

	// Check NCCL bandwidth
	if baseline.NcclExpectedBandwidthGBps > 0 && metrics.NcclBandwidthGBps > 0 {
		bwRatio := metrics.NcclBandwidthGBps / baseline.NcclExpectedBandwidthGBps
		if bwRatio < 0.3 {
			recs = append(recs, Recommendation{
				Category:        CategoryDistributedStrategy,
				Severity:        SeverityCritical,
				Description:     fmt.Sprintf("NCCL bandwidth is only %.1f GB/s (%.0f%% of expected %.1f GB/s for %s). Check NVLink/NVSwitch topology, NCCL environment variables, and network configuration.", metrics.NcclBandwidthGBps, bwRatio*100, baseline.NcclExpectedBandwidthGBps, metrics.GpuType),
				EstimatedImpact: fmt.Sprintf("%.0f-%.0f%% reduction in communication time", (1-bwRatio)*40, (1-bwRatio)*70),
			})
		} else if bwRatio < 0.6 {
			recs = append(recs, Recommendation{
				Category:        CategoryDistributedStrategy,
				Severity:        SeverityWarning,
				Description:     fmt.Sprintf("NCCL bandwidth at %.1f GB/s (%.0f%% of expected). Consider enabling NCCL_NET_GDR_LEVEL=5 or using RDMA network.", metrics.NcclBandwidthGBps, bwRatio*100),
				EstimatedImpact: "15-30% communication time reduction",
			})
		}
	}

	// Check communication overhead
	if metrics.AllReduceTimeFraction > 0.4 {
		recs = append(recs, Recommendation{
			Category:        CategoryDistributedStrategy,
			Severity:        SeverityCritical,
			Description:     fmt.Sprintf("AllReduce takes %.0f%% of step time. Communication is dominating training. Consider gradient compression, increasing batch size per GPU, or switching to a pipeline parallel strategy.", metrics.AllReduceTimeFraction*100),
			EstimatedImpact: "20-50% throughput improvement",
		})
	} else if metrics.AllReduceTimeFraction > 0.2 {
		recs = append(recs, Recommendation{
			Category:        CategoryDistributedStrategy,
			Severity:        SeverityWarning,
			Description:     fmt.Sprintf("AllReduce takes %.0f%% of step time. Enable compute-communication overlap or use gradient bucketing to hide latency.", metrics.AllReduceTimeFraction*100),
			EstimatedImpact: "10-25% throughput improvement",
		})
	}

	// Check compute-communication overlap
	if metrics.AllReduceTimeFraction > 0.15 && metrics.ComputeCommOverlap < 0.5 {
		recs = append(recs, Recommendation{
			Category:        CategoryDistributedStrategy,
			Severity:        SeverityWarning,
			Description:     fmt.Sprintf("Compute-communication overlap is only %.0f%%. Enable overlap in DDP (set find_unused_parameters=False) or use FSDP with limit_all_gathers=True.", metrics.ComputeCommOverlap*100),
			EstimatedImpact: "10-20% throughput improvement",
		})
	}

	return recs
}

func analyzeGpuType(metrics GpuMetrics, currentBaseline GpuBaseline) []Recommendation {
	var recs []Recommendation

	// Only make GPU type recommendations if we have meaningful data
	if metrics.AchievedTFLOPS == 0 {
		return recs
	}

	mfu := CalculateMFU(metrics.AchievedTFLOPS, metrics.GpuType, metrics.GpuCount)

	// If MFU is very low on expensive GPUs, suggest a cheaper alternative
	if metrics.GpuType == "H100" && mfu < 15 {
		recs = append(recs, Recommendation{
			Category:        CategoryGpuTypeRecommendation,
			Severity:        SeverityWarning,
			Description:     fmt.Sprintf("MFU is only %.1f%% on H100, which is very low for this GPU class. If the workload is memory-bandwidth-bound rather than compute-bound, A100 may deliver similar performance at lower cost.", mfu),
			EstimatedImpact: "40-60% cost reduction with similar throughput on A100",
		})
	}

	if metrics.GpuType == "A100" && mfu < 10 {
		recs = append(recs, Recommendation{
			Category:        CategoryGpuTypeRecommendation,
			Severity:        SeverityInfo,
			Description:     fmt.Sprintf("MFU is only %.1f%% on A100. For inference or small-model fine-tuning workloads, T4 or L40 may be more cost-effective.", mfu),
			EstimatedImpact: "50-70% cost reduction for inference/fine-tuning workloads",
		})
	}

	// If memory is the bottleneck, suggest higher-memory GPU
	if metrics.TotalMemoryGB > 0 && metrics.TotalMemoryGB < 80 {
		memRatio := metrics.PeakMemoryUsageGB / metrics.TotalMemoryGB
		if memRatio > 0.9 && (metrics.GpuType == "V100" || metrics.GpuType == "T4") {
			recs = append(recs, Recommendation{
				Category:        CategoryGpuTypeRecommendation,
				Severity:        SeverityWarning,
				Description:     fmt.Sprintf("GPU memory is %.0f%% full on %s (%.0fGB). Upgrading to A100 (80GB) would allow larger batch sizes and models without memory constraints.", memRatio*100, metrics.GpuType, metrics.TotalMemoryGB),
				EstimatedImpact: "Enables 2-5x larger batch sizes; potential 2x throughput improvement",
			})
		}
	}

	return recs
}

// filterBySeverity filters recommendations to only include those at or above the threshold
func filterBySeverity(recs []Recommendation, threshold string) []Recommendation {
	thresholdRank := severityRank(threshold)
	var filtered []Recommendation
	for _, r := range recs {
		if severityRank(r.Severity) >= thresholdRank {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// severityRank returns a numeric rank for severity comparison
func severityRank(severity string) int {
	switch severity {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	case SeverityInfo:
		return 1
	default:
		return 0
	}
}

// defaultRecommendationConfig returns a config with all recommendations enabled
func defaultRecommendationConfig() *gryviav1.RecommendationConfig {
	return &gryviav1.RecommendationConfig{
		BatchSize:               true,
		DataLoading:             true,
		MixedPrecision:          true,
		CompilationOptimization: true,
		DistributedStrategy:     true,
		GpuTypeRecommendation:   true,
	}
}

// ExtractGpuMetricsFromAnnotations extracts GPU metrics from pod annotations.
// These annotations are expected to be set by a monitoring sidecar or the
// gryvia GPU operator.
func ExtractGpuMetricsFromAnnotations(annotations map[string]string, jobName, gpuType string, gpuCount int32, isDistributed bool) GpuMetrics {
	m := GpuMetrics{
		JobName:       jobName,
		GpuType:       gpuType,
		GpuCount:      gpuCount,
		IsDistributed: isDistributed,
	}

	m.SMUtilization = parseFloatAnnotation(annotations, "gryvia.io/gpu-sm-utilization")
	m.TensorCoreUtilization = parseFloatAnnotation(annotations, "gryvia.io/gpu-tensor-utilization")
	m.AchievedTFLOPS = parseFloatAnnotation(annotations, "gryvia.io/gpu-tflops")
	m.MemoryBandwidthUtilization = parseFloatAnnotation(annotations, "gryvia.io/gpu-memory-bw-utilization")
	m.PeakMemoryUsageGB = parseFloatAnnotation(annotations, "gryvia.io/gpu-peak-memory-gb")
	m.TotalMemoryGB = parseFloatAnnotation(annotations, "gryvia.io/gpu-total-memory-gb")
	m.IoWaitRatio = parseFloatAnnotation(annotations, "gryvia.io/gpu-io-wait-ratio")
	m.DataloaderThroughput = parseFloatAnnotation(annotations, "gryvia.io/dataloader-throughput")
	m.NcclBandwidthGBps = parseFloatAnnotation(annotations, "gryvia.io/nccl-bandwidth-gbps")
	m.AllReduceTimeFraction = parseFloatAnnotation(annotations, "gryvia.io/allreduce-time-fraction")
	m.ComputeCommOverlap = parseFloatAnnotation(annotations, "gryvia.io/compute-comm-overlap")

	// Check for mixed precision and compilation flags
	if v, ok := annotations["gryvia.io/mixed-precision"]; ok && v == "true" {
		m.UsesMixedPrecision = true
	}
	if v, ok := annotations["gryvia.io/torch-compile"]; ok && v == "true" {
		m.UsesCompilation = true
	}

	// Set default total memory from baseline if not annotated
	if m.TotalMemoryGB == 0 {
		if baseline, ok := knownBaselines[gpuType]; ok {
			m.TotalMemoryGB = baseline.MemoryGB
		}
	}

	return m
}

// parseFloatAnnotation safely parses a float64 from an annotation value
func parseFloatAnnotation(annotations map[string]string, key string) float64 {
	v, ok := annotations[key]
	if !ok || v == "" {
		return 0
	}
	var f float64
	_, err := fmt.Sscanf(v, "%f", &f)
	if err != nil {
		return 0
	}
	return f
}

// ShouldProfile determines if a job should be profiled based on cooldown
// and the job's current training step.
func ShouldProfile(lastProfileTime *metav1.Time, cooldownMinutes int32, currentStep int32, warmupSteps int32) bool {
	// Must be past warmup
	if currentStep < warmupSteps {
		return false
	}

	// If never profiled, profile now
	if lastProfileTime == nil || lastProfileTime.IsZero() {
		return true
	}

	// Check cooldown
	elapsed := time.Since(lastProfileTime.Time)
	cooldown := time.Duration(cooldownMinutes) * time.Minute
	return elapsed >= cooldown
}
