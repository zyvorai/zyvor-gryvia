// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
)

func gpuLabels() map[string]string {
	return map[string]string{
		"nvidia.com/gpu.present":                  "true",
		"nvidia.com/gpu.product":                  "NVIDIA-H100-80GB-HBM3",
		"nvidia.com/gpu.count":                    "8",
		"nvidia.com/gpu.memory":                   "81559",
		"feature.node.kubernetes.io/rdma.capable": "true",
	}
}

func newDiscoveryReconciler(objs ...client.Object) (*NodeDiscoveryReconciler, client.Client) {
	s := newGpuNodeTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&gryviav1.GryviaGpuNode{}).Build()
	return &NodeDiscoveryReconciler{Client: c, Scheme: s, Log: ctrl.Log.WithName("test")}, c
}

func reconcileDiscovery(t *testing.T, r *NodeDiscoveryReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getGpuNode(c client.Client, name string) (*gryviav1.GryviaGpuNode, error) {
	g := &gryviav1.GryviaGpuNode{}
	err := c.Get(context.Background(), types.NamespacedName{Name: name}, g)
	return g, err
}

func TestDiscovery_CreatesAutoRegistered(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: gpuLabels()}}
	r, c := newDiscoveryReconciler(node)
	reconcileDiscovery(t, r, "n1")

	g, err := getGpuNode(c, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if g.Labels[AutoRegisteredLabel] != "true" {
		t.Errorf("missing owner label: %v", g.Labels)
	}
	want := gryviav1.GryviaGpuNodeSpec{NodeName: "n1", GpuType: "H100", GpuCount: 8, MemoryGB: 80, RDMA: true}
	if g.Spec.NodeName != want.NodeName || g.Spec.GpuType != want.GpuType || g.Spec.GpuCount != 8 ||
		g.Spec.MemoryGB != 80 || !g.Spec.RDMA || g.Spec.SRIOV {
		t.Errorf("spec %+v", g.Spec)
	}
}

func TestDiscovery_NonGPUNodeIgnored(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"a": "b"}}}
	r, c := newDiscoveryReconciler(node)
	reconcileDiscovery(t, r, "n1")
	if _, err := getGpuNode(c, "n1"); !apierrors.IsNotFound(err) {
		t.Errorf("expected not found, got %v", err)
	}
}

func TestDiscovery_UpdatesOnLabelChangeKeepsOtherFields(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: gpuLabels()}}
	r, c := newDiscoveryReconciler(node)
	reconcileDiscovery(t, r, "n1")

	g, _ := getGpuNode(c, "n1")
	g.Spec.Interconnect = "nvlink"
	if err := c.Update(context.Background(), g); err != nil {
		t.Fatal(err)
	}

	n := &corev1.Node{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "n1"}, n)
	n.Labels["nvidia.com/gpu.count"] = "4"
	n.Labels["nvidia.com/gpu.product"] = "NVIDIA-A100-SXM4-80GB"
	if err := c.Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	reconcileDiscovery(t, r, "n1")

	g, _ = getGpuNode(c, "n1")
	if g.Spec.GpuCount != 4 || g.Spec.GpuType != "A100-80G" || g.Spec.Interconnect != "nvlink" {
		t.Errorf("spec %+v", g.Spec)
	}
}

func TestDiscovery_CountFallsBackToAllocatable(t *testing.T) {
	l := gpuLabels()
	delete(l, "nvidia.com/gpu.count")
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: l},
		Status:     corev1.NodeStatus{Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")}},
	}
	r, c := newDiscoveryReconciler(node)
	reconcileDiscovery(t, r, "n1")
	if g, _ := getGpuNode(c, "n1"); g.Spec.GpuCount != 2 {
		t.Errorf("count %d", g.Spec.GpuCount)
	}
}

func TestDiscovery_DoesNotTouchHandMade(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: gpuLabels()}}
	hand := &gryviav1.GryviaGpuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       gryviav1.GryviaGpuNodeSpec{NodeName: "n1", GpuType: "T4", GpuCount: 1},
	}
	r, c := newDiscoveryReconciler(node, hand)
	reconcileDiscovery(t, r, "n1")
	g, _ := getGpuNode(c, "n1")
	if g.Spec.GpuType != "T4" || g.Spec.GpuCount != 1 {
		t.Errorf("hand-made object modified: %+v", g.Spec)
	}

	// Label removed and node deleted: still untouched.
	n := &corev1.Node{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "n1"}, n)
	delete(n.Labels, "nvidia.com/gpu.present")
	_ = c.Update(context.Background(), n)
	reconcileDiscovery(t, r, "n1")
	_ = c.Delete(context.Background(), n)
	reconcileDiscovery(t, r, "n1")
	if _, err := getGpuNode(c, "n1"); err != nil {
		t.Errorf("hand-made object deleted: %v", err)
	}
}

func TestDiscovery_DeletesAutoRegisteredWhenLabelGoneOrNodeDeleted(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: gpuLabels()}}
	r, c := newDiscoveryReconciler(node)
	reconcileDiscovery(t, r, "n1")

	n := &corev1.Node{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "n1"}, n)
	n.Labels["nvidia.com/gpu.present"] = "false"
	_ = c.Update(context.Background(), n)
	reconcileDiscovery(t, r, "n1")
	if _, err := getGpuNode(c, "n1"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected deleted after label loss, got %v", err)
	}

	// Node deleted entirely.
	n.Labels["nvidia.com/gpu.present"] = "true"
	_ = c.Update(context.Background(), n)
	reconcileDiscovery(t, r, "n1")
	if _, err := getGpuNode(c, "n1"); err != nil {
		t.Fatal(err)
	}
	_ = c.Delete(context.Background(), n)
	reconcileDiscovery(t, r, "n1")
	if _, err := getGpuNode(c, "n1"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected deleted after node deletion, got %v", err)
	}
}
