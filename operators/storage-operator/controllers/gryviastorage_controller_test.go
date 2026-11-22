// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/storage-operator/api/v1"
)

func newStorageTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = storagev1.AddToScheme(s)
	return s
}

func newStorageReconciler(objs ...client.Object) (*GryviaStorageReconciler, client.Client) {
	scheme := newStorageTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaStorage{}).
		Build()
	r := &GryviaStorageReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Log:    ctrl.Log.WithName("test"),
	}
	return r, fakeClient
}

func newTestStorage(name string) *gryviav1.GryviaStorage {
	return &gryviav1.GryviaStorage{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: gryviav1.GryviaStorageSpec{
			Backend:  "vast",
			Capacity: "100Ti",
			IOPS:     "1000000",
			RDMA:     true,
			Protocol: "nfs",
			Endpoint: "storage.example.com",
			StorageClass: &gryviav1.StorageClassSpec{
				Name:                 "fast-storage",
				ReclaimPolicy:        "Retain",
				VolumeBindingMode:    "WaitForFirstConsumer",
				AllowVolumeExpansion: true,
				Parameters: map[string]string{
					"tier": "premium",
				},
			},
			Performance: &gryviav1.PerformanceSpec{
				Tier:        "ultra",
				Caching:     true,
				Compression: false,
				Encryption:  true,
			},
		},
	}
}

func TestStorage_Reconcile_NotFound(t *testing.T) {
	r, _ := newStorageReconciler()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for not-found")
	}
}

func TestStorage_Reconcile_AddsFinalizer(t *testing.T) {
	storage := newTestStorage("test-storage")
	r, fakeClient := newStorageReconciler(storage)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-storage"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after adding finalizer")
	}

	updated := &gryviav1.GryviaStorage{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-storage"}, updated); err != nil {
		t.Fatalf("failed to get storage: %v", err)
	}
	if !controllerutil.ContainsFinalizer(updated, storageFinalizer) {
		t.Error("expected finalizer to be added")
	}
}

func TestStorage_Reconcile_InitializesPhase(t *testing.T) {
	storage := newTestStorage("test-storage")
	controllerutil.AddFinalizer(storage, storageFinalizer)

	r, fakeClient := newStorageReconciler(storage)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-storage"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after phase initialization")
	}

	updated := &gryviav1.GryviaStorage{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-storage"}, updated); err != nil {
		t.Fatalf("failed to get storage: %v", err)
	}
	if updated.Status.Phase != PhasePending {
		t.Errorf("expected phase %s, got %s", PhasePending, updated.Status.Phase)
	}
}

func TestStorage_Reconcile_DeletionCleansUpStorageClass(t *testing.T) {
	now := metav1.Now()
	storage := newTestStorage("test-storage")
	storage.DeletionTimestamp = &now
	storage.Finalizers = []string{storageFinalizer}

	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fast-storage",
		},
		Provisioner: "csi.vastdata.com",
	}

	r, fakeClient := newStorageReconciler(storage, sc)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-storage"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error during deletion, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue after deletion")
	}

	// Verify StorageClass deleted
	sc2 := &storagev1.StorageClass{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "fast-storage"}, sc2)
	if err == nil {
		t.Error("expected StorageClass to be deleted during cleanup")
	}

	// Verify finalizer removed
	updatedStorage := &gryviav1.GryviaStorage{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-storage"}, updatedStorage); err != nil {
		if apierrors.IsNotFound(err) {
			return // object is gone once its last finalizer is removed
		}
		t.Fatalf("failed to get storage: %v", err)
	}
	if controllerutil.ContainsFinalizer(updatedStorage, storageFinalizer) {
		t.Error("expected finalizer to be removed")
	}
}

func TestStorage_Reconcile_DeletionWithDefaultStorageClassName(t *testing.T) {
	now := metav1.Now()
	storage := newTestStorage("test-storage")
	storage.Spec.StorageClass = nil // No explicit name, should use default
	storage.DeletionTimestamp = &now
	storage.Finalizers = []string{storageFinalizer}

	defaultSCName := "vast-test-storage"
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: defaultSCName,
		},
		Provisioner: "csi.vastdata.com",
	}

	r, fakeClient := newStorageReconciler(storage, sc)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-storage"}}
	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error during deletion, got %v", err)
	}

	// Verify default-named StorageClass deleted
	sc2 := &storagev1.StorageClass{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: defaultSCName}, sc2)
	if err == nil {
		t.Error("expected default-named StorageClass to be deleted")
	}
}

func TestStorage_GetProvisioner(t *testing.T) {
	r, _ := newStorageReconciler()

	tests := []struct {
		backend  string
		expected string
	}{
		{"vast", "csi.vastdata.com"},
		{"weka", "csi.weka.io"},
		{"ddn", "csi.ddn.com"},
		{"lustre", "csi.lustre.org"},
		{"ceph", "rook-ceph.cephfs.csi.ceph.com"},
		{"unknown", "kubernetes.io/no-provisioner"},
	}

	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			result := r.getProvisioner(tt.backend)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestStorage_BuildStorageClass(t *testing.T) {
	r, _ := newStorageReconciler()
	storage := newTestStorage("test-storage")

	sc := r.buildStorageClass(storage, "fast-storage")

	if sc.Name != "fast-storage" {
		t.Errorf("expected name fast-storage, got %s", sc.Name)
	}
	if sc.Provisioner != "csi.vastdata.com" {
		t.Errorf("expected provisioner csi.vastdata.com, got %s", sc.Provisioner)
	}
	if *sc.ReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Error("expected Retain reclaim policy")
	}
	if *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer {
		t.Error("expected WaitForFirstConsumer binding mode")
	}
	if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
		t.Error("expected volume expansion to be allowed")
	}

	// Check parameters
	if sc.Parameters["backend"] != "vast" {
		t.Errorf("expected backend=vast, got %s", sc.Parameters["backend"])
	}
	if sc.Parameters["endpoint"] != "storage.example.com" {
		t.Errorf("expected endpoint=storage.example.com, got %s", sc.Parameters["endpoint"])
	}
	if sc.Parameters["rdma"] != "true" {
		t.Errorf("expected rdma=true, got %s", sc.Parameters["rdma"])
	}
	if sc.Parameters["tier"] != "premium" {
		t.Errorf("expected custom parameter tier=premium, got %s", sc.Parameters["tier"])
	}

	// Check labels
	if sc.Labels["gryvia.io/storage"] != "test-storage" {
		t.Errorf("expected storage label, got %s", sc.Labels["gryvia.io/storage"])
	}
	if sc.Labels["gryvia.io/backend"] != "vast" {
		t.Errorf("expected backend label, got %s", sc.Labels["gryvia.io/backend"])
	}
}

func TestStorage_BuildStorageClass_DefaultReclaimPolicy(t *testing.T) {
	r, _ := newStorageReconciler()
	storage := newTestStorage("test-storage")
	storage.Spec.StorageClass.ReclaimPolicy = "Delete"

	sc := r.buildStorageClass(storage, "fast-storage")
	if *sc.ReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		t.Error("expected Delete reclaim policy for non-Retain value")
	}
}

func TestStorage_BuildStorageClass_ImmediateBindingMode(t *testing.T) {
	r, _ := newStorageReconciler()
	storage := newTestStorage("test-storage")
	storage.Spec.StorageClass.VolumeBindingMode = "Immediate"

	sc := r.buildStorageClass(storage, "fast-storage")
	if *sc.VolumeBindingMode != storagev1.VolumeBindingImmediate {
		t.Error("expected Immediate binding mode")
	}
}

func TestStorage_UpdateCondition(t *testing.T) {
	r, _ := newStorageReconciler()
	storage := newTestStorage("test-storage")

	// Add condition
	r.updateCondition(storage, ConditionCSIInstalled, metav1.ConditionTrue, "Installed", "CSI driver OK")
	if len(storage.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(storage.Status.Conditions))
	}

	// Update same condition
	r.updateCondition(storage, ConditionCSIInstalled, metav1.ConditionFalse, "Failed", "CSI error")
	if len(storage.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(storage.Status.Conditions))
	}
	if storage.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Error("expected condition False after update")
	}

	// Add different condition
	r.updateCondition(storage, ConditionStorageClassReady, metav1.ConditionTrue, "Ready", "SC OK")
	if len(storage.Status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(storage.Status.Conditions))
	}

	// Add third condition
	r.updateCondition(storage, ConditionHealthy, metav1.ConditionTrue, "Healthy", "All healthy")
	if len(storage.Status.Conditions) != 3 {
		t.Fatalf("expected 3 conditions, got %d", len(storage.Status.Conditions))
	}
}

func TestStorage_EnsureStorageClass_CreatesIfMissing(t *testing.T) {
	storage := newTestStorage("test-storage")
	controllerutil.AddFinalizer(storage, storageFinalizer)

	r, fakeClient := newStorageReconciler(storage)

	err := r.ensureStorageClass(context.Background(), storage)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	sc := &storagev1.StorageClass{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "fast-storage"}, sc); err != nil {
		t.Fatalf("expected StorageClass to be created: %v", err)
	}
	if sc.Provisioner != "csi.vastdata.com" {
		t.Errorf("expected provisioner csi.vastdata.com, got %s", sc.Provisioner)
	}
}

func TestStorage_EnsureStorageClass_SkipsIfExists(t *testing.T) {
	storage := newTestStorage("test-storage")
	controllerutil.AddFinalizer(storage, storageFinalizer)

	existingSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fast-storage",
		},
		Provisioner: "existing-provisioner",
	}

	r, fakeClient := newStorageReconciler(storage, existingSC)

	err := r.ensureStorageClass(context.Background(), storage)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	sc := &storagev1.StorageClass{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "fast-storage"}, sc); err != nil {
		t.Fatalf("failed to get StorageClass: %v", err)
	}
	// Should not be overwritten
	if sc.Provisioner != "existing-provisioner" {
		t.Error("expected existing StorageClass to not be overwritten")
	}
}
