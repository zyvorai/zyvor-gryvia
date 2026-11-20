// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/storage-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/storage-operator/pkg/ceph"
	"github.com/zyvorai/gryvia/operators/storage-operator/pkg/ddn"
	"github.com/zyvorai/gryvia/operators/storage-operator/pkg/lustre"
	"github.com/zyvorai/gryvia/operators/storage-operator/pkg/vast"
	"github.com/zyvorai/gryvia/operators/storage-operator/pkg/weka"
)

const (
	storageFinalizer = "gryvia.io/storage-finalizer"

	PhasePending     = "Pending"
	PhaseConfiguring = "Configuring"
	PhaseReady       = "Ready"
	PhaseDegraded    = "Degraded"
	PhaseFailed      = "Failed"

	ConditionCSIInstalled      = "CSIInstalled"
	ConditionStorageClassReady = "StorageClassReady"
	ConditionHealthy           = "Healthy"
)

type GryviaStorageReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviastorages,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviastorages/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviastorages/finalizers,verbs=update
//+kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;update;patch;delete

func (r *GryviaStorageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviastorage", req.NamespacedName)

	storage := &gryviav1.GryviaStorage{}
	err := r.Get(ctx, req.NamespacedName, storage)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaStorage resource not found")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaStorage")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !storage.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, storage)
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(storage, storageFinalizer) {
		controllerutil.AddFinalizer(storage, storageFinalizer)
		if err := r.Update(ctx, storage); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Initialize status
	if storage.Status.Phase == "" {
		storage.Status.Phase = PhasePending
		if err := r.Status().Update(ctx, storage); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile storage
	result, err := r.reconcileStorage(ctx, storage)
	if err != nil {
		log.Error(err, "Failed to reconcile storage")
		return result, err
	}

	// Use the result from reconcileStorage if it specifies a requeue, otherwise default
	if result.RequeueAfter > 0 || result.Requeue {
		return result, nil
	}
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

func (r *GryviaStorageReconciler) reconcileStorage(ctx context.Context, storage *gryviav1.GryviaStorage) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviastorage", storage.Name)

	// Only set Configuring if this is the first reconciliation pass
	if storage.Status.Phase == "" {
		storage.Status.Phase = PhaseConfiguring
	}

	// Step 1: Install CSI Driver based on backend
	if err := r.ensureCSIDriver(ctx, storage); err != nil {
		log.Error(err, "Failed to ensure CSI driver")
		r.updateCondition(storage, ConditionCSIInstalled, metav1.ConditionFalse, "InstallFailed", err.Error())
		storage.Status.Phase = PhaseFailed
		if updateErr := r.Status().Update(ctx, storage); updateErr != nil {
			log.Error(updateErr, "Failed to update status after CSI driver install failure")
		}
		// CSI driver is a prerequisite; do not proceed to StorageClass or health check.
		return ctrl.Result{}, fmt.Errorf("CSI driver installation failed: %w", err)
	}

	storage.Status.CSIDriverInstalled = true
	r.updateCondition(storage, ConditionCSIInstalled, metav1.ConditionTrue, "Installed", "CSI driver installed successfully")

	// Step 2: Create StorageClass
	if err := r.ensureStorageClass(ctx, storage); err != nil {
		log.Error(err, "Failed to ensure StorageClass")
		r.updateCondition(storage, ConditionStorageClassReady, metav1.ConditionFalse, "CreateFailed", err.Error())
		storage.Status.Phase = PhaseDegraded
		if updateErr := r.Status().Update(ctx, storage); updateErr != nil {
			log.Error(updateErr, "Failed to update status after StorageClass create failure")
		}
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	storage.Status.StorageClassCreated = true
	r.updateCondition(storage, ConditionStorageClassReady, metav1.ConditionTrue, "Created", "StorageClass created successfully")

	// Step 3: Health check storage backend
	if err := r.healthCheckStorage(ctx, storage); err != nil {
		log.Error(err, "Storage health check failed")
		r.updateCondition(storage, ConditionHealthy, metav1.ConditionFalse, "Unhealthy", err.Error())
		storage.Status.Phase = PhaseDegraded
		if updateErr := r.Status().Update(ctx, storage); updateErr != nil {
			log.Error(updateErr, "Failed to update status after health check failure")
		}
		return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
	} else {
		r.updateCondition(storage, ConditionHealthy, metav1.ConditionTrue, "Healthy", "Storage is healthy")
		storage.Status.Phase = PhaseReady
	}

	// Update last health check time
	now := metav1.Now()
	storage.Status.LastHealthCheck = &now

	// Update status
	if err := r.Status().Update(ctx, storage); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *GryviaStorageReconciler) ensureCSIDriver(ctx context.Context, storage *gryviav1.GryviaStorage) error {
	switch storage.Spec.Backend {
	case "vast":
		return vast.InstallCSIDriver(ctx, r.Client, storage)
	case "weka":
		return weka.InstallCSIDriver(ctx, r.Client, storage)
	case "ddn":
		return ddn.InstallCSIDriver(ctx, r.Client, storage)
	case "lustre":
		return lustre.InstallCSIDriver(ctx, r.Client, storage)
	case "ceph":
		return ceph.InstallCSIDriver(ctx, r.Client, storage)
	default:
		return fmt.Errorf("unsupported storage backend: %s", storage.Spec.Backend)
	}
}

func (r *GryviaStorageReconciler) ensureStorageClass(ctx context.Context, storage *gryviav1.GryviaStorage) error {
	var scName string
	if storage.Spec.StorageClass != nil {
		scName = storage.Spec.StorageClass.Name
	}
	if scName == "" {
		scName = fmt.Sprintf("%s-%s", storage.Spec.Backend, storage.Name)
	}

	sc := &storagev1.StorageClass{}
	err := r.Get(ctx, types.NamespacedName{Name: scName}, sc)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get StorageClass %s: %w", scName, err)
		}
		// Create StorageClass
		sc = r.buildStorageClass(storage, scName)
		// Set owner reference for garbage collection (both are cluster-scoped)
		if err := controllerutil.SetControllerReference(storage, sc, r.Scheme); err != nil {
			r.Log.Error(err, "Failed to set owner reference on StorageClass", "name", scName)
		}
		if err := r.Create(ctx, sc); err != nil {
			return err
		}
		r.Log.Info("Created StorageClass", "name", scName)
	}

	return nil
}

func (r *GryviaStorageReconciler) buildStorageClass(storage *gryviav1.GryviaStorage, name string) *storagev1.StorageClass {
	reclaimPolicy := corev1.PersistentVolumeReclaimDelete
	if storage.Spec.StorageClass != nil && storage.Spec.StorageClass.ReclaimPolicy == "Retain" {
		reclaimPolicy = corev1.PersistentVolumeReclaimRetain
	}

	volumeBindingMode := storagev1.VolumeBindingWaitForFirstConsumer
	if storage.Spec.StorageClass != nil && storage.Spec.StorageClass.VolumeBindingMode == "Immediate" {
		volumeBindingMode = storagev1.VolumeBindingImmediate
	}

	// Default to allowing volume expansion
	allowExpansion := true

	provisioner := r.getProvisioner(storage.Spec.Backend)

	parameters := make(map[string]string)
	if storage.Spec.StorageClass != nil && storage.Spec.StorageClass.Parameters != nil {
		for k, v := range storage.Spec.StorageClass.Parameters {
			parameters[k] = v
		}
	}

	// Add backend-specific parameters
	parameters["backend"] = storage.Spec.Backend
	parameters["endpoint"] = storage.Spec.Endpoint
	if storage.Spec.RDMA {
		parameters["rdma"] = "true"
	}

	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"gryvia.io/storage": storage.Name,
				"gryvia.io/backend": storage.Spec.Backend,
			},
		},
		Provisioner:          provisioner,
		ReclaimPolicy:        &reclaimPolicy,
		VolumeBindingMode:    &volumeBindingMode,
		AllowVolumeExpansion: &allowExpansion,
		Parameters:           parameters,
	}
}

func (r *GryviaStorageReconciler) getProvisioner(backend string) string {
	switch backend {
	case "vast":
		return "csi.vastdata.com"
	case "weka":
		return "csi.weka.io"
	case "ddn":
		return "csi.ddn.com"
	case "lustre":
		return "csi.lustre.org"
	case "ceph":
		return "rook-ceph.cephfs.csi.ceph.com"
	default:
		return "kubernetes.io/no-provisioner"
	}
}

func (r *GryviaStorageReconciler) healthCheckStorage(ctx context.Context, storage *gryviav1.GryviaStorage) error {
	switch storage.Spec.Backend {
	case "vast":
		return vast.HealthCheck(ctx, storage.Spec.Endpoint)
	case "weka":
		return weka.HealthCheck(ctx, storage.Spec.Endpoint)
	case "ddn":
		return ddn.HealthCheck(ctx, storage.Spec.Endpoint)
	case "lustre":
		return lustre.HealthCheck(ctx, storage.Spec.Endpoint)
	case "ceph":
		return ceph.HealthCheck(ctx, storage.Spec.Endpoint)
	default:
		r.Log.Info("Health check not implemented for backend", "backend", storage.Spec.Backend)
		return nil
	}
}

func (r *GryviaStorageReconciler) handleDeletion(ctx context.Context, storage *gryviav1.GryviaStorage) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(storage, storageFinalizer) {
		// Cleanup: delete StorageClass
		var scName string
		if storage.Spec.StorageClass != nil {
			scName = storage.Spec.StorageClass.Name
		}
		if scName == "" {
			scName = fmt.Sprintf("%s-%s", storage.Spec.Backend, storage.Name)
		}

		sc := &storagev1.StorageClass{}
		err := r.Get(ctx, types.NamespacedName{Name: scName}, sc)
		if err == nil {
			if delErr := r.Delete(ctx, sc); delErr != nil {
				r.Log.Error(delErr, "Failed to delete StorageClass during cleanup", "storageclass", scName)
				return ctrl.Result{}, fmt.Errorf("failed to delete StorageClass %s: %w", scName, delErr)
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get StorageClass %s during cleanup: %w", scName, err)
		}

		// Remove finalizer
		controllerutil.RemoveFinalizer(storage, storageFinalizer)
		if err := r.Update(ctx, storage); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *GryviaStorageReconciler) updateCondition(storage *gryviav1.GryviaStorage, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	// Only update LastTransitionTime when status actually changes
	for _, cond := range storage.Status.Conditions {
		if cond.Type == condType && cond.Status == status {
			condition.LastTransitionTime = cond.LastTransitionTime
			break
		}
	}

	found := false
	for i, cond := range storage.Status.Conditions {
		if cond.Type == condType {
			storage.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		storage.Status.Conditions = append(storage.Status.Conditions, condition)
	}
}

func (r *GryviaStorageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaStorage{}).
		Owns(&storagev1.StorageClass{}).
		Complete(r)
}
