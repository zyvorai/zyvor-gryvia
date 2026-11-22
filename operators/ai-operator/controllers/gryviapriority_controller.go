// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// GryviaPriorityReconciler reconciles a GryviaPriority object
type GryviaPriorityReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviapriorities,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviapriorities/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviapriorities/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *GryviaPriorityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviapriority", req.NamespacedName)

	// Fetch the GryviaPriority instance
	priority := &gryviav1.GryviaPriority{}
	err := r.Get(ctx, req.NamespacedName, priority)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaPriority resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaPriority")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !priority.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	log.Info("Reconciling GryviaPriority", "name", priority.Name, "value", priority.Spec.Value)

	// Reconcile the priority
	result, err := r.reconcilePriority(ctx, priority)
	if err != nil {
		log.Error(err, "Failed to reconcile priority")
		return result, err
	}

	return result, nil
}

func (r *GryviaPriorityReconciler) reconcilePriority(ctx context.Context, priority *gryviav1.GryviaPriority) (ctrl.Result, error) {
	log := r.Log.WithValues("priority", priority.Name)

	if priority.Spec.QuotaOverride != nil || priority.Spec.SLA != nil {
		r.updatePriorityCondition(priority, "Ready", metav1.ConditionFalse, "UnsupportedOptions", "Quota override and SLA guarantees are unsupported; use native quota/admission controls")
		return ctrl.Result{}, r.Status().Update(ctx, priority)
	}

	if priority.Spec.Value < 0 || priority.Spec.Value > 1000000 {
		return ctrl.Result{}, fmt.Errorf("priority value must be 0-1000000")
	}
	policy := corev1.PreemptNever
	if priority.Spec.PreemptionPolicy == "PreemptLowerPriority" {
		policy = corev1.PreemptLowerPriority
	} else if priority.Spec.PreemptionPolicy != "" && priority.Spec.PreemptionPolicy != "Never" {
		return ctrl.Result{}, fmt.Errorf("invalid preemptionPolicy")
	}
	pc := &schedulingv1.PriorityClass{}
	err := r.Get(ctx, types.NamespacedName{Name: priority.Name}, pc)
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if errors.IsNotFound(err) {
		pc = &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: priority.Name}, Value: int32(priority.Spec.Value), Description: priority.Spec.Description, PreemptionPolicy: &policy}
		if err := controllerutil.SetControllerReference(priority, pc, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, pc); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		if !metav1.IsControlledBy(pc, priority) {
			return ctrl.Result{}, fmt.Errorf("PriorityClass name collision")
		}
		if pc.Value != int32(priority.Spec.Value) || pc.PreemptionPolicy == nil || *pc.PreemptionPolicy != policy {
			return ctrl.Result{}, fmt.Errorf("PriorityClass value and preemptionPolicy are immutable; create a new priority name")
		}
		base := pc.DeepCopy()
		pc.Description = priority.Spec.Description
		if err := r.Patch(ctx, pc, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}
	// List all jobs
	jobList := &gryviav1.GryviaAIJobList{}
	if err := r.List(ctx, jobList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list GryviaAIJobs: %w", err)
	}

	// Count active and queued jobs with this priority
	activeJobs := 0
	queuedJobs := 0
	var queueTimes []time.Duration

	for _, job := range jobList.Items {
		jobPriority := r.getJobPriorityClass(job)
		if jobPriority != priority.Name {
			continue
		}

		switch job.Status.Phase {
		case "Running":
			activeJobs++
		case "Pending", "Queued":
			queuedJobs++
			queueTime := time.Since(job.CreationTimestamp.Time)
			queueTimes = append(queueTimes, queueTime)
		}
	}

	priority.Status.ActiveJobs = activeJobs
	priority.Status.QueuedJobs = queuedJobs

	// Calculate average queue time
	if len(queueTimes) > 0 {
		total := time.Duration(0)
		for _, qt := range queueTimes {
			total += qt
		}
		avg := total / time.Duration(len(queueTimes))
		priority.Status.AvgQueueTime = avg.Round(time.Second).String()
	} else {
		priority.Status.AvgQueueTime = "0s"
	}

	// Kubernetes/Kueue own preemption. Never invent a Preempted status without terminating a workload.

	// Update condition
	r.updatePriorityCondition(priority, "Ready", metav1.ConditionTrue, "PriorityActive",
		fmt.Sprintf("Priority %s (value=%d): %d active, %d queued jobs",
			priority.Name, priority.Spec.Value, activeJobs, queuedJobs))

	// Update status
	if err := r.Status().Update(ctx, priority); err != nil {
		log.Error(err, "Failed to update priority status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *GryviaPriorityReconciler) getJobPriorityClass(job gryviav1.GryviaAIJob) string {
	// Check for priority class label or annotation
	if pc, ok := job.Labels["gryvia.io/priority-class"]; ok {
		return pc
	}
	if pc, ok := job.Annotations["gryvia.io/priority-class"]; ok {
		return pc
	}
	// Default to "normal"
	return "normal"
}

func (r *GryviaPriorityReconciler) updatePriorityCondition(priority *gryviav1.GryviaPriority, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: priority.Generation,
		LastTransitionTime: metav1.Now(),
	}

	found := false
	for i, cond := range priority.Status.Conditions {
		if cond.Type == condType {
			priority.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		priority.Status.Conditions = append(priority.Status.Conditions, condition)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaPriorityReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaPriority{}).
		Complete(r)
}
