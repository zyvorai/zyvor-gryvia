package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

type BenchmarkResult struct {
	Test                string  `json:"test"`
	TotalJobs           int     `json:"total_jobs"`
	SuccessfulJobs      int     `json:"successful_jobs"`
	FailedJobs          int     `json:"failed_jobs"`
	AvgTimeToScheduleMs int64   `json:"avg_time_to_schedule_ms"`
	AvgTimeToCompleteMs int64   `json:"avg_time_to_complete_ms"`
	TotalDurationMs     int64   `json:"total_duration_ms"`
	JobsPerSecond       float64 `json:"jobs_per_second"`
}

type JobMetrics struct {
	Name           string
	Created        time.Time
	Scheduled      time.Time
	Completed      time.Time
	TimeToSchedule time.Duration
	TimeToComplete time.Duration
	Success        bool
}

var dynamicClient dynamic.Interface

func main() {
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		fmt.Printf("Error building kubeconfig: %v\n", err)
		os.Exit(1)
	}

	dynamicClient, err = dynamic.NewForConfig(config)
	if err != nil {
		fmt.Printf("Error creating dynamic client: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Gryvia Performance Benchmark")
	fmt.Println("=================================")

	// Run benchmarks
	results := []BenchmarkResult{}

	// Benchmark 1: Sequential job submission
	fmt.Println("\n[1/4] Running sequential job submission benchmark...")
	result1 := benchmarkSequentialSubmission(10)
	results = append(results, result1)
	printResult(result1)

	// Benchmark 2: Parallel job submission
	fmt.Println("\n[2/4] Running parallel job submission benchmark...")
	result2 := benchmarkParallelSubmission(20, 5)
	results = append(results, result2)
	printResult(result2)

	// Benchmark 3: Burst submission
	fmt.Println("\n[3/4] Running burst submission benchmark...")
	result3 := benchmarkBurstSubmission(50)
	results = append(results, result3)
	printResult(result3)

	// Benchmark 4: Large-scale submission
	fmt.Println("\n[4/4] Running large-scale submission benchmark...")
	result4 := benchmarkLargeScale(100, 10)
	results = append(results, result4)
	printResult(result4)

	// Save results to JSON
	saveResults(results)

	fmt.Println("\n=================================")
	fmt.Println("Benchmark complete!")
	fmt.Println("Results saved to benchmark_results.json")
}

func benchmarkSequentialSubmission(count int) BenchmarkResult {
	ctx := context.Background()
	namespace := "gryvia-bench"

	startTime := time.Now()
	metrics := []JobMetrics{}

	for i := 0; i < count; i++ {
		jobName := fmt.Sprintf("seq-bench-%d", i)
		metric := submitAndTrackJob(ctx, namespace, jobName)
		metrics = append(metrics, metric)
	}

	return calculateResults("Sequential Submission", metrics, time.Since(startTime))
}

func benchmarkParallelSubmission(count, parallelism int) BenchmarkResult {
	ctx := context.Background()
	namespace := "gryvia-bench"

	startTime := time.Now()
	metrics := []JobMetrics{}
	metricsMu := sync.Mutex{}
	wg := sync.WaitGroup{}

	sem := make(chan struct{}, parallelism)

	for i := 0; i < count; i++ {
		wg.Add(1)
		sem <- struct{}{}

		go func(index int) {
			defer wg.Done()
			defer func() { <-sem }()

			jobName := fmt.Sprintf("par-bench-%d", index)
			metric := submitAndTrackJob(ctx, namespace, jobName)

			metricsMu.Lock()
			metrics = append(metrics, metric)
			metricsMu.Unlock()
		}(i)
	}

	wg.Wait()

	return calculateResults("Parallel Submission", metrics, time.Since(startTime))
}

func benchmarkBurstSubmission(count int) BenchmarkResult {
	ctx := context.Background()
	namespace := "gryvia-bench"

	startTime := time.Now()
	metrics := []JobMetrics{}
	metricsMu := sync.Mutex{}
	wg := sync.WaitGroup{}

	// Submit all jobs with bounded concurrency to avoid API server overload
	sem := make(chan struct{}, 20)
	for i := 0; i < count; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(index int) {
			defer wg.Done()
			defer func() { <-sem }()
			jobName := fmt.Sprintf("burst-bench-%d", index)
			metric := submitAndTrackJob(ctx, namespace, jobName)

			metricsMu.Lock()
			metrics = append(metrics, metric)
			metricsMu.Unlock()
		}(i)
	}

	wg.Wait()

	return calculateResults("Burst Submission", metrics, time.Since(startTime))
}

func benchmarkLargeScale(count, parallelism int) BenchmarkResult {
	ctx := context.Background()
	namespace := "gryvia-bench"

	startTime := time.Now()
	metrics := []JobMetrics{}
	metricsMu := sync.Mutex{}
	wg := sync.WaitGroup{}

	sem := make(chan struct{}, parallelism)

	for i := 0; i < count; i++ {
		wg.Add(1)
		sem <- struct{}{}

		go func(index int) {
			defer wg.Done()
			defer func() { <-sem }()

			jobName := fmt.Sprintf("large-bench-%d", index)
			metric := submitAndTrackJob(ctx, namespace, jobName)

			metricsMu.Lock()
			metrics = append(metrics, metric)
			metricsMu.Unlock()
		}(i)
	}

	wg.Wait()

	return calculateResults("Large Scale", metrics, time.Since(startTime))
}

func submitAndTrackJob(ctx context.Context, namespace, jobName string) JobMetrics {
	gvr := schema.GroupVersionResource{
		Group:    "gryvia.io",
		Version:  "v1alpha1",
		Resource: "gryviaaijobs",
	}

	metric := JobMetrics{
		Name:    jobName,
		Created: time.Now(),
		Success: false,
	}

	job := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gryvia.io/v1alpha1",
			"kind":       "GryviaAIJob",
			"metadata": map[string]interface{}{
				"name":      jobName,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"type":    "pytorch",
				"gpus":    1,
				"gpuType": "H100",
				"image":   "nvcr.io/nvidia/pytorch:24.01-py3",
				"command": []string{
					"python", "-c", "import time; time.sleep(5); print('Benchmark job completed')",
				},
			},
		},
	}

	// Submit job
	_, err := dynamicClient.Resource(gvr).Namespace(namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("Error creating job %s: %v\n", jobName, err)
		return metric
	}

	// Track job progress
	timeout := time.After(5 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return metric
		case <-timeout:
			return metric
		case <-ticker.C:
			obj, err := dynamicClient.Resource(gvr).Namespace(namespace).Get(ctx, jobName, metav1.GetOptions{})
			if err != nil {
				continue
			}

			status, found, _ := unstructured.NestedMap(obj.Object, "status")
			if !found {
				continue
			}

			phase, _, _ := unstructured.NestedString(status, "phase")

			if phase == "Running" && metric.Scheduled.IsZero() {
				metric.Scheduled = time.Now()
				metric.TimeToSchedule = metric.Scheduled.Sub(metric.Created)
			}

			if phase == "Completed" {
				metric.Completed = time.Now()
				metric.TimeToComplete = metric.Completed.Sub(metric.Created)
				metric.Success = true

				// Clean up job
				dynamicClient.Resource(gvr).Namespace(namespace).Delete(ctx, jobName, metav1.DeleteOptions{})
				return metric
			}

			if phase == "Failed" {
				dynamicClient.Resource(gvr).Namespace(namespace).Delete(ctx, jobName, metav1.DeleteOptions{})
				return metric
			}
		}
	}
}

func calculateResults(testName string, metrics []JobMetrics, totalDuration time.Duration) BenchmarkResult {
	result := BenchmarkResult{
		Test:      testName,
		TotalJobs: len(metrics),
	}

	var totalScheduleTime, totalCompleteTime time.Duration
	scheduledCount := 0
	completedCount := 0

	for _, m := range metrics {
		if m.Success {
			result.SuccessfulJobs++
			completedCount++
			totalCompleteTime += m.TimeToComplete
		} else {
			result.FailedJobs++
		}

		if !m.Scheduled.IsZero() {
			scheduledCount++
			totalScheduleTime += m.TimeToSchedule
		}
	}

	if scheduledCount > 0 {
		result.AvgTimeToScheduleMs = (totalScheduleTime / time.Duration(scheduledCount)).Milliseconds()
	}

	if completedCount > 0 {
		result.AvgTimeToCompleteMs = (totalCompleteTime / time.Duration(completedCount)).Milliseconds()
	}

	result.TotalDurationMs = totalDuration.Milliseconds()
	if totalDuration.Seconds() > 0 {
		result.JobsPerSecond = float64(result.TotalJobs) / totalDuration.Seconds()
	}

	return result
}

func printResult(result BenchmarkResult) {
	fmt.Printf("\n%s Results:\n", result.Test)
	fmt.Printf("  Total Jobs:           %d\n", result.TotalJobs)
	fmt.Printf("  Successful:           %d\n", result.SuccessfulJobs)
	fmt.Printf("  Failed:               %d\n", result.FailedJobs)
	fmt.Printf("  Avg Time to Schedule: %dms\n", result.AvgTimeToScheduleMs)
	fmt.Printf("  Avg Time to Complete: %dms\n", result.AvgTimeToCompleteMs)
	fmt.Printf("  Total Duration:       %dms\n", result.TotalDurationMs)
	fmt.Printf("  Jobs/Second:          %.2f\n", result.JobsPerSecond)
}

func saveResults(results []BenchmarkResult) {
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		fmt.Printf("Error marshaling results: %v\n", err)
		return
	}

	err = os.WriteFile("benchmark_results.json", data, 0600)
	if err != nil {
		fmt.Printf("Error writing results: %v\n", err)
	}
}
