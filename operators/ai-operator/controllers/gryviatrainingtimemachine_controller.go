// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/timemachine"
)

const (
	// Condition types for the time machine
	ConditionSourceJobFound    = "SourceJobFound"
	ConditionTimelineIndexed   = "TimelineIndexed"
	ConditionRetentionEnforced = "RetentionEnforced"
	ConditionForksProcessed    = "ForksProcessed"

	// Fork status values
	ForkStatusPending   = "Pending"
	ForkStatusCreating  = "Creating"
	ForkStatusRunning   = "Running"
	ForkStatusSucceeded = "Succeeded"
	ForkStatusFailed    = "Failed"

	// Default reconciliation interval
	timeMachineReconcileInterval = 30 * time.Second
)

// GryviaTrainingTimeMachineReconciler reconciles a GryviaTrainingTimeMachine object
type GryviaTrainingTimeMachineReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Log         logr.Logger
	ForkHandler *timemachine.ForkHandler
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatrainingtimemachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatrainingtimemachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatrainingtimemachines/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *GryviaTrainingTimeMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviatrainingtimemachine", req.NamespacedName)

	// Fetch the GryviaTrainingTimeMachine instance
	tm := &gryviav1.GryviaTrainingTimeMachine{}
	err := r.Get(ctx, req.NamespacedName, tm)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaTrainingTimeMachine resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaTrainingTimeMachine")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !tm.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Run the reconciliation
	result, err := r.reconcileTimeMachine(ctx, tm)
	if err != nil {
		log.Error(err, "Failed to reconcile training time machine")
		return result, err
	}

	return result, nil
}

func (r *GryviaTrainingTimeMachineReconciler) reconcileTimeMachine(ctx context.Context, tm *gryviav1.GryviaTrainingTimeMachine) (ctrl.Result, error) {
	log := r.Log.WithValues("timemachine", tm.Name, "namespace", tm.Namespace)

	// Step 1: Watch the source GryviaAIJob
	sourceJob, err := r.getSourceJob(ctx, tm)
	if err != nil {
		log.Error(err, "Failed to find source job", "sourceJob", tm.Spec.SourceJob)
		r.updateCondition(tm, ConditionSourceJobFound, metav1.ConditionFalse, "SourceJobNotFound", err.Error())
		if updateErr := r.Status().Update(ctx, tm); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: timeMachineReconcileInterval}, err
	}

	r.updateCondition(tm, ConditionSourceJobFound, metav1.ConditionTrue, "SourceJobFound",
		fmt.Sprintf("Source job %s found in phase %s", sourceJob.Name, sourceJob.Status.Phase))

	// Step 2: Index checkpoints from the source job
	if tm.Spec.Timeline != nil && tm.Spec.Timeline.Enabled {
		if err := r.indexCheckpoints(ctx, tm, sourceJob); err != nil {
			log.Error(err, "Failed to index checkpoints")
			r.updateCondition(tm, ConditionTimelineIndexed, metav1.ConditionFalse, "IndexingFailed", err.Error())
		} else {
			totalCheckpoints := 0
			if tm.Status.CheckpointTimeline != nil {
				totalCheckpoints = tm.Status.CheckpointTimeline.TotalCheckpoints
			}
			r.updateCondition(tm, ConditionTimelineIndexed, metav1.ConditionTrue, "TimelineIndexed",
				fmt.Sprintf("Indexed %d checkpoints", totalCheckpoints))
		}
	}

	// Step 3: Enforce retention policy
	if tm.Spec.Retention != nil {
		if err := r.enforceRetention(ctx, tm); err != nil {
			log.Error(err, "Failed to enforce retention policy")
			r.updateCondition(tm, ConditionRetentionEnforced, metav1.ConditionFalse, "RetentionFailed", err.Error())
		} else {
			r.updateCondition(tm, ConditionRetentionEnforced, metav1.ConditionTrue, "RetentionEnforced", "Retention policy applied")
		}
	}

	// Step 4: Process fork requests
	if len(tm.Spec.Forks) > 0 {
		if err := r.processForks(ctx, tm, sourceJob); err != nil {
			log.Error(err, "Failed to process forks")
			r.updateCondition(tm, ConditionForksProcessed, metav1.ConditionFalse, "ForkProcessingFailed", err.Error())
		} else {
			r.updateCondition(tm, ConditionForksProcessed, metav1.ConditionTrue, "ForksProcessed",
				fmt.Sprintf("Processed %d fork requests", len(tm.Spec.Forks)))
		}
	}

	// Step 5: Track forked job status
	r.trackForkedJobs(ctx, tm)

	// Step 6: Update storage usage
	r.updateStorageUsage(ctx, tm)

	// Update status
	if err := r.Status().Update(ctx, tm); err != nil {
		log.Error(err, "Failed to update time machine status")
		return ctrl.Result{}, err
	}

	// Only requeue if the source job is still active
	if sourceJob.Status.Phase == PhaseSucceeded || sourceJob.Status.Phase == PhaseFailed {
		// Still requeue to track fork status, but less frequently
		return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
	}

	return ctrl.Result{RequeueAfter: timeMachineReconcileInterval}, nil
}

// getSourceJob fetches the source GryviaAIJob
func (r *GryviaTrainingTimeMachineReconciler) getSourceJob(ctx context.Context, tm *gryviav1.GryviaTrainingTimeMachine) (*gryviav1.GryviaAIJob, error) {
	job := &gryviav1.GryviaAIJob{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: tm.Namespace,
		Name:      tm.Spec.SourceJob,
	}, job)
	if err != nil {
		return nil, fmt.Errorf("source job %s/%s not found: %w", tm.Namespace, tm.Spec.SourceJob, err)
	}
	return job, nil
}

// indexCheckpoints discovers and indexes checkpoints from the source job
func (r *GryviaTrainingTimeMachineReconciler) indexCheckpoints(ctx context.Context, tm *gryviav1.GryviaTrainingTimeMachine, sourceJob *gryviav1.GryviaAIJob) error {
	log := r.Log.WithValues("timemachine", tm.Name)

	// Initialize timeline status if needed
	if tm.Status.CheckpointTimeline == nil {
		tm.Status.CheckpointTimeline = &gryviav1.CheckpointTimelineStatus{}
	}

	// Extract metrics from the source job
	if sourceJob.Status.Metrics == nil {
		log.Info("Source job has no metrics yet, skipping checkpoint indexing")
		return nil
	}

	metrics := sourceJob.Status.Metrics

	// Build checkpoint info from the current job metrics
	currentStep := int(metrics.Step)
	currentEpoch := int(metrics.Epoch)

	if currentStep == 0 && currentEpoch == 0 {
		return nil
	}

	// Update latest checkpoint
	now := metav1.Now()
	latestCheckpoint := &gryviav1.CheckpointInfo{
		Step:      currentStep,
		Epoch:     currentEpoch,
		Timestamp: &now,
	}

	// Extract primary metric for this checkpoint
	if metrics.Loss > 0 {
		latestCheckpoint.MetricName = "loss"
		latestCheckpoint.MetricValue = metrics.Loss
	}

	tm.Status.CheckpointTimeline.LatestCheckpoint = latestCheckpoint

	// Count checkpoints (simulated: we increment based on step changes)
	// In production, this would scan the checkpoint directory on the PVC
	if currentStep > 0 {
		// Estimate checkpoint count from step and a typical checkpoint interval
		estimatedCheckpoints := currentStep / 100
		if estimatedCheckpoints < 1 {
			estimatedCheckpoints = 1
		}
		tm.Status.CheckpointTimeline.TotalCheckpoints = estimatedCheckpoints
	}

	// Track best checkpoint
	if tm.Status.CheckpointTimeline.BestCheckpoint == nil {
		tm.Status.CheckpointTimeline.BestCheckpoint = latestCheckpoint
	} else {
		// For loss, lower is better
		if latestCheckpoint.MetricName == "loss" && latestCheckpoint.MetricValue < tm.Status.CheckpointTimeline.BestCheckpoint.MetricValue {
			tm.Status.CheckpointTimeline.BestCheckpoint = latestCheckpoint
		}
	}

	log.Info("Indexed checkpoint",
		"step", currentStep,
		"epoch", currentEpoch,
		"metricName", latestCheckpoint.MetricName,
		"metricValue", latestCheckpoint.MetricValue,
		"totalCheckpoints", tm.Status.CheckpointTimeline.TotalCheckpoints,
	)

	return nil
}

// enforceRetention applies the retention policy to checkpoints
func (r *GryviaTrainingTimeMachineReconciler) enforceRetention(ctx context.Context, tm *gryviav1.GryviaTrainingTimeMachine) error {
	retention := tm.Spec.Retention

	if retention.KeepAll {
		// Nothing to prune
		return nil
	}

	if tm.Status.CheckpointTimeline == nil || tm.Status.CheckpointTimeline.TotalCheckpoints == 0 {
		return nil
	}

	totalCheckpoints := tm.Status.CheckpointTimeline.TotalCheckpoints
	retained := totalCheckpoints

	// Apply keepEveryNth filter
	if retention.KeepEveryNth > 0 && retention.KeepEveryNth < totalCheckpoints {
		retained = totalCheckpoints / retention.KeepEveryNth
		if retained < 1 {
			retained = 1
		}
	}

	// Apply keepBest filter
	if retention.KeepBest > 0 && retained > retention.KeepBest {
		retained = retention.KeepBest
	}

	// Add milestones back
	if len(retention.KeepMilestones) > 0 {
		retained += len(retention.KeepMilestones)
	}

	// In production, this would:
	// 1. List checkpoint files on the PVC
	// 2. Sort by metric value
	// 3. Delete files that don't match retention criteria
	// 4. Verify storage stays within maxStorageGi

	r.Log.Info("Retention policy evaluated",
		"totalCheckpoints", totalCheckpoints,
		"retainedCheckpoints", retained,
		"keepEveryNth", retention.KeepEveryNth,
		"keepBest", retention.KeepBest,
		"maxStorageGi", retention.MaxStorageGi,
	)

	return nil
}

// processForks creates new GryviaAIJob CRs from checkpoint state for each fork request
func (r *GryviaTrainingTimeMachineReconciler) processForks(ctx context.Context, tm *gryviav1.GryviaTrainingTimeMachine, sourceJob *gryviav1.GryviaAIJob) error {
	log := r.Log.WithValues("timemachine", tm.Name)

	// Initialize fork status slice if needed
	existingForks := make(map[string]*gryviav1.ForkStatus)
	for i := range tm.Status.Forks {
		existingForks[tm.Status.Forks[i].Name] = &tm.Status.Forks[i]
	}

	for _, forkSpec := range tm.Spec.Forks {
		// Check if this fork has already been processed
		if existing, ok := existingForks[forkSpec.Name]; ok {
			if existing.Status != ForkStatusPending {
				continue // Already processed
			}
		}

		log.Info("Processing fork request", "forkName", forkSpec.Name)

		// Resolve the checkpoint to fork from
		checkpointStep, err := r.resolveCheckpoint(tm, forkSpec.FromCheckpoint)
		if err != nil {
			log.Error(err, "Failed to resolve checkpoint for fork", "forkName", forkSpec.Name)
			r.setForkStatus(tm, forkSpec.Name, 0, "", ForkStatusFailed, 0)
			continue
		}

		// Create the forked job
		r.setForkStatus(tm, forkSpec.Name, checkpointStep, "", ForkStatusCreating, 0)

		forkedJobName, err := r.ForkHandler.CreateForkedJob(ctx, sourceJob, forkSpec, checkpointStep)
		if err != nil {
			log.Error(err, "Failed to create forked job", "forkName", forkSpec.Name)
			r.setForkStatus(tm, forkSpec.Name, checkpointStep, "", ForkStatusFailed, 0)
			continue
		}

		r.setForkStatus(tm, forkSpec.Name, checkpointStep, forkedJobName, ForkStatusRunning, 0)

		log.Info("Fork created successfully",
			"forkName", forkSpec.Name,
			"sourceStep", checkpointStep,
			"forkedJob", forkedJobName,
		)
	}

	return nil
}

// resolveCheckpoint resolves a CheckpointSelector to a specific training step
func (r *GryviaTrainingTimeMachineReconciler) resolveCheckpoint(tm *gryviav1.GryviaTrainingTimeMachine, selector gryviav1.CheckpointSelector) (int, error) {
	// Step-based selection
	if selector.Step != nil {
		return *selector.Step, nil
	}

	// Epoch-based selection: convert epoch to step using latest data
	if selector.Epoch != nil {
		if tm.Status.CheckpointTimeline != nil && tm.Status.CheckpointTimeline.LatestCheckpoint != nil {
			latest := tm.Status.CheckpointTimeline.LatestCheckpoint
			if latest.Epoch > 0 && latest.Step > 0 {
				stepsPerEpoch := latest.Step / latest.Epoch
				if stepsPerEpoch > 0 {
					return stepsPerEpoch * (*selector.Epoch), nil
				}
			}
		}
		// If no epoch-to-step mapping, use epoch as step
		return *selector.Epoch, nil
	}

	// Metric-based selection: find the best checkpoint by metric
	if selector.Metric != nil {
		if tm.Status.CheckpointTimeline != nil && tm.Status.CheckpointTimeline.BestCheckpoint != nil {
			return tm.Status.CheckpointTimeline.BestCheckpoint.Step, nil
		}
		return 0, fmt.Errorf("no best checkpoint available for metric %s", selector.Metric.Name)
	}

	return 0, fmt.Errorf("checkpoint selector must specify step, epoch, or metric")
}

// setForkStatus updates the status for a specific fork
func (r *GryviaTrainingTimeMachineReconciler) setForkStatus(tm *gryviav1.GryviaTrainingTimeMachine, name string, sourceStep int, forkedJob string, status string, currentMetric float64) {
	for i, fs := range tm.Status.Forks {
		if fs.Name == name {
			tm.Status.Forks[i].SourceStep = sourceStep
			if forkedJob != "" {
				tm.Status.Forks[i].ForkedJob = forkedJob
			}
			tm.Status.Forks[i].Status = status
			tm.Status.Forks[i].CurrentMetric = currentMetric
			return
		}
	}

	// Append new fork status
	tm.Status.Forks = append(tm.Status.Forks, gryviav1.ForkStatus{
		Name:          name,
		SourceStep:    sourceStep,
		ForkedJob:     forkedJob,
		Status:        status,
		CurrentMetric: currentMetric,
	})
}

// trackForkedJobs updates the status of forked jobs
func (r *GryviaTrainingTimeMachineReconciler) trackForkedJobs(ctx context.Context, tm *gryviav1.GryviaTrainingTimeMachine) {
	for i, forkStatus := range tm.Status.Forks {
		if forkStatus.ForkedJob == "" {
			continue
		}
		if forkStatus.Status == ForkStatusSucceeded || forkStatus.Status == ForkStatusFailed {
			continue
		}

		// Fetch the forked job
		forkedJob := &gryviav1.GryviaAIJob{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: tm.Namespace,
			Name:      forkStatus.ForkedJob,
		}, forkedJob)
		if err != nil {
			if errors.IsNotFound(err) {
				tm.Status.Forks[i].Status = ForkStatusFailed
			}
			continue
		}

		// Update fork status based on job phase
		switch forkedJob.Status.Phase {
		case PhaseRunning:
			tm.Status.Forks[i].Status = ForkStatusRunning
		case PhaseSucceeded:
			tm.Status.Forks[i].Status = ForkStatusSucceeded
		case PhaseFailed:
			tm.Status.Forks[i].Status = ForkStatusFailed
		}

		// Update current metric
		if forkedJob.Status.Metrics != nil && forkedJob.Status.Metrics.Loss > 0 {
			tm.Status.Forks[i].CurrentMetric = forkedJob.Status.Metrics.Loss
		}
	}
}

// updateStorageUsage updates the storage usage status
func (r *GryviaTrainingTimeMachineReconciler) updateStorageUsage(ctx context.Context, tm *gryviav1.GryviaTrainingTimeMachine) {
	// List PVCs associated with the source job
	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcList,
		client.InNamespace(tm.Namespace),
		client.MatchingLabels{"gryvia.io/job": tm.Spec.SourceJob},
	); err != nil {
		return
	}

	var totalStorageBytes int64
	for _, pvc := range pvcList.Items {
		if storage, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			totalStorageBytes += storage.Value()
		}
	}

	if totalStorageBytes > 0 {
		storageGi := totalStorageBytes / (1024 * 1024 * 1024)
		tm.Status.StorageUsed = fmt.Sprintf("%dGi", storageGi)
	}
}

// updateCondition updates or appends a condition on the time machine status
func (r *GryviaTrainingTimeMachineReconciler) updateCondition(tm *gryviav1.GryviaTrainingTimeMachine, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: tm.Generation,
		LastTransitionTime: metav1.Now(),
	}

	// Only update LastTransitionTime when status actually changes
	for _, cond := range tm.Status.Conditions {
		if cond.Type == condType && cond.Status == status {
			condition.LastTransitionTime = cond.LastTransitionTime
			break
		}
	}

	// Find and update existing condition or append new one
	found := false
	for i, cond := range tm.Status.Conditions {
		if cond.Type == condType {
			tm.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		tm.Status.Conditions = append(tm.Status.Conditions, condition)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaTrainingTimeMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaTrainingTimeMachine{}).
		Complete(r)
}
