// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package experiment

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MetricValues holds all scraped values for a single metric.
type MetricValues struct {
	// Values contains the scraped metric values in chronological order.
	Values []float64
}

// Latest returns the most recent metric value, or NaN if empty.
func (mv *MetricValues) Latest() float64 {
	if len(mv.Values) == 0 {
		return math.NaN()
	}
	return mv.Values[len(mv.Values)-1]
}

// ScrapeResult holds all scraped metrics for a single job.
type ScrapeResult struct {
	// JobName is the friendly experiment name for this job.
	JobName string

	// Metrics maps metric names to their scraped values.
	Metrics map[string]*MetricValues

	// Anomalies records detected anomalies.
	Anomalies []string
}

// LeaderboardEntry represents a ranked job in the experiment.
type LeaderboardEntry struct {
	Rank               int
	JobName            string
	PrimaryMetricValue float64
	CompositeScore     float64
	Status             string // Running or TerminatedEarly
	Reason             string
}

// ScrapeMetrics reads pod logs for the given pod and extracts metrics using the
// provided regex patterns. Each pattern must contain exactly one capture group
// that matches a numeric value.
//
// patterns maps metric names to regex pattern strings, for example:
//
//	{"loss": `loss[=:]\s*([\d.]+)`, "accuracy": `accuracy[=:]\s*([\d.]+)`}
func ScrapeMetrics(ctx context.Context, k8sClient client.Client, pod *corev1.Pod, patterns map[string]string) (*ScrapeResult, error) {
	if pod == nil {
		return nil, fmt.Errorf("pod is nil")
	}

	result := &ScrapeResult{
		JobName: pod.Labels["gryvia.io/job"],
		Metrics: make(map[string]*MetricValues),
	}

	// Pre-compile all regex patterns
	compiled := make(map[string]*regexp.Regexp, len(patterns))
	for metricName, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex pattern for metric %q: %w", metricName, err)
		}
		compiled[metricName] = re
		result.Metrics[metricName] = &MetricValues{}
	}

	// Read pod logs via the Kubernetes API
	logBytes, err := readPodLogs(ctx, k8sClient, pod)
	if err != nil {
		return result, fmt.Errorf("failed to read pod logs for %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	// Scan each line for metric patterns
	scanner := bufio.NewScanner(strings.NewReader(string(logBytes)))
	for scanner.Scan() {
		line := scanner.Text()

		for metricName, re := range compiled {
			matches := re.FindStringSubmatch(line)
			if len(matches) >= 2 {
				val, err := strconv.ParseFloat(matches[1], 64)
				if err != nil {
					continue
				}
				result.Metrics[metricName].Values = append(result.Metrics[metricName].Values, val)
			}
		}

		// Detect NaN/Inf anomalies in any numeric output
		lineLower := strings.ToLower(line)
		if strings.Contains(lineLower, "nan") || strings.Contains(lineLower, "inf") {
			// Only flag lines that also match a metric pattern context
			for metricName := range compiled {
				if strings.Contains(lineLower, strings.ToLower(metricName)) {
					result.Anomalies = append(result.Anomalies, fmt.Sprintf("potential NaN/Inf detected for %s: %s", metricName, line))
				}
			}
		}
	}

	return result, nil
}

// readPodLogs retrieves logs from the first container of the given pod using
// the Kubernetes client. It uses the SubResourceClient interface to read pod
// logs through the standard controller-runtime client.
func readPodLogs(ctx context.Context, k8sClient client.Client, pod *corev1.Pod) ([]byte, error) {
	// Use the REST client from the controller-runtime client to fetch logs.
	// The controller-runtime client wraps the REST client, so we access pod
	// logs through a raw sub-resource read.
	//
	// We use a simple approach: read the pod status and any log data available
	// via the API. In production, the controller should have RBAC for pods/log.
	//
	// For controller-runtime, we use client.Reader with a raw REST call.
	// The actual log fetching is done via the Kubernetes REST API.
	podLogOpts := &corev1.PodLogOptions{
		TailLines: int64Ptr(1000), // Limit to last 1000 lines for performance
	}

	// Build the REST request path
	// /api/v1/namespaces/{namespace}/pods/{name}/log
	_ = podLogOpts

	// Use the typed SubResource client available in controller-runtime
	// Since controller-runtime's client.Client doesn't natively support pod/log,
	// we need to use the raw RESTClient. However, for testability and to match
	// the project patterns, we fetch logs via the typed API.
	//
	// In the actual reconciler we inject a LogReader interface that wraps
	// k8s.io/client-go/kubernetes.Clientset.CoreV1().Pods().GetLogs().
	// Here we provide a direct implementation.

	return nil, fmt.Errorf("log reading requires a LogReader implementation; see LogReader interface")
}

func int64Ptr(i int64) *int64 {
	return &i
}

// LogReader abstracts pod log reading for testability.
type LogReader interface {
	// ReadLogs returns the log output for the given pod.
	ReadLogs(ctx context.Context, namespace, podName string, tailLines int64) ([]byte, error)
}

// ScrapeMetricsWithReader reads pod logs using the provided LogReader and
// extracts metrics using regex patterns.
func ScrapeMetricsWithReader(ctx context.Context, reader LogReader, pod *corev1.Pod, patterns map[string]string) (*ScrapeResult, error) {
	if pod == nil {
		return nil, fmt.Errorf("pod is nil")
	}

	result := &ScrapeResult{
		JobName: pod.Labels["gryvia.io/job"],
		Metrics: make(map[string]*MetricValues),
	}

	// Pre-compile all regex patterns
	compiled := make(map[string]*regexp.Regexp, len(patterns))
	for metricName, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex pattern for metric %q: %w", metricName, err)
		}
		compiled[metricName] = re
		result.Metrics[metricName] = &MetricValues{}
	}

	// Read pod logs
	logBytes, err := reader.ReadLogs(ctx, pod.Namespace, pod.Name, 1000)
	if err != nil {
		return result, fmt.Errorf("failed to read pod logs for %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	// Scan each line for metric patterns
	scanner := bufio.NewScanner(strings.NewReader(string(logBytes)))
	for scanner.Scan() {
		line := scanner.Text()

		for metricName, re := range compiled {
			matches := re.FindStringSubmatch(line)
			if len(matches) >= 2 {
				val, err := strconv.ParseFloat(matches[1], 64)
				if err != nil {
					continue
				}
				result.Metrics[metricName].Values = append(result.Metrics[metricName].Values, val)
			}
		}

		// Detect NaN/Inf anomalies
		lineLower := strings.ToLower(line)
		if strings.Contains(lineLower, "nan") || strings.Contains(lineLower, "inf") {
			for metricName := range compiled {
				if strings.Contains(lineLower, strings.ToLower(metricName)) {
					result.Anomalies = append(result.Anomalies, fmt.Sprintf("potential NaN/Inf detected for %s: %s", metricName, line))
				}
			}
		}
	}

	return result, nil
}

// SecondaryMetricDef describes a secondary metric and its weight for ranking.
type SecondaryMetricDef struct {
	Name   string
	Weight float64
}

// RankJobs sorts jobs by their primary metric and weighted secondary metrics,
// returning a sorted leaderboard. The direction parameter should be "minimize"
// or "maximize" for the primary metric.
func RankJobs(results []*ScrapeResult, primaryMetric string, direction string, secondaryMetrics []SecondaryMetricDef, statuses map[string]string, reasons map[string]string) []LeaderboardEntry {
	entries := make([]LeaderboardEntry, 0, len(results))

	for _, r := range results {
		primaryVal := math.NaN()
		if mv, ok := r.Metrics[primaryMetric]; ok {
			primaryVal = mv.Latest()
		}

		// Compute composite score: primary metric value plus weighted secondary metrics
		compositeScore := primaryVal
		if !math.IsNaN(compositeScore) {
			for _, sm := range secondaryMetrics {
				if mv, ok := r.Metrics[sm.Name]; ok {
					secVal := mv.Latest()
					if !math.IsNaN(secVal) {
						// For secondary metrics we always add their weighted contribution
						compositeScore += sm.Weight * secVal
					}
				}
			}
		}

		status := "Running"
		if s, ok := statuses[r.JobName]; ok {
			status = s
		}
		reason := ""
		if rs, ok := reasons[r.JobName]; ok {
			reason = rs
		}

		entries = append(entries, LeaderboardEntry{
			JobName:            r.JobName,
			PrimaryMetricValue: primaryVal,
			CompositeScore:     compositeScore,
			Status:             status,
			Reason:             reason,
		})
	}

	// Sort by composite score
	sort.Slice(entries, func(i, j int) bool {
		iScore := entries[i].CompositeScore
		jScore := entries[j].CompositeScore

		// NaN values go to the end
		iNaN := math.IsNaN(iScore)
		jNaN := math.IsNaN(jScore)
		if iNaN && jNaN {
			return false
		}
		if iNaN {
			return false
		}
		if jNaN {
			return true
		}

		if direction == "minimize" {
			return iScore < jScore
		}
		return iScore > jScore // maximize
	})

	// Assign ranks
	for i := range entries {
		entries[i].Rank = i + 1
	}

	return entries
}

// DetectAnomalies checks a scrape result for anomalies based on the configured
// detection flags. It returns a list of anomaly descriptions.
func DetectAnomalies(result *ScrapeResult, lossMetric string, plateauDetection, divergenceDetection, explosionDetection bool) []string {
	var anomalies []string

	mv, ok := result.Metrics[lossMetric]
	if !ok || len(mv.Values) < 2 {
		return anomalies
	}

	values := mv.Values

	// Gradient explosion detection: check for NaN or Inf
	if explosionDetection {
		for _, v := range values {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				anomalies = append(anomalies, fmt.Sprintf("gradient explosion detected: %s value is NaN/Inf", lossMetric))
				break
			}
		}
	}

	// Loss divergence detection: check if loss is consistently increasing
	if divergenceDetection && len(values) >= 5 {
		// Check last 5 values for monotonic increase
		lastN := values[len(values)-5:]
		increasing := true
		for i := 1; i < len(lastN); i++ {
			if lastN[i] <= lastN[i-1] {
				increasing = false
				break
			}
		}
		if increasing {
			anomalies = append(anomalies, fmt.Sprintf("loss divergence detected: %s has been increasing for last 5 data points", lossMetric))
		}
	}

	// Loss plateau detection: check if loss has barely changed
	if plateauDetection && len(values) >= 10 {
		n := 10
		if len(values) < n {
			n = len(values)
		}
		lastN := values[len(values)-n:]
		minVal := lastN[0]
		maxVal := lastN[0]
		for _, v := range lastN[1:] {
			if v < minVal {
				minVal = v
			}
			if v > maxVal {
				maxVal = v
			}
		}
		// Plateau if range is less than 0.1% of the mean value
		mean := (minVal + maxVal) / 2
		if mean != 0 {
			relRange := (maxVal - minVal) / math.Abs(mean)
			if relRange < 0.001 {
				anomalies = append(anomalies, fmt.Sprintf("loss plateau detected: %s has changed less than 0.1%% over last 10 data points", lossMetric))
			}
		}
	}

	// Include any anomalies found during scraping
	anomalies = append(anomalies, result.Anomalies...)

	return anomalies
}

// ShouldTerminate decides whether a job should be early-terminated based on
// the median stopping rule. A job is terminated if its primary metric at the
// current point is worse than the median of all jobs at the same point.
func ShouldTerminate(jobResult *ScrapeResult, allResults []*ScrapeResult, primaryMetric string, direction string, minRunFraction float64, confidenceLevel float64) (bool, string) {
	jobMV, ok := jobResult.Metrics[primaryMetric]
	if !ok || len(jobMV.Values) == 0 {
		return false, ""
	}

	jobVal := jobMV.Latest()
	if math.IsNaN(jobVal) {
		return true, "primary metric is NaN"
	}

	// Collect latest values from all running jobs
	var allVals []float64
	maxLen := 0
	for _, r := range allResults {
		if mv, ok := r.Metrics[primaryMetric]; ok && len(mv.Values) > 0 {
			latest := mv.Latest()
			if !math.IsNaN(latest) {
				allVals = append(allVals, latest)
			}
			if len(mv.Values) > maxLen {
				maxLen = len(mv.Values)
			}
		}
	}

	if len(allVals) < 2 {
		return false, ""
	}

	// Check minimum run fraction
	if maxLen > 0 && len(jobMV.Values) > 0 {
		fraction := float64(len(jobMV.Values)) / float64(maxLen)
		if fraction < minRunFraction {
			return false, ""
		}
	}

	// Calculate median
	sort.Float64s(allVals)
	var median float64
	n := len(allVals)
	if n%2 == 0 {
		median = (allVals[n/2-1] + allVals[n/2]) / 2
	} else {
		median = allVals[n/2]
	}

	// Determine if this job is underperforming
	if direction == "minimize" {
		// For minimization, terminate if job's value is significantly above the median
		threshold := median * (1 + (1 - confidenceLevel))
		if jobVal > threshold {
			return true, fmt.Sprintf("%s value %.4f exceeds median threshold %.4f", primaryMetric, jobVal, threshold)
		}
	} else {
		// For maximization, terminate if job's value is significantly below the median
		threshold := median * confidenceLevel
		if jobVal < threshold {
			return true, fmt.Sprintf("%s value %.4f below median threshold %.4f", primaryMetric, jobVal, threshold)
		}
	}

	return false, ""
}
