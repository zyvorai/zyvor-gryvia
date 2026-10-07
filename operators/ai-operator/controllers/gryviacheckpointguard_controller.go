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
)

const (
	// Checkpoint guard condition types
	ConditionGuardActive        = "GuardActive"
	ConditionCheckpointValid    = "CheckpointValid"
	ConditionEmergencyTriggered = "EmergencyTriggered"

	// Checkpoint guard phases
	PhaseIdle          = "Idle"
	PhaseMonitoring    = "Monitoring"
	PhaseCheckpointing = "Checkpointing"
	PhaseRestoring     = "Restoring"
	PhaseError         = "Error"

	// Emergency trigger constants
	TriggerGpuHealthDegraded    = "GpuHealthDegraded"
	TriggerSpotPreemptionSignal = "SpotPreemptionSignal"
	TriggerMemoryPressure       = "MemoryPressure"
	TriggerLossDivergence       = "LossDivergence"
	TriggerNvlinkDegraded       = "NvlinkDegraded"
)

// GryviaCheckpointGuardReconciler reconciles a GryviaCheckpointGuard object
type GryviaCheckpointGuardReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
	// Enabled mirrors the ai-operator --checkpoint-guard flag: without it no job gets the guard's environment.
	Enabled bool
	Now     func() time.Time // nil = time.Now
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviacheckpointguards,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviacheckpointguards/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviacheckpointguards/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviagpunodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;update
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *GryviaCheckpointGuardReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviacheckpointguard", req.NamespacedName)

	// Fetch the GryviaCheckpointGuard instance
	guard := &gryviav1.GryviaCheckpointGuard{}
	err := r.Get(ctx, req.NamespacedName, guard)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaCheckpointGuard resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaCheckpointGuard")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !guard.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Initialize status if needed
	if guard.Status.Phase == "" {
		guard.Status.Phase = PhaseIdle
		if err := r.Status().Update(ctx, guard); err != nil {
			log.Error(err, "Failed to initialize GryviaCheckpointGuard status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile the checkpoint guard
	result, err := r.reconcileCheckpointGuard(ctx, guard)
	if err != nil {
		log.Error(err, "Failed to reconcile checkpoint guard")
		return result, err
	}

	return result, nil
}

func (r *GryviaCheckpointGuardReconciler) reconcileCheckpointGuard(ctx context.Context, guard *gryviav1.GryviaCheckpointGuard) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviacheckpointguard", guard.Name)

	// Step 1: Find matching GryviaAIJob resources using the jobSelector
	matchedJobs, err := r.findMatchingJobs(ctx, guard)
	if err != nil {
		log.Error(err, "Failed to find matching jobs")
		guard.Status.Phase = PhaseError
		r.updateGuardCondition(guard, ConditionGuardActive, metav1.ConditionFalse, "JobLookupFailed", err.Error())
		if updateErr := r.Status().Update(ctx, guard); updateErr != nil {
			log.Error(updateErr, "Failed to update status after job lookup failure")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	guard.Status.MatchedJobs = int32(len(matchedJobs))

	// If no jobs matched, set to Idle
	if len(matchedJobs) == 0 {
		guard.Status.Phase = PhaseIdle
		r.updateGuardCondition(guard, ConditionGuardActive, metav1.ConditionFalse, "NoMatchingJobs", "No GryviaAIJob resources match the jobSelector")
		if err := r.Status().Update(ctx, guard); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	// Step 2: Check running jobs and their pod/GPU health
	guard.Status.Phase = PhaseMonitoring
	r.updateGuardCondition(guard, ConditionGuardActive, metav1.ConditionTrue, "Monitoring", fmt.Sprintf("Monitoring %d matched job(s)", len(matchedJobs)))

	emergencyNeeded := false
	emergencyReason := ""

	for _, job := range matchedJobs {
		// Only monitor running jobs
		if job.Status.Phase != PhaseRunning {
			continue
		}

		// Step 3: Check the job's pods (Job or StatefulSet)
		podHealthy, podReason := r.checkJobPodHealth(ctx, &job)
		if !podHealthy {
			log.Info("Pod health issue detected", "job", job.Name, "reason", podReason)
			if r.isEmergencyTrigger(guard, TriggerMemoryPressure) {
				emergencyNeeded = true
				emergencyReason = fmt.Sprintf("Pod health issue on job %s: %s", job.Name, podReason)
			}
		}

		// Step 4: Check GPU health from GryviaGpuNode resources
		gpuHealthy, gpuReason := r.checkGpuHealth(ctx, &job)
		if !gpuHealthy {
			log.Info("GPU health issue detected", "job", job.Name, "reason", gpuReason)
			if r.isEmergencyTrigger(guard, TriggerGpuHealthDegraded) {
				emergencyNeeded = true
				emergencyReason = fmt.Sprintf("GPU health degraded for job %s: %s", job.Name, gpuReason)
			}
		}

		// Step 5: Check for NVLink degradation via GPU node status
		nvlinkHealthy := r.checkNvlinkHealth(ctx, &job)
		if !nvlinkHealthy {
			log.Info("NVLink degradation detected", "job", job.Name)
			if r.isEmergencyTrigger(guard, TriggerNvlinkDegraded) {
				emergencyNeeded = true
				emergencyReason = fmt.Sprintf("NVLink degraded for job %s", job.Name)
			}
		}

		// Step 6: Check for spot preemption signals via pod conditions
		if r.detectSpotPreemption(ctx, &job) {
			log.Info("Spot preemption signal detected", "job", job.Name)
			if r.isEmergencyTrigger(guard, TriggerSpotPreemptionSignal) {
				emergencyNeeded = true
				emergencyReason = fmt.Sprintf("Spot preemption signal for job %s", job.Name)
			}
		}

		// Step 7: Check for loss divergence via job metrics
		if r.detectLossDivergence(&job) {
			log.Info("Loss divergence detected", "job", job.Name)
			if r.isEmergencyTrigger(guard, TriggerLossDivergence) {
				emergencyNeeded = true
				emergencyReason = fmt.Sprintf("Loss divergence for job %s", job.Name)
			}
		}
	}

	// Step 8: copy what the jobs committed; ask the trainers for an early checkpoint on an emergency.
	r.observeCheckpoints(guard, matchedJobs)
	if emergencyNeeded {
		log.Info("Emergency checkpoint triggered", "reason", emergencyReason)
		requested := r.requestCheckpoints(ctx, guard, matchedJobs, emergencyReason)
		msg := emergencyReason
		if requested > 0 {
			guard.Status.EmergencyCheckpointsTaken += int32(requested)
			msg = fmt.Sprintf("%s; checkpoint requested from %d job(s)", emergencyReason, requested)
		}
		r.updateGuardCondition(guard, ConditionEmergencyTriggered, metav1.ConditionTrue, "EmergencyCheckpoint", msg)
	}

	// Step 10: Update status
	if err := r.Status().Update(ctx, guard); err != nil {
		log.Error(err, "Failed to update checkpoint guard status")
		return ctrl.Result{}, err
	}

	// Requeue based on checkpoint interval
	requeueAfter := r.getRequeueInterval(guard)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// findMatchingJobs finds GryviaAIJob resources that match the guard's jobSelector
func (r *GryviaCheckpointGuardReconciler) findMatchingJobs(ctx context.Context, guard *gryviav1.GryviaCheckpointGuard) ([]gryviav1.GryviaAIJob, error) {
	jobList := &gryviav1.GryviaAIJobList{}

	listOpts := []client.ListOption{
		client.InNamespace(guard.Namespace),
	}

	if len(guard.Spec.JobSelector.MatchLabels) > 0 {
		listOpts = append(listOpts, client.MatchingLabels(guard.Spec.JobSelector.MatchLabels))
	}

	if err := r.List(ctx, jobList, listOpts...); err != nil {
		return nil, fmt.Errorf("failed to list GryviaAIJobs: %w", err)
	}

	return jobList.Items, nil
}

// checkJobPodHealth checks the health of the pods of a job. Pods are found by the
// gryvia.io/job label, so it works for both workload kinds (batch Job and StatefulSet).
// Pods that already finished (Succeeded/Failed) are skipped: a completed Job pod is
// not Ready and a failed one is replaced by the Job controller.
func (r *GryviaCheckpointGuardReconciler) checkJobPodHealth(ctx context.Context, job *gryviav1.GryviaAIJob) (bool, string) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{
		"gryvia.io/job": job.Name,
	}); err != nil {
		return false, fmt.Sprintf("failed to list pods: %v", err)
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		// Check for memory pressure via pod conditions
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionFalse {
				return false, fmt.Sprintf("pod %s is not ready: %s", pod.Name, condition.Message)
			}
		}

		// Check for containers in OOMKilled or CrashLoopBackOff state
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				return false, fmt.Sprintf("pod %s container %s in CrashLoopBackOff", pod.Name, cs.Name)
			}
			if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
				return false, fmt.Sprintf("pod %s container %s OOMKilled", pod.Name, cs.Name)
			}
		}
	}

	return true, ""
}

// checkGpuHealth checks GPU health status from GryviaGpuNode resources for nodes running the job
func (r *GryviaCheckpointGuardReconciler) checkGpuHealth(ctx context.Context, job *gryviav1.GryviaAIJob) (bool, string) {
	if len(job.Status.NodesAllocated) == 0 {
		return true, ""
	}

	// List all GryviaGpuNode resources (they are cluster-scoped)
	gpuNodeList := &gryviav1.GryviaGpuNodeList{}
	if err := r.List(ctx, gpuNodeList); err != nil {
		// If the CRD is not installed, skip the check gracefully
		return true, ""
	}

	// Build a map of node names to GPU nodes for quick lookup
	gpuNodesByNodeName := make(map[string]*gryviav1.GryviaGpuNode)
	for i := range gpuNodeList.Items {
		gpuNode := &gpuNodeList.Items[i]
		gpuNodesByNodeName[gpuNode.Spec.NodeName] = gpuNode
	}

	for _, nodeName := range job.Status.NodesAllocated {
		gpuNode, exists := gpuNodesByNodeName[nodeName]
		if !exists {
			continue // No GryviaGpuNode for this node, skip
		}

		// Check GPU node phase
		if gpuNode.Status.Phase == "Degraded" || gpuNode.Status.Phase == "Failed" {
			return false, fmt.Sprintf("GPU node %s is in %s phase", nodeName, gpuNode.Status.Phase)
		}

		// Check individual GPU health statuses
		for _, gpuStatus := range gpuNode.Status.GpuStatus {
			if gpuStatus.Health != "Healthy" && gpuStatus.Health != "" {
				return false, fmt.Sprintf("GPU %d on node %s health: %s", gpuStatus.Index, nodeName, gpuStatus.Health)
			}

			// Check for thermal throttling (temperature > 85C)
			if gpuStatus.Temperature > 85 {
				return false, fmt.Sprintf("GPU %d on node %s overheating: %d°C", gpuStatus.Index, nodeName, gpuStatus.Temperature)
			}
		}
	}

	return true, ""
}

// checkNvlinkHealth checks NVLink interconnect health from GryviaGpuNode resources
func (r *GryviaCheckpointGuardReconciler) checkNvlinkHealth(ctx context.Context, job *gryviav1.GryviaAIJob) bool {
	if len(job.Status.NodesAllocated) == 0 {
		return true
	}

	gpuNodeList := &gryviav1.GryviaGpuNodeList{}
	if err := r.List(ctx, gpuNodeList); err != nil {
		return true // Cannot check, assume healthy
	}

	for _, nodeName := range job.Status.NodesAllocated {
		for i := range gpuNodeList.Items {
			gpuNode := &gpuNodeList.Items[i]
			if gpuNode.Spec.NodeName != nodeName {
				continue
			}

			// Check conditions for NVLink-related issues
			for _, condition := range gpuNode.Status.Conditions {
				if condition.Type == "NvlinkHealthy" && condition.Status == metav1.ConditionFalse {
					return false
				}
			}

			// If interconnect is NVLink and node is degraded, NVLink may be affected
			if gpuNode.Spec.Interconnect == "nvlink" && gpuNode.Status.Phase == "Degraded" {
				return false
			}
		}
	}

	return true
}

// detectSpotPreemption checks if any pods have received spot preemption signals
func (r *GryviaCheckpointGuardReconciler) detectSpotPreemption(ctx context.Context, job *gryviav1.GryviaAIJob) bool {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{
		"gryvia.io/job": job.Name,
	}); err != nil {
		return false
	}

	for _, pod := range pods.Items {
		// Check for spot preemption annotations (set by cloud provider or node termination handler)
		if _, hasAnnotation := pod.Annotations["gryvia.io/spot-preemption"]; hasAnnotation {
			return true
		}

		// Check node conditions for spot termination notices
		if pod.Spec.NodeName != "" {
			node := &corev1.Node{}
			if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err == nil {
				for _, condition := range node.Status.Conditions {
					if condition.Type == "SpotTermination" && condition.Status == corev1.ConditionTrue {
						return true
					}
				}
				// Check for preemption-related taints
				for _, taint := range node.Spec.Taints {
					if taint.Key == "cloud.google.com/impending-node-termination" ||
						taint.Key == "aws-node-termination-handler/spot-itn" {
						return true
					}
				}
			}
		}
	}

	return false
}

// detectLossDivergence checks if the job's training loss is diverging
func (r *GryviaCheckpointGuardReconciler) detectLossDivergence(job *gryviav1.GryviaAIJob) bool {
	if job.Status.Metrics == nil {
		return false
	}

	// Detect loss divergence: NaN, Inf, or extremely large values
	loss := job.Status.Metrics.Loss
	if loss != loss { // NaN check
		return true
	}
	// Loss values above 1e6 typically indicate divergence in most training scenarios
	if loss > 1e6 {
		return true
	}

	return false
}

// isEmergencyTrigger checks if a given trigger is configured in the guard's emergency checkpoint policy
func (r *GryviaCheckpointGuardReconciler) isEmergencyTrigger(guard *gryviav1.GryviaCheckpointGuard, trigger string) bool {
	if guard.Spec.CheckpointPolicy.EmergencyCheckpoint == nil {
		return false
	}

	for _, t := range guard.Spec.CheckpointPolicy.EmergencyCheckpoint.Triggers {
		if t == trigger {
			return true
		}
	}

	return false
}

// getRequeueInterval returns the requeue interval based on checkpoint policy
func (r *GryviaCheckpointGuardReconciler) getRequeueInterval(guard *gryviav1.GryviaCheckpointGuard) time.Duration {
	interval := guard.Spec.CheckpointPolicy.IntervalMinutes
	if interval <= 0 {
		interval = 30
	}

	// Requeue at half the checkpoint interval for responsive monitoring,
	// but no more frequently than every 30 seconds
	requeue := time.Duration(interval) * time.Minute / 2
	if requeue < 30*time.Second {
		requeue = 30 * time.Second
	}

	return requeue
}

// updateGuardCondition updates a condition on the checkpoint guard status
func (r *GryviaCheckpointGuardReconciler) updateGuardCondition(guard *gryviav1.GryviaCheckpointGuard, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: guard.Generation,
		LastTransitionTime: metav1.Now(),
	}

	// Only update LastTransitionTime when status actually changes
	for _, cond := range guard.Status.Conditions {
		if cond.Type == condType && cond.Status == status {
			condition.LastTransitionTime = cond.LastTransitionTime
			break
		}
	}

	// Find and update existing condition or append new one
	found := false
	for i, cond := range guard.Status.Conditions {
		if cond.Type == condType {
			guard.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		guard.Status.Conditions = append(guard.Status.Conditions, condition)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaCheckpointGuardReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaCheckpointGuard{}).
		Complete(r)
}
