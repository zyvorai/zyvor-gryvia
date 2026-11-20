// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	"math"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/predictor"
)

const (
	// Annotation keys for cost/time estimates injected into jobs
	annotationEstimatedCost     = "gryvia.io/estimated-cost"
	annotationEstimatedDuration = "gryvia.io/estimated-duration"
	annotationEstimatedQueue    = "gryvia.io/estimated-queue-wait"
	annotationDryRun            = "gryvia.io/dry-run"
	annotationCostConfidence    = "gryvia.io/cost-confidence"

	// Annotation for tracking actual vs estimated for accuracy metrics
	annotationActualCost     = "gryvia.io/actual-cost"
	annotationActualDuration = "gryvia.io/actual-duration"
)

// GryviaCostPredictorReconciler reconciles a GryviaCostPredictor object
type GryviaCostPredictorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviacostpredictors,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviacostpredictors/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviacostpredictors/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch;update;patch

func (r *GryviaCostPredictorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the GryviaCostPredictor instance
	costPredictor := &gryviav1.GryviaCostPredictor{}
	err := r.Get(ctx, req.NamespacedName, costPredictor)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("GryviaCostPredictor resource not found, ignoring")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get GryviaCostPredictor")
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling GryviaCostPredictor", "name", costPredictor.Name)

	// List all GryviaAIJobs across all namespaces for historical data
	allJobs := &gryviav1.GryviaAIJobList{}
	if err := r.List(ctx, allJobs); err != nil {
		logger.Error(err, "Failed to list GryviaAIJobs")
		return ctrl.Result{}, err
	}

	// Process jobs that need estimates
	if err := r.processNewJobs(ctx, costPredictor, allJobs.Items); err != nil {
		logger.Error(err, "Failed to process new jobs")
		r.updateStatusCondition(ctx, costPredictor, "Ready", metav1.ConditionFalse, "ProcessingFailed", err.Error())
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Update prediction accuracy from completed jobs
	r.updateAccuracyMetrics(ctx, costPredictor, allJobs.Items)

	// Update status
	costPredictor.Status.LastUpdated = metav1.Now()
	r.updateStatusCondition(ctx, costPredictor, "Ready", metav1.ConditionTrue, "Active", "Cost predictor is active")

	if err := r.Status().Update(ctx, costPredictor); err != nil {
		logger.Error(err, "Failed to update GryviaCostPredictor status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
}

// processNewJobs finds jobs that need cost/time estimates and annotates them
func (r *GryviaCostPredictorReconciler) processNewJobs(
	ctx context.Context,
	costPredictor *gryviav1.GryviaCostPredictor,
	allJobs []gryviav1.GryviaAIJob,
) error {
	logger := log.FromContext(ctx)

	for i := range allJobs {
		job := &allJobs[i]

		// Skip jobs that already have estimates
		if job.Annotations != nil {
			if _, exists := job.Annotations[annotationEstimatedCost]; exists {
				continue
			}
		}

		// Process dry-run annotated jobs or pending jobs (if annotation injection is enabled)
		isDryRun := job.Annotations != nil && job.Annotations[annotationDryRun] == "true"
		isPending := job.Status.Phase == "Pending" || job.Status.Phase == ""

		if !isDryRun && !isPending {
			continue
		}

		if !isDryRun && !costPredictor.Spec.Integration.InjectEstimateAnnotation {
			continue
		}

		// Generate estimate
		estimate, err := predictor.EstimateJobCost(ctx, r.Client, costPredictor, job, allJobs)
		if err != nil {
			logger.Error(err, "Failed to estimate job cost", "job", job.Name)
			continue
		}

		// Annotate the job with estimates
		if job.Annotations == nil {
			job.Annotations = make(map[string]string)
		}

		job.Annotations[annotationEstimatedCost] = fmt.Sprintf("%.2f", estimate.EstimatedCost)
		job.Annotations[annotationEstimatedDuration] = estimate.EstimatedDuration.String()
		job.Annotations[annotationEstimatedQueue] = estimate.EstimatedQueueWait.String()
		job.Annotations[annotationCostConfidence] = fmt.Sprintf("%.2f", estimate.Confidence)

		// Generate alternative recommendations
		alternatives, err := predictor.GenerateAlternatives(ctx, r.Client, costPredictor, job, allJobs, estimate)
		if err != nil {
			logger.Error(err, "Failed to generate alternatives", "job", job.Name)
		} else if len(alternatives) > 0 {
			// Add the best cost-saving alternative as an annotation
			bestSaving := alternatives[0]
			for _, alt := range alternatives {
				if alt.CostSavings > bestSaving.CostSavings {
					bestSaving = alt
				}
			}
			if bestSaving.CostSavings > 0 {
				job.Annotations["gryvia.io/alternative-gpu"] = bestSaving.GPUType
				job.Annotations["gryvia.io/alternative-cost"] = fmt.Sprintf("%.2f", bestSaving.EstimatedCost)
				job.Annotations["gryvia.io/potential-savings"] = fmt.Sprintf("%.2f", bestSaving.CostSavings)
			}
		}

		// Update the job
		if !costPredictor.Spec.Integration.DryRunMode {
			if err := r.Update(ctx, job); err != nil {
				logger.Error(err, "Failed to annotate job with estimates", "job", job.Name)
				continue
			}
			logger.Info("Annotated job with cost estimate",
				"job", job.Name,
				"estimatedCost", estimate.EstimatedCost,
				"estimatedDuration", estimate.EstimatedDuration,
				"confidence", estimate.Confidence,
			)
		} else {
			logger.Info("Dry-run: would annotate job with cost estimate",
				"job", job.Name,
				"estimatedCost", estimate.EstimatedCost,
				"estimatedDuration", estimate.EstimatedDuration,
			)
		}

		costPredictor.Status.JobsEstimated++
	}

	return nil
}

// updateAccuracyMetrics compares estimates with actual results from completed jobs
func (r *GryviaCostPredictorReconciler) updateAccuracyMetrics(
	ctx context.Context,
	costPredictor *gryviav1.GryviaCostPredictor,
	allJobs []gryviav1.GryviaAIJob,
) {
	var timeErrors []float64
	var costErrors []float64
	timeWithin25 := 0
	costWithin25 := 0
	totalCompleted := 0

	for _, job := range allJobs {
		// Only evaluate completed jobs that have estimates
		if job.Status.Phase != "Succeeded" {
			continue
		}

		if job.Annotations == nil {
			continue
		}

		estimatedCostStr, hasEstimatedCost := job.Annotations[annotationEstimatedCost]
		if !hasEstimatedCost {
			continue
		}

		// Compute actual cost and duration for completed jobs
		if job.Status.StartTime == nil || job.Status.CompletionTime == nil {
			continue
		}

		totalCompleted++

		// Parse estimated values
		var estimatedCost float64
		fmt.Sscanf(estimatedCostStr, "%f", &estimatedCost)

		// Calculate actual duration
		actualDuration := job.Status.CompletionTime.Time.Sub(job.Status.StartTime.Time)

		// Calculate actual cost (GPU hours * rate)
		rate := getJobGPURate(costPredictor, job.Spec.GpuType)
		actualCost := rate * actualDuration.Hours() * float64(job.Spec.TotalGPUs())

		// Parse estimated duration
		estimatedDurationStr := job.Annotations[annotationEstimatedDuration]
		estimatedDuration, err := time.ParseDuration(estimatedDurationStr)
		if err != nil {
			continue
		}

		// Calculate errors
		timeError := math.Abs(actualDuration.Hours() - estimatedDuration.Hours())
		costError := math.Abs(actualCost - estimatedCost)

		timeErrors = append(timeErrors, timeError)
		costErrors = append(costErrors, costError)

		// Check if within 25%
		if actualDuration > 0 {
			timeErrorPct := timeError / actualDuration.Hours()
			if timeErrorPct <= 0.25 {
				timeWithin25++
			}
		}

		if actualCost > 0 {
			costErrorPct := costError / actualCost
			if costErrorPct <= 0.25 {
				costWithin25++
			}
		}

		// Track savings from recommendations
		if savingsStr, ok := job.Annotations["gryvia.io/potential-savings"]; ok {
			if altGPU, altOK := job.Annotations["gryvia.io/alternative-gpu"]; altOK && altGPU == job.Spec.GpuType {
				// User followed the recommendation
				var savings float64
				fmt.Sscanf(savingsStr, "%f", &savings)
				costPredictor.Status.TotalSavingsFromRecommendations += savings
			}
		}
	}

	// Update accuracy metrics
	if len(timeErrors) > 0 {
		totalTimeError := 0.0
		for _, e := range timeErrors {
			totalTimeError += e
		}
		costPredictor.Status.PredictionAccuracy.TimeEstimate.MAE = totalTimeError / float64(len(timeErrors))
		costPredictor.Status.PredictionAccuracy.TimeEstimate.Within25Percent = float64(timeWithin25) / float64(totalCompleted) * 100
	}

	if len(costErrors) > 0 {
		totalCostError := 0.0
		for _, e := range costErrors {
			totalCostError += e
		}
		costPredictor.Status.PredictionAccuracy.CostEstimate.MAE = totalCostError / float64(len(costErrors))
		costPredictor.Status.PredictionAccuracy.CostEstimate.Within25Percent = float64(costWithin25) / float64(totalCompleted) * 100
	}
}

// getJobGPURate returns the GPU rate from predictor pricing for a given GPU type
func getJobGPURate(p *gryviav1.GryviaCostPredictor, gpuType string) float64 {
	if len(p.Spec.Pricing.PerGpuHour) > 0 {
		if rate, ok := p.Spec.Pricing.PerGpuHour[gpuType]; ok {
			return rate
		}
		if rate, ok := p.Spec.Pricing.PerGpuHour["default"]; ok {
			return rate
		}
	}
	// Default rate
	return 2.00
}

func (r *GryviaCostPredictorReconciler) updateStatusCondition(
	ctx context.Context,
	costPredictor *gryviav1.GryviaCostPredictor,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	meta.SetStatusCondition(&costPredictor.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaCostPredictorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaCostPredictor{}).
		Watches(&gryviav1.GryviaAIJob{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				// When a GryviaAIJob changes, re-reconcile all predictors
				// to process new jobs and update accuracy metrics.
				job := obj.(*gryviav1.GryviaAIJob)

				// Only trigger for relevant job state changes
				isDryRun := job.Annotations != nil && job.Annotations[annotationDryRun] == "true"
				isPending := job.Status.Phase == "Pending" || job.Status.Phase == ""
				isCompleted := job.Status.Phase == "Succeeded" || job.Status.Phase == "Failed"
				needsEstimate := job.Annotations == nil || job.Annotations[annotationEstimatedCost] == ""

				if !isDryRun && !((isPending && needsEstimate) || isCompleted) {
					return nil
				}

				predictorList := &gryviav1.GryviaCostPredictorList{}
				if err := mgr.GetClient().List(ctx, predictorList); err != nil {
					return nil
				}

				var requests []reconcile.Request
				for _, p := range predictorList.Items {
					requests = append(requests, reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&p),
					})
				}
				return requests
			},
		)).
		Complete(r)
}
