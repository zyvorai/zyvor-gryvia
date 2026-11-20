// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/experiment"
)

const (
	// Experiment phases
	ExperimentPhasePending   = "Pending"
	ExperimentPhaseRunning   = "Running"
	ExperimentPhaseCompleted = "Completed"
	ExperimentPhaseFailed    = "Failed"

	// Leaderboard entry statuses
	EntryStatusRunning         = "Running"
	EntryStatusTerminatedEarly = "TerminatedEarly"

	// Default cost per GPU-hour in dollars (H100 on-demand pricing approximation)
	defaultGPUHourCost = 3.50
)

// GryviaLiveExperimentReconciler reconciles a GryviaLiveExperiment object
type GryviaLiveExperimentReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Log       logr.Logger
	Clientset kubernetes.Interface
}

// k8sLogReader implements experiment.LogReader using the kubernetes clientset.
type k8sLogReader struct {
	clientset kubernetes.Interface
}

func (r *k8sLogReader) ReadLogs(ctx context.Context, namespace, podName string, tailLines int64) ([]byte, error) {
	req := r.clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		TailLines: &tailLines,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open log stream for pod %s/%s: %w", namespace, podName, err)
	}
	defer stream.Close()
	return io.ReadAll(stream)
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryvialiveexperiments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryvialiveexperiments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryvialiveexperiments/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods/log,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop
func (r *GryviaLiveExperimentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryvialiveexperiment", req.NamespacedName)

	// Fetch the GryviaLiveExperiment instance
	exp := &gryviav1.GryviaLiveExperiment{}
	err := r.Get(ctx, req.NamespacedName, exp)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaLiveExperiment resource not found, ignoring")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaLiveExperiment")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !exp.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Initialize status if needed
	if exp.Status.Phase == "" {
		exp.Status.Phase = ExperimentPhasePending
		if err := r.Status().Update(ctx, exp); err != nil {
			log.Error(err, "Failed to initialize experiment status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Don't reconcile completed or failed experiments
	if exp.Status.Phase == ExperimentPhaseCompleted || exp.Status.Phase == ExperimentPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Reconcile the experiment
	result, err := r.reconcileExperiment(ctx, exp)
	if err != nil {
		log.Error(err, "Failed to reconcile experiment")
		return result, err
	}

	return result, nil
}

func (r *GryviaLiveExperimentReconciler) reconcileExperiment(ctx context.Context, exp *gryviav1.GryviaLiveExperiment) (ctrl.Result, error) {
	log := r.Log.WithValues("experiment", exp.Name)

	// Track statuses and reasons for the leaderboard
	statuses := make(map[string]string)
	reasons := make(map[string]string)

	// Populate existing terminated statuses from current leaderboard
	for _, entry := range exp.Status.Leaderboard {
		if entry.Status == EntryStatusTerminatedEarly {
			statuses[entry.Job] = EntryStatusTerminatedEarly
			reasons[entry.Job] = entry.Reason
		}
	}

	// Fetch each referenced GryviaAIJob and collect metrics
	var scrapeResults []*experiment.ScrapeResult
	allJobsTerminal := true
	anyJobRunning := false
	totalGPUs := int32(0)

	// Build metric patterns from spec
	patterns := r.getMetricPatterns(exp)

	// Create log reader
	logReader := &k8sLogReader{clientset: r.Clientset}

	for _, jobDef := range exp.Spec.Jobs {
		jobRef := &gryviav1.GryviaAIJob{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: exp.Namespace,
			Name:      jobDef.JobRef,
		}, jobRef)
		if err != nil {
			if errors.IsNotFound(err) {
				log.Info("Referenced job not found", "jobRef", jobDef.JobRef)
				continue
			}
			return ctrl.Result{}, fmt.Errorf("failed to get job %s: %w", jobDef.JobRef, err)
		}

		totalGPUs += jobRef.Spec.GPUs

		// Check job phase
		switch jobRef.Status.Phase {
		case PhaseRunning:
			anyJobRunning = true
			allJobsTerminal = false
		case PhasePending, PhaseScheduling:
			allJobsTerminal = false
		case PhaseSucceeded, PhaseFailed:
			// Terminal - check if we terminated it
			if _, alreadyTerminated := statuses[jobDef.Name]; !alreadyTerminated {
				statuses[jobDef.Name] = EntryStatusRunning // completed naturally
			}
		}

		// Only scrape metrics from running or completed pods
		if jobRef.Status.Phase == PhaseRunning || jobRef.Status.Phase == PhaseSucceeded {
			// Find pods for this job
			pods := &corev1.PodList{}
			err := r.List(ctx, pods, client.InNamespace(exp.Namespace), client.MatchingLabels{
				"gryvia.io/job": jobDef.JobRef,
			})
			if err != nil {
				log.Error(err, "Failed to list pods for job", "job", jobDef.JobRef)
				continue
			}

			// Scrape metrics from each pod (use the first running pod)
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded {
					continue
				}

				result, err := experiment.ScrapeMetricsWithReader(ctx, logReader, pod, patterns)
				if err != nil {
					log.Error(err, "Failed to scrape metrics", "pod", pod.Name)
					continue
				}
				result.JobName = jobDef.Name
				scrapeResults = append(scrapeResults, result)

				// Detect anomalies
				if exp.Spec.Strategy != nil && exp.Spec.Strategy.AnomalyDetection != nil {
					ad := exp.Spec.Strategy.AnomalyDetection
					anomalies := experiment.DetectAnomalies(
						result,
						exp.Spec.Comparison.PrimaryMetric,
						ad.LossPlateauDetection,
						ad.LossDivergenceDetection,
						ad.GradientExplosionDetection,
					)
					if len(anomalies) > 0 {
						exp.Status.AnomaliesDetected += len(anomalies)
						for _, a := range anomalies {
							log.Info("Anomaly detected", "job", jobDef.Name, "anomaly", a)
						}
					}
				}

				break // One pod per job is sufficient
			}
		}
	}

	// Transition to Running if any job is running
	if anyJobRunning && exp.Status.Phase == ExperimentPhasePending {
		exp.Status.Phase = ExperimentPhaseRunning
		now := metav1.Now()
		exp.Status.StartTime = &now
	}

	// Check early termination for each job
	if exp.Spec.Strategy != nil && exp.Spec.Strategy.EarlyTermination != nil && exp.Spec.Strategy.EarlyTermination.Enabled {
		et := exp.Spec.Strategy.EarlyTermination
		minRunFraction := et.MinRunFraction
		if minRunFraction == 0 {
			minRunFraction = 0.25
		}
		confidenceLevel := et.ConfidenceLevel
		if confidenceLevel == 0 {
			confidenceLevel = 0.95
		}

		for _, result := range scrapeResults {
			// Skip already terminated jobs
			if s, ok := statuses[result.JobName]; ok && s == EntryStatusTerminatedEarly {
				continue
			}

			shouldTerminate, reason := experiment.ShouldTerminate(
				result,
				scrapeResults,
				exp.Spec.Comparison.PrimaryMetric,
				exp.Spec.Comparison.Direction,
				minRunFraction,
				confidenceLevel,
			)

			if shouldTerminate {
				log.Info("Early terminating job", "job", result.JobName, "reason", reason)

				// Find and terminate the job
				if err := r.terminateJob(ctx, exp, result.JobName, reason); err != nil {
					log.Error(err, "Failed to terminate job", "job", result.JobName)
					continue
				}

				statuses[result.JobName] = EntryStatusTerminatedEarly
				reasons[result.JobName] = reason
			}
		}
	}

	// Build secondary metrics for ranking
	var secondaryMetrics []experiment.SecondaryMetricDef
	for _, sm := range exp.Spec.Comparison.SecondaryMetrics {
		secondaryMetrics = append(secondaryMetrics, experiment.SecondaryMetricDef{
			Name:   sm.Name,
			Weight: sm.Weight,
		})
	}

	// Rank jobs and build leaderboard
	if len(scrapeResults) > 0 {
		ranked := experiment.RankJobs(
			scrapeResults,
			exp.Spec.Comparison.PrimaryMetric,
			exp.Spec.Comparison.Direction,
			secondaryMetrics,
			statuses,
			reasons,
		)

		leaderboard := make([]gryviav1.LeaderboardEntry, 0, len(ranked))
		for _, entry := range ranked {
			primaryVal := entry.PrimaryMetricValue
			if math.IsNaN(primaryVal) {
				primaryVal = 0
			}
			leaderboard = append(leaderboard, gryviav1.LeaderboardEntry{
				Rank:               entry.Rank,
				Job:                entry.JobName,
				PrimaryMetricValue: primaryVal,
				Status:             entry.Status,
				Reason:             entry.Reason,
			})
		}
		exp.Status.Leaderboard = leaderboard
	}

	// Calculate GPU hours saved from early terminations
	if exp.Status.StartTime != nil {
		terminatedCount := 0
		for _, s := range statuses {
			if s == EntryStatusTerminatedEarly {
				terminatedCount++
			}
		}
		if terminatedCount > 0 && len(exp.Spec.Jobs) > 0 {
			elapsedHours := time.Since(exp.Status.StartTime.Time).Hours()
			// Estimate: each terminated job saves the remaining run time worth of GPU hours
			// Conservative estimate: save 50% of remaining time per terminated job
			gpuPerJob := float64(totalGPUs) / float64(len(exp.Spec.Jobs))
			exp.Status.GPUHoursSaved = float64(terminatedCount) * gpuPerJob * elapsedHours * 0.5
			exp.Status.CostSaved = exp.Status.GPUHoursSaved * defaultGPUHourCost
		}
	}

	// Check if experiment is complete
	if allJobsTerminal && exp.Status.Phase == ExperimentPhaseRunning {
		exp.Status.Phase = ExperimentPhaseCompleted
	}

	// Update status
	if err := r.Status().Update(ctx, exp); err != nil {
		log.Error(err, "Failed to update experiment status")
		return ctrl.Result{}, err
	}

	// Requeue if experiment is still active
	if exp.Status.Phase == ExperimentPhaseRunning || exp.Status.Phase == ExperimentPhasePending {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// getMetricPatterns returns the metric regex patterns from the experiment spec.
// If no patterns are configured, it returns sensible defaults for common metrics.
func (r *GryviaLiveExperimentReconciler) getMetricPatterns(exp *gryviav1.GryviaLiveExperiment) map[string]string {
	if exp.Spec.Comparison.MetricSource != nil && exp.Spec.Comparison.MetricSource.Type == "log-pattern" {
		if len(exp.Spec.Comparison.MetricSource.Patterns) > 0 {
			return exp.Spec.Comparison.MetricSource.Patterns
		}
	}

	// Default patterns for common training metrics
	patterns := map[string]string{
		"loss":     `(?i)loss[=:\s]+([\d.]+)`,
		"accuracy": `(?i)accuracy[=:\s]+([\d.]+)`,
	}

	// Ensure primary metric has a pattern
	if _, ok := patterns[exp.Spec.Comparison.PrimaryMetric]; !ok {
		patterns[exp.Spec.Comparison.PrimaryMetric] = fmt.Sprintf(`(?i)%s[=:\s]+([\d.]+)`, exp.Spec.Comparison.PrimaryMetric)
	}

	// Ensure secondary metrics have patterns
	for _, sm := range exp.Spec.Comparison.SecondaryMetrics {
		if _, ok := patterns[sm.Name]; !ok {
			patterns[sm.Name] = fmt.Sprintf(`(?i)%s[=:\s]+([\d.]+)`, sm.Name)
		}
	}

	return patterns
}

// terminateJob patches the referenced GryviaAIJob to Failed status with the
// given reason, causing the ai-operator to stop the workload.
func (r *GryviaLiveExperimentReconciler) terminateJob(ctx context.Context, exp *gryviav1.GryviaLiveExperiment, jobName string, reason string) error {
	// Find the jobRef for this friendly name
	var jobRef string
	for _, j := range exp.Spec.Jobs {
		if j.Name == jobName {
			jobRef = j.JobRef
			break
		}
	}
	if jobRef == "" {
		return fmt.Errorf("no jobRef found for job name %q", jobName)
	}

	job := &gryviav1.GryviaAIJob{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: exp.Namespace,
		Name:      jobRef,
	}, job)
	if err != nil {
		return fmt.Errorf("failed to get job %s: %w", jobRef, err)
	}

	// Patch the job status to Failed
	job.Status.Phase = PhaseFailed
	job.Status.Message = fmt.Sprintf("Early terminated by experiment %s: %s", exp.Name, reason)
	now := metav1.Now()
	job.Status.CompletionTime = &now

	if err := r.Status().Update(ctx, job); err != nil {
		return fmt.Errorf("failed to update job %s status: %w", jobRef, err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaLiveExperimentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaLiveExperiment{}).
		Complete(r)
}
