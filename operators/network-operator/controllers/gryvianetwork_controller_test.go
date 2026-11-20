// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-operator/api/v1"
)

func newNetworkTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	return s
}

func newNetworkReconciler(objs ...client.Object) (*GryviaNetworkReconciler, client.Client) {
	scheme := newNetworkTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaNetwork{}).
		Build()
	r := &GryviaNetworkReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}
	return r, fakeClient
}

func newTestNetwork(name string) *gryviav1.GryviaNetwork {
	return &gryviav1.GryviaNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: gryviav1.GryviaNetworkSpec{
			NetworkType: "rdma",
			RDMA: &gryviav1.RDMAConfig{
				Mode:    "infiniband",
				Devices: []string{"mlx5_0", "mlx5_1"},
				Subnet:  "10.0.0.0/24",
				Gateway: "10.0.0.1",
			},
			NodeSelector: map[string]string{
				"gryvia.io/gpu": "H100",
			},
			MTU:             9000,
			TargetNamespace: "default",
		},
	}
}

func TestNetwork_Reconcile_NotFound(t *testing.T) {
	r, _ := newNetworkReconciler()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for not-found")
	}
}

func TestNetwork_Reconcile_AddsFinalizer(t *testing.T) {
	network := newTestNetwork("test-network")
	r, fakeClient := newNetworkReconciler(network)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-network"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after adding finalizer")
	}

	updated := &gryviav1.GryviaNetwork{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-network"}, updated); err != nil {
		t.Fatalf("failed to get network: %v", err)
	}
	if !controllerutil.ContainsFinalizer(updated, gryviaNetworkFinalizer) {
		t.Error("expected finalizer to be added")
	}
}

func TestNetwork_Reconcile_DeletionCleanup(t *testing.T) {
	now := metav1.Now()
	network := newTestNetwork("test-network")
	network.DeletionTimestamp = &now
	network.Finalizers = []string{gryviaNetworkFinalizer}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-node-1",
			Labels: map[string]string{
				"gryvia.io/gpu":       "H100",
				"gryvia.io/rdma":      "true",
				"gryvia.io/rdma-mode": "infiniband",
				"gryvia.io/sriov":     "true",
				"other-label":         "keep",
			},
			Annotations: map[string]string{
				"gryvia.io/rdma-devices":    "mlx5_0,mlx5_1",
				"gryvia.io/sriov-interface": "eth0",
			},
		},
	}

	r, fakeClient := newNetworkReconciler(network, node)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-network"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error during deletion, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue after deletion")
	}

	// Verify labels/annotations removed
	updatedNode := &corev1.Node{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "gpu-node-1"}, updatedNode); err != nil {
		t.Fatalf("failed to get node: %v", err)
	}
	if _, exists := updatedNode.Labels["gryvia.io/rdma"]; exists {
		t.Error("expected gryvia.io/rdma label to be removed")
	}
	if _, exists := updatedNode.Labels["gryvia.io/rdma-mode"]; exists {
		t.Error("expected gryvia.io/rdma-mode label to be removed")
	}
	if _, exists := updatedNode.Labels["gryvia.io/sriov"]; exists {
		t.Error("expected gryvia.io/sriov label to be removed")
	}
	if val, exists := updatedNode.Labels["other-label"]; !exists || val != "keep" {
		t.Error("expected non-gryvia labels to be preserved")
	}
	if _, exists := updatedNode.Annotations["gryvia.io/rdma-devices"]; exists {
		t.Error("expected rdma-devices annotation to be removed")
	}

	// Verify finalizer removed
	updatedNetwork := &gryviav1.GryviaNetwork{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-network"}, updatedNetwork); err != nil {
		if apierrors.IsNotFound(err) {
			return // object is gone once its last finalizer is removed
		}
		t.Fatalf("failed to get network: %v", err)
	}
	if controllerutil.ContainsFinalizer(updatedNetwork, gryviaNetworkFinalizer) {
		t.Error("expected finalizer to be removed")
	}
}

func TestNetwork_GetMatchingNodes(t *testing.T) {
	network := newTestNetwork("test-network")
	network.Spec.NodeSelector = map[string]string{"gryvia.io/gpu": "H100"}

	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{"gryvia.io/gpu": "H100"},
		},
	}
	node2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-2",
			Labels: map[string]string{"gryvia.io/gpu": "A100"},
		},
	}
	node3 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-3",
			Labels: map[string]string{"gryvia.io/gpu": "H100"},
		},
	}

	r, _ := newNetworkReconciler(network, node1, node2, node3)

	nodes, err := r.getMatchingNodes(context.Background(), network)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("expected 2 matching nodes, got %d", len(nodes))
	}
}

func TestNetwork_GetMatchingNodes_NoSelector(t *testing.T) {
	network := newTestNetwork("test-network")
	network.Spec.NodeSelector = nil

	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
	}
	node2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
	}

	r, _ := newNetworkReconciler(network, node1, node2)

	nodes, err := r.getMatchingNodes(context.Background(), network)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("expected all 2 nodes with no selector, got %d", len(nodes))
	}
}

func TestNetwork_UpdateStatus(t *testing.T) {
	network := newTestNetwork("test-network")
	r, fakeClient := newNetworkReconciler(network)

	r.updateStatus(context.Background(), network, "Ready", "OK")

	updated := &gryviav1.GryviaNetwork{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-network"}, updated); err != nil {
		t.Fatalf("failed to get network: %v", err)
	}
	if updated.Status.Phase != "Ready" {
		t.Errorf("expected phase Ready, got %s", updated.Status.Phase)
	}

	// Check condition
	foundReady := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
			foundReady = true
		}
	}
	if !foundReady {
		t.Error("expected Ready condition True")
	}

	// Failed status
	r.updateStatus(context.Background(), network, "Failed", "error")
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-network"}, updated); err != nil {
		t.Fatalf("failed to get network: %v", err)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == "Ready" && cond.Status != metav1.ConditionFalse {
			t.Error("expected Ready condition False for Failed phase")
		}
	}
}

func TestNetwork_ConfigureRDMA_NilConfig(t *testing.T) {
	network := newTestNetwork("test-network")
	network.Spec.RDMA = nil

	r, _ := newNetworkReconciler(network)

	err := r.configureRDMA(context.Background(), network, nil)
	if err == nil {
		t.Error("expected error for nil RDMA config")
	}
}

func TestNetwork_ConfigureSRIOV_NilConfig(t *testing.T) {
	network := newTestNetwork("test-network")
	network.Spec.NetworkType = "sriov"
	network.Spec.SRIOV = nil

	r, _ := newNetworkReconciler(network)

	err := r.configureSRIOV(context.Background(), network, nil)
	if err == nil {
		t.Error("expected error for nil SR-IOV config")
	}
}
