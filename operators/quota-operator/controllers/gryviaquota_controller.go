// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/budget"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/usage"
)

const (
	gryviaQuotaFinalizer = "gryvia.io/quota-finalizer"
)

// GryviaQuotaReconciler reconciles a GryviaQuota object
type GryviaQuotaReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaquotas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaquotas/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaquotas/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatenants;gryviagpuskus,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete

func (r *GryviaQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the GryviaQuota instance
	quota := &gryviav1.GryviaQuota{}
	err := r.Get(ctx, req.NamespacedName, quota)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("GryviaQuota resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get GryviaQuota")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !quota.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, quota)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(quota, gryviaQuotaFinalizer) {
		controllerutil.AddFinalizer(quota, gryviaQuotaFinalizer)
		if err := r.Update(ctx, quota); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("Reconciling GryviaQuota", "team", quota.Spec.Team, "maxGPUs", quota.Spec.GPUQuota.MaxGPUs)

	// Reconcile the quota
	result, err := r.reconcileQuota(ctx, quota)
	if err != nil {
		logger.Error(err, "Failed to reconcile quota")
		if statusErr := r.updateStatus(ctx, quota, "Failed", err.Error()); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after reconcile failure")
		}
		return result, err
	}

	// Update status
	if err := r.updateStatus(ctx, quota, quota.Status.Phase, "Quota reconciled successfully"); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}

func (r *GryviaQuotaReconciler) reconcileQuota(ctx context.Context, quota *gryviav1.GryviaQuota) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Ensure namespaces have correct labels
	if err := r.labelNamespaces(ctx, quota); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to label namespaces: %w", err)
	}

	// Calculate current usage
	currentUsage, err := usage.CalculateUsage(ctx, r.Client, quota)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to calculate usage: %w", err)
	}

	quota.Status.CurrentUsage = *currentUsage

	// Check quota violations
	phase := "Active"
	if currentUsage.AllocatedGPUs > quota.Spec.GPUQuota.MaxGPUs {
		phase = "QuotaExceeded"
		logger.Info("GPU quota exceeded", "allocated", currentUsage.AllocatedGPUs, "max", quota.Spec.GPUQuota.MaxGPUs)
	}

	if quota.Spec.GPUQuota.MaxRunningJobs > 0 && currentUsage.RunningJobs > quota.Spec.GPUQuota.MaxRunningJobs {
		phase = "QuotaExceeded"
		logger.Info("Running jobs quota exceeded", "running", currentUsage.RunningJobs, "max", quota.Spec.GPUQuota.MaxRunningJobs)
	}

	// Calculate budget if specified
	if quota.Spec.Budget != nil {
		budgetStatus, err := budget.CalculateBudget(ctx, r.Client, quota, currentUsage)
		if err != nil {
			logger.Error(err, "Failed to calculate budget")
		} else {
			quota.Status.BudgetStatus = budgetStatus

			// Check budget limits
			if budgetStatus.PercentUsed >= 100 && quota.Spec.Budget.HardLimit {
				phase = "BudgetExceeded"
				logger.Info("Budget exceeded", "spent", budgetStatus.SpentThisMonth, "budget", quota.Spec.Budget.MonthlyBudget)
			} else if budgetStatus.PercentUsed >= quota.Spec.Budget.AlertThreshold {
				logger.Info("Budget alert threshold reached", "percentUsed", budgetStatus.PercentUsed, "threshold", quota.Spec.Budget.AlertThreshold)
			}
		}
	}

	quota.Status.Phase = phase

	// Enforce quota by updating jobs. GPU type / SKU checks apply in every phase;
	// the per-job size and budget checks only once the quota is exceeded.
	if err := r.enforceQuota(ctx, quota); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to enforce quota: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *GryviaQuotaReconciler) labelNamespaces(ctx context.Context, quota *gryviav1.GryviaQuota) error {
	logger := log.FromContext(ctx)

	for _, nsName := range quota.Spec.Namespaces {
		ns := &corev1.Namespace{}
		err := r.Get(ctx, types.NamespacedName{Name: nsName}, ns)
		if err != nil {
			if errors.IsNotFound(err) {
				logger.Info("Namespace not found, skipping", "namespace", nsName)
				continue
			}
			return err
		}

		if ns.Labels == nil {
			ns.Labels = make(map[string]string)
		}

		// Add team label
		if ns.Labels["gryvia.io/team"] != quota.Spec.Team {
			ns.Labels["gryvia.io/team"] = quota.Spec.Team
			ns.Labels["gryvia.io/quota"] = quota.Name

			if err := r.Update(ctx, ns); err != nil {
				return fmt.Errorf("failed to update namespace %s: %w", nsName, err)
			}
			logger.Info("Labeled namespace", "namespace", nsName, "team", quota.Spec.Team)
		}
	}

	return nil
}

func (r *GryviaQuotaReconciler) enforceQuota(ctx context.Context, quota *gryviav1.GryviaQuota) error {
	logger := log.FromContext(ctx)

	// Get all AI jobs in quota namespaces
	jobList := &gryviav1.GryviaAIJobList{}
	for _, nsName := range quota.Spec.Namespaces {
		jobs := &gryviav1.GryviaAIJobList{}
		if err := r.List(ctx, jobs, client.InNamespace(nsName)); err != nil {
			return fmt.Errorf("failed to list jobs in namespace %s: %w", nsName, err)
		}
		jobList.Items = append(jobList.Items, jobs.Items...)
	}

	// Sort jobs by priority and creation time
	pendingJobs := []gryviav1.GryviaAIJob{}
	for _, job := range jobList.Items {
		if job.Status.Phase == "Pending" || job.Status.Phase == "Queued" {
			pendingJobs = append(pendingJobs, job)
		}
	}

	exceeded := quota.Status.Phase == "QuotaExceeded" || quota.Status.Phase == "BudgetExceeded"

	// SKUs are only loaded when some tenant might restrict them.
	var skus []gryviav1.GryviaGpuSku
	skusLoaded := false

	// Reject jobs that violate the quota
	for _, job := range pendingJobs {
		if job.Spec.TotalGPUs() <= 0 {
			continue // skip invalid job specs
		}

		reason := ""
		reasonCode := "QuotaExceeded"

		if quota.Status.Phase == "BudgetExceeded" && quota.Spec.Budget != nil && quota.Spec.Budget.HardLimit {
			reason = fmt.Sprintf("Budget exceeded for team %s", quota.Spec.Team)
		}

		if exceeded && quota.Spec.GPUQuota.MaxGPUsPerJob > 0 && int(job.Spec.TotalGPUs()) > quota.Spec.GPUQuota.MaxGPUsPerJob {
			reason = fmt.Sprintf("Job requests %d GPUs, exceeds max %d per job", job.Spec.TotalGPUs(), quota.Spec.GPUQuota.MaxGPUsPerJob)
		}

		// GPU type must be in the quota's allowed list
		if len(quota.Spec.GPUQuota.AllowedGPUTypes) > 0 && !containsString(quota.Spec.GPUQuota.AllowedGPUTypes, job.Spec.GpuType) {
			reason = fmt.Sprintf("GPU type %q is not allowed for team %s (allowed: %s)",
				job.Spec.GpuType, quota.Spec.Team, strings.Join(quota.Spec.GPUQuota.AllowedGPUTypes, ", "))
			reasonCode = "GpuTypeNotAllowed"
		}

		// When the tenant restricts SKUs, the GPU type needs an enabled SKU among them
		if reason == "" {
			allowedSkus, err := r.tenantAllowedSkus(ctx, job.Namespace)
			if err != nil {
				return err
			}
			if len(allowedSkus) > 0 {
				if !skusLoaded {
					list := &gryviav1.GryviaGpuSkuList{}
					if err := r.List(ctx, list); err != nil {
						return fmt.Errorf("failed to list GPU SKUs: %w", err)
					}
					skus, skusLoaded = list.Items, true
				}
				if findSku(skus, job.Spec.GpuType, allowedSkus) == nil {
					reason = fmt.Sprintf("GPU type %q has no enabled SKU among the SKUs allowed for this tenant", job.Spec.GpuType)
					reasonCode = "NoEnabledSku"
				}
			}
		}

		if reason != "" {
			if err := patchJobPhase(ctx, r.Client, &job, "Rejected", reason, &metav1.Condition{
				Type:               "Rejected",
				Status:             metav1.ConditionTrue,
				Reason:             reasonCode,
				Message:            reason,
				LastTransitionTime: metav1.Now(),
			}); err != nil {
				return fmt.Errorf("failed to reject job %s: %w", job.Name, err)
			}
			logger.Info("Rejected job", "job", job.Name, "reason", reason)
		}
	}

	return nil
}

// tenantAllowedSkus returns spec.allowedSkus of the GryviaTenant owning the namespace
// (tenant-<name>), or nil when there is no such tenant.
func (r *GryviaQuotaReconciler) tenantAllowedSkus(ctx context.Context, namespace string) ([]string, error) {
	if !strings.HasPrefix(namespace, tenantNamespacePrefix) {
		return nil, nil
	}
	t := &gryviav1.GryviaTenant{}
	if err := r.Get(ctx, types.NamespacedName{Name: strings.TrimPrefix(namespace, tenantNamespacePrefix)}, t); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return t.Spec.AllowedSkus, nil
}

func (r *GryviaQuotaReconciler) updateStatus(ctx context.Context, quota *gryviav1.GryviaQuota, phase, message string) error {
	quota.Status.Phase = phase
	quota.Status.LastUpdated = metav1.Now()

	condition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             phase,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	if phase == "Failed" || phase == "QuotaExceeded" || phase == "BudgetExceeded" {
		condition.Status = metav1.ConditionFalse
	}

	meta.SetStatusCondition(&quota.Status.Conditions, condition)

	if err := r.Status().Update(ctx, quota); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update GryviaQuota status")
		return err
	}
	return nil
}

func (r *GryviaQuotaReconciler) handleDeletion(ctx context.Context, quota *gryviav1.GryviaQuota) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(quota, gryviaQuotaFinalizer) {
		logger.Info("Running cleanup for GryviaQuota", "team", quota.Spec.Team)

		// Remove gryvia labels from namespaces
		for _, nsName := range quota.Spec.Namespaces {
			ns := &corev1.Namespace{}
			err := r.Get(ctx, types.NamespacedName{Name: nsName}, ns)
			if err != nil {
				if !errors.IsNotFound(err) {
					logger.Error(err, "Failed to get namespace during cleanup", "namespace", nsName)
				}
				continue
			}

			if ns.Labels != nil {
				delete(ns.Labels, "gryvia.io/team")
				delete(ns.Labels, "gryvia.io/quota")
				if err := r.Update(ctx, ns); err != nil {
					logger.Error(err, "Failed to remove labels from namespace", "namespace", nsName)
				}
			}
		}

		// Remove finalizer
		controllerutil.RemoveFinalizer(quota, gryviaQuotaFinalizer)
		if err := r.Update(ctx, quota); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaQuota{}).
		Watches(&gryviav1.GryviaAIJob{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				// When a GryviaAIJob changes, enqueue all GryviaQuota objects
				// in the same namespace so quota usage is recalculated.
				quotaList := &gryviav1.GryviaQuotaList{}
				if err := mgr.GetClient().List(ctx, quotaList); err != nil {
					return nil
				}
				var requests []reconcile.Request
				for _, quota := range quotaList.Items {
					for _, ns := range quota.Spec.Namespaces {
						if ns == obj.GetNamespace() {
							requests = append(requests, reconcile.Request{
								NamespacedName: types.NamespacedName{
									Name:      quota.Name,
									Namespace: quota.Namespace,
								},
							})
							break
						}
					}
				}
				return requests
			},
		)).
		Complete(r)
}
