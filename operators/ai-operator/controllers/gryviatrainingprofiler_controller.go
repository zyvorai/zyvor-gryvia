// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/profiler"
)

const (
	// Maximum number of top recommendations to keep in status
	maxTopRecommendations = 20

	// Default reconcile interval for profiler
	profilerRequeueInterval = 60 * time.Second

	// Annotation keys
	annotationLastProfileTime = "gryvia.io/last-profile-time"
)

// GryviaTrainingProfilerReconciler reconciles a GryviaTrainingProfiler object
type GryviaTrainingProfilerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Log      logr.Logger
	Recorder EventRecorder
}

// EventRecorder is an interface for recording Kubernetes events
type EventRecorder interface {
	Event(object runtime.Object, eventtype, reason, message string)
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{})
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatrainingprofilers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatrainingprofilers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatrainingprofilers/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *GryviaTrainingProfilerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviatrainingprofiler", req.NamespacedName)

	// Fetch the GryviaTrainingProfiler instance
	fp := &gryviav1.GryviaTrainingProfiler{}
	err := r.Get(ctx, req.NamespacedName, fp)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaTrainingProfiler resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaTrainingProfiler")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !fp.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Find matching GryviaAIJob resources
	jobs, err := r.findMatchingJobs(ctx, fp)
	if err != nil {
		log.Error(err, "Failed to find matching jobs")
		r.updateCondition(fp, "Ready", metav1.ConditionFalse, "JobDiscoveryFailed", err.Error())
		if updateErr := r.Status().Update(ctx, fp); updateErr != nil {
			log.Error(updateErr, "Failed to update status after job discovery failure")
		}
		return ctrl.Result{RequeueAfter: profilerRequeueInterval}, err
	}

	if len(jobs) == 0 {
		log.Info("No matching jobs found for profiling")
		r.updateCondition(fp, "Ready", metav1.ConditionTrue, "NoJobs", "No matching jobs found for profiling")
		if updateErr := r.Status().Update(ctx, fp); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: profilerRequeueInterval}, nil
	}

	// Profile each eligible running job
	var analysisResults []*profiler.AnalysisResult
	jobsProfiledCount := int32(0)

	for i := range jobs {
		job := &jobs[i]

		// Only profile running jobs
		if job.Status.Phase != PhaseRunning {
			continue
		}

		// Check if profiling is needed based on cooldown and step count
		if !r.shouldProfileJob(fp, job) {
			continue
		}

		// Collect GPU metrics from pod annotations
		metrics, err := r.collectJobMetrics(ctx, fp, job)
		if err != nil {
			log.Error(err, "Failed to collect metrics for job", "job", job.Name)
			continue
		}

		// Run analysis and generate recommendations
		result := profiler.AnalyzeGpuEfficiency(*metrics, &fp.Spec.Analysis)
		analysisResults = append(analysisResults, result)
		jobsProfiledCount++

		log.Info("Profiled job",
			"job", job.Name,
			"mfu", fmt.Sprintf("%.1f%%", result.MFU),
			"efficiency", fmt.Sprintf("%.1f", result.EfficiencyScore),
			"recommendations", len(result.Recommendations),
		)

		// Emit events for recommendations if configured
		if fp.Spec.Output.EmitEvents && r.Recorder != nil {
			r.emitRecommendationEvents(fp, job, result)
		}

		// Store profiling results in job status if configured
		if fp.Spec.Output.StoreInJobStatus {
			r.updateJobWithProfilingResults(ctx, log, job, result)
		}

		// Mark job as profiled by updating the annotation
		r.markJobProfiled(ctx, log, job)
	}

	// Update GryviaTrainingProfiler status
	if len(analysisResults) > 0 {
		fp.Status.JobsProfiled += jobsProfiledCount
		fp.Status.AvgMFU = profiler.AverageMFU(analysisResults)
		fp.Status.ClusterEfficiencyScore = profiler.AverageEfficiency(analysisResults)

		// Merge new recommendations with existing ones, keeping the most recent
		newRecs := profiler.ToProfilerRecommendations(analysisResults, maxTopRecommendations)
		fp.Status.TopRecommendations = mergeRecommendations(fp.Status.TopRecommendations, newRecs, maxTopRecommendations)

		now := metav1.Now()
		fp.Status.LastProfileTime = &now

		r.updateCondition(fp, "Ready", metav1.ConditionTrue, "ProfilingComplete",
			fmt.Sprintf("Profiled %d jobs, avg MFU: %.1f%%, efficiency: %.1f",
				jobsProfiledCount, fp.Status.AvgMFU, fp.Status.ClusterEfficiencyScore))
	} else {
		r.updateCondition(fp, "Ready", metav1.ConditionTrue, "NoEligibleJobs",
			"No jobs eligible for profiling at this time")
	}

	if err := r.Status().Update(ctx, fp); err != nil {
		log.Error(err, "Failed to update GryviaTrainingProfiler status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: profilerRequeueInterval}, nil
}

// findMatchingJobs discovers GryviaAIJob resources matching the profiler target configuration
func (r *GryviaTrainingProfilerReconciler) findMatchingJobs(ctx context.Context, fp *gryviav1.GryviaTrainingProfiler) ([]gryviav1.GryviaAIJob, error) {
	switch fp.Spec.Target.Type {
	case "job-ref":
		// Target a specific job by name
		if fp.Spec.Target.JobRef == "" {
			return nil, fmt.Errorf("target type is job-ref but jobRef is empty")
		}
		job := &gryviav1.GryviaAIJob{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: fp.Namespace,
			Name:      fp.Spec.Target.JobRef,
		}, job)
		if err != nil {
			if errors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		return []gryviav1.GryviaAIJob{*job}, nil

	case "on-demand":
		// On-demand mode still uses the job selector, but only runs when triggered
		// (the controller reconciles when the CR is updated, acting as the trigger)
		return r.listJobsBySelector(ctx, fp)

	default: // "auto" or empty
		return r.listJobsBySelector(ctx, fp)
	}
}

// listJobsBySelector lists GryviaAIJob resources matching the label selector
func (r *GryviaTrainingProfilerReconciler) listJobsBySelector(ctx context.Context, fp *gryviav1.GryviaTrainingProfiler) ([]gryviav1.GryviaAIJob, error) {
	jobList := &gryviav1.GryviaAIJobList{}

	listOpts := []client.ListOption{
		client.InNamespace(fp.Namespace),
	}

	if len(fp.Spec.Target.JobSelector) > 0 {
		listOpts = append(listOpts, client.MatchingLabels(fp.Spec.Target.JobSelector))
	}

	if err := r.List(ctx, jobList, listOpts...); err != nil {
		return nil, fmt.Errorf("failed to list GryviaAIJobs: %w", err)
	}

	return jobList.Items, nil
}

// shouldProfileJob checks if a specific job should be profiled based on warmup steps and cooldown
func (r *GryviaTrainingProfilerReconciler) shouldProfileJob(fp *gryviav1.GryviaTrainingProfiler, job *gryviav1.GryviaAIJob) bool {
	warmupSteps := int32(100)
	cooldownMinutes := int32(30)

	if fp.Spec.Target.AutoProfile != nil {
		if fp.Spec.Target.AutoProfile.WarmupSteps > 0 {
			warmupSteps = fp.Spec.Target.AutoProfile.WarmupSteps
		}
		if fp.Spec.Target.AutoProfile.CooldownMinutes > 0 {
			cooldownMinutes = fp.Spec.Target.AutoProfile.CooldownMinutes
		}
	}

	// Get current training step from job metrics
	currentStep := int32(0)
	if job.Status.Metrics != nil {
		currentStep = job.Status.Metrics.Step
	}

	// Get last profile time from job annotation
	var lastProfileTime *metav1.Time
	if job.Annotations != nil {
		if timeStr, ok := job.Annotations[annotationLastProfileTime]; ok {
			t, err := time.Parse(time.RFC3339, timeStr)
			if err == nil {
				mt := metav1.NewTime(t)
				lastProfileTime = &mt
			}
		}
	}

	return profiler.ShouldProfile(lastProfileTime, cooldownMinutes, currentStep, warmupSteps)
}

// collectJobMetrics gathers GPU metrics from pod annotations for a job
func (r *GryviaTrainingProfilerReconciler) collectJobMetrics(ctx context.Context, fp *gryviav1.GryviaTrainingProfiler, job *gryviav1.GryviaAIJob) (*profiler.GpuMetrics, error) {
	// Find pods belonging to this job
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"gryvia.io/job": job.Name},
	); err != nil {
		return nil, fmt.Errorf("failed to list pods for job %s: %w", job.Name, err)
	}

	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pods found for job %s", job.Name)
	}

	// Aggregate metrics from all pods
	var (
		totalSMUtil        float64
		totalTensorUtil    float64
		totalTFLOPS        float64
		totalMemBWUtil     float64
		totalPeakMem       float64
		totalMem           float64
		totalIoWait        float64
		totalDataloaderTP  float64
		totalNcclBW        float64
		totalAllReduceTime float64
		totalCommOverlap   float64
		podCount           float64
		usesMixedPrecision bool
		usesCompilation    bool
	)

	gpuType := job.Spec.GpuType
	if gpuType == "" || gpuType == "any" {
		gpuType = "A100" // default assumption
	}

	isDistributed := job.Spec.Distributed != nil && job.Spec.Distributed.Enabled

	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		annotations := pod.Annotations
		if annotations == nil {
			continue
		}

		m := profiler.ExtractGpuMetricsFromAnnotations(annotations, job.Name, gpuType, job.Spec.GPUs, isDistributed)

		totalSMUtil += m.SMUtilization
		totalTensorUtil += m.TensorCoreUtilization
		totalTFLOPS += m.AchievedTFLOPS
		totalMemBWUtil += m.MemoryBandwidthUtilization
		totalPeakMem += m.PeakMemoryUsageGB
		totalMem += m.TotalMemoryGB
		totalIoWait += m.IoWaitRatio
		totalDataloaderTP += m.DataloaderThroughput
		totalNcclBW += m.NcclBandwidthGBps
		totalAllReduceTime += m.AllReduceTimeFraction
		totalCommOverlap += m.ComputeCommOverlap

		if m.UsesMixedPrecision {
			usesMixedPrecision = true
		}
		if m.UsesCompilation {
			usesCompilation = true
		}

		podCount++
	}

	if podCount == 0 {
		return nil, fmt.Errorf("no running pods with annotations found for job %s", job.Name)
	}

	// Compute GPU count: if distributed, total GPUs across all pods
	gpuCount := job.Spec.GPUs
	if isDistributed && job.Spec.Distributed.GpusPerNode > 0 && job.Spec.Distributed.Nodes > 0 {
		gpuCount = job.Spec.Distributed.GpusPerNode * job.Spec.Distributed.Nodes
	}

	metrics := &profiler.GpuMetrics{
		JobName:                    job.Name,
		GpuType:                    gpuType,
		GpuCount:                   gpuCount,
		SMUtilization:              totalSMUtil / podCount,
		TensorCoreUtilization:      totalTensorUtil / podCount,
		AchievedTFLOPS:             totalTFLOPS, // sum across pods for total cluster TFLOPS
		MemoryBandwidthUtilization: totalMemBWUtil / podCount,
		PeakMemoryUsageGB:          totalPeakMem / podCount, // average per GPU
		TotalMemoryGB:              totalMem / podCount,
		IoWaitRatio:                totalIoWait / podCount,
		DataloaderThroughput:       totalDataloaderTP, // sum across pods
		NcclBandwidthGBps:          totalNcclBW / podCount,
		AllReduceTimeFraction:      totalAllReduceTime / podCount,
		ComputeCommOverlap:         totalCommOverlap / podCount,
		IsDistributed:              isDistributed,
		UsesMixedPrecision:         usesMixedPrecision,
		UsesCompilation:            usesCompilation,
	}

	return metrics, nil
}

// emitRecommendationEvents emits Kubernetes events for profiling recommendations
func (r *GryviaTrainingProfilerReconciler) emitRecommendationEvents(fp *gryviav1.GryviaTrainingProfiler, job *gryviav1.GryviaAIJob, result *profiler.AnalysisResult) {
	for _, rec := range result.Recommendations {
		eventType := corev1.EventTypeNormal
		if rec.Severity == profiler.SeverityCritical {
			eventType = corev1.EventTypeWarning
		}

		reason := fmt.Sprintf("Profiler%s", capitalize(rec.Category))
		message := fmt.Sprintf("[%s] %s (impact: %s)", rec.Severity, rec.Description, rec.EstimatedImpact)

		r.Recorder.Event(fp, eventType, reason, fmt.Sprintf("Job %s: %s", job.Name, message))
	}

	// Emit summary event
	r.Recorder.Eventf(fp, corev1.EventTypeNormal, "ProfilingComplete",
		"Job %s profiled: MFU=%.1f%%, Efficiency=%.1f, %d recommendations",
		job.Name, result.MFU, result.EfficiencyScore, len(result.Recommendations))
}

// updateJobWithProfilingResults updates the GryviaAIJob status with profiling data
func (r *GryviaTrainingProfilerReconciler) updateJobWithProfilingResults(ctx context.Context, log logr.Logger, job *gryviav1.GryviaAIJob, result *profiler.AnalysisResult) {
	// Update GPU utilization in job metrics
	if job.Status.Metrics == nil {
		job.Status.Metrics = &gryviav1.JobMetrics{}
	}
	job.Status.Metrics.GpuUtilization = result.EfficiencyScore

	if err := r.Status().Update(ctx, job); err != nil {
		log.Error(err, "Failed to update job status with profiling results", "job", job.Name)
	}
}

// markJobProfiled sets the last profile timestamp annotation on the job
func (r *GryviaTrainingProfilerReconciler) markJobProfiled(ctx context.Context, log logr.Logger, job *gryviav1.GryviaAIJob) {
	if job.Annotations == nil {
		job.Annotations = make(map[string]string)
	}
	job.Annotations[annotationLastProfileTime] = time.Now().Format(time.RFC3339)

	if err := r.Update(ctx, job); err != nil {
		log.Error(err, "Failed to annotate job with profile time", "job", job.Name)
	}
}

// updateCondition updates or appends a condition on the profiler status
func (r *GryviaTrainingProfilerReconciler) updateCondition(fp *gryviav1.GryviaTrainingProfiler, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: fp.Generation,
		LastTransitionTime: metav1.Now(),
	}

	// Preserve LastTransitionTime when status hasn't changed
	for _, cond := range fp.Status.Conditions {
		if cond.Type == condType && cond.Status == status {
			condition.LastTransitionTime = cond.LastTransitionTime
			break
		}
	}

	found := false
	for i, cond := range fp.Status.Conditions {
		if cond.Type == condType {
			fp.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		fp.Status.Conditions = append(fp.Status.Conditions, condition)
	}
}

// mergeRecommendations merges new recommendations with existing ones,
// keeping the most recent entries up to maxCount
func mergeRecommendations(existing, new []gryviav1.ProfilerRecommendation, maxCount int) []gryviav1.ProfilerRecommendation {
	// Prepend new recommendations (most recent first)
	merged := append(new, existing...)
	if len(merged) > maxCount {
		merged = merged[:maxCount]
	}
	return merged
}

// capitalize returns the string with its first letter uppercased
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// mapJobToProfilers maps a GryviaAIJob to its matching GryviaTrainingProfiler CRs
// so that changes to jobs trigger profiler reconciliation
func (r *GryviaTrainingProfilerReconciler) mapJobToProfilers(ctx context.Context, obj client.Object) []reconcile.Request {
	job, ok := obj.(*gryviav1.GryviaAIJob)
	if !ok {
		return nil
	}

	// List all profilers in the same namespace
	profilerList := &gryviav1.GryviaTrainingProfilerList{}
	if err := r.List(ctx, profilerList, client.InNamespace(job.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, p := range profilerList.Items {
		match := false

		switch p.Spec.Target.Type {
		case "job-ref":
			match = p.Spec.Target.JobRef == job.Name
		default:
			// Check if job labels match the selector
			match = labelsMatch(job.Labels, p.Spec.Target.JobSelector)
		}

		if match {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      p.Name,
					Namespace: p.Namespace,
				},
			})
		}
	}

	return requests
}

// labelsMatch checks if objectLabels contain all entries from selector
func labelsMatch(objectLabels, selector map[string]string) bool {
	if len(selector) == 0 {
		return true
	}
	for k, v := range selector {
		if objectLabels[k] != v {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaTrainingProfilerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaTrainingProfiler{}).
		Watches(
			&gryviav1.GryviaAIJob{},
			handler.EnqueueRequestsFromMapFunc(r.mapJobToProfilers),
		).
		Complete(r)
}
